// chatenc_test.go covers the client half of chat encryption (91-135): the
// webview boundary (no ciphertext, no key ids ever leave Go), the archival vs
// current key split, the placeholder selection, the MOTD one-shot read,
// attachment sealing and naming, search over decrypted pages, and the
// encrypted export container.
//
// The control channel is faked in-process, so none of this needs a live
// server: the tests assert the client's own invariants, which is exactly where
// a plaintext leak would appear.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"

	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

// --- in-process control channel ---------------------------------------------

// frameHandler answers one client frame. ok=false sends nothing back.
type frameHandler func(f *netproto.Frame) (netproto.MessageType, any, bool)

// serveFrames answers frames on conn until it closes.
func serveFrames(conn net.Conn, handle frameHandler) {
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return
		}
		mt, msg, ok := handle(f)
		if !ok {
			continue
		}
		out, err := netproto.Encode(mt, msg)
		if err != nil {
			return
		}
		if err := netproto.WriteFrame(conn, out); err != nil {
			return
		}
	}
}

// newPipedApp wires an App to a connManager whose connection is an in-process
// pipe answered by handle. It bypasses auth: these tests exercise the request
// paths, not the handshake.
func newPipedApp(t *testing.T, handle frameHandler) (*App, *connManager) {
	t.Helper()
	cli, srv := net.Pipe()
	cm := newConnManager(context.Background())
	cm.sink = &eventRecorder{}
	cm.id = mustTempIdentity(t)
	cm.mu.Lock()
	cm.conn = cli
	cm.mu.Unlock()
	go serveFrames(srv, handle)
	go cm.readLoop(cli)
	t.Cleanup(func() {
		cm.disconnect()
		_ = srv.Close()
	})
	return appWithCM(cm), cm
}

// sealKeyFor seals a scope generation to the client's own X25519 key, the way
// the server does (box.SealAnonymous envelope).
func sealKeyFor(t *testing.T, cm *connManager, scope int64, keyID uint32, key [32]byte) netproto.ChannelKey {
	t.Helper()
	id, err := cm.identity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	pub, _, err := id.x25519()
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	sealed, err := box.SealAnonymous(nil, key[:], &pub, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	return netproto.ChannelKey{
		ChannelID: scope,
		KeyID:     keyID,
		SealedKey: base64.StdEncoding.EncodeToString(sealed),
	}
}

func randKey(t *testing.T) [32]byte {
	t.Helper()
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func mustSeal(t *testing.T, text string, key [32]byte) string {
	t.Helper()
	blob, err := sealScope(text, key)
	if err != nil {
		t.Fatalf("sealScope: %v", err)
	}
	return blob
}

// --- 40 ----------------------------------------------------------------------

// TestChatHistoryStripsCiphertextBeforeReturn is the webview-boundary
// invariant: what ChatHistory returns carries plaintext and nothing else.
func TestChatHistoryStripsCiphertextBeforeReturn(t *testing.T) {
	const canary = "canary-7f3a"
	key := randKey(t)
	var cm *connManager
	app, cm := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) != netproto.MsgChatHistory {
			return 0, nil, false
		}
		return netproto.MsgChatHistoryResponse, netproto.ChatHistoryResponse{
			ChannelID: 7,
			Messages: []netproto.ChatHistoryEntry{{
				ID: 11, FromUniqueID: "u1", FromNickname: "alice",
				BodyEnc: mustSeal(t, canary, key), KeyID: 3, SentAt: 1700000000,
			}},
			Keys: []netproto.ChannelKey{sealKeyFor(t, cm, 7, 3, key)},
		}, true
	})

	resp, err := app.ChatHistory(7, 0, 50)
	if err != nil {
		t.Fatalf("ChatHistory: %v", err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(resp.Messages))
	}
	m := resp.Messages[0]
	if m.Body != canary {
		t.Fatalf("body = %q, want %q", m.Body, canary)
	}
	if !m.EncVerified {
		t.Fatal("EncVerified not set on a body this client opened itself")
	}
	if m.BodyEnc != "" || m.KeyID != 0 {
		t.Fatalf("ciphertext reached the webview: body_enc=%q key_id=%d", m.BodyEnc, m.KeyID)
	}
	if resp.Keys != nil {
		t.Fatal("sealed key material reached the webview")
	}
}

