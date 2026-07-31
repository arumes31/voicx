// Package filetransfer implements the voicx file-transfer system: a
// token-authorized TCP server for uploading and downloading channel files.
//
// Model (TS3-inspired): the control channel issues short-lived, single-use
// transfer tokens after permission and quota checks (see InitUpload /
// InitDownload). The file-transfer port trusts only the token — no further
// permission logic runs here. Transfers use the netproto framing (4-byte
// length + 2-byte type + payload) with a tiny frame vocabulary (see
// protocol.go): one init frame authenticates the connection, raw chunk
// frames carry the data, and a digest frame carrying the SHA-256 of the file
// closes the transfer. Uploads are written to <root>/<channel_id>/<name>.part
// and atomically renamed on success; a same-name upload replaces the
// previous file (both on disk and in the files table).
//
// Per-channel file passwords (TS3 b_ft_ignore_password) are out of scope.
package filetransfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"voicx/internal/store"
)

// tokenTTL is how long an issued transfer token stays valid. It is a var so
// tests can shorten it (mirrors authCacheTTL in the auth package).
var tokenTTL = 60 * time.Second

// ErrQuotaExceeded is returned by InitUpload when the channel's file quota
// would be exceeded.
var ErrQuotaExceeded = errors.New("filetransfer: channel quota exceeded")

// ErrTooLarge is returned by InitUpload when the declared size exceeds the
// per-transfer maximum.
var ErrTooLarge = errors.New("filetransfer: file too large")

// ErrInvalidName is returned when a file name fails sanitization.
var ErrInvalidName = errors.New("filetransfer: invalid file name")

// FileStore is the subset of the store the file-transfer server needs. It is
// satisfied by *store.Store.
type FileStore interface {
	AddFile(ctx context.Context, rec store.FileRecord) error
	GetFile(ctx context.Context, channelID int64, name string) (*store.FileRecord, error)
	ListFiles(ctx context.Context, channelID int64) ([]store.FileRecord, error)
	ChannelFileUsage(ctx context.Context, channelID int64) (int64, error)
}

// Config holds the file-transfer server settings (populated from the
// "file_*" config keys).
type Config struct {
	// Addr is the listen address (e.g. ":30033").
	Addr string
	// RootDir is where uploaded files are stored, laid out per channel.
	RootDir string
	// MaxKBps is the per-connection bandwidth cap in KiB/s. 0 = unlimited.
	MaxKBps int
	// ChannelQuotaMB is the per-channel total file size quota in MiB.
	// 0 = unlimited.
	ChannelQuotaMB int64
	// MaxSizeMB is the per-transfer size cap in MiB. 0 = unlimited.
	MaxSizeMB int64
}

// transfer is a pending (token-issued, not yet consumed) file transfer.
type transfer struct {
	ID        string
	Token     string
	Direction string // "upload" | "download"
	ChannelID int64
	Name      string
	Size      int64
	Uploader  string
	Expires   time.Time
}

// Server is the file-transfer listener and token registry.
type Server struct {
	cfg    Config
	store  FileStore
	logger *zap.Logger
	port   int

	// OnTransferComplete, when set, is called with the direction and result
	// ("ok"/"error") when a transfer finishes (metrics).
	OnTransferComplete func(direction, result string)

	listener net.Listener

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	mu        sync.Mutex
	transfers map[string]*transfer // keyed by token
}

// New constructs a Server. The listener is created lazily in Start.
func New(cfg Config, st FileStore, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	port := 0
	if _, p, err := net.SplitHostPort(cfg.Addr); err == nil {
		port, _ = strconv.Atoi(p)
	}
	return &Server{
		cfg:       cfg,
		store:     st,
		logger:    logger,
		port:      port,
		stopCh:    make(chan struct{}),
		transfers: make(map[string]*transfer),
	}
}

// Port returns the configured file-transfer port (0 when the address has no
// numeric port), used in FileTransferInitResponse.
func (s *Server) Port() int {
	return s.port
}

