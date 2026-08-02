// filetransfer_test.go exercises the token registry, name sanitization, and
// the rate limiter without network I/O.
package filetransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"voicx/internal/store"
)

// fakeFileStore implements FileStore in memory.
type fakeFileStore struct {
	mu    sync.Mutex
	files map[string]store.FileRecord
	added []store.FileRecord
}

func newFakeFileStore() *fakeFileStore {
	return &fakeFileStore{files: make(map[string]store.FileRecord)}
}

func key(channelID int64, folder, name string) string {
	return string(rune(channelID)) + "/" + folder + "/" + name
}

func (f *fakeFileStore) AddFile(_ context.Context, rec store.FileRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[key(rec.ChannelID, rec.Folder, rec.Name)] = rec
	f.added = append(f.added, rec)
	return nil
}

func (f *fakeFileStore) GetFile(_ context.Context, channelID int64, folder, name string) (*store.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.files[key(channelID, folder, name)]
	if !ok {
		return nil, store.ErrFileNotFound
	}
	return &rec, nil
}

func (f *fakeFileStore) ListFiles(_ context.Context, channelID int64, folder string) ([]store.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.FileRecord
	for _, rec := range f.files {
		if rec.ChannelID == channelID && rec.Folder == folder {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeFileStore) ListFileFolders(_ context.Context, channelID int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, rec := range f.files {
		if rec.ChannelID == channelID && rec.Folder != "" && !seen[rec.Folder] {
			seen[rec.Folder] = true
			out = append(out, rec.Folder)
		}
	}
	return out, nil
}

func (f *fakeFileStore) ListFileVersions(_ context.Context, channelID int64, folder, baseName string) ([]store.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.FileRecord
	for _, rec := range f.files {
		if rec.ChannelID == channelID && rec.Folder == folder &&
			len(rec.Name) > len(baseName)+2 && rec.Name[:len(baseName)+2] == baseName+".v" {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeFileStore) RenameFile(ctx context.Context, channelID int64, folder, name, newFolder, newName string) error {
	return f.MoveFile(ctx, channelID, folder, name, channelID, newFolder, newName)
}

func (f *fakeFileStore) MoveFile(_ context.Context, channelID int64, folder, name string, newChannelID int64, newFolder, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(channelID, folder, name)
	rec, ok := f.files[k]
	if !ok {
		return store.ErrFileNotFound
	}
	if _, exists := f.files[key(newChannelID, newFolder, newName)]; exists {
		return store.ErrFileExists
	}
	delete(f.files, k)
	rec.ChannelID = newChannelID
	rec.Folder = newFolder
	rec.Name = newName
	f.files[key(newChannelID, newFolder, newName)] = rec
	return nil
}

func (f *fakeFileStore) DeleteFile(_ context.Context, channelID int64, folder, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(channelID, folder, name)
	if _, ok := f.files[k]; !ok {
		return store.ErrFileNotFound
	}
	delete(f.files, k)
	return nil
}

func (f *fakeFileStore) FindFileBySHA(_ context.Context, channelID int64, sha256, exclFolder, exclName string) (*store.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.files {
		if rec.ChannelID == channelID && rec.SHA256 == sha256 && !(rec.Folder == exclFolder && rec.Name == exclName) {
			cp := rec
			return &cp, nil
		}
	}
	return nil, nil
}

// ChannelFileUsage mirrors the store: identical blobs are hard-linked, so a
// content hash costs disk once however many rows point at it (265/275).
func (f *fakeFileStore) ChannelFileUsage(_ context.Context, channelID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]int64{}
	for _, rec := range f.files {
		if rec.ChannelID == channelID && rec.Size > seen[rec.SHA256] {
			seen[rec.SHA256] = rec.Size
		}
	}
	var total int64
	for _, sz := range seen {
		total += sz
	}
	return total, nil
}

// UploaderFileUsage mirrors the store: dedup is per channel, so the same blob
// in two channels really is stored twice (266).
func (f *fakeFileStore) UploaderFileUsage(_ context.Context, uploader string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	type key struct {
		channelID int64
		sha       string
	}
	seen := map[key]int64{}
	for _, rec := range f.files {
		if rec.Uploader != uploader {
			continue
		}
		k := key{rec.ChannelID, rec.SHA256}
		if rec.Size > seen[k] {
			seen[k] = rec.Size
		}
	}
	var total int64
	for _, sz := range seen {
		total += sz
	}
	return total, nil
}

func (f *fakeFileStore) addedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.added)
}

// TestTokenLifecycle verifies issue, single-use consume, ID matching, and
// expiry.
func TestTokenLifecycle(t *testing.T) {
	fs := newFakeFileStore()
	s := New(Config{Addr: ":0", RootDir: t.TempDir()}, fs, nil)

	id, token, err := s.InitUpload(context.Background(), 7, "", "a.txt", 10, "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}
	if id == "" || token == "" {
		t.Fatal("empty transfer id or token")
	}
	if got := s.pendingCount(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}

	// Wrong transfer ID: rejected, and the token is burned (single-use).
	if _, err := s.consume(token, "wrong-id"); err == nil {
		t.Fatal("consume with wrong ID succeeded")
	}
	if _, err := s.consume(token, id); err == nil {
		t.Fatal("second consume succeeded: token not single-use")
	}

	// Expired tokens are rejected.
	old := tokenTTL
	tokenTTL = 10 * time.Millisecond
	defer func() { tokenTTL = old }()
	id2, token2, err := s.InitUpload(context.Background(), 7, "", "b.txt", 10, "uid-1", 0)
	if err != nil {
		t.Fatalf("InitUpload 2: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := s.consume(token2, id2); err == nil {
		t.Fatal("expired token accepted")
	}
}

// TestSanitizeName verifies name validation.
func TestSanitizeName(t *testing.T) {
	valid := []string{"a.txt", "report 2024.pdf", ".hidden", "a-b_c.d"}
	for _, name := range valid {
		if _, err := sanitizeName(name); err != nil {
			t.Errorf("sanitizeName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", ".", "..", "../etc/passwd", "a/b", `a\b`, "a..b", "/abs/path"}
	for _, name := range invalid {
		if _, err := sanitizeName(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("sanitizeName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

// TestInitUploadLimits verifies the per-transfer size cap and the channel
// quota.
func TestInitUploadLimits(t *testing.T) {
	fs := newFakeFileStore()
	_ = fs.AddFile(context.Background(), store.FileRecord{ChannelID: 7, Name: "big.bin", Size: 1024 * 1024})
	s := New(Config{
		Addr:           ":0",
		RootDir:        t.TempDir(),
		MaxSizeMB:      5,
		ChannelQuotaMB: 2,
	}, fs, nil)

	// Over the per-transfer cap.
	if _, _, err := s.InitUpload(context.Background(), 7, "", "x.bin", 6<<20, "u", 0); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize init = %v, want ErrTooLarge", err)
	}
	// 1 MiB existing + 1.5 MiB new > 2 MiB quota.
	if _, _, err := s.InitUpload(context.Background(), 7, "", "y.bin", 1536*1024, "u", 0); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("quota init = %v, want ErrQuotaExceeded", err)
	}
	// 1 MiB existing + 1 MiB new == 2 MiB quota: allowed.
	if _, _, err := s.InitUpload(context.Background(), 7, "", "z.bin", 1024*1024, "u", 0); err != nil {
		t.Errorf("at-quota init = %v, want nil", err)
	}
}

// TestQuotaAxes covers the two quota axes (265/266): the channel axis ignores
// deduped copies, and the uploader axis follows the user across channels.
func TestQuotaAxes(t *testing.T) {
	ctx := context.Background()
	fs := newFakeFileStore()
	// Same content stored twice in channel 7: hard-linked, so one blob's worth.
	_ = fs.AddFile(ctx, store.FileRecord{ChannelID: 7, Name: "a.bin", Size: 1024 * 1024, SHA256: "dup", Uploader: "u"})
	_ = fs.AddFile(ctx, store.FileRecord{ChannelID: 7, Name: "b.bin", Size: 1024 * 1024, SHA256: "dup", Uploader: "u"})
	// The same content in another channel is a second real blob.
	_ = fs.AddFile(ctx, store.FileRecord{ChannelID: 8, Name: "c.bin", Size: 1024 * 1024, SHA256: "dup", Uploader: "u"})

	s := New(Config{Addr: ":0", RootDir: t.TempDir(), ChannelQuotaMB: 2}, fs, nil)

	q, err := s.ChannelQuotaState(ctx, 7)
	if err != nil {
		t.Fatalf("ChannelQuotaState: %v", err)
	}
	if q.Used != 1024*1024 {
		t.Errorf("channel usage = %d, want %d (deduped copy must not be charged twice)", q.Used, 1024*1024)
	}
	if q.Limit != 2<<20 {
		t.Errorf("channel limit = %d, want %d", q.Limit, 2<<20)
	}

	uq, err := s.UploaderQuotaState(ctx, "u", 3)
	if err != nil {
		t.Fatalf("UploaderQuotaState: %v", err)
	}
	if uq.Used != 2*1024*1024 {
		t.Errorf("uploader usage = %d, want %d (one blob per channel)", uq.Used, 2*1024*1024)
	}

	// 2 MiB stored + 1.5 MiB new > the 3 MiB personal ceiling.
	if _, _, err := s.InitUpload(ctx, 9, "", "d.bin", 1536*1024, "u", 3); !errors.Is(err, ErrUploaderQuotaExceeded) {
		t.Errorf("over personal quota = %v, want ErrUploaderQuotaExceeded", err)
	}
	// The same upload with no personal ceiling is allowed.
	if _, _, err := s.InitUpload(ctx, 9, "", "d.bin", 1536*1024, "u", 0); err != nil {
		t.Errorf("unlimited personal quota = %v, want nil", err)
	}
	// A different user is unaffected by u's usage.
	if _, _, err := s.InitUpload(ctx, 9, "", "e.bin", 1536*1024, "other", 3); err != nil {
		t.Errorf("other user init = %v, want nil", err)
	}
}

// TestQuietHours covers the download bandwidth schedule (276): the window is
// inclusive of its start and exclusive of its end, wraps past midnight, and
// is disabled when the bounds are equal.
func TestQuietHours(t *testing.T) {
	cases := []struct {
		hour, start, end int
		want             bool
	}{
		{2, 22, 6, true},   // inside a wrapping window
		{23, 22, 6, true},  // inside, before midnight
		{6, 22, 6, false},  // end is exclusive
		{22, 22, 6, true},  // start is inclusive
		{12, 22, 6, false}, // outside
		{3, 1, 5, true},    // inside a same-day window
		{5, 1, 5, false},   // end is exclusive
		{0, 0, 0, false},   // equal bounds disable the window
		{9, 9, 9, false},   // equal bounds disable the window
		{9, -1, 5, false},  // out-of-range bounds disable the window
		{9, 1, 24, false},  // out-of-range bounds disable the window
	}
	for _, c := range cases {
		if got := inQuietHours(c.hour, c.start, c.end); got != c.want {
			t.Errorf("inQuietHours(%d, %d, %d) = %v, want %v", c.hour, c.start, c.end, got, c.want)
		}
	}

	// Inside the window the cap is lifted; outside it the configured cap
	// applies.
	s := New(Config{Addr: ":0", RootDir: t.TempDir(), MaxKBps: 64, QuietHoursStart: 1, QuietHoursEnd: 5}, newFakeFileStore(), nil)
	quiet := time.Date(2024, 1, 1, 3, 0, 0, 0, time.Local)
	if lim := s.limiterFor(quiet); lim.bytesPerSec != 0 {
		t.Errorf("quiet-hours limiter = %v B/s, want unlimited", lim.bytesPerSec)
	}
	busy := time.Date(2024, 1, 1, 12, 0, 0, 0, time.Local)
	if lim := s.limiterFor(busy); lim.bytesPerSec != 64*1024 {
		t.Errorf("daytime limiter = %v B/s, want %d", lim.bytesPerSec, 64*1024)
	}
}

// TestMoveFileAcrossChannels verifies a cross-channel move relocates both the
// row and the blob, and refuses to overwrite an occupied target (262).
func TestMoveFileAcrossChannels(t *testing.T) {
	ctx := context.Background()
	fs := newFakeFileStore()
	s := New(Config{Addr: ":0", RootDir: t.TempDir()}, fs, nil)

	seed := func(channelID int64, folder, name, content string) {
		t.Helper()
		p := s.filePath(channelID, folder, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("seed blob: %v", err)
		}
		if err := fs.AddFile(ctx, store.FileRecord{
			ChannelID: channelID, Folder: folder, Name: name,
			Size: int64(len(content)), SHA256: "sha-" + name, Uploader: "u",
		}); err != nil {
			t.Fatalf("AddFile: %v", err)
		}
	}
	seed(7, "", "doc.txt", "hello")
	seed(8, "", "taken.txt", "other")

	if err := s.MoveFile(ctx, 7, "", "doc.txt", 8, "shared", "doc.txt"); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	if _, err := fs.GetFile(ctx, 7, "", "doc.txt"); err == nil {
		t.Error("source row still present after the move")
	}
	rec, err := fs.GetFile(ctx, 8, "shared", "doc.txt")
	if err != nil {
		t.Fatalf("target row missing: %v", err)
	}
	if rec.ChannelID != 8 || rec.Uploader != "u" {
		t.Errorf("moved record = %+v, want channel 8 keeping its uploader", rec)
	}
	got, err := os.ReadFile(s.filePath(8, "shared", "doc.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("moved blob = %q, err=%v", got, err)
	}
	if _, err := os.Stat(s.filePath(7, "", "doc.txt")); !os.IsNotExist(err) {
		t.Error("source blob left behind")
	}

	// Moving onto an occupied name must not orphan the blob already there.
	seed(7, "", "again.txt", "again")
	if err := s.MoveFile(ctx, 7, "", "again.txt", 8, "", "taken.txt"); err == nil {
		t.Error("move onto an existing name accepted")
	}
	if got, _ := os.ReadFile(s.filePath(8, "", "taken.txt")); string(got) != "other" {
		t.Errorf("occupant blob = %q, want it untouched", got)
	}
}

func TestMoveFileRollsBackMetadataWhenBlobMoveFails(t *testing.T) {
	ctx := context.Background()
	fs := newFakeFileStore()
	s := New(Config{Addr: ":0", RootDir: t.TempDir()}, fs, nil)
	oldPath := s.filePath(7, "", "doc.txt")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fs.AddFile(ctx, store.FileRecord{ChannelID: 7, Name: "doc.txt"}); err != nil {
		t.Fatal(err)
	}
	s.moveBlobFn = func(string, string) error { return errors.New("injected blob failure") }

	if err := s.MoveFile(ctx, 7, "", "doc.txt", 8, "shared", "doc.txt"); err == nil {
		t.Fatal("MoveFile succeeded despite blob failure")
	}
	if _, err := fs.GetFile(ctx, 7, "", "doc.txt"); err != nil {
		t.Fatalf("source metadata was not restored: %v", err)
	}
	if _, err := fs.GetFile(ctx, 8, "shared", "doc.txt"); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("target metadata remains after rollback: %v", err)
	}
	if got, err := os.ReadFile(oldPath); err != nil || string(got) != "payload" {
		t.Fatalf("source blob = %q, err=%v", got, err)
	}
}

// TestMoveBlobCrossVolumeFallback verifies the copy+unlink path used when
// Rename cannot cross a mount point (262).
func TestMoveBlobCrossVolumeFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := copyBlobAndRemove(src, dst, errors.New("injected cross-volume rename failure")); err != nil {
		t.Fatalf("copyBlobAndRemove: %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "payload" {
		t.Fatalf("moved blob = %q, err=%v", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source left behind")
	}
	// A missing source is not an error: the row is the record of truth.
	if err := moveBlob(filepath.Join(dir, "gone.bin"), filepath.Join(dir, "x.bin")); err != nil {
		t.Errorf("moveBlob on a missing source = %v, want nil", err)
	}
}

// TestRateLimiter verifies the bucket paces traffic and that unlimited means
// no blocking.
func TestRateLimiter(t *testing.T) {
	// Unlimited: instant.
	r := newRateLimiter(0)
	start := time.Now()
	if err := r.wait(context.Background(), 1<<20); err != nil {
		t.Fatalf("unlimited wait: %v", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("unlimited limiter blocked")
	}

	// 10000 B/s with a full 10000-byte bucket: 15000 bytes costs ~0.5s for
	// the 5000-byte deficit.
	slow := &rateLimiter{bytesPerSec: 10000, burst: 10000, tokens: 10000, last: time.Now()}
	start = time.Now()
	if err := slow.wait(context.Background(), 15000); err != nil {
		t.Fatalf("limited wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 400*time.Millisecond {
		t.Errorf("limiter too fast: %v", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("limiter too slow: %v", elapsed)
	}
}