// --- 41 ----------------------------------------------------------------------

// TestArchivalKeyDoesNotDowngradeLatest guards the landmine directly: an
// archival generation from a history page must never push the send key
// backwards, or every following send is rejected as stale.
func TestArchivalKeyDoesNotDowngradeLatest(t *testing.T) {
	s := newScopeKeyStore()
	k7, k3 := randKey(t), randKey(t)
	s.put(5, 7, k7)
	s.putGen(5, 3, k3)

	id, cur, ok := s.current(5)
	if !ok || id != 7 || cur != k7 {
		t.Fatalf("current = %d (ok=%v), want 7", id, ok)
	}
	if got, ok := s.get(5, 3); !ok || got != k3 {
		t.Fatal("archival generation 3 not installed")
	}
	// An out-of-order CURRENT delivery must not move latest backwards either.
	s.put(5, 4, randKey(t))
	if id, _, _ := s.current(5); id != 7 {
		t.Fatalf("current = %d after a stale put, want 7", id)
	}
	if !s.has(5, 4) {
		t.Fatal("has() does not see an installed generation")
	}
}

// --- 42 ----------------------------------------------------------------------

// TestRefuseServerPlaintextHistory closes the downgrade hole from the client
// side: no server config can make this client render a plaintext body.
func TestRefuseServerPlaintextHistory(t *testing.T) {
	app, _ := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) != netproto.MsgChatHistory {
			return 0, nil, false
		}
		return netproto.MsgChatHistoryResponse, netproto.ChatHistoryResponse{
			ChannelID: 7,
			Messages: []netproto.ChatHistoryEntry{{
				ID: 1, FromUniqueID: "u1", Body: "plaintext from a hostile server", SentAt: 1,
			}},
		}, true
	})

	resp, err := app.ChatHistory(7, 0, 50)
	if err != nil {
		t.Fatalf("ChatHistory: %v", err)
	}
	m := resp.Messages[0]
	if m.Body != refusedPlainText {
		t.Fatalf("body = %q, want the refusal placeholder", m.Body)
	}
	if m.EncVerified {
		t.Fatal("EncVerified set on a refused plaintext body")
	}
}

// --- 43 ----------------------------------------------------------------------

// TestMissingVsRefusedPlaceholders keeps a transient truncation apart from a
// permanent refusal: conflating them turns a retryable miss into a permanent
// "[missing key]" on screen.
func TestMissingVsRefusedPlaceholders(t *testing.T) {
	key := randKey(t)
	app, _ := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) != netproto.MsgChatHistory {
			return 0, nil, false
		}
		return netproto.MsgChatHistoryResponse, netproto.ChatHistoryResponse{
			ChannelID: 7,
			Messages: []netproto.ChatHistoryEntry{
				{ID: 2, BodyEnc: mustSeal(t, "withheld", key), KeyID: 5, SentAt: 2},
				{ID: 1, BodyEnc: mustSeal(t, "truncated", key), KeyID: 9, SentAt: 1},
			},
			Refused:   []uint32{5},
			Truncated: true,
		}, true
	})

	resp, err := app.ChatHistory(7, 0, 50)
	if err != nil {
		t.Fatalf("ChatHistory: %v", err)
	}
	if got := resp.Messages[0].Body; got != refusedKeyText {
		t.Fatalf("refused generation rendered %q, want the no-access placeholder", got)
	}
	if got := resp.Messages[1].Body; got != missingKeyText {
		t.Fatalf("truncated generation rendered %q, want the retry placeholder", got)
	}
	for _, m := range resp.Messages {
		if m.EncVerified || m.BodyEnc != "" || m.KeyID != 0 {
			t.Fatalf("unopened entry leaked state: %+v", m)
		}
	}
}

// --- 44 ----------------------------------------------------------------------

