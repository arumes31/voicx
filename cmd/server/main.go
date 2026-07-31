package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/broadcast"
	"voicx/internal/channels"
	"voicx/internal/config"
	"voicx/internal/filetransfer"
	"voicx/internal/health"
	"voicx/internal/logging"
	"voicx/internal/metrics"
	"voicx/internal/permissions"
	"voicx/internal/query"
	"voicx/internal/recorder"
	"voicx/internal/redisx"
	"voicx/internal/server"
	"voicx/internal/state"
	"voicx/internal/store"
	"voicx/internal/version"
	"voicx/internal/webrtc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "voicx: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger, err := logging.New(cfg.DevMode, cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	defer logger.Sync()

	logger.Info("voicx server starting",
		zap.String("version", version.String()),
		zap.String("server_name", cfg.ServerName),
		zap.Bool("dev_mode", cfg.DevMode),
		zap.String("log_level", cfg.LogLevel),
		zap.String("tcp_addr", cfg.TCPAddr),
		zap.String("udp_addr", cfg.UDPAddr),
		zap.String("grpc_addr", cfg.GRPCAddr),
		zap.String("database_url", cfg.DatabaseURL),
		zap.String("redis_addr", cfg.RedisAddr),
		zap.Int("max_clients", cfg.MaxClients),
	)
	logger.Info("config summary", zap.String("config", cfg.Summary()))

	// Initialize the PostgreSQL store and run migrations on startup.
	dbStore, err := store.New(cfg.DatabaseURL, logger,
		cfg.DBMaxOpenConns, cfg.DBMaxIdleConns, cfg.DBConnMaxLifetime)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer dbStore.Close()

	if err := dbStore.Migrate(); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	logger.Info("database migrations applied")

	// First-run bootstrap: with no admin user and no tokens, issue a one-time
	// admin privilege token (TS3-style initial privilege key) and log it.
	bootstrapToken, err := dbStore.BootstrapAdminToken(context.Background())
	if err != nil {
		return fmt.Errorf("bootstrap admin token: %w", err)
	}
	if bootstrapToken != "" {
		logger.Warn("no admin user found: initial admin privilege token created (redeem once via TokenUse)",
			zap.String("token", bootstrapToken),
		)
	}

	// Initialize the authentication service. It wraps the store and provides
	// password (Argon2id) and challenge-response (Ed25519) authentication.
	authSvc := auth.New(dbStore, logger)
	logger.Info("auth service ready")

	// Initialize the in-memory state manager. It tracks connected clients,
	// active channels, channel membership, and current speaking states.
	stateManager := state.New(logger)
	initStats := stateManager.Stats()
	logger.Info("state manager initialized",
		zap.Int("clients", initStats.ClientCount),
		zap.Int("channels", initStats.ChannelCount),
		zap.Int("speaking", initStats.SpeakingCount),
	)

	// Initialize the channel manager. It coordinates channel lifecycle across
	// the database and the in-memory state manager, including automatic cleanup
	// of empty temporary channels.
	channelMgr := channels.New(dbStore, stateManager, logger)
	defer channelMgr.Close()
	logger.Info("channel manager ready",
		zap.Duration("cleanup_delay", channels.DefaultCleanupDelay),
	)

	// Load persisted channels into the in-memory state so the channel tree is
	// complete before any client connects and requests a snapshot.
	loadedChannels, err := channelMgr.LoadIntoState(context.Background())
	if err != nil {
		return fmt.Errorf("loading channels into state: %w", err)
	}
	logger.Info("persisted channels loaded", zap.Int("count", loadedChannels))

	// Initialize the broadcaster. It builds snapshots of the channel tree and
	// active users and delivers them to registered clients via per-client
	// outbound channels.
	broadcaster := broadcast.New(logger, stateManager)
	defer broadcaster.Close()
	logger.Info("broadcaster ready")

	// Initialize the permission loader (DB-backed, with a short-lived cache)
	// and the stateless resolver used by the TCP permission middleware.
	permLoader := permissions.NewLoader(dbStore, logger)
	permResolver := permissions.NewResolver()
	logger.Info("permissions ready")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initialize the Pion WebRTC engine and the voice facade (engine + SFU
	// router) that the TCP control server drives via signaling messages.
	engine, err := webrtc.New(logger, cfg.WebRTC.ICEServers, cfg.WebRTC.EnableAV1)
	if err != nil {
		return fmt.Errorf("initializing webrtc engine: %w", err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			logger.Warn("WebRTC engine shutdown error", zap.Error(err))
		}
	}()
	voiceRouter := webrtc.NewRouter(logger)
	voice := webrtc.NewVoice(engine, voiceRouter, logger)
	logger.Info("voice pipeline ready")

	// Initialize the recorder. It manages ffmpeg subprocesses that record
	// channel streams; it is inert unless recording.enabled is set.
	rec := recorder.New(recorder.Config{
		Enabled:    cfg.Recording.Enabled,
		Dir:        cfg.Recording.Dir,
		FFmpegPath: cfg.Recording.FFmpegPath,
		Format:     cfg.Recording.Format,
		VideoArgs:  cfg.Recording.VideoArgs,
		AudioArgs:  cfg.Recording.AudioArgs,
	}, logger)
	defer rec.Close()

	// Initialize the Redis client when enabled. Redis backs later-phase
	// features (pub/sub, rate limiting); when it is unreachable the server
	// logs a warning and continues without it.
	if cfg.RedisEnabled && cfg.RedisAddr != "" {
		rdb := redisx.New(cfg.RedisAddr, cfg.RedisPassword, logger)
		pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		pingErr := rdb.Ping(pingCtx)
		cancel()
		if pingErr != nil {
			logger.Warn("redis unavailable, continuing without it",
				zap.String("addr", cfg.RedisAddr),
				zap.Error(pingErr),
			)
			_ = rdb.Close()
		} else {
			logger.Info("redis connected", zap.String("addr", cfg.RedisAddr))
			defer rdb.Close()
		}
	} else {
		logger.Info("redis disabled")
	}

	// Start the health/readiness HTTP endpoint. /healthz reports liveness;
	// /readyz pings Postgres. /metrics serves Prometheus metrics.
	m := metrics.New()
	voiceRouter.SetForwardObserver(m.IncRTPForwarded)
	healthServer := health.New(cfg.HealthAddr, logger, func(context.Context) error {
		// Retry once on transient pool errors (e.g. "driver: bad connection"
		// right after the database container restarts).
		err := dbStore.Ping()
		if err != nil && strings.Contains(err.Error(), "bad connection") {
			time.Sleep(100 * time.Millisecond)
			err = dbStore.Ping()
		}
		return err
	})
	healthServer.Handle("/metrics", m.Handler())
	healthErr := make(chan error, 1)
	go func() {
		healthErr <- healthServer.Start()
	}()

	// Construct the file-transfer server before the control server (which
	// references it for token issuance). The control channel issues transfer
	// tokens (permission-checked); the file port trusts only the token.
	ftServer := filetransfer.New(filetransfer.Config{
		Addr:           cfg.FileAddr,
		RootDir:        cfg.FileRoot,
		MaxKBps:        cfg.FileMaxKBps,
		ChannelQuotaMB: cfg.FileChannelQuotaMB,
		MaxSizeMB:      cfg.FileMaxSizeMB,
	}, dbStore, logger)
	ftServer.OnTransferComplete = m.IncFileTransfer
	if err := ftServer.CheckRoot(); err != nil {
		logger.Warn("file storage root not writable, file transfers will fail",
			zap.String("root", cfg.FileRoot),
			zap.Error(err),
		)
	}

	// Global server password (plaintext in config, hashed once at startup
	// with Argon2id). Empty means an open server.
	var serverPasswordHash string
	if cfg.ServerPassword != "" {
		hash, err := auth.HashPassword(cfg.ServerPassword)
		if err != nil {
			return fmt.Errorf("hashing server password: %w", err)
		}
		serverPasswordHash = hash
		logger.Info("server password enabled")
	}

	// Flag channels that have an icon on disk so snapshots reflect it.
	if entries, err := os.ReadDir(filepath.Join(cfg.FileRoot, "icons")); err == nil {
		for _, e := range entries {
			name := e.Name()
			if id, err := strconv.ParseInt(strings.TrimSuffix(name, filepath.Ext(name)), 10, 64); err == nil {
				if ch, ok := stateManager.GetChannel(id); ok {
					ch.HasIcon = true
				}
			}
		}
	}

	// Start the TCP control listener, wired to the auth, state, channels,
	// broadcast, permissions, and voice backends.
	tcpServer := server.New(cfg, logger, &server.Deps{
		Auth:               authSvc,
		State:              stateManager,
		Channels:           channelMgr,
		Broadcast:          broadcaster,
		Perms:              permLoader,
		Resolver:           permResolver,
		Bans:               dbStore,
		Spool:              dbStore,
		Voice:              voice,
		Recorder:           rec,
		FileTransfer:       ftServer,
		Tokens:             dbStore,
		Complaints:         dbStore,
		Metrics:            m,
		ServerPasswordHash: serverPasswordHash,
	})
	serverErr := make(chan error, 1)
	go func() {
		if err := tcpServer.Start(ctx); err != nil {
			serverErr <- err
		}
	}()

	// Start the ServerQuery listener (TS3-style admin/bot protocol). Only
	// server admins may log in.
	queryServer := query.New(cfg.QueryAddr, logger, &queryBackend{
		authSvc:    authSvc,
		stateMgr:   stateManager,
		channelMgr: channelMgr,
		tcp:        tcpServer,
		db:         dbStore,
		startedAt:  time.Now(),
		serverName: cfg.ServerName,
		maxClients: cfg.MaxClients,
	})
	queryErr := make(chan error, 1)
	go func() {
		if err := queryServer.Start(ctx); err != nil {
			queryErr <- err
		}
	}()

	// Start the file-transfer listener.
	ftErr := make(chan error, 1)
	go func() {
		if err := ftServer.Start(ctx); err != nil {
			ftErr <- err
		}
	}()

	// Start the UDP media/signaling listener.
	udpServer := server.NewUDP(cfg, logger)
	udpServer.Metrics = m
	udpErr := make(chan error, 1)
	go func() {
		if err := udpServer.Start(ctx); err != nil {
			udpErr <- err
		}
	}()

	logger.Info("voicx server running, waiting for shutdown signal")
	select {
	case <-ctx.Done():
		logger.Info("voicx server shutting down")
	case err := <-serverErr:
		logger.Error("TCP server exited unexpectedly", zap.Error(err))
	case err := <-udpErr:
		logger.Error("UDP server exited unexpectedly", zap.Error(err))
	case err := <-healthErr:
		logger.Error("health server exited unexpectedly", zap.Error(err))
	case err := <-queryErr:
		logger.Error("query server exited unexpectedly", zap.Error(err))
	case err := <-ftErr:
		logger.Error("file transfer server exited unexpectedly", zap.Error(err))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("health server shutdown error", zap.Error(err))
	}

	if err := tcpServer.Shutdown(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Warn("TCP server shutdown error", zap.Error(err))
	}
	if err := queryServer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Warn("query server shutdown error", zap.Error(err))
	}
	if err := ftServer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Warn("file transfer server shutdown error", zap.Error(err))
	}
	if err := udpServer.Shutdown(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Warn("UDP server shutdown error", zap.Error(err))
	}
	stats := udpServer.Stats()
	logger.Info("udp server stats",
		zap.Uint64("packets_received", stats.PacketsReceived),
		zap.Uint64("packets_dropped", stats.PacketsDropped),
		zap.Uint64("packets_processed", stats.PacketsProcessed),
	)
	return nil
}

