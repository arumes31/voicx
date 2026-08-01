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
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/query"
	"voicx/internal/recorder"
	"voicx/internal/redisx"
	"voicx/internal/server"
	"voicx/internal/state"
	"voicx/internal/store"
	"voicx/internal/turn"
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

	// (223) tee log lines into the in-memory ring buffer for `logview`.
	logger = logger.WithOptions(logging.Tee())

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

	// Default groups (138/143/144): ensure the Guest and Member server groups
	// exist so the TCP server can assign them at login. Seeding is idempotent
	// and disabled via default_groups_enabled=false.
	var defaultGuestGroupID, defaultMemberGroupID int64
	if cfg.DefaultGroupsEnabled {
		if defaultGuestGroupID, err = ensureServerGroup(context.Background(), dbStore, "Guest", 0); err != nil {
			return fmt.Errorf("ensuring default Guest group: %w", err)
		}
		if defaultMemberGroupID, err = ensureServerGroup(context.Background(), dbStore, "Member", 100); err != nil {
			return fmt.Errorf("ensuring default Member group: %w", err)
		}
		logger.Info("default groups ready",
			zap.Int64("guest_group_id", defaultGuestGroupID),
			zap.Int64("member_group_id", defaultMemberGroupID),
		)
	}

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

	// Echo test channel (15): ensure the loopback channel exists; publishers
	// in it hear their own audio routed back. Empty name disables it.
	var echoChannelID int64
	if cfg.EchoChannelName != "" {
		for _, ch := range stateManager.ChannelTree() {
			if ch.Name == cfg.EchoChannelName {
				echoChannelID = ch.ChannelID
				break
			}
		}
		if echoChannelID == 0 {
			id, err := channelMgr.CreateChannel(context.Background(), channels.ChannelSpec{
				Name: cfg.EchoChannelName,
				Type: channels.ChannelTypePermanent,
			})
			if err != nil {
				return fmt.Errorf("creating echo test channel: %w", err)
			}
			echoChannelID = id
			logger.Info("echo test channel created",
				zap.String("name", cfg.EchoChannelName),
				zap.Int64("channel_id", id),
			)
		}
	}

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
	voice.SetEchoChannel(echoChannelID)
	// Per-channel Opus audio configuration (21-25): SDP fmtp rewriting and
	// music-channel talk-gate bypass read the channel's stored settings.
	voice.SetChannelAudioLookup(func(channelID int64) webrtc.ChannelAudio {
		ch, ok := stateManager.GetChannel(channelID)
		if !ok {
			return webrtc.ChannelAudio{}
		}
		return webrtc.ChannelAudio{
			Bitrate: ch.OpusBitrate,
			FEC:     ch.OpusFEC,
			DTX:     ch.OpusDTX,
			Stereo:  ch.OpusStereo,
		}
	})
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
	// Download links (267): the control channel mints expiring tokens; the
	// health HTTP server serves them (LAN-friendly, no extra auth).
	healthServer.Handle("/dl/", ftServer.Links())
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
		Chat:               dbStore,
		Groups:             dbStore,
		BanAdmin:           dbStore,
		Metrics:            m,
		ServerPasswordHash: serverPasswordHash,
		ICEServers:         iceServersProvider(cfg, logger),

		DefaultGuestGroupID:  defaultGuestGroupID,
		DefaultMemberGroupID: defaultMemberGroupID,
	})
	serverErr := make(chan error, 1)
	go func() {
		if err := tcpServer.Start(ctx); err != nil {
			serverErr <- err
		}
	}()

	// Timed group memberships (145): reap expired rows every 60s, invalidate
	// the permission cache, and notify affected online users.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tcpServer.ReapExpiredGroups(ctx)
			}
		}
	}()

	// Start the ServerQuery listener (TS3-style admin/bot protocol). Only
	// server admins may log in.
	// (218) serverstop/serverrestart: a query-initiated graceful shutdown
	// takes the same path as SIGTERM. restart only logs the supervisor
	// dependency (docker restart=unless-stopped brings the server back).
	shutdownReq := make(chan bool, 1)
	queryServer := query.New(cfg.QueryAddr, logger, &queryBackend{
		authSvc:    authSvc,
		stateMgr:   stateManager,
		channelMgr: channelMgr,
		tcp:        tcpServer,
		db:         dbStore,
		permLoader: permLoader,
		shutdown: func(restart bool) {
			if restart {
				logger.Warn("serverrestart via ServerQuery: stopping gracefully; a supervisor (docker restart policy) must restart the process")
			} else {
				logger.Warn("serverstop via ServerQuery: shutting down gracefully")
			}
			shutdownReq <- restart
		},
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
	case restart := <-shutdownReq:
		logger.Info("voicx server shutting down via ServerQuery", zap.Bool("restart", restart))
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

// ensureServerGroup returns the ID of the named server group, creating it
// when absent (idempotent default-group seeding).
func ensureServerGroup(ctx context.Context, dbStore *store.Store, name string, sortID int) (int64, error) {
	g, err := dbStore.FindGroupByName(ctx, "server", name)
	if err != nil {
		return 0, err
	}
	if g != nil {
		return g.ID, nil
	}
	return dbStore.CreateGroup(ctx, "server", name, sortID)
}

// iceServersProvider builds the Deps.ICEServers callback: clients receive the
// configured STUN servers plus, when turn.secret is set, a TURN entry with
// time-limited REST API credentials minted per client unique ID (445/446).
// It returns nil when there is nothing to deliver (client then uses its own
// defaults).
func iceServersProvider(cfg *config.Config, logger *zap.Logger) func(string) []netproto.ICEServer {
	var stun []string
	for _, u := range cfg.WebRTC.ICEServers {
		if strings.HasPrefix(u, "stun:") {
			stun = append(stun, u)
		}
	}
	turnEnabled := cfg.TURN.Secret != "" && len(cfg.TURN.URIs) > 0
	if !turnEnabled && len(stun) == 0 {
		return nil
	}
	if cfg.TURN.Secret != "" && !turnEnabled {
		logger.Warn("turn.secret set but turn.uris is empty; TURN disabled")
	}
	return func(uniqueID string) []netproto.ICEServer {
		var out []netproto.ICEServer
		if len(stun) > 0 {
			out = append(out, netproto.ICEServer{URLs: stun})
		}
		if turnEnabled {
			username, credential := turn.Credentials(cfg.TURN.Secret, uniqueID, cfg.TURN.CredentialsTTL, time.Now())
			out = append(out, netproto.ICEServer{
				URLs:       cfg.TURN.URIs,
				Username:   username,
				Credential: credential,
			})
		}
		return out
	}
}

// queryBackend adapts the server's building blocks to the query.Backend
// interface. Only admins may log in; every operation after that is trusted.
type queryBackend struct {
	authSvc    *auth.AuthService
	stateMgr   *state.Manager
	channelMgr *channels.ChannelManager
	tcp        *server.TCPServer
	db         *store.Store
	permLoader *permissions.Loader
	shutdown   func(restart bool)
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
			ChannelID:       ch.ChannelID,
			ParentID:        ch.ParentID,
			Name:            ch.Name,
			Topic:           ch.Topic,
			Type:            ch.ChannelType,
			MaxClients:      ch.MaxClients,
			ClientCount:     ch.ClientCount,
			OpusBitrate:     ch.OpusBitrate,
			OpusFEC:         ch.OpusFEC,
			OpusDTX:         ch.OpusDTX,
			OpusStereo:      ch.OpusStereo,
			SlowModeSeconds: ch.SlowModeSeconds,
		})
	}
	return out
}