// TestMOTDDecryptedBeforeConnectReturns asserts the MOTD is resolved inside
// Connect: App.MOTD() is a one-shot read with no sleep and no event wait.
func TestMOTDDecryptedBeforeConnectReturns(t *testing.T) {
	const motd = "welcome to the canary server"
	key := randKey(t)

	cert, _, err := tlscert.Ensure(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatalf("tlscert.Ensure: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return
		}
		var auth netproto.Authenticate
		if err := netproto.Decode(f, &auth); err != nil {
			return
		}
		// The server can only seal anything if the client published its
		// encryption key WITH the authentication message.
		raw, err := base64.StdEncoding.DecodeString(auth.X25519PublicKey)
		if err != nil || len(raw) != 32 {
			_ = netproto.WriteFrame(conn, mustEncode(netproto.MsgAuthResponse,
				netproto.AuthResponse{OK: false, Reason: "no x25519 key at auth"}))
			return
		}
		var pub [32]byte
		copy(pub[:], raw)
		sealed, err := box.SealAnonymous(nil, key[:], &pub, rand.Reader)
		if err != nil {
			return
		}
		blob, err := sealScope(motd, key)
		if err != nil {
			return
		}
		_ = netproto.WriteFrame(conn, mustEncode(netproto.MsgAuthResponse, netproto.AuthResponse{
			OK: true, ClientID: "c1", UniqueID: "u1", Nickname: "alice",
			MOTD: blob, MOTDEnc: true, MOTDKeyID: 4,
			ChatKeys: []netproto.ChannelKey{{
				ChannelID: 0, KeyID: 4,
				SealedKey: base64.StdEncoding.EncodeToString(sealed),
			}},
		}))
		// Drain whatever the client sends next (the key publish) until close.
		for {
			if _, err := netproto.ReadFrame(conn); err != nil {
				return
			}
		}
	}()

	cm, _ := newTestBackend(t)
	app := appWithCM(cm)
	if reason := cm.connect(ln.Addr().String(), "alice", "pw", ""); reason != "" {
		t.Fatalf("connect: %s", reason)
	}
	defer cm.disconnect()

	if got := app.MOTD(); got != motd {
		t.Fatalf("MOTD() = %q immediately after Connect, want %q", got, motd)
	}
	if _, _, ok := cm.scopeKeys.current(0); !ok {
		t.Fatal("the global generation from the AuthResponse was not installed")
	}
}

// mustEncode panics-free frame encoding for the fake server goroutine.
func mustEncode(mt netproto.MessageType, msg any) *netproto.Frame {
	f, err := netproto.Encode(mt, msg)
	if err != nil {
		return &netproto.Frame{Type: uint16(mt)}
	}
	return f
}

// --- 45 ----------------------------------------------------------------------

// TestAttachmentSealRoundTrip verifies the attachment container: raw bytes,
// nonce[24] || secretbox, wrong key and tampering both rejected.
func TestAttachmentSealRoundTrip(t *testing.T) {
	key, other := randKey(t), randKey(t)
	data := []byte{0x00, 0xFF, 0x10, 'h', 'i', 0x00}

	blob, err := sealFile(data, key)
	if err != nil {
		t.Fatalf("sealFile: %v", err)
	}
	if len(blob) != 24+len(data)+16 {
		t.Fatalf("blob length = %d, want nonce+data+tag", len(blob))
	}
	got, err := openFile(blob, key)
	if err != nil {
		t.Fatalf("openFile: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("round trip = %q", got)
	}
	if _, err := openFile(blob, other); err == nil {
		t.Fatal("openFile with the wrong key succeeded")
	}
	blob[len(blob)-1] ^= 0xFF
	if _, err := openFile(blob, key); err == nil {
		t.Fatal("openFile on a tampered blob succeeded")
	}
	if _, err := openFile([]byte{1, 2, 3}, key); err == nil {
		t.Fatal("openFile on a truncated blob succeeded")
	}
}

// TestFileTokenParse covers the body-token grammar, including the two legacy
// shapes that must degrade to a plain reference rather than throwing.
func TestFileTokenParse(t *testing.T) {
	tests := []struct {
		name, capture, storage, key, display string
	}{
		{"full", "abc.vcx#S0VZ#photo.png", "abc.vcx", "S0VZ", "photo.png"},
		{"legacy", "photo.png", "photo.png", "", "photo.png"},
		{"single separator", "abc.vcx#S0VZ", "abc.vcx#S0VZ", "", "abc.vcx#S0VZ"},
		{"empty display", "abc.vcx#S0VZ#", "abc.vcx", "S0VZ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storage, key, display := parseFileRef(tc.capture)
			if storage != tc.storage || key != tc.key || display != tc.display {
				t.Fatalf("parseFileRef(%q) = %q/%q/%q, want %q/%q/%q",
					tc.capture, storage, key, display, tc.storage, tc.key, tc.display)
			}
		})
	}

	// The display name can never reintroduce a separator.
	if got := safeDisplayName("we#ird]name.png"); strings.ContainsAny(got, "#]") {
		t.Fatalf("safeDisplayName left a separator: %q", got)
	}
}

