package filetransfer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"voicx/internal/store"
)

func TestDeleteChannelDataRevokesCapabilitiesAndRemovesDirectory(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()
	channelDir := filepath.Join(rootDir, "7")
	if err := os.Mkdir(channelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(channelDir, "shared.txt")
	if err := os.WriteFile(filePath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := newFakeFileStore()
	if err := files.AddFile(ctx, store.FileRecord{ChannelID: 7, Name: "shared.txt", Size: 7}); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Addr: ":0", RootDir: rootDir}, files, nil)

	transferID, transferToken, err := s.InitUpload(ctx, 7, "", "pending.txt", 1, "u", 0)
	if err != nil {
		t.Fatalf("InitUpload: %v", err)
	}
	linkToken, _, err := s.CreateLink(ctx, 7, "", "shared.txt")
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	if err := s.DeleteChannelData(ctx, 7); err != nil {
		t.Fatalf("DeleteChannelData: %v", err)
	}
	if _, err := os.Stat(channelDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("channel directory still exists: %v", err)
	}
	if got := s.pendingCount(); got != 0 {
		t.Fatalf("pending transfers = %d, want 0", got)
	}
	if _, err := s.consume(transferToken, transferID); err == nil {
		t.Fatal("revoked pending transfer token was accepted")
	}
	if _, _, err := s.InitUpload(ctx, 7, "", "new.txt", 1, "u", 0); !errors.Is(err, ErrChannelDeleted) {
		t.Fatalf("InitUpload after delete = %v, want ErrChannelDeleted", err)
	}
	if _, _, err := s.InitDownload(ctx, 7, "", "shared.txt"); !errors.Is(err, ErrChannelDeleted) {
		t.Fatalf("InitDownload after delete = %v, want ErrChannelDeleted", err)
	}
	if _, _, err := s.CreateLink(ctx, 7, "", "shared.txt"); !errors.Is(err, ErrChannelDeleted) {
		t.Fatalf("CreateLink after delete = %v, want ErrChannelDeleted", err)
	}

	recorder := httptest.NewRecorder()
	s.Links().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dl/"+linkToken, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("revoked link status = %d, want 404", recorder.Code)
	}
}

func TestDeleteChannelDataStopsActiveTransferBeforeReturning(t *testing.T) {
	s := New(Config{Addr: ":0", RootDir: t.TempDir()}, newFakeFileStore(), nil)
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	active, err := s.activateTransfer(&transfer{ID: "active", ChannelID: 7}, serverConn)
	if err != nil {
		t.Fatalf("activateTransfer: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- s.DeleteChannelData(context.Background(), 7)
	}()
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := clientConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("active transfer connection remained open")
	}
	s.deactivateTransfer(active)

	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("DeleteChannelData: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeleteChannelData did not wait for active transfer teardown")
	}
	if _, _, err := s.register(&transfer{ChannelID: 7}); !errors.Is(err, ErrChannelDeleted) {
		t.Fatalf("register after delete = %v, want ErrChannelDeleted", err)
	}
}