// queryBackend adapts the server's building blocks to the query.Backend
// interface. Only admins may log in; every operation after that is trusted.
type queryBackend struct {
	authSvc    *auth.AuthService
	stateMgr   *state.Manager
	channelMgr *channels.ChannelManager
	tcp        *server.TCPServer
	db         *store.Store
	startedAt  time.Time
	serverName string
	maxClients int
}

func (q *queryBackend) Authenticate(ctx context.Context, uniqueID, password string) (bool, bool, error) {
	ok, err := q.authSvc.AuthenticatePassword(ctx, uniqueID, password)
	if err != nil || !ok {
		return false, false, err
	}
	user, err := q.authSvc.LookupUser(ctx, uniqueID)
	if err != nil {
		return false, false, err
	}
	return true, user.IsAdmin, nil
}

func (q *queryBackend) ListClients(context.Context) []query.ClientInfo {
	clients := q.stateMgr.ListClients()
	out := make([]query.ClientInfo, 0, len(clients))
	for _, c := range clients {
		out = append(out, query.ClientInfo{
			ClientID:  c.ClientID,
			UniqueID:  c.UniqueID,
			Nickname:  c.Nickname,
			ChannelID: c.ChannelID,
		})
	}
	return out
}

func (q *queryBackend) ListChannels(context.Context) []query.ChannelInfo {
	channels := q.stateMgr.ChannelTree()
	out := make([]query.ChannelInfo, 0, len(channels))
	for _, ch := range channels {
		out = append(out, query.ChannelInfo{
			ChannelID:   ch.ChannelID,
			ParentID:    ch.ParentID,
			Name:        ch.Name,
			Type:        ch.ChannelType,
			ClientCount: ch.ClientCount,
		})
	}
	return out
}