// --- 46 ----------------------------------------------------------------------

// TestAttachmentStorageNameIsContentDerived verifies two uploads of the same
// display name land on different objects, which is what removes the
// (channel_id, name) collision and the .v1..v3 rotation for chat attachments.
func TestAttachmentStorageNameIsContentDerived(t *testing.T) {
	data := []byte("the same screenshot pasted twice")
	first, err := sealFile(data, randKey(t))
	if err != nil {
		t.Fatalf("sealFile: %v", err)
	}
	second, err := sealFile(data, randKey(t))
	if err != nil {
		t.Fatalf("sealFile: %v", err)
	}

	n1, n2 := attachmentStorageName(first), attachmentStorageName(second)
	if n1 == n2 {
		t.Fatal("identical storage names: the second upload would overwrite the first")
	}
	for _, n := range []string{n1, n2} {
		if !strings.HasSuffix(n, ".vcx") || len(n) != 36 {
			t.Fatalf("storage name %q is not hex(sha256)[:32]+.vcx", n)
		}
	}
	// The name is derived from the ciphertext, so the same blob always
	// addresses the same object.
	if attachmentStorageName(first) != n1 {
		t.Fatal("storage name is not stable for the same blob")
	}
}

// --- 47 ----------------------------------------------------------------------

// TestChatSearchDecryptsPages finds a canary that exists only as ciphertext
// server-side, and reports the messages under a generation it cannot obtain
// instead of silently dropping them.
func TestChatSearchDecryptsPages(t *testing.T) {
	const canary = "needle-9c2b"
	key := randKey(t)
	lost := randKey(t) // a generation the server never hands over
	var cm *connManager

	app, cm := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) != netproto.MsgChatHistory {
			return 0, nil, false
		}
		var req netproto.ChatHistory
		if err := netproto.Decode(f, &req); err != nil {
			return 0, nil, false
		}
		resp := netproto.ChatHistoryResponse{
			ChannelID: 7,
			Keys:      []netproto.ChannelKey{sealKeyFor(t, cm, 7, 1, key)},
		}
		if req.BeforeID == 0 {
			// A full page, newest first: ids 1200 down to 1001.
			for i := 0; i < chatSearchPage; i++ {
				e := netproto.ChatHistoryEntry{ID: int64(1200 - i), KeyID: 1, SentAt: 1}
				if i == 0 {
					e.KeyID = 2 // unobtainable generation
					e.BodyEnc = mustSeal(t, canary+" but sealed to a lost key", lost)
				} else {
					e.BodyEnc = mustSeal(t, "filler", key)
				}
				resp.Messages = append(resp.Messages, e)
			}
			return netproto.MsgChatHistoryResponse, resp, true
		}
		// Short second page ends the paging loop.
		resp.Messages = []netproto.ChatHistoryEntry{
			{ID: 1000, KeyID: 1, BodyEnc: mustSeal(t, "a "+canary+" in the haystack", key), SentAt: 1},
			{ID: 999, KeyID: 1, BodyEnc: mustSeal(t, "filler", key), SentAt: 1},
			{ID: 998, Deleted: true, SentAt: 1},
		}
		return netproto.MsgChatHistoryResponse, resp, true
	})

	res, err := app.ChatSearch(7, canary, 1000)
	if err != nil {
		t.Fatalf("ChatSearch: %v", err)
	}
	if len(res.Messages) != 1 || !strings.Contains(res.Messages[0].Body, canary) {
		t.Fatalf("matches = %+v, want exactly the decrypted canary", res.Messages)
	}
	if res.Undecryptable != 1 {
		t.Fatalf("undecryptable = %d, want 1", res.Undecryptable)
	}
	if res.Scanned != chatSearchPage+3 {
		t.Fatalf("scanned = %d, want %d", res.Scanned, chatSearchPage+3)
	}
	for _, m := range res.Messages {
		if m.BodyEnc != "" || m.KeyID != 0 {
			t.Fatalf("search result carries ciphertext: %+v", m)
		}
	}
}

