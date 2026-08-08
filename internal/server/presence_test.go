// presence_test.go exercises the wave-8b presence handlers: status, pokes,
// and the public server-info query.
package server

import (
	"encoding/json"
	"testing"
	"time"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
)

// TestSetStatus verifies status updates reach state and the broadcast.
func TestSetStatus(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	aliceConn, aliceID := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = aliceConn.Close() }()
	bobConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = bobConn.Close() }()

	send(t, aliceConn, netproto.MsgSetStatus, netproto.SetStatus{Status: "away", Message: "brb"})
	data := readEventOfType(t, bobConn, "status_changed")
	var evt statusEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if evt.ClientID != aliceID || evt.Status != "away" || evt.Message != "brb" {
		t.Fatalf("event = %+v", evt)
	}

	sc, ok := env.state.GetClient(aliceID)
	if !ok || sc.Status != "away" || sc.StatusMessage != "brb" {
		t.Fatalf("state status = %+v", sc)
	}

	// "online" clears the status.
	send(t, aliceConn, netproto.MsgSetStatus, netproto.SetStatus{Status: "online"})
	readEventOfType(t, bobConn, "status_changed")
	sc, _ = env.state.GetClient(aliceID)
	if sc.Status != "" {
		t.Fatalf("status after clearing = %q", sc.Status)
	}

	// Invalid status rejected.
	send(t, aliceConn, netproto.MsgSetStatus, netproto.SetStatus{Status: "sleeping"})
	if e := readError(t, aliceConn); e.Code != errCodeMalformed {
		t.Fatalf("error = %+v, want malformed", e)
	}
}

// TestPoke verifies the gate, the relay, and the cooldown (321/322).
func TestPoke(t *testing.T) {
	// Caller needs b_client_poke (deny-on-unset).
	perms := tieredWith(boolPerm(permissions.PermissionKeyClientPoke, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	aliceConn, aliceID := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = aliceConn.Close() }()
	bobConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = bobConn.Close() }()
	bobID := bobClientID(t, env)

	send(t, aliceConn, netproto.MsgPoke, netproto.Poke{ClientID: bobID, Message: "hey"})
	data := readEventOfType(t, bobConn, "poke")
	var evt pokeEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if evt.FromClientID != aliceID || evt.Message != "hey" {
		t.Fatalf("poke event = %+v", evt)
	}

	// Cooldown: an immediate second poke to the same target is refused.
	send(t, aliceConn, netproto.MsgPoke, netproto.Poke{ClientID: bobID, Message: "again"})
	if e := readError(t, aliceConn); e.Code != errCodeMalformed {
		t.Fatalf("error = %+v, want cooldown refusal", e)
	}
}

// TestPokeDenied verifies deny-on-unset for callers without poke permission.
func TestPokeDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	aliceConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = aliceConn.Close() }()
	bobConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = bobConn.Close() }()

	send(t, aliceConn, netproto.MsgPoke, netproto.Poke{ClientID: bobClientID(t, env)})
	if e := readError(t, aliceConn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}
}

// TestServerInfoQuery verifies the public info response (313).
func TestServerInfoQuery(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	send(t, conn, netproto.MsgServerInfoQuery, netproto.ServerInfoQuery{})
	f := readOfType(t, conn, netproto.MsgServerInfoResponse)
	var resp netproto.ServerInfoResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version == "" || resp.ClientsOnline < 1 {
		t.Fatalf("server info = %+v", resp)
	}
}

// bobClientID returns the admin's client ID in the test env.
func bobClientID(t *testing.T, env *testEnv) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range env.state.ListClients() {
			if c.UniqueID == "admin-uid" {
				return c.ClientID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("admin client not in state")
	return ""
}

// TestInvisibleGate verifies invisible is admin-only (381).
func TestInvisibleGate(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()
	send(t, conn, netproto.MsgSetStatus, netproto.SetStatus{Status: "invisible"})
	if e := readError(t, conn); e.Code != errCodePermissionDenied {
		t.Fatalf("error = %+v, want permission denied", e)
	}
}

// readSnapshot reads the post-auth snapshot of a freshly dialed user and
// returns the visible unique IDs.
func snapshotUIDs(t *testing.T, addr, uid string) map[string]bool {
	t.Helper()
	conn := dialRetry(t, addr)
	defer func() { _ = conn.Close() }()
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: uid, Password: "pw"})
	readOfType(t, conn, netproto.MsgAuthResponse)
	f := readOfType(t, conn, netproto.MsgSnapshot)
	var snap struct {
		RootChannels []struct {
			Clients []struct {
				UniqueID string `json:"unique_id"`
			} `json:"clients"`
			Children []struct {
				Clients []struct {
					UniqueID string `json:"unique_id"`
				} `json:"clients"`
			} `json:"children"`
		} `json:"root_channels"`
	}
	if err := netproto.Decode(f, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	out := map[string]bool{}
	for _, ch := range snap.RootChannels {
		for _, c := range ch.Clients {
			out[c.UniqueID] = true
		}
		for _, child := range ch.Children {
			for _, c := range child.Clients {
				out[c.UniqueID] = true
			}
		}
	}
	return out
}

// TestInvisibleSnapshotFiltering verifies invisible users are hidden from
// non-admin snapshots but visible to admins and themselves (381).
func TestInvisibleSnapshotFiltering(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = adminConn.Close() }()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = userConn.Close() }()

	// Both users must be in a channel to appear in snapshots.
	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)
	send(t, adminConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	waitFor(t, "both clients in channel", func() bool {
		return len(env.state.ChannelMembers(1)) == 2
	})

	// Admin goes invisible.
	send(t, adminConn, netproto.MsgSetStatus, netproto.SetStatus{Status: "invisible"})
	readEventOfType(t, userConn, "user_left") // looks like a leave to non-admins

	// Non-admin snapshot: admin hidden, user visible.
	uids := snapshotUIDs(t, env.addr, "user-uid")
	if uids["admin-uid"] {
		t.Fatal("invisible admin visible in non-admin snapshot")
	}
	if !uids["user-uid"] {
		t.Fatal("user missing from own snapshot")
	}

	// Admin snapshot (also the invisible user's own view): both visible.
	uids = snapshotUIDs(t, env.addr, "admin-uid")
	if !uids["admin-uid"] || !uids["user-uid"] {
		t.Fatalf("admin snapshot missing users: %v", uids)
	}

	// Coming back: non-admin sees a join.
	send(t, adminConn, netproto.MsgSetStatus, netproto.SetStatus{Status: "online"})
	readEventOfType(t, userConn, "user_joined")
	uids = snapshotUIDs(t, env.addr, "user-uid")
	if !uids["admin-uid"] {
		t.Fatal("admin missing after returning online")
	}
}