func (q *queryBackend) ServerInfo(context.Context) query.Info {
	stats := q.stateMgr.Stats()
	return query.Info{
		Name:           q.serverName,
		Uptime:         time.Since(q.startedAt),
		ClientsOnline:  stats.ClientCount,
		MaxClients:     q.maxClients,
		ChannelsOnline: stats.ChannelCount,
	}
}

func (q *queryBackend) MoveClient(_ context.Context, clientID string, channelID int64) error {
	return q.tcp.MoveClient(clientID, channelID)
}

func (q *queryBackend) KickClient(_ context.Context, clientID string, fromServer bool, reason string) error {
	return q.tcp.KickClient("serverquery", clientID, fromServer, reason)
}

func (q *queryBackend) SendText(_ context.Context, targetMode int, target, msg string) error {
	return q.tcp.SendServerText(targetMode, target, msg)
}

func (q *queryBackend) CreateChannel(ctx context.Context, name, topic string, channelType int) (int64, error) {
	return q.channelMgr.CreateChannel(ctx, channels.ChannelSpec{
		Name:  name,
		Topic: topic,
		Type:  channels.ChannelType(channelType),
	})
}

func (q *queryBackend) DeleteChannel(ctx context.Context, channelID int64) error {
	return q.channelMgr.DeleteChannel(ctx, channelID)
}

