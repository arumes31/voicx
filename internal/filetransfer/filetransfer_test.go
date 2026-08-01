// filetransfer_test.go exercises the token registry, name sanitization, and
// the rate limiter without network I/O.
package filetransfer

import (
	"context"
	"errors"
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

func (f *fakeFileStore) RenameFile(_ context.Context, channelID int64, folder, name, newFolder, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(channelID, folder, name)
	rec, ok := f.files[k]
	if !ok {
		return store.ErrFileNotFound
	}
	delete(f.files, k)
	rec.Folder = newFolder
	rec.Name = newName
	f.files[key(channelID, newFolder, newName)] = rec
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

func (f *fakeFileStore) ChannelFileUsage(_ context.Context, channelID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, rec := range f.files {
		if rec.ChannelID == channelID {
			total += rec.Size
		}
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

	id, token, err := s.InitUpload(context.Background(), 7, "", "a.txt", 10, "uid-1")
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
	id2, token2, err := s.InitUpload(context.Background(), 7, "", "b.txt", 10, "uid-1")
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
	if _, _, err := s.InitUpload(context.Background(), 7, "", "x.bin", 6<<20, "u"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize init = %v, want ErrTooLarge", err)
	}
	// 1 MiB existing + 1.5 MiB new > 2 MiB quota.
	if _, _, err := s.InitUpload(context.Background(), 7, "", "y.bin", 1536*1024, "u"); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("quota init = %v, want ErrQuotaExceeded", err)
	}
	// 1 MiB existing + 1 MiB new == 2 MiB quota: allowed.
	if _, _, err := s.InitUpload(context.Background(), 7, "", "z.bin", 1024*1024, "u"); err != nil {
		t.Errorf("at-quota init = %v, want nil", err)
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
