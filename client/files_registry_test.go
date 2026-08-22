package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func transferPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

func requireTransferClosed(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	defer func() { _ = peer.SetReadDeadline(time.Time{}) }()
	var one [1]byte
	if _, err := peer.Read(one[:]); err == nil {
		t.Fatal("transfer connection remained open")
	}
}

type progressEventCounter struct {
	mu    sync.Mutex
	count int
}

func (c *progressEventCounter) Emit(name string, _ any) {
	if name != "ft_progress" {
		return
	}
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
}

func (c *progressEventCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func TestProgressReporterThrottlesDeterministicallyAndBypassesTerminal(t *testing.T) {
	originalClock := ftProgressClock
	t.Cleanup(func() { ftProgressClock = originalClock })
	now := time.Unix(1, 0)
	ftProgressClock = func() time.Time { return now }
	counter := &progressEventCounter{}
	cm := newTestConnManager()
	cm.sink = counter
	p := ftProgress{ID: "transfer", Direction: "upload", Name: "a.bin", Total: 100, Status: "active"}
	cm.ftEmit(p) // first is immediate
	cm.ftEmit(p) // no elapsed time or completed percent
	p.Transferred = 1
	cm.ftEmit(p) // one percent is immediate
	p.Transferred = 1
	cm.ftEmit(p)
	now = now.Add(99 * time.Millisecond)
	cm.ftEmit(p)
	now = now.Add(time.Millisecond)
	cm.ftEmit(p) // time interval is immediate even without data
	p.Status = "done"
	cm.ftEmit(p) // terminal bypasses the throttle
	if got := counter.Count(); got != 4 {
		t.Fatalf("progress events = %d, want first, 1%%, 100ms, terminal", got)
	}
}

func TestTransferRegistryIsPerManagerAndCancelsDuplicates(t *testing.T) {
	first := newTestConnManager()
	second := newTestConnManager()
	firstClient, firstPeer := transferPipe(t)
	secondClient, secondPeer := transferPipe(t)
	otherClient, otherPeer := transferPipe(t)
	first.trackTransfer("same-id", firstClient)
	first.trackTransfer("same-id", secondClient)
	second.trackTransfer("same-id", otherClient)

	first.cancelTransfers("same-id")
	requireTransferClosed(t, firstPeer)
	requireTransferClosed(t, secondPeer)

	second.transfers.mu.Lock()
	remaining := len(second.transfers.entries)
	second.transfers.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("cross-manager cancellation changed %d unrelated entries", remaining)
	}
	second.cancelTransfers("same-id")
	requireTransferClosed(t, otherPeer)
}

func TestTransferRegistryUntrackCannotRemoveLaterEntry(t *testing.T) {
	cm := newTestConnManager()
	oldClient, oldPeer := transferPipe(t)
	newClient, newPeer := transferPipe(t)
	untrackOld := cm.trackTransfer("duplicate", oldClient)
	untrackOld()
	untrackNew := cm.trackTransfer("duplicate", newClient)
	defer untrackNew()
	untrackOld() // stale deferred cleanup must be harmless.
	cm.cancelTransfers("duplicate")
	requireTransferClosed(t, newPeer)
	_ = oldPeer.Close()
}

func TestDisconnectDetachesAndClosesTransfers(t *testing.T) {
	cm := newTestConnManager()
	firstClient, firstPeer := transferPipe(t)
	secondClient, secondPeer := transferPipe(t)
	cm.trackTransfer("one", firstClient)
	cm.trackTransfer("two", secondClient)
	cm.disconnect()
	requireTransferClosed(t, firstPeer)
	requireTransferClosed(t, secondPeer)
	cm.transfers.mu.Lock()
	remaining := len(cm.transfers.entries)
	cm.transfers.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("disconnect left %d transfer entries", remaining)
	}
}

func TestTransferRegistryCancelUntrackRace(t *testing.T) {
	cm := newTestConnManager()
	for range 100 {
		client, peer := transferPipe(t)
		untrack := cm.trackTransfer("race", client)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cm.cancelTransfers("race") }()
		go func() { defer wg.Done(); untrack() }()
		wg.Wait()
		_ = peer.Close()
	}
}

