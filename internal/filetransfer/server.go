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
// The port speaks TLS (91-135). The single-use token is minted on the TLS
// control channel and then presented here, so a plaintext data port leaks the
// token as well as the bytes: an on-path observer could lift it and pull the
// file itself. The certificate is the one the control channel already
// presents, so clients re-use the fingerprint they have already pinned.
//
// The package has no awareness of chat-attachment encryption: it moves opaque
// bytes. Chat attachments are sealed by the client before upload and arrive
// here under a content-derived ".vcx" name; the only two places that care are
// the files.encrypted flag and the download-link refusal in links.go.
//
// Per-channel file passwords (TS3 b_ft_ignore_password) are out of scope.
package filetransfer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
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

// ErrEncryptedAttachment is returned when an expiring download link (267) is
// requested for a client-encrypted chat attachment.
var ErrEncryptedAttachment = errors.New("encrypted chat attachment — open it in the client")

// encryptedSuffix marks a client-encrypted chat attachment (91-135): the blob
// is nonce||secretbox and its key lives only inside the encrypted chat body,
// so nothing server-side can ever read it back.
const encryptedSuffix = ".vcx"

// isEncryptedAttachment reports whether a stored name is a client-encrypted
// chat attachment. The name is content-derived by the client
// (sha256(ciphertext) + ".vcx"), which is why these never collide with an
// existing name and so never reach the .v1..v3 rotation path.
func isEncryptedAttachment(name string) bool {
	return strings.HasSuffix(name, encryptedSuffix)
}

// FileStore is the subset of the store the file-transfer server needs. It is
// satisfied by *store.Store.
type FileStore interface {
	AddFile(ctx context.Context, rec store.FileRecord) error
	GetFile(ctx context.Context, channelID int64, folder, name string) (*store.FileRecord, error)
	ListFiles(ctx context.Context, channelID int64, folder string) ([]store.FileRecord, error)
	ListFileFolders(ctx context.Context, channelID int64) ([]string, error)
	ListFileVersions(ctx context.Context, channelID int64, folder, baseName string) ([]store.FileRecord, error)
	RenameFile(ctx context.Context, channelID int64, folder, name, newFolder, newName string) error
	DeleteFile(ctx context.Context, channelID int64, folder, name string) error
	FindFileBySHA(ctx context.Context, channelID int64, sha256, exclFolder, exclName string) (*store.FileRecord, error)
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
	// TLSEnabled wraps the listener in TLS (91-135). False is a dev-only
	// escape hatch: it leaks both the file bytes and the transfer token.
	TLSEnabled bool
	// Cert is the certificate presented on the data port. It is the same
	// certificate as the control channel, so clients need no second trust
	// decision.
	Cert tls.Certificate
	// Fingerprint is Cert's SHA-256 fingerprint, handed to clients in
	// FileTransferInitResponse so they can pin it.
	Fingerprint string
}

// transfer is a pending (token-issued, not yet consumed) file transfer.
type transfer struct {
	ID        string
	Token     string
	Direction string // "upload" | "download"
	ChannelID int64
	Folder    string
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

	// links is the download-link registry (267), served by the health HTTP
	// server at /dl/<token>.
	links *LinkRegistry

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
		links:     NewLinkRegistry(),
		stopCh:    make(chan struct{}),
		transfers: make(map[string]*transfer),
	}
}

// Port returns the configured file-transfer port (0 when the address has no
// numeric port), used in FileTransferInitResponse.
func (s *Server) Port() int {
	return s.port
}

// Fingerprint returns the SHA-256 fingerprint of the certificate this port
// presents, empty when TLS is disabled. Clients cross-check it against the
// control-channel pin so a hostile server cannot redirect transfers to a
// third-party host.
func (s *Server) Fingerprint() string {
	if !s.cfg.TLSEnabled {
		return ""
	}
	return s.cfg.Fingerprint
}

// InitUpload issues a single-use upload token after validating the folder
// and name, the per-transfer size cap, and the channel quota. uploader is
// the initiating user's unique ID, recorded in the files table.
func (s *Server) InitUpload(ctx context.Context, channelID int64, folder, name string, size int64, uploader string) (string, string, error) {
	name, err := sanitizeName(name)
	if err != nil {
		return "", "", err
	}
	folder, err = sanitizeFolder(folder)
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
		Folder:    folder,
		Name:      name,
		Size:      size,
		Uploader:  uploader,
	})
}

// InitDownload issues a single-use download token for an existing file.
func (s *Server) InitDownload(ctx context.Context, channelID int64, folder, name string) (string, string, error) {
	name, err := sanitizeName(name)
	if err != nil {
		return "", "", err
	}
	folder, err = sanitizeFolder(folder)
	if err != nil {
		return "", "", err
	}
	rec, err := s.store.GetFile(ctx, channelID, folder, name)
	if err != nil {
		return "", "", err
	}
	return s.register(&transfer{
		Direction: "download",
		ChannelID: channelID,
		Folder:    folder,
		Name:      rec.Name,
		Size:      rec.Size,
	})
}

