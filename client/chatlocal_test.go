// chatlocal_test.go covers the wave-4b client chat work that keeps data on
// THIS device or on the account rather than in a channel: the server-held
// last-read pointer (121), the encrypted local DM log (122), the full-history
// export (125), and the scrollback's behaviour across generations this client
// was never granted (103/101).
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voicx/internal/netproto"
)

// timeoutC bounds every "did this frame arrive" wait in this file. It is short
// on purpose: these are in-process pipes, so a miss is a bug, not slowness.
func timeoutC(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(2 * time.Second)
}

// newLocalApp builds an App with a private settings path, so every local file
// this suite writes lands in the test's temp dir and never in the developer's
// real config directory.
func newLocalApp(t *testing.T, cm *connManager) *App {
	t.Helper()
	a := appWithCM(cm)
	a.settingsPath = filepath.Join(t.TempDir(), "settings.json")
	return a
}

// --- 122 ---------------------------------------------------------------------

// TestDMHistoryIsCiphertextAtRest is the 122 equivalent of wave 1's storage
// guarantee: the DM log on disk holds no plaintext, survives a restart, and is
// unreadable to any other identity.
func TestDMHistoryIsCiphertextAtRest(t *testing.T) {
	const canary = "dm-canary-31af"
	_, cm := newPipedApp(t, func(*netproto.Frame) (netproto.MessageType, any, bool) {
		return 0, nil, false
	})
	a := newLocalApp(t, cm)

	if err := a.DMHistoryAppend("peer-1", "bob", DMEntry{
		FromUniqueID: "peer-1", FromNickname: "bob", Body: canary, SentAt: 1000,
	}); err != "" {
		t.Fatalf("DMHistoryAppend: %s", err)
	}
	if err := a.DMHistoryAppend("peer-1", "bob", DMEntry{
		FromUniqueID: "me", FromNickname: "alice", Body: "reply", SentAt: 1001, Self: true,
	}); err != "" {
		t.Fatalf("DMHistoryAppend: %s", err)
	}

	path, err := a.dmHistoryPath("peer-1")
	if err != nil {
		t.Fatalf("dmHistoryPath: %v", err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(blob), canary) {
		t.Fatal("DM log holds plaintext at rest")
	}
	if strings.Contains(string(blob), "peer-1") {
		t.Fatal("DM log names the peer outside the ciphertext")
	}
	if strings.Contains(filepath.Base(path), "peer-1") {
		t.Fatal("DM log file name enumerates the peer")
	}

	// A fresh App over the same directory and identity reads it back: this is
	// the "survives a restart" half.
	restarted := appWithCM(cm)
	restarted.settingsPath = a.settingsPath
	msgs, err := restarted.DMHistoryLoad("peer-1")
	if err != nil {
		t.Fatalf("DMHistoryLoad: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Body != canary || !msgs[1].Self {
		t.Fatalf("loaded %+v, want the two stored messages in order", msgs)
	}
	if msgs[0].Seq != 1 || msgs[1].Seq != 2 {
		t.Fatalf("seq = %d,%d, want 1,2", msgs[0].Seq, msgs[1].Seq)
	}

	peers := restarted.DMHistoryPeers()
	if len(peers) != 1 || peers[0].UniqueID != "peer-1" || peers[0].Messages != 2 {
		t.Fatalf("peers = %+v, want one peer with 2 messages", peers)
	}

	// A different identity over the same files reads nothing at all.
	other := newConnManager(nil)
	other.sink = &eventRecorder{}
	other.id = mustTempIdentity(t)
	stranger := appWithCM(other)
	stranger.settingsPath = a.settingsPath
	if got := stranger.DMHistoryPeers(); len(got) != 0 {
		t.Fatalf("a foreign identity listed %d conversations", len(got))
	}
}

// TestDMHistoryDedupesAndSearches covers the two behaviours the DM tabs rely
// on: a replayed offline message does not double up, and the local log answers
// in the shape the existing client-side search renders.
func TestDMHistoryDedupesAndSearches(t *testing.T) {
	_, cm := newPipedApp(t, func(*netproto.Frame) (netproto.MessageType, any, bool) {
		return 0, nil, false
	})
	a := newLocalApp(t, cm)

	e := DMEntry{FromUniqueID: "peer-1", FromNickname: "bob", Body: "needle in here", SentAt: 5, ClientMsgID: "abc"}
	if err := a.DMHistoryAppend("peer-1", "bob", e); err != "" {
		t.Fatalf("append: %s", err)
	}
	if err := a.DMHistoryAppend("peer-1", "bob", e); err != "" {
		t.Fatalf("replayed append: %s", err)
	}
	msgs, err := a.DMHistoryLoad("peer-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("%d messages stored, want 1 (a replayed client_msg_id duplicated the log)", len(msgs))
	}

	if err := a.DMHistoryAppend("peer-2", "carol", DMEntry{
		FromUniqueID: "peer-2", FromNickname: "carol", Body: "another needle", SentAt: 6,
	}); err != "" {
		t.Fatalf("append peer-2: %s", err)
	}

	res, err := a.DMSearch("", "needle", 0)
	if err != nil {
		t.Fatalf("DMSearch: %v", err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("matches = %d, want 2 across both conversations", len(res.Messages))
	}
	for _, m := range res.Messages {
		if !m.EncVerified {
			t.Fatal("a locally stored DM is not marked verified")
		}
		if m.BodyEnc != "" || m.KeyID != 0 {
			t.Fatalf("DM search result carries crypto state: %+v", m)
		}
	}

	if err := a.DMHistoryClear("peer-1"); err != "" {
		t.Fatalf("DMHistoryClear: %s", err)
	}
	if msgs, _ := a.DMHistoryLoad("peer-1"); len(msgs) != 0 {
		t.Fatalf("%d messages survived a clear", len(msgs))
	}
}

// TestDMHistoryRefusesWithoutSettingsPath keeps the suite (and any App built
// without a settings path) from writing into the real config directory.
func TestDMHistoryRefusesWithoutSettingsPath(t *testing.T) {
	_, cm := newPipedApp(t, func(*netproto.Frame) (netproto.MessageType, any, bool) {
		return 0, nil, false
	})
	a := appWithCM(cm) // no settingsPath, and the default fallback is disarmed
	if err := a.DMHistoryAppend("peer-1", "bob", DMEntry{Body: "x"}); err == "" {
		t.Fatal("DM history wrote somewhere with no settings path configured")
	}
}

// --- 125 ---------------------------------------------------------------------

// TestChatExportHistoryPagesWholeChannel exports past the loaded window: it
// pages the whole scope, keeps the unreadable entries visible, and orders the
// transcript oldest-first.
func TestChatExportHistoryPagesWholeChannel(t *testing.T) {
	key := randKey(t)
	lost := randKey(t)
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
			Refused:   []uint32{2},
		}
		if req.BeforeID == 0 {
			for i := 0; i < chatSearchPage; i++ {
				e := netproto.ChatHistoryEntry{
					ID: int64(1200 - i), KeyID: 1, SentAt: 1700000000,
					FromNickname: "alice", BodyEnc: mustSeal(t, "page one", key),
				}
				if i == 0 {
					// A generation this client was never granted: it must show
					// as a refusal, not vanish from the export (103).
					e.KeyID, e.BodyEnc = 2, mustSeal(t, "withheld", lost)
				}
				resp.Messages = append(resp.Messages, e)
			}
			return netproto.MsgChatHistoryResponse, resp, true
		}
		resp.Messages = []netproto.ChatHistoryEntry{
			{ID: 1000, KeyID: 1, FromNickname: "alice", BodyEnc: mustSeal(t, "oldest line", key), SentAt: 1700000000},
			{ID: 999, Deleted: true, FromNickname: "bob", SentAt: 1700000000},
		}
		return netproto.MsgChatHistoryResponse, resp, true
	})

	res, err := app.ChatExportHistory(7, 0)
	if err != nil {
		t.Fatalf("ChatExportHistory: %v", err)
	}
	if res.Messages != chatSearchPage+2 {
		t.Fatalf("exported %d messages, want %d — the export stopped at the loaded window",
			res.Messages, chatSearchPage+2)
	}
	if !res.Complete {
		t.Fatal("a fully paged export reported itself incomplete")
	}
	if res.Undecryptable != 1 {
		t.Fatalf("undecryptable = %d, want 1", res.Undecryptable)
	}
	if !strings.Contains(res.Text, refusedKeyText) {
		t.Fatal("the withheld message is missing from the transcript entirely")
	}
	if !strings.Contains(res.Text, "(deleted)") {
		t.Fatal("the tombstoned message is missing from the transcript")
	}
	lines := strings.Split(strings.TrimSuffix(res.Text, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "# voicx export") {
		t.Fatalf("first line = %q, want the partial-export notice", lines[0])
	}
	// Page two holds the oldest ids (999 tombstoned, then 1000), so a
	// transcript in reading order opens with them and closes with page one.
	if !strings.Contains(lines[1], "(deleted)") || !strings.Contains(lines[2], "oldest line") {
		t.Fatalf("transcript opens with %q / %q, want the oldest messages first", lines[1], lines[2])
	}
	if strings.Index(res.Text, "oldest line") > strings.Index(res.Text, "page one") {
		t.Fatal("transcript is newest-first")
	}
}

// --- 103 / 101 ----------------------------------------------------------------

// TestScrollbackIntoUngrantedGenerations walks history back into generations
// this client can never obtain and asserts it degrades into placeholders and
// keeps paging, instead of erroring out and looking like data loss.
func TestScrollbackIntoUngrantedGenerations(t *testing.T) {
	key := randKey(t)
	lost := randKey(t)
	var cm *connManager
	app, cm := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) != netproto.MsgChatHistory {
			return 0, nil, false
		}
		var req netproto.ChatHistory
		if err := netproto.Decode(f, &req); err != nil {
			return 0, nil, false
		}
		if req.BeforeID == 0 {
			return netproto.MsgChatHistoryResponse, netproto.ChatHistoryResponse{
				ChannelID: 7,
				Keys:      []netproto.ChannelKey{sealKeyFor(t, cm, 7, 3, key)},
				Messages: []netproto.ChatHistoryEntry{
					{ID: 30, KeyID: 3, BodyEnc: mustSeal(t, "recent", key), SentAt: 3},
				},
			}, true
		}
		// Older page: generation 2 is refused outright, generation 1 is merely
		// truncated out of the bundle, and generation 3's body was tampered.
		return netproto.MsgChatHistoryResponse, netproto.ChatHistoryResponse{
			ChannelID: 7,
			Keys:      []netproto.ChannelKey{sealKeyFor(t, cm, 7, 3, key)},
			Refused:   []uint32{2},
			Truncated: true,
			Messages: []netproto.ChatHistoryEntry{
				{ID: 20, KeyID: 2, BodyEnc: mustSeal(t, "no access", lost), SentAt: 2},
				{ID: 19, KeyID: 1, BodyEnc: mustSeal(t, "not bundled", lost), SentAt: 2},
				{ID: 18, KeyID: 3, BodyEnc: mustSeal(t, "tampered", lost), SentAt: 2},
			},
		}, true
	})

	if _, err := app.ChatHistory(7, 0, 50); err != nil {
		t.Fatalf("first page: %v", err)
	}
	resp, err := app.ChatHistory(7, 30, 50)
	if err != nil {
		t.Fatalf("scrollback page: %v", err)
	}
	if !resp.Truncated {
		t.Fatal("Truncated was dropped before the UI could see it")
	}
	want := []string{refusedKeyText, missingKeyText, decryptFailedText}
	for i, w := range want {
		if got := resp.Messages[i].Body; got != w {
			t.Fatalf("message %d rendered %q, want %q", resp.Messages[i].ID, got, w)
		}
		if resp.Messages[i].EncVerified {
			t.Fatalf("message %d claims verified decryption", resp.Messages[i].ID)
		}
		if resp.Messages[i].BodyEnc != "" || resp.Messages[i].KeyID != 0 {
			t.Fatalf("message %d leaked crypto state to the webview", resp.Messages[i].ID)
		}
	}
}