func TestTransferRegistrationRejectsDisconnectedOrReplacedEpoch(t *testing.T) {
	cm := newTestConnManager()
	control, controlPeer := transferPipe(t)
	cm.mu.Lock()
	cm.conn = control
	cm.transferEpoch = 7
	cm.acceptingTransfers = true
	cm.mu.Unlock()

	cm.disconnect()
	late, latePeer := transferPipe(t)
	if _, ok := cm.trackTransferAt("late", 7, late); ok {
		t.Fatal("post-disconnect transfer registered")
	}
	requireTransferClosed(t, latePeer)

	replacement, replacementPeer := transferPipe(t)
	cm.mu.Lock()
	cm.conn = replacement
	cm.transferEpoch = 9
	cm.acceptingTransfers = true
	cm.mu.Unlock()
	oldWorker, oldPeer := transferPipe(t)
	if _, ok := cm.trackTransferAt("old-worker", 7, oldWorker); ok {
		t.Fatal("old transfer epoch registered into reconnect")
	}
	requireTransferClosed(t, oldPeer)
	_ = controlPeer.Close()
	_ = replacementPeer.Close()
}

func TestPlainTransferDialBarrierRejectsPostDisconnectSocket(t *testing.T) {
	originalDial := transferDial
	t.Cleanup(func() { transferDial = originalDial })

	for _, operation := range []struct {
		name string
		run  func(*connManager, ftEndpoint) error
	}{
		{name: "upload", run: func(cm *connManager, ep ftEndpoint) error {
			return cm.ftUpload(ep, "token", "plain-upload", []byte("data"))
		}},
		{name: "download", run: func(cm *connManager, ep ftEndpoint) error {
			_, err := cm.ftDownload(ep, "token", "plain-download")
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			cm := newTestConnManager()
			control, controlPeer := transferPipe(t)
			cm.mu.Lock()
			cm.conn = control
			cm.transferEpoch = 4
			cm.acceptingTransfers = true
			cm.mu.Unlock()
			data, peer := transferPipe(t)
			started := make(chan struct{})
			release := make(chan struct{})
			transferDial = func(ftEndpoint) (net.Conn, error) {
				close(started)
				<-release
				return data, nil
			}
			done := make(chan error, 1)
			go func() { done <- operation.run(cm, ftEndpoint{epoch: 4}) }()
			<-started
			cm.disconnect()
			close(release)
			if err := <-done; !errors.Is(err, errTransferCanceled) {
				t.Fatalf("post-disconnect %s error = %v", operation.name, err)
			}
			requireTransferClosed(t, peer)
			_ = controlPeer.Close()
		})
	}
}

func TestProgressDownloadDialBarrierClosesPartFile(t *testing.T) {
	originalDial := transferDial
	t.Cleanup(func() { transferDial = originalDial })
	cm := newTestConnManager()
	control, controlPeer := transferPipe(t)
	cm.mu.Lock()
	cm.conn = control
	cm.transferEpoch = 12
	cm.acceptingTransfers = true
	cm.mu.Unlock()
	data, peer := transferPipe(t)
	started := make(chan struct{})
	release := make(chan struct{})
	transferDial = func(ftEndpoint) (net.Conn, error) {
		close(started)
		<-release
		return data, nil
	}
	dest := filepath.Join(t.TempDir(), "download.bin")
	done := make(chan error, 1)
	go func() {
		done <- cm.ftDownloadProgress("progress", ftEndpoint{epoch: 12}, "token", "download", dest, &ftProgress{})
	}()
	<-started
	cm.disconnect()
	close(release)
	if err := <-done; !errors.Is(err, errTransferCanceled) {
		t.Fatalf("post-disconnect progress download = %v", err)
	}
	requireTransferClosed(t, peer)
	part := dest + partSuffix
	if raw, err := os.ReadFile(part); err != nil || len(raw) != 0 {
		t.Fatalf("partial file after rejected dial = %q, %v", raw, err)
	}
	// Rename is a useful Windows assertion: it fails while the writer handle
	// remains open. The rejected registration must always close it first.
	if err := os.Rename(part, dest); err != nil {
		t.Fatalf("partial file remained open: %v", err)
	}
	_ = controlPeer.Close()
}