func TestLinkRegistryRevokesExactChannelOnly(t *testing.T) {
	rootDir := t.TempDir()
	for _, channel := range []string{"7", "70"} {
		if err := os.Mkdir(filepath.Join(rootDir, channel), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootDir, channel, "file.txt"), []byte(channel), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry := NewLinkRegistry(rootDir)
	token7, _, err := registry.Create(filepath.Join("7", "file.txt"), "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	token70, _, err := registry.Create(filepath.Join("70", "file.txt"), "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	registry.RevokeChannel(7)
	if _, ok := registry.take(token7); ok {
		t.Fatal("channel 7 link survived revocation")
	}
	if _, ok := registry.take(token70); !ok {
		t.Fatal("channel 70 link was revoked with channel 7")
	}
}

func TestLinkRegistryRevokeWaitsForOpenRegistration(t *testing.T) {
	registry, rel := newTestLinkRegistry(t, []byte("race-safe"))
	token, _, err := registry.Create(rel, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	openEntered := make(chan struct{})
	allowOpen := make(chan struct{})
	registry.beforeServeOpen = func() {
		close(openEntered)
		<-allowOpen
	}
	serveDone := make(chan struct{})
	go func() {
		recorder := httptest.NewRecorder()
		registry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dl/"+token, nil))
		close(serveDone)
	}()
	<-openEntered
	revokeDone := make(chan struct{})
	go func() {
		registry.RevokeChannel(7)
		close(revokeDone)
	}()
	select {
	case <-revokeDone:
		t.Fatal("RevokeChannel returned during the lookup/open registration gap")
	case <-time.After(50 * time.Millisecond):
	}
	close(allowOpen)
	select {
	case <-revokeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("RevokeChannel did not complete after file registration")
	}
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("link handler did not stop after revocation")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.active) != 0 {
		t.Fatalf("active link handles = %d, want 0", len(registry.active))
	}
}

func TestDeleteChannelDataSerializesWithMove(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()
	files := newFakeFileStore()
	s := New(Config{Addr: ":0", RootDir: rootDir}, files, nil)
	path := s.filePath(7, "", "old.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := files.AddFile(ctx, store.FileRecord{ChannelID: 7, Name: "old.txt", Size: 7}); err != nil {
		t.Fatal(err)
	}

	moveEntered := make(chan struct{})
	allowMove := make(chan struct{})
	s.moveBlobFn = func(root *os.Root, oldPath, newPath string) error {
		close(moveEntered)
		<-allowMove
		return moveBlob(root, oldPath, newPath)
	}
	moveDone := make(chan error, 1)
	go func() {
		moveDone <- s.MoveFile(ctx, 7, "", "old.txt", 7, "", "new.txt")
	}()
	<-moveEntered
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- s.DeleteChannelData(ctx, 7)
	}()
	for {
		if _, _, err := s.register(&transfer{ChannelID: 7}); errors.Is(err, ErrChannelDeleted) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-deleteDone:
		t.Fatalf("DeleteChannelData crossed an in-flight move: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowMove)
	if err := <-moveDone; err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteChannelData: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "7")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("channel directory survived move/delete race: %v", err)
	}
	if err := s.MoveFile(ctx, 7, "", "new.txt", 8, "", "new.txt"); !errors.Is(err, ErrChannelDeleted) {
		t.Fatalf("move from deleted channel = %v, want ErrChannelDeleted", err)
	}
	if err := files.AddFile(ctx, store.FileRecord{ChannelID: 8, Name: "source.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveFile(ctx, 8, "", "source.txt", 7, "", "source.txt"); !errors.Is(err, ErrChannelDeleted) {
		t.Fatalf("move into deleted channel = %v, want ErrChannelDeleted", err)
	}
}

func TestDeleteChannelDataRevokesBeforeContextBoundedDrain(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()
	files := newFakeFileStore()
	s := New(Config{Addr: ":0", RootDir: rootDir}, files, nil)
	path := s.filePath(7, "", "old.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := files.AddFile(ctx, store.FileRecord{ChannelID: 7, Name: "old.txt", Size: 7}); err != nil {
		t.Fatal(err)
	}
	linkToken, _, err := s.CreateLink(ctx, 7, "", "old.txt")
	if err != nil {
		t.Fatal(err)
	}

	moveEntered := make(chan struct{})
	allowMove := make(chan struct{})
	s.moveBlobFn = func(root *os.Root, oldPath, newPath string) error {
		close(moveEntered)
		<-allowMove
		return moveBlob(root, oldPath, newPath)
	}
	moveDone := make(chan error, 1)
	go func() {
		moveDone <- s.MoveFile(ctx, 7, "", "old.txt", 7, "", "new.txt")
	}()
	<-moveEntered

	deleteCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = s.DeleteChannelData(deleteCtx, 7)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeleteChannelData = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context-bounded cleanup returned after %v", elapsed)
	}
	if _, _, err := s.InitUpload(ctx, 7, "", "late.txt", 1, "u", 0); !errors.Is(err, ErrChannelDeleted) {
		t.Fatalf("InitUpload during deferred drain = %v, want ErrChannelDeleted", err)
	}
	recorder := httptest.NewRecorder()
	s.Links().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dl/"+linkToken, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("link during deferred drain status = %d, want 404", recorder.Code)
	}

	close(allowMove)
	if err := <-moveDone; err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(rootDir, "7")); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("deferred cleanup did not remove the channel directory")
}

func TestDeleteChannelDataRetriesTransientAndLaterPermanentFailure(t *testing.T) {
	rootDir := t.TempDir()
	channelDir := filepath.Join(rootDir, "7")
	if err := os.Mkdir(channelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Addr: ":0", RootDir: rootDir}, newFakeFileStore(), nil)

	var attempts atomic.Int32
	allowSuccess := atomic.Bool{}
	s.removeChannelDataFn = func(channelID int64) error {
		attempts.Add(1)
		if !allowSuccess.Load() {
			return errors.New("transient sharing violation")
		}
		return s.removeChannelData(channelID)
	}

	if err := s.DeleteChannelData(context.Background(), 7); err == nil {
		t.Fatal("first cleanup unexpectedly succeeded")
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("cleanup attempts = %d, want 4 bounded attempts", got)
	}
	if _, err := os.Stat(channelDir); err != nil {
		t.Fatalf("failed cleanup unexpectedly changed channel directory: %v", err)
	}

	allowSuccess.Store(true)
	if err := s.DeleteChannelData(context.Background(), 7); err != nil {
		t.Fatalf("retrying completed cleanup: %v", err)
	}
	if got := attempts.Load(); got != 5 {
		t.Fatalf("cleanup attempts after retry = %d, want 5", got)
	}
	if _, err := os.Stat(channelDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("channel directory survived successful retry: %v", err)
	}
}