// ListFiles returns the channel's file listing for one folder (passthrough
// to the store).
func (s *Server) ListFiles(ctx context.Context, channelID int64, folder string) ([]store.FileRecord, error) {
	folder, err := sanitizeFolder(folder)
	if err != nil {
		return nil, err
	}
	return s.store.ListFiles(ctx, channelID, folder)
}

// ListFileFolders returns the channel's virtual folders (derived from file
// rows, 261).
func (s *Server) ListFileFolders(ctx context.Context, channelID int64) ([]string, error) {
	return s.store.ListFileFolders(ctx, channelID)
}

// ListFileVersions returns the rotated old versions of a file (264).
func (s *Server) ListFileVersions(ctx context.Context, channelID int64, folder, name string) ([]store.FileRecord, error) {
	name, err := sanitizeName(name)
	if err != nil {
		return nil, err
	}
	folder, err = sanitizeFolder(folder)
	if err != nil {
		return nil, err
	}
	return s.store.ListFileVersions(ctx, channelID, folder, name)
}

// DeleteFile removes a file record and its blob (263).
func (s *Server) DeleteFile(ctx context.Context, channelID int64, folder, name string) error {
	name, err := sanitizeName(name)
	if err != nil {
		return err
	}
	folder, err = sanitizeFolder(folder)
	if err != nil {
		return err
	}
	if err := s.store.DeleteFile(ctx, channelID, folder, name); err != nil {
		return err
	}
	if err := os.Remove(s.filePath(channelID, folder, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("removing file blob failed",
			zap.Int64("channel_id", channelID),
			zap.String("name", name),
			zap.Error(err),
		)
	}
	return nil
}

// RenameFile moves/renames a file record and its blob (262).
func (s *Server) RenameFile(ctx context.Context, channelID int64, folder, name, newFolder, newName string) error {
	name, err := sanitizeName(name)
	if err != nil {
		return err
	}
	newName, err = sanitizeName(newName)
	if err != nil {
		return err
	}
	folder, err = sanitizeFolder(folder)
	if err != nil {
		return err
	}
	newFolder, err = sanitizeFolder(newFolder)
	if err != nil {
		return err
	}
	if folder == newFolder && name == newName {
		return errors.New("nothing to rename")
	}
	if err := s.store.RenameFile(ctx, channelID, folder, name, newFolder, newName); err != nil {
		return err
	}
	oldPath := s.filePath(channelID, folder, name)
	newPath := s.filePath(channelID, newFolder, newName)
	if err := os.MkdirAll(filepath.Dir(newPath), 0o750); err != nil {
		return fmt.Errorf("creating target folder: %w", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("moving file blob: %w", err)
	}
	return nil
}

// ChannelQuota returns the channel's used bytes and quota (0 = unlimited).
func (s *Server) ChannelQuota(ctx context.Context, channelID int64) (int64, int64, error) {
	used, err := s.store.ChannelFileUsage(ctx, channelID)
	if err != nil {
		return 0, 0, err
	}
	return used, s.cfg.ChannelQuotaMB << 20, nil
}

// Links returns the download-link registry (mounted on the health server).
func (s *Server) Links() *LinkRegistry {
	return s.links
}

// CreateLink issues an expiring download link for a channel file (267).
func (s *Server) CreateLink(ctx context.Context, channelID int64, folder, name string) (string, time.Time, error) {
	name, err := sanitizeName(name)
	if err != nil {
		return "", time.Time{}, err
	}
	folder, err = sanitizeFolder(folder)
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err := s.store.GetFile(ctx, channelID, folder, name); err != nil {
		return "", time.Time{}, err
	}
	return s.links.Create(s.filePath(channelID, folder, name), name)
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

// sanitizeFolder validates a virtual folder path (261): "" is the channel
// root; otherwise '/'-separated segments, each a bare name — no traversal,
// no backslashes, no leading/trailing/duplicate separators.
func sanitizeFolder(folder string) (string, error) {
	if folder == "" {
		return "", nil
	}
	if strings.ContainsAny(folder, "\\") || strings.HasPrefix(folder, "/") ||
		strings.HasSuffix(folder, "/") || strings.Contains(folder, "//") {
		return "", fmt.Errorf("%w: %q", ErrInvalidName, folder)
	}
	for _, seg := range strings.Split(folder, "/") {
		if _, err := sanitizeName(seg); err != nil {
			return "", fmt.Errorf("%w: %q", ErrInvalidName, folder)
		}
	}
	return folder, nil
}

// filePath returns the on-disk path for a channel file.
func (s *Server) filePath(channelID int64, folder, name string) string {
	return filepath.Join(s.cfg.RootDir, strconv.FormatInt(channelID, 10), filepath.FromSlash(folder), name)
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
	if s.cfg.TLSEnabled {
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{s.cfg.Cert},
			MinVersion:   tls.VersionTLS12,
		})
	}
	s.listener = ln
	if s.cfg.TLSEnabled {
		s.logger.Info("file transfer listener started",
			zap.String("addr", s.cfg.Addr),
			zap.String("fingerprint", s.cfg.Fingerprint),
		)
	} else {
		// same shape as the control channel's plaintext warning: the token is
		// minted over TLS and then replayed here in the clear (91-135).
		s.logger.Warn("file transfer listener started (PLAINTEXT!)", zap.String("addr", s.cfg.Addr))
	}

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