func (q *queryBackend) ChannelInfo(_ context.Context, channelID int64) (query.ChannelInfo, bool) {
	ch, ok := q.stateMgr.GetChannel(channelID)
	if !ok {
		return query.ChannelInfo{}, false
	}
	return query.ChannelInfo{
		ChannelID:       ch.ChannelID,
		ParentID:        ch.ParentID,
		Name:            ch.Name,
		Topic:           ch.Topic,
		Type:            ch.ChannelType,
		MaxClients:      ch.MaxClients,
		ClientCount:     ch.ClientCount,
		OpusBitrate:     ch.OpusBitrate,
		OpusFEC:         ch.OpusFEC,
		OpusDTX:         ch.OpusDTX,
		OpusStereo:      ch.OpusStereo,
		SlowModeSeconds: ch.SlowModeSeconds,
	}, true
}

func (q *queryBackend) EditChannel(ctx context.Context, channelID int64, params query.ChannelEditParams) error {
	if err := q.channelMgr.UpdateChannel(ctx, channelID, channels.ChannelUpdate{
		Topic:           params.Topic,
		MaxClients:      params.MaxClients,
		OpusBitrate:     params.OpusBitrate,
		OpusFEC:         params.OpusFEC,
		OpusDTX:         params.OpusDTX,
		OpusStereo:      params.OpusStereo,
		SlowModeSeconds: params.SlowModeSeconds,
	}); err != nil {
		return err
	}
	q.db.Audit(ctx, "serverquery", "channel_edit", strconv.FormatInt(channelID, 10), "")
	// Keep connected clients in sync (the control-channel edit path does the
	// same after MsgChannelEdit).
	q.tcp.BroadcastChannelUpdated(channelID)
	return nil
}

