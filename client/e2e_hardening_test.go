package main

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"voicx/internal/netproto"
)

func TestPubKeyCacheTTLAndLRUEviction(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := newPubKeyCacheAt(func() time.Time { return now })
	cache.put("expired", [32]byte{1})
	now = now.Add(pubKeyCacheTTL)
	if _, ok := cache.get("expired"); ok {
		t.Fatal("expired public key remained in cache")
	}

	for i := range pubKeyCacheCapacity {
		cache.put("peer-"+strconv.Itoa(i), [32]byte{byte(i)})
	}
	first := "peer-0"
	second := "peer-1"
	if _, ok := cache.get(first); !ok {
		t.Fatal("cache unexpectedly lost first key before eviction")
	}
	cache.put("newest", [32]byte{255})
	if _, ok := cache.get(second); ok {
		t.Fatal("LRU eviction retained the oldest untouched key")
	}
	if _, ok := cache.get(first); !ok {
		t.Fatal("LRU eviction removed recently used key")
	}

	cm := newTestConnManager()
	cm.pubKeys.put("peer", [32]byte{9})
	cm.disconnect()
	if _, ok := cm.pubKeys.get("peer"); ok {
		t.Fatal("disconnect did not clear public-key cache")
	}
}

func TestPeerPubKeyCoalescesConcurrentDirectoryFetch(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	cm := newTestConnManager()
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	go cm.readLoop(client)

	peer := [32]byte{7}
	requests := make(chan struct{}, 2)
	go func() {
		frame := readFrame(t, server)
		if got := netproto.MessageType(frame.Type); got != netproto.MsgKeyRequest {
			t.Errorf("directory request = %v", got)
			return
		}
		requests <- struct{}{}
		_ = netproto.WriteFrame(server, mustEncode(netproto.MsgKeyResponse, netproto.KeyResponse{
			UniqueID:  "peer",
			PublicKey: base64.StdEncoding.EncodeToString(peer[:]),
		}))
	}()

	var wg sync.WaitGroup
	errs := make(chan bool, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, ok := cm.peerPubKey("peer")
			errs <- !ok || got != peer
		}()
	}
	wg.Wait()
	close(errs)
	for bad := range errs {
		if bad {
			t.Fatal("concurrent peer lookup returned wrong key")
		}
	}
	if len(requests) != 1 {
		t.Fatalf("directory requests = %d, want 1", len(requests))
	}
}

func TestPubKeyCacheEpochClearsFlightsAndRejectsOldLeader(t *testing.T) {
	cache := newPubKeyCache()
	_, cached, leader, oldDone, oldEpoch := cache.getOrClaimFetch("peer")
	if cached || !leader {
		t.Fatal("first fetch was not leader")
	}
	cache.clear()
	select {
	case <-oldDone:
	default:
		t.Fatal("clear did not wake old fetch follower")
	}
	_, cached, leader, newDone, newEpoch := cache.getOrClaimFetch("peer")
	if cached || !leader || newEpoch == oldEpoch {
		t.Fatalf("post-clear fetch = cached:%v leader:%v epoch:%d/%d", cached, leader, newEpoch, oldEpoch)
	}
	if cache.putAt("peer", [32]byte{1}, oldEpoch) {
		t.Fatal("old leader repopulated current cache")
	}
	if cache.putAt("peer", [32]byte{2}, newEpoch) == false {
		t.Fatal("current leader result rejected")
	}
	cache.finishFetch("peer", oldEpoch)
	select {
	case <-newDone:
		t.Fatal("old leader completed new epoch flight")
	default:
	}
	cache.finishFetch("peer", newEpoch)
	select {
	case <-newDone:
	default:
		t.Fatal("current flight did not complete")
	}
	if got, ok := cache.get("peer"); !ok || got != [32]byte{2} {
		t.Fatalf("cache = %v/%v, want current key", got, ok)
	}
}

func TestPeerPubKeyRejectsMismatchedDirectoryUID(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	cm := newTestConnManager()
	cm.mu.Lock()
	cm.conn = client
	cm.mu.Unlock()
	go cm.readLoop(client)
	result := make(chan bool, 1)
	go func() { _, ok := cm.peerPubKey("wanted"); result <- ok }()
	_ = readFrame(t, server)
	key := [32]byte{1}
	if err := netproto.WriteFrame(server, mustEncode(netproto.MsgKeyResponse, netproto.KeyResponse{
		UniqueID:  "other",
		PublicKey: base64.StdEncoding.EncodeToString(key[:]),
	})); err != nil {
		t.Fatal(err)
	}
	if <-result {
		t.Fatal("mismatched KeyResponse UID was accepted")
	}
	if _, ok := cm.pubKeys.get("wanted"); ok {
		t.Fatal("mismatched KeyResponse populated cache")
	}
}