func (q *queryBackend) BanClient(ctx context.Context, clientID string, seconds int64, reason string) error {
	return q.tcp.BanClient(ctx, "serverquery", clientID, seconds, reason)
}

func (q *queryBackend) ListComplaints(ctx context.Context) ([]query.Complaint, error) {
	rows, err := q.db.ListComplaints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]query.Complaint, 0, len(rows))
	for _, c := range rows {
		out = append(out, query.Complaint{
			ID:        c.ID,
			Reporter:  c.Reporter,
			Target:    c.Target,
			Reason:    c.Reason,
			CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

func (q *queryBackend) DeleteComplaint(ctx context.Context, id int64) error {
	return q.db.DeleteComplaint(ctx, id)
}

func (q *queryBackend) DeleteAllComplaints(ctx context.Context) error {
	return q.db.DeleteAllComplaints(ctx)
}

func (q *queryBackend) TokenAdd(ctx context.Context, tokenType int, groupID int64) (string, error) {
	return q.db.CreateToken(ctx, tokenType, groupID, 1)
}

func (q *queryBackend) TokenList(ctx context.Context) ([]query.Token, error) {
	rows, err := q.db.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]query.Token, 0, len(rows))
	for _, t := range rows {
		out = append(out, query.Token{
			Key:     t.Key,
			Type:    t.Type,
			GroupID: t.GroupID,
			Uses:    t.Uses,
			MaxUses: t.MaxUses,
		})
	}
	return out, nil
}

func (q *queryBackend) TokenDelete(ctx context.Context, key string) error {
	return q.db.DeleteToken(ctx, key)
}
