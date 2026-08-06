// botbadge_test.go covers the bot flag reaching the in-memory client at
// authentication time (180).
package server

import (
	"testing"

	"voicx/internal/permissions"
	"voicx/internal/state"
)

// stateClientFor returns the in-memory state client of a connected user.
func stateClientFor(t *testing.T, env *testEnv, uniqueID string) *state.Client {
	t.Helper()
	c := srvClient(t, env, uniqueID)
	sc, ok := env.state.GetClient(c.ID)
	if !ok {
		t.Fatalf("no state client for %s", uniqueID)
	}
	return sc
}

// TestAuthSetsBotFlag verifies an account holding b_client_is_bot is marked at
// authentication time, which is what puts the badge in every later snapshot.
func TestAuthSetsBotFlag(t *testing.T) {
	tp := tieredWith(&permissions.Permission{Key: permissions.PermissionKeyClientIsBot, Value: 1})
	env := startTestEnv(t, &tp)
	defer env.stop()
	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = conn.Close() }()

	if !stateClientFor(t, env, "user-uid").IsBot {
		t.Fatalf("account holding b_client_is_bot is not flagged in state")
	}
}

// TestAuthLeavesNonBotsUnflagged covers the two ways a client must NOT get a
// badge: no permission at all, and an admin who would otherwise be swept up by
// the admin bypass that ClientIsBot deliberately skips.
func TestAuthLeavesNonBotsUnflagged(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer func() { _ = userConn.Close() }()
	if stateClientFor(t, env, "user-uid").IsBot {
		t.Errorf("plain user is flagged as a bot")
	}

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer func() { _ = adminConn.Close() }()
	if stateClientFor(t, env, "admin-uid").IsBot {
		t.Errorf("server admin is flagged as a bot")
	}
}

// TestGuestAuthLeavesBotUnflagged: guests have no users row, so the flag can
// never be granted to them.
func TestGuestAuthLeavesBotUnflagged(t *testing.T) {
	tp := tieredWith(&permissions.Permission{Key: permissions.PermissionKeyClientIsBot, Value: 1})
	env := startTestEnv(t, &tp)
	defer env.stop()
	conn, _ := dialGuest(t, env.addr, "g", "")
	defer func() { _ = conn.Close() }()

	for _, c := range env.state.ListClients() {
		if c.IsBot {
			t.Fatalf("guest %s flagged as a bot", c.UniqueID)
		}
	}
}