func (q *queryBackend) ServerInfo(ctx context.Context) query.Info {
	stats := q.stateMgr.Stats()
	return query.Info{
		Name:           q.effectiveName(ctx),
		Uptime:         time.Since(q.startedAt),
		ClientsOnline:  stats.ClientCount,
		MaxClients:     q.tcp.EffectiveMaxClients(ctx),
		ChannelsOnline: stats.ChannelCount,
		TLSFingerprint: q.tcp.TLSFingerprint(),
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
	id, err := q.channelMgr.CreateChannel(ctx, channels.ChannelSpec{
		Name:  name,
		Topic: topic,
		Type:  channels.ChannelType(channelType),
	})
	if err != nil {
		return 0, err
	}
	q.db.Audit(ctx, "serverquery", "channel_create", strconv.FormatInt(id, 10), name)
	return id, nil
}

func (q *queryBackend) DeleteChannel(ctx context.Context, channelID int64) error {
	if err := q.channelMgr.DeleteChannel(ctx, channelID); err != nil {
		return err
	}
	q.db.Audit(ctx, "serverquery", "channel_delete", strconv.FormatInt(channelID, 10), "")
	return nil
}

func (q *queryBackend) ServerSet(ctx context.Context, key, value string) error {
	return q.tcp.SetServerSettingAndAnnounce(ctx, key, value)
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
	key, err := q.db.CreateToken(ctx, tokenType, groupID, 1)
	if err != nil {
		return "", err
	}
	q.db.Audit(ctx, "serverquery", "token_create", strconv.FormatInt(groupID, 10), fmt.Sprintf("type=%d", tokenType))
	return key, nil
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

// AuditLog returns the newest audit entries (149), newest first.
func (q *queryBackend) AuditLog(ctx context.Context, limit int) ([]query.AuditEntry, error) {
	rows, err := q.db.AuditList(ctx, 0, limit)
	if err != nil {
		return nil, err
	}
	out := make([]query.AuditEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, query.AuditEntry{
			ID:        e.ID,
			Actor:     e.Actor,
			Action:    e.Action,
			Target:    e.Target,
			Detail:    e.Detail,
			CreatedAt: e.CreatedAt,
		})
	}
	return out, nil
}

// --- wave 10a (217-223) -------------------------------------------------------

// effectiveName returns the effective server name (217 serveredit override).
func (q *queryBackend) effectiveName(ctx context.Context) string {
	if name, err := q.db.GetServerSetting(ctx, "server_name"); err == nil && name != "" {
		return name
	}
	return q.serverName
}

func (q *queryBackend) ServerEdit(ctx context.Context, name, welcome string, maxClients int) error {
	if name != "" {
		if err := q.db.SetServerSetting(ctx, "server_name", name); err != nil {
			return err
		}
	}
	if welcome != "" {
		if err := q.db.SetServerSetting(ctx, "motd", welcome); err != nil {
			return err
		}
	}
	if maxClients > 0 {
		if err := q.db.SetServerSetting(ctx, "max_clients_override", fmt.Sprint(maxClients)); err != nil {
			return err
		}
	}
	q.db.Audit(ctx, "serverquery", "serveredit", name,
		fmt.Sprintf("welcome=%t maxclients=%d", welcome != "", maxClients))
	return nil
}

func (q *queryBackend) Shutdown(_ context.Context, restart bool) error {
	q.shutdown(restart)
	return nil
}

func (q *queryBackend) PermOverview(ctx context.Context, uniqueID string, channelID int64) ([]query.PermLine, error) {
	lines, err := q.tcp.PermOverview(ctx, uniqueID, channelID)
	if err != nil {
		return nil, err
	}
	out := make([]query.PermLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, query.PermLine{Key: l.Key, Value: l.Value, Grant: l.Grant, Tier: l.Tier})
	}
	return out, nil
}

func (q *queryBackend) ChannelPermList(ctx context.Context, channelID int64) ([]query.ChannelPerm, error) {
	entries, err := q.db.ListPermissions(ctx, store.PermTierChannel, store.PermTarget{ChannelID: channelID})
	if err != nil {
		return nil, err
	}
	out := make([]query.ChannelPerm, 0, len(entries))
	for _, e := range entries {
		out = append(out, query.ChannelPerm{Key: e.Key, Value: e.Value, Grant: e.Grant, Skip: e.Skip, Negate: e.Negate})
	}
	return out, nil
}

func (q *queryBackend) ChannelAddPerm(ctx context.Context, actor string, channelID int64, key string, value, grant int, skip, negate bool) error {
	if err := q.db.SetPermission(ctx, store.PermTierChannel, store.PermTarget{ChannelID: channelID}, key, value, grant, skip, negate); err != nil {
		return err
	}
	q.db.Audit(ctx, actor, "perm_set", fmt.Sprintf("channel/%s", key),
		fmt.Sprintf("cid=%d value=%d grant=%d skip=%t negate=%t", channelID, value, grant, skip, negate))
	q.permInvalidate()
	return nil
}