// TestEditSealsUnderCurrentGeneration is the 101 invariant: an edit is sealed
// with the scope's CURRENT generation, and an older generation installed by a
// scrollback page must not drag the edit key backwards.
func TestEditSealsUnderCurrentGeneration(t *testing.T) {
	cur, old := randKey(t), randKey(t)
	edits := make(chan netproto.ChatEdit, 1)
	_, cm := newPipedApp(t, func(f *netproto.Frame) (netproto.MessageType, any, bool) {
		if netproto.MessageType(f.Type) == netproto.MsgChatEdit {
			var e netproto.ChatEdit
			if err := netproto.Decode(f, &e); err == nil {
				edits <- e
			}
		}
		return 0, nil, false
	})
	app := newLocalApp(t, cm)

	cm.scopeKeys.put(7, 9, cur)
	cm.scopeKeys.putGen(7, 4, old) // archival generation from a history page

	if err := app.ChatEditMessage(7, 11, "corrected text"); err != "" {
		t.Fatalf("ChatEditMessage: %s", err)
	}
	select {
	case e := <-edits:
		if e.KeyID != 9 {
			t.Fatalf("edit sealed under generation %d, want the current 9", e.KeyID)
		}
		if !e.Enc || e.NewText == "corrected text" {
			t.Fatalf("edit left the client unsealed: %+v", e)
		}
		if got, err := openScope(e.NewText, cur); err != nil || got != "corrected text" {
			t.Fatalf("edit does not open under the current generation: %v / %q", err, got)
		}
	case <-timeoutC(t):
		t.Fatal("no edit frame reached the server")
	}

	// With no generation at all the edit is refused locally rather than sent
	// in the clear.
	if err := app.ChatEditMessage(42, 11, "x"); err == "" {
		t.Fatal("an edit for a scope with no key was accepted")
	}
}