// --- 48 ----------------------------------------------------------------------

// TestExportChatEncryptedRoundTrip verifies the export container: magic,
// argon2id salt, nonce, secretbox — and that a wrong passphrase fails closed.
func TestExportChatEncryptedRoundTrip(t *testing.T) {
	const contents = "[2026-08-01 10:00] alice: canary-7f3a\n"

	blob, err := sealExport(contents, "correct horse battery staple")
	if err != nil {
		t.Fatalf("sealExport: %v", err)
	}
	if strings.Contains(string(blob), "canary-7f3a") {
		t.Fatal("export container holds plaintext")
	}
	if len(blob) < len(exportMagic)+exportSaltLen+24 {
		t.Fatalf("container too short: %d bytes", len(blob))
	}
	if string(blob[:len(exportMagic)]) != string(exportMagic) {
		t.Fatal("container does not start with the magic")
	}

	got, err := openExport(blob, "correct horse battery staple")
	if err != nil {
		t.Fatalf("openExport: %v", err)
	}
	if got != contents {
		t.Fatalf("round trip = %q", got)
	}
	if _, err := openExport(blob, "wrong"); err == nil {
		t.Fatal("openExport with the wrong passphrase succeeded")
	}
	if _, err := openExport([]byte("NOTVOICX................................"), "x"); err == nil {
		t.Fatal("openExport accepted a foreign file")
	}
	if _, err := sealExport(contents, ""); err == nil {
		t.Fatal("sealExport accepted an empty passphrase")
	}
}

// TestChatPinsStripsCiphertextBeforeReturn is the pins half of the webview
// boundary — pinned bodies are ciphertext on the wire for the same reason
// history bodies are.
func TestChatPinsStripsCiphertextBeforeReturn(t *testing.T) {
	const canary = "pinned-4d1e"
	key := randKey(t)
	var cm *connManager
	app, cm := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) != netproto.MsgChatPins {
			return 0, nil, false
		}
		return netproto.MsgChatPinsResponse, netproto.ChatPinsResponse{
			ChannelID: 7,
			Pins: []netproto.ChatPinEntry{{
				MessageID: 11, PinnedBy: "u1", PinnedAt: time.Now().Unix(),
				Message: &netproto.ChatHistoryEntry{
					ID: 11, BodyEnc: mustSeal(t, canary, key), KeyID: 2, SentAt: 1,
				},
			}},
			Keys: []netproto.ChannelKey{sealKeyFor(t, cm, 7, 2, key)},
		}, true
	})

	resp, err := app.ChatPins(7)
	if err != nil {
		t.Fatalf("ChatPins: %v", err)
	}
	m := resp.Pins[0].Message
	if m.Body != canary || !m.EncVerified {
		t.Fatalf("pinned body = %q (verified=%v)", m.Body, m.EncVerified)
	}
	if m.BodyEnc != "" || m.KeyID != 0 || resp.Keys != nil {
		t.Fatal("ciphertext or key material reached the webview")
	}
}