func (q *queryBackend) ChannelDelPerm(ctx context.Context, actor string, channelID int64, key string) error {
	if err := q.db.UnsetPermission(ctx, store.PermTierChannel, store.PermTarget{ChannelID: channelID}, key); err != nil {
		return err
	}
	q.db.Audit(ctx, actor, "perm_unset", fmt.Sprintf("channel/%s", key), fmt.Sprintf("cid=%d", channelID))
	q.permInvalidate()
	return nil
}

// permInvalidate clears the permission cache after query-side writes.
func (q *queryBackend) permInvalidate() {
	if q.permLoader != nil {
		q.permLoader.InvalidateAll()
	}
}

func (q *queryBackend) ServerGroupAdd(ctx context.Context, actor, name string, sortID int) (int64, error) {
	id, err := q.db.CreateGroup(ctx, "server", name, sortID)
	if err != nil {
		return 0, err
	}
	q.db.Audit(ctx, actor, "group_create", "server:"+name, fmt.Sprintf("id=%d", id))
	return id, nil
}

func (q *queryBackend) ServerGroupDel(ctx context.Context, actor string, groupID int64, force bool) error {
	if err := q.db.DeleteGroup(ctx, "server", groupID, force); err != nil {
		return err
	}
	q.db.Audit(ctx, actor, "group_delete", fmt.Sprintf("server:%d", groupID), fmt.Sprintf("force=%t", force))
	q.permInvalidate()
	return nil
}

func (q *queryBackend) ServerGroupAddClient(ctx context.Context, actor string, groupID int64, uniqueID string, durationSeconds int64) error {
	user, err := q.authSvc.LookupUser(ctx, uniqueID)
	if err != nil {
		return err
	}
	if err := q.db.AssignServerGroup(ctx, groupID, user.ID, time.Duration(durationSeconds)*time.Second); err != nil {
		return err
	}
	q.db.Audit(ctx, actor, "group_assign", fmt.Sprintf("server:%d", groupID),
		fmt.Sprintf("user=%s expires_in=%ds", uniqueID, durationSeconds))
	q.permInvalidate()
	return nil
}

func (q *queryBackend) ServerGroupDelClient(ctx context.Context, actor string, groupID int64, uniqueID string) error {
	user, err := q.authSvc.LookupUser(ctx, uniqueID)
	if err != nil {
		return err
	}
	if err := q.db.UnassignServerGroup(ctx, groupID, user.ID); err != nil {
		return err
	}
	q.db.Audit(ctx, actor, "group_unassign", fmt.Sprintf("server:%d", groupID), "user="+uniqueID)
	q.permInvalidate()
	return nil
}

func (q *queryBackend) ServerGroupList(ctx context.Context) ([]query.GroupInfo, error) {
	groups, err := q.db.ListGroups(ctx, "server")
	if err != nil {
		return nil, err
	}
	out := make([]query.GroupInfo, 0, len(groups))
	for _, g := range groups {
		out = append(out, query.GroupInfo{ID: g.ID, Name: g.Name, SortID: g.SortID, MemberCount: g.MemberCount})
	}
	return out, nil
}

func (q *queryBackend) ServerGroupClientList(ctx context.Context, groupID int64) ([]query.GroupMemberInfo, error) {
	members, err := q.db.ListServerGroupMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]query.GroupMemberInfo, 0, len(members))
	for _, m := range members {
		var expires int64
		if m.ExpiresAt != nil {
			expires = m.ExpiresAt.Unix()
		}
		out = append(out, query.GroupMemberInfo{UniqueID: m.UniqueID, Nickname: m.Nickname, ExpiresAt: expires})
	}
	return out, nil
}

func (q *queryBackend) CustomSet(ctx context.Context, uniqueID, key, value string) error {
	return q.db.CustomSet(ctx, uniqueID, key, value)
}

func (q *queryBackend) CustomDel(ctx context.Context, uniqueID, key string) error {
	return q.db.CustomDel(ctx, uniqueID, key)
}

func (q *queryBackend) CustomInfo(ctx context.Context, uniqueID string) ([]query.CustomProp, error) {
	entries, err := q.db.CustomInfo(ctx, uniqueID)
	if err != nil {
		return nil, err
	}
	out := make([]query.CustomProp, 0, len(entries))
	for _, e := range entries {
		out = append(out, query.CustomProp{Key: e.Key, Value: e.Value})
	}
	return out, nil
}

func (q *queryBackend) LogView(_ context.Context, lines int, filter string) ([]string, error) {
	return logging.Recent(lines, filter), nil
}
