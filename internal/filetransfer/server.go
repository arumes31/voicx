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
// closes the transfer. Uploads are written to a random, exclusive temporary
// file below <root>/<channel_id> and atomically renamed on success; a same-name upload replaces the
// previous file (both on disk and in the files table).
//
// The port speaks TLS whenever Config.TLSEnabled is set (91-135); when it is
// not, Start logs a PLAINTEXT warning, because that is a dev-only escape
// hatch and not a supported deployment. The single-use token is minted on the
// TLS control channel and then presented here, so a plaintext data port leaks
// the token as well as the bytes: an on-path observer could lift it and pull
// the file itself. The certificate is the one the control channel already
// presents, so clients re-use the fingerprint they have already pinned.
//
// The package has no awareness of chat-attachment encryption: it moves opaque
// bytes. Chat attachments are sealed by the client before upload and arrive
// here under a content-derived ".vcx" name; the only two places that care are
// the files.encrypted flag and the download-link refusal in links.go. In
// particular nothing here validates that name — the bytes are opaque, so the
// server has no opinion on how the client derived it.
//
// Per-channel file passwords (TS3 b_ft_ignore_password) are out of scope.
package filetransfer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// ErrUploaderQuotaExceeded is returned by InitUpload when the uploader's own
// quota would be exceeded (266).
var ErrUploaderQuotaExceeded = errors.New("filetransfer: upload quota exceeded")

// ErrTooLarge is returned by InitUpload when the declared size exceeds the
// per-transfer maximum.
var ErrTooLarge = errors.New("filetransfer: file too large")

// ErrInvalidName is returned when a file name fails sanitization.
var ErrInvalidName = errors.New("filetransfer: invalid file name")

// ErrChannelDeleted is returned when work is requested for a channel whose
// deletion cleanup has started. Channel IDs are never reused, so the
// tombstone is intentionally permanent for the lifetime of the server.
var ErrChannelDeleted = errors.New("filetransfer: channel deleted")

// ErrEncryptedAttachment is returned when an expiring download link (267) is
// requested for a client-encrypted chat attachment.
var ErrEncryptedAttachment = errors.New("encrypted chat attachment — open it in the client")

// encryptedSuffix marks a client-encrypted chat attachment (91-135): the blob
// is nonce||secretbox and its key lives only inside the encrypted chat body,
// so nothing server-side can ever read it back.
const encryptedSuffix = ".vcx"

// encryptedNameLen is how much of the hex digest the client keeps in the
// storage name (hex(sha256(ciphertext))[:32] + ".vcx"). finalizeUpload
// re-derives it from the received bytes, so the name cannot be forged.
const encryptedNameLen = 32

// isEncryptedAttachment reports whether a stored name is a client-encrypted
// chat attachment. Content-derived names never collide with an existing name
// and so never reach the .v1..v3 rotation path. The suffix is the only
// signal: an ordinary browser upload that happens to end in ".vcx" is treated
// the same way, which costs nothing but a lock icon.
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
	MoveFile(ctx context.Context, channelID int64, folder, name string, newChannelID int64, newFolder, newName string) error
	DeleteFile(ctx context.Context, channelID int64, folder, name string) error
	FindFileBySHA(ctx context.Context, channelID int64, sha256, exclFolder, exclName string) (*store.FileRecord, error)
	// ChannelFileUsage and UploaderFileUsage are the two axes of the quota
	// model (265/266). Both report PHYSICAL bytes: dedup hard-links identical
	// blobs, and a copy that costs no disk must not be charged for.
	ChannelFileUsage(ctx context.Context, channelID int64) (int64, error)
	UploaderFileUsage(ctx context.Context, uploader string) (int64, error)
}

// Quota is one axis of the file quota model (265 per channel, 266 per
// uploader): bytes already used and the ceiling, where 0 means unlimited.
// Both axes resolve through quotaFor so they cannot drift apart.
type Quota struct {
	Used  int64
	Limit int64
}

// Exceeded reports whether storing size more bytes would cross the ceiling.
func (q Quota) Exceeded(size int64) bool {
	return q.Limit > 0 && q.Used+size > q.Limit
}