// InitUpload issues a single-use upload token after validating the name,
// the per-transfer size cap, and the channel quota. uploader is the
// initiating user's unique ID, recorded in the files table.
func (s *Server) InitUpload(ctx context.Context, channelID int64, name string, size int64, uploader string) (string, string, error) {
	name, err := sanitizeName(name)
	if err != nil {
		return "", "", err
	}
	if size <= 0 {
		return "", "", fmt.Errorf("%w: size must be positive", ErrTooLarge)
	}
	if s.cfg.MaxSizeMB > 0 && size > s.cfg.MaxSizeMB<<20 {
		return "", "", fmt.Errorf("%w: %d bytes exceeds the %d MiB limit", ErrTooLarge, size, s.cfg.MaxSizeMB)
	}

	if s.cfg.ChannelQuotaMB > 0 {
		usage, err := s.store.ChannelFileUsage(ctx, channelID)
		if err != nil {
			return "", "", fmt.Errorf("checking channel quota: %w", err)
		}
		if usage+size > s.cfg.ChannelQuotaMB<<20 {
			return "", "", fmt.Errorf("%w: %d MiB", ErrQuotaExceeded, s.cfg.ChannelQuotaMB)
		}
	}

	return s.register(&transfer{
		Direction: "upload",
		ChannelID: channelID,
		Name:      name,
		Size:      size,
		Uploader:  uploader,
	})
}

// InitDownload issues a single-use download token for an existing file.
func (s *Server) InitDownload(ctx context.Context, channelID int64, name string) (string, string, error) {
	name, err := sanitizeName(name)
	if err != nil {
		return "", "", err
	}
	rec, err := s.store.GetFile(ctx, channelID, name)
	if err != nil {
		return "", "", err
	}
	return s.register(&transfer{
		Direction: "download",
		ChannelID: channelID,
		Name:      rec.Name,
		Size:      rec.Size,
	})
}

// ListFiles returns the channel's file listing (passthrough to the store).
func (s *Server) ListFiles(ctx context.Context, channelID int64) ([]store.FileRecord, error) {
	return s.store.ListFiles(ctx, channelID)
}

// register creates a transfer ID and token and records the pending transfer.
func (s *Server) register(tr *transfer) (string, string, error) {
	id, err := randomHex(8)
	if err != nil {
		return "", "", err
	}
	token, err := randomHex(16)
	if err != nil {
		return "", "", err
	}
	tr.ID = id
	tr.Token = token
	tr.Expires = time.Now().Add(tokenTTL)

	s.mu.Lock()
	s.transfers[token] = tr
	s.mu.Unlock()
	return id, token, nil
}

// consume validates and consumes a token (single-use). It returns the
// transfer or an error when the token is unknown or expired.
func (s *Server) consume(token, transferID string) (*transfer, error) {
	s.mu.Lock()
	tr, ok := s.transfers[token]
	if ok {
		delete(s.transfers, token)
	}
	s.mu.Unlock()

	if !ok || tr.ID != transferID {
		return nil, errors.New("invalid transfer token")
	}
	if time.Now().After(tr.Expires) {
		return nil, errors.New("transfer token expired")
	}
	return tr, nil
}

// pendingCount returns the number of unconsumed tokens (for tests and
// observability).
func (s *Server) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.transfers)
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// sanitizeName validates a file name for on-disk storage: it must be a bare
// base name — no separators, no parent traversal, no absolute paths.
func sanitizeName(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if filepath.IsAbs(name) || filepath.Base(name) != name {
		return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return name, nil
}

// filePath returns the on-disk path for a channel file.
func (s *Server) filePath(channelID int64, name string) string {
	return filepath.Join(s.cfg.RootDir, strconv.FormatInt(channelID, 10), name)
}

// CheckRoot verifies the storage root exists and is writable, creating it if
// needed. It is called at startup so a misconfigured volume is logged loudly
// instead of failing on the first upload.
func (s *Server) CheckRoot() error {
	if err := os.MkdirAll(s.cfg.RootDir, 0o750); err != nil {
		return fmt.Errorf("creating file root %s: %w", s.cfg.RootDir, err)
	}
	probe := filepath.Join(s.cfg.RootDir, ".writetest")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("file root %s is not writable: %w", s.cfg.RootDir, err)
	}
	_ = os.Remove(probe)
	return nil
}

// Start binds the listener and serves connections until ctx is cancelled or
// Close is called.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("filetransfer listen on %s: %w", s.cfg.Addr, err)
	}
	s.listener = ln
	s.logger.Info("file transfer listener started", zap.String("addr", s.cfg.Addr))

	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.stopCh:
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return nil
			default:
				return fmt.Errorf("filetransfer accept: %w", err)
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(ctx, conn)
		}()
	}
}

// Close stops the listener and waits for active transfers. It is safe to
// call multiple times.
func (s *Server) Close() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.listener != nil {
			err = s.listener.Close()
		}
	})
	s.wg.Wait()
	return err
}