func TestScopeDecryptFollowersQueueForReplay(t *testing.T) {
	store := newScopeKeyStore()
	var got []string
	leader, queued := store.queuePull(7, 3, scopeDecryptWork{
		resolve: func() string { return "leader plain" },
		emit:    func(text string) { got = append(got, "leader:"+text) },
	})
	if !leader || !queued {
		t.Fatal("first scope decrypt did not become leader")
	}
	for _, label := range []string{"one", "two", "three"} {
		leader, queued = store.queuePull(7, 3, scopeDecryptWork{
			resolve: func() string { return label + " plain" },
			emit:    func(text string) { got = append(got, label+":"+text) },
		})
		if leader || !queued {
			t.Fatal("follower was not queued")
		}
	}
	callbacks := store.finishQueuedPull(7, 3)
	got = append(got, "leader:leader plain")
	for _, callback := range callbacks {
		callback.emit(callback.resolve())
	}
	if len(got) != 4 {
		t.Fatalf("replayed events = %v", got)
	}
	if want := []string{"leader:leader plain", "one:one plain", "two:two plain", "three:three plain"}; !slices.Equal(got, want) {
		t.Fatalf("distinct follower plaintexts = %v, want %v", got, want)
	}
}

func TestDecryptWorkIsBoundedAndSaturationStripsCiphertext(t *testing.T) {
	cm := newTestConnManager()
	hold := make(chan struct{})
	started := make(chan struct{}, maxAsyncDecrypts)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for range maxAsyncDecrypts {
		if !cm.startDecrypt("test", func() {
			current := active.Add(1)
			started <- struct{}{}
			for {
				prior := peak.Load()
				if current <= prior || peak.CompareAndSwap(prior, current) {
					break
				}
			}
			<-hold
			active.Add(-1)
		}) {
			t.Fatal("failed to fill decrypt capacity")
		}
	}
	for range maxAsyncDecrypts {
		<-started
	}
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cm.startDecrypt("overflow", func() {}) {
				t.Error("decrypt work exceeded bounded capacity")
			}
		}()
	}
	wg.Wait()

	payload, err := json.Marshal(map[string]any{"type": "chat", "data": netproto.ChatBroadcast{
		Text: "attacker-ciphertext", Enc: true, KeyID: 99,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := cm.maybeDecryptEvent(string(payload))
	if out == "" || strings.Contains(out, "attacker-ciphertext") || !strings.Contains(out, missingKeyText) {
		t.Fatalf("saturated decrypt result leaked ciphertext: %s", out)
	}
	for _, tc := range []struct {
		name      string
		eventType string
		data      any
	}{
		{name: "dm", eventType: "chat", data: netproto.ChatBroadcast{Text: "dm-ciphertext", Enc: true, E2E: true, KeyID: 99}},
		{name: "scope_chat", eventType: "chat", data: netproto.ChatBroadcast{Text: "chat-ciphertext", Enc: true, KeyID: 99}},
		{name: "chat_edited", eventType: "chat_edited", data: map[string]any{"enc": true, "key_id": 99, "body": "edit-ciphertext", "body_enc": "edit-ciphertext", "keys": []string{"secret"}}},
		{name: "announcement", eventType: "announcement", data: map[string]any{"enc": true, "key_id": 99, "text": "announce-ciphertext", "body_enc": "announce-ciphertext", "keys": []string{"secret"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"type": tc.eventType, "data": tc.data})
			if err != nil {
				t.Fatal(err)
			}
			out := cm.maybeDecryptEvent(string(payload))
			if out == "" || !strings.Contains(out, missingKeyText) {
				t.Fatalf("saturated output = %q", out)
			}
			for _, forbidden := range []string{"ciphertext", `"enc"`, `"e2e"`, `"key_id"`, `"body_enc"`, `"keys"`} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("saturated %s leaked %s: %s", tc.name, forbidden, out)
				}
			}
		})
	}
	if peak.Load() > maxAsyncDecrypts {
		t.Fatalf("active decrypts = %d", peak.Load())
	}
	close(hold)
	deadline := time.Now().Add(time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !cm.startDecrypt("recovered", func() {}) {
		t.Fatal("decrypt slot did not recover")
	}
}