// quotaFor pairs a usage lookup with a MiB ceiling.
func quotaFor(used int64, limitMB int64) Quota {
	if limitMB <= 0 {
		return Quota{Used: used}
	}
	return Quota{Used: used, Limit: limitMB << 20}
}

// Config holds the file-transfer server settings (populated from the
// "file_*" config keys).
type Config struct {
	// Addr is the listen address (e.g. ":12336").
	Addr string
	// RootDir is where uploaded files are stored, laid out per channel.
	RootDir string
	// MaxKBps is the per-connection bandwidth cap in KiB/s. 0 = unlimited.
	MaxKBps int
	// QuietHoursStart/End lift MaxKBps during a local-time window (276):
	// both 0-23, equal = disabled, and a start after the end wraps past
	// midnight. The window is evaluated when a transfer starts, so a transfer
	// that begins inside it keeps full speed to the end rather than being
	// throttled mid-stream.
	QuietHoursStart int
	QuietHoursEnd   int
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

// activeTransfer lets channel deletion revoke transfers that already
// consumed their single-use token. Closing conn interrupts both upload and
// download I/O; done closes only after any partial upload has been removed.
type activeTransfer struct {
	transferID string
	channelID  int64
	conn       net.Conn
	done       chan struct{}
}

type channelCleanup struct {
	done chan struct{}
	err  error
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
	// moveBlobFn is injectable in tests so metadata rollback can be exercised
	// without depending on the host's volume layout.
	moveBlobFn func(*os.Root, string, string) error
	// removeChannelDataFn is injectable in tests so cleanup retry semantics can
	// be verified without relying on platform-specific filesystem failures.
	removeChannelDataFn func(int64) error

	// OnTransferComplete, when set, is called with the direction and result
	// ("ok"/"error") when a transfer finishes (metrics).
	OnTransferComplete func(direction, result string)

	lifecycleMu sync.Mutex
	listener    net.Listener
	started     bool
	closed      bool

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// fileOpsMu makes filesystem-producing mutations linearizable with a
	// channel tombstone. Deletion takes the write side only long enough to
	// drain prior mutations and publish the tombstone; later operations acquire
	// the read side, observe it, and fail before touching disk or metadata.
	fileOpsMu       sync.RWMutex
	mu              sync.Mutex
	transfers       map[string]*transfer // keyed by token digest
	activeTransfers map[string]*activeTransfer
	deletedChannels map[int64]*channelCleanup
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
	s := &Server{
		cfg:             cfg,
		store:           st,
		logger:          logger,
		port:            port,
		links:           NewLinkRegistry(cfg.RootDir),
		moveBlobFn:      moveBlob,
		stopCh:          make(chan struct{}),
		transfers:       make(map[string]*transfer),
		activeTransfers: make(map[string]*activeTransfer),
		deletedChannels: make(map[int64]*channelCleanup),
	}
	s.removeChannelDataFn = s.removeChannelData
	return s
}

// Port returns the configured file-transfer port (0 when the address has no
// numeric port), used in FileTransferInitResponse.
func (s *Server) Port() int {
	return s.port
}

// Fingerprint returns the SHA-256 fingerprint of the certificate this port
// presents, empty when TLS is disabled. Clients cross-check it against the
// control-channel pin so a hostile server cannot redirect transfers to a
// third-party host. Empty therefore also answers "is the port plaintext?",
// which is all the control channel needs to fill both
// FileTransferInitResponse.TLS and .TLSFingerprint from this one call.
func (s *Server) Fingerprint() string {
	if !s.cfg.TLSEnabled {
		return ""
	}
	return s.cfg.Fingerprint
}

// InitUpload issues a single-use upload token after validating the folder
// and name, the per-transfer size cap, and BOTH quota axes. uploader is the
// initiating user's unique ID, recorded in the files table;
// uploaderQuotaMB is that user's personal ceiling (266), resolved by the
// caller from the client's permissions (0 = unlimited).
func (s *Server) InitUpload(ctx context.Context, channelID int64, folder, name string, size int64, uploader string, uploaderQuotaMB int64) (string, string, error) {
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
		q, err := s.ChannelQuotaState(ctx, channelID)
		if err != nil {
			return "", "", fmt.Errorf("checking channel quota: %w", err)
		}
		if q.Exceeded(size) {
			return "", "", fmt.Errorf("%w: %d MiB", ErrQuotaExceeded, s.cfg.ChannelQuotaMB)
		}
	}
	if uploaderQuotaMB > 0 {
		q, err := s.UploaderQuotaState(ctx, uploader, uploaderQuotaMB)
		if err != nil {
			return "", "", fmt.Errorf("checking upload quota: %w", err)
		}
		if q.Exceeded(size) {
			return "", "", fmt.Errorf("%w: %d MiB", ErrUploaderQuotaExceeded, uploaderQuotaMB)
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
	root, err := s.openBlobRoot()
	if err != nil {
		s.logger.Warn("opening file root failed while removing blob",
			zap.Int64("channel_id", channelID),
			zap.String("name", name),
			zap.Error(err),
		)
		return nil
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(blobPath(channelID, folder, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn("removing file blob failed",
			zap.Int64("channel_id", channelID),
			zap.String("name", name),
			zap.Error(err),
		)
	}
	return nil
}

// RenameFile moves/renames a file record and its blob within one channel
// (262).
func (s *Server) RenameFile(ctx context.Context, channelID int64, folder, name, newFolder, newName string) error {
	return s.MoveFile(ctx, channelID, folder, name, channelID, newFolder, newName)
}

// MoveFile relocates a file and its blob, possibly into another channel
// (262). The caller is responsible for the permission check on BOTH channels.
func (s *Server) MoveFile(ctx context.Context, channelID int64, folder, name string, newChannelID int64, newFolder, newName string) error {
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
	if newChannelID == 0 {
		newChannelID = channelID
	}
	s.fileOpsMu.RLock()
	defer s.fileOpsMu.RUnlock()
	if s.channelDeleted(channelID) || s.channelDeleted(newChannelID) {
		return ErrChannelDeleted
	}
	if channelID == newChannelID && folder == newFolder && name == newName {
		return errors.New("nothing to rename")
	}
	root, err := s.openBlobRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	oldPath := blobPath(channelID, folder, name)
	newPath := blobPath(newChannelID, newFolder, newName)
	if err := root.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		return fmt.Errorf("creating target folder: %w", err)
	}
	// A move into an occupied name would silently orphan the blob already
	// there, so refuse instead of overwriting.
	if channelID != newChannelID || folder != newFolder || name != newName {
		if _, err := s.store.GetFile(ctx, newChannelID, newFolder, newName); err == nil {
			return fmt.Errorf("%s already exists in the target folder", newName)
		}
	}
	if err := s.store.MoveFile(ctx, channelID, folder, name, newChannelID, newFolder, newName); err != nil {
		if errors.Is(err, store.ErrFileExists) {
			return fmt.Errorf("%s already exists in the target folder: %w", newName, err)
		}
		return err
	}
	if err := s.moveBlobFn(root, oldPath, newPath); err != nil {
		rbCtx, rbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rbCancel()
		rollbackErr := s.store.MoveFile(rbCtx, newChannelID, newFolder, newName, channelID, folder, name)
		if rollbackErr != nil {
			return fmt.Errorf("moving file blob: %w (metadata rollback failed: %v)", err, rollbackErr)
		}
		return fmt.Errorf("moving file blob: %w", err)
	}
	return nil
}

// moveBlob renames a blob, falling back to copy+unlink when the two paths sit
// on different volumes (a cross-channel move can cross a mount point when the
// storage root spans devices, and Rename fails with EXDEV there). A missing
// source is not an error: the row is the record of truth.
func moveBlob(root *os.Root, oldPath, newPath string) error {
	info, err := root.Lstat(oldPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking source blob: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source blob %q is not a regular file", oldPath)
	}

	err = root.Rename(oldPath, newPath)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return copyBlobAndRemove(root, oldPath, newPath, err)
}

// copyBlobAndRemove is the cross-volume fallback after Rename fails.
func copyBlobAndRemove(root *os.Root, oldPath, newPath string, renameErr error) error {
	src, openErr := openRegularBlob(root, oldPath)
	if openErr != nil {
		if errors.Is(openErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("copy fallback after rename failed (%v): %w", renameErr, openErr)
	}
	srcClosed := false
	defer func() {
		if !srcClosed {
			_ = src.Close()
		}
	}()
	dst, createErr := root.OpenFile(newPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if createErr != nil {
		return createErr
	}
	if _, copyErr := io.Copy(dst, src); copyErr != nil {
		_ = dst.Close()
		_ = root.Remove(newPath)
		return copyErr
	}
	if syncErr := dst.Sync(); syncErr != nil {
		_ = dst.Close()
		_ = root.Remove(newPath)
		return syncErr
	}
	if closeErr := dst.Close(); closeErr != nil {
		_ = root.Remove(newPath)
		return closeErr
	}
	closeErr := src.Close()
	srcClosed = true
	if closeErr != nil {
		_ = root.Remove(newPath)
		return closeErr
	}
	// Only drop the source once the copy is safely closed.
	_ = root.Remove(oldPath)
	return nil
}

// ChannelQuotaState resolves the per-channel axis (265): physical usage
// against the configured channel ceiling.
func (s *Server) ChannelQuotaState(ctx context.Context, channelID int64) (Quota, error) {
	used, err := s.store.ChannelFileUsage(ctx, channelID)
	if err != nil {
		return Quota{}, err
	}
	return quotaFor(used, s.cfg.ChannelQuotaMB), nil
}

// UploaderQuotaState resolves the per-uploader axis (266): what this user
// already stores server-wide against the ceiling their permissions grant
// them. limitMB <= 0 is unlimited, in which case the usage query is skipped.
func (s *Server) UploaderQuotaState(ctx context.Context, uploader string, limitMB int64) (Quota, error) {
	if limitMB <= 0 || uploader == "" {
		return Quota{}, nil
	}
	used, err := s.store.UploaderFileUsage(ctx, uploader)
	if err != nil {
		return Quota{}, err
	}
	return quotaFor(used, limitMB), nil
}

// ChannelQuota returns the channel's used bytes and quota (0 = unlimited),
// the shape the file-list response wants.
func (s *Server) ChannelQuota(ctx context.Context, channelID int64) (int64, int64, error) {
	q, err := s.ChannelQuotaState(ctx, channelID)
	if err != nil {
		return 0, 0, err
	}
	return q.Used, q.Limit, nil
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
	return s.links.Create(blobPath(channelID, folder, name), name)
}

// TombstoneChannelData permanently rejects new work for a deleted channel and
// immediately revokes all of its pending and active capabilities. Physical
// cleanup continues asynchronously so callers can tombstone an entire subtree
// before waiting on slower recorder or filesystem teardown.
func (s *Server) TombstoneChannelData(channelID int64) error {
	_, err := s.beginChannelCleanup(channelID)
	return err
}

// DeleteChannelData tombstones a deleted channel and waits for its confined
// on-disk directory to be removed. A context deadline only bounds the wait;
// cleanup continues asynchronously and a later call can observe completion or
// retry a failed cleanup.
func (s *Server) DeleteChannelData(ctx context.Context, channelID int64) error {
	cleanup, err := s.beginChannelCleanup(channelID)
	if err != nil {
		return err
	}
	select {
	case <-cleanup.done:
		return cleanup.err
	case <-ctx.Done():
		return fmt.Errorf("waiting for channel %d file cleanup: %w", channelID, ctx.Err())
	}
}

func (s *Server) beginChannelCleanup(channelID int64) (*channelCleanup, error) {
	if channelID <= 0 {
		return nil, fmt.Errorf("%w: invalid channel id %d", ErrChannelDeleted, channelID)
	}

	s.mu.Lock()
	cleanup, exists := s.deletedChannels[channelID]
	startCleanup := !exists
	if exists {
		select {
		case <-cleanup.done:
			// A successful cleanup is permanently idempotent. A failed cleanup
			// remains a tombstone but receives a fresh worker on the next call.
			if cleanup.err != nil {
				cleanup = &channelCleanup{done: make(chan struct{})}
				s.deletedChannels[channelID] = cleanup
				startCleanup = true
			}
		default:
		}
	}
	if !exists {
		cleanup = &channelCleanup{done: make(chan struct{})}
		s.deletedChannels[channelID] = cleanup
	}
	for digest, tr := range s.transfers {
		if tr.ChannelID == channelID {
			delete(s.transfers, digest)
		}
	}
	active := make([]*activeTransfer, 0)
	for _, tr := range s.activeTransfers {
		if tr.channelID == channelID {
			active = append(active, tr)
		}
	}
	s.mu.Unlock()

	// Capability revocation deliberately precedes the potentially blocking
	// drain. A wedged pre-delete move may delay physical reclamation, but it
	// cannot keep a bearer link or transfer token valid after this call starts.
	s.links.RevokeChannel(channelID)
	for _, tr := range active {
		_ = tr.conn.Close()
	}
	if startCleanup {
		go s.finishChannelCleanup(channelID, active, cleanup)
	}
	return cleanup, nil
}

func (s *Server) finishChannelCleanup(channelID int64, active []*activeTransfer, cleanup *channelCleanup) {
	for _, tr := range active {
		<-tr.done
	}
	// A queued writer prevents later readers from starving cleanup. Operations
	// already holding the read side finish first; they began before the
	// tombstone and their output is removed below. Later operations observe the
	// tombstone immediately after acquiring the read side and fail closed.
	// Short bounded retries absorb transient sharing violations and delayed
	// network-volume visibility without holding the global writer while asleep.
	retryDelays := [...]time.Duration{25 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond}
	for attempt := 0; ; attempt++ {
		s.fileOpsMu.Lock()
		cleanup.err = s.removeChannelDataFn(channelID)
		s.fileOpsMu.Unlock()
		if cleanup.err == nil || attempt == len(retryDelays) {
			break
		}
		time.Sleep(retryDelays[attempt])
	}
	close(cleanup.done)
}

func (s *Server) removeChannelData(channelID int64) error {
	root, err := s.openBlobRoot()
	if err != nil {
		return fmt.Errorf("opening file root for channel cleanup: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(strconv.FormatInt(channelID, 10)); err != nil {
		return fmt.Errorf("removing channel %d files: %w", channelID, err)
	}
	return nil
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
	if _, deleted := s.deletedChannels[tr.ChannelID]; deleted {
		s.mu.Unlock()
		return "", "", ErrChannelDeleted
	}
	s.transfers[tokenDigest(token)] = tr
	s.mu.Unlock()
	return id, token, nil
}

// consume validates and consumes a token (single-use). It returns the
// transfer or an error when the token is unknown or expired.
func (s *Server) consume(token, transferID string) (*transfer, error) {
	s.mu.Lock()
	digest := tokenDigest(token)
	tr, ok := s.transfers[digest]
	if ok {
		delete(s.transfers, digest)
	}
	deleted := ok && s.channelDeletedLocked(tr.ChannelID)
	s.mu.Unlock()

	if !ok || !constantTimeStringEqual(tr.ID, transferID) {
		return nil, errors.New("invalid transfer token")
	}
	if deleted {
		return nil, ErrChannelDeleted
	}
	if time.Now().After(tr.Expires) {
		return nil, errors.New("transfer token expired")
	}
	return tr, nil
}

func (s *Server) activateTransfer(tr *transfer, conn net.Conn) (*activeTransfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channelDeletedLocked(tr.ChannelID) {
		return nil, ErrChannelDeleted
	}
	active := &activeTransfer{
		transferID: tr.ID,
		channelID:  tr.ChannelID,
		conn:       conn,
		done:       make(chan struct{}),
	}
	s.activeTransfers[tr.ID] = active
	return active, nil
}

func (s *Server) deactivateTransfer(active *activeTransfer) {
	s.mu.Lock()
	if current := s.activeTransfers[active.transferID]; current == active {
		delete(s.activeTransfers, active.transferID)
	}
	s.mu.Unlock()
	close(active.done)
}

func (s *Server) channelDeletedLocked(channelID int64) bool {
	_, deleted := s.deletedChannels[channelID]
	return deleted
}

func (s *Server) channelDeleted(channelID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelDeletedLocked(channelID)
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return string(sum[:])
}

func constantTimeStringEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
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

// blobPath returns a path relative to the configured storage root. Remote
// folders and names are validated before this helper is reached; os.Root is
// the final confinement layer for every filesystem operation.
func blobPath(channelID int64, folder, name string) string {
	return filepath.Join(strconv.FormatInt(channelID, 10), filepath.FromSlash(folder), name)
}

// filePath returns the absolute on-disk path for APIs that require one. Blob
// reads and writes use blobPath with os.Root instead.
func (s *Server) filePath(channelID int64, folder, name string) string {
	return filepath.Join(s.cfg.RootDir, blobPath(channelID, folder, name))
}

// openBlobRoot creates the trusted configured root when necessary and opens
// a capability that rejects traversal and symlink escapes for relative blob
// paths. Callers close the root after their operation; Root itself is safe for
// concurrent use while open.
func (s *Server) openBlobRoot() (*os.Root, error) {
	if err := os.MkdirAll(s.cfg.RootDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating file root %s: %w", s.cfg.RootDir, err)
	}
	root, err := os.OpenRoot(s.cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("opening file root %s: %w", s.cfg.RootDir, err)
	}
	return root, nil
}

// openRegularBlob opens an existing blob only when the directory entry and
// the opened target are regular files. The second check rejects a non-regular
// target swapped in between Lstat and Open; os.Root keeps either lookup
// confined in all cases.
func openRegularBlob(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("blob %q is not a regular file", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	openedInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("blob %q is not a regular file", name)
	}
	return f, nil
}

// CheckRoot verifies the storage root exists and is writable, creating it if
// needed. It is called at startup so a misconfigured volume is logged loudly
// instead of failing on the first upload.
func (s *Server) CheckRoot() error {
	root, err := s.openBlobRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	probeID, err := randomHex(8)
	if err != nil {
		return err
	}
	probe := ".writetest-" + probeID
	f, err := root.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("file root %s is not writable: %w", s.cfg.RootDir, err)
	}
	if _, err := f.Write([]byte("ok")); err != nil {
		_ = f.Close()
		_ = root.Remove(probe)
		return fmt.Errorf("file root %s is not writable: %w", s.cfg.RootDir, err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(probe)
		return fmt.Errorf("file root %s is not writable: %w", s.cfg.RootDir, err)
	}
	_ = root.Remove(probe)
	return nil
}

// Start binds the listener and serves connections until ctx is cancelled or
// Close is called.
func (s *Server) Start(ctx context.Context) error {
	s.lifecycleMu.Lock()
	if s.closed {
		s.lifecycleMu.Unlock()
		return nil
	}
	if s.started {
		s.lifecycleMu.Unlock()
		return errors.New("filetransfer server already started")
	}
	s.started = true
	// Establish a positive parent count before Close can call Wait. The accept
	// loop owns this count until it exits; connection workers are its children.
	s.wg.Add(1)
	s.lifecycleMu.Unlock()
	defer s.wg.Done()

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("filetransfer listen on %s: %w", s.cfg.Addr, err)
	}
	if s.cfg.TLSEnabled {
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{s.cfg.Cert},
			MinVersion:   tls.VersionTLS13,
		})
	}
	s.lifecycleMu.Lock()
	if s.closed {
		s.lifecycleMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.lifecycleMu.Unlock()
	if s.cfg.QuietHoursStart != s.cfg.QuietHoursEnd {
		s.logger.Info("file transfer quiet hours active",
			zap.Int("start_hour", s.cfg.QuietHoursStart),
			zap.Int("end_hour", s.cfg.QuietHoursEnd),
			zap.Int("max_kbps_outside", s.cfg.MaxKBps),
		)
	}
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
		s.lifecycleMu.Lock()
		s.closed = true
		close(s.stopCh)
		ln := s.listener
		s.listener = nil
		s.lifecycleMu.Unlock()
		if ln != nil {
			err = ln.Close()
		}
	})
	s.wg.Wait()
	return err
}
