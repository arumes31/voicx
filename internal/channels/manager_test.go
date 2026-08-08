package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/state"
	"voicx/internal/store"
)

// testCleanupDelay is the short cleanup delay used by tests so the cleanup
// goroutine fires quickly.
const testCleanupDelay = 50 * time.Millisecond

// testEnv constructs a ChannelManager backed by a real Postgres store if one
// is reachable. It skips the calling test when no database is available. It
// also runs migrations and returns a fresh state.Manager.
func testEnv(t *testing.T) (*ChannelManager, *store.Store, *state.Manager) {
	t.Helper()

	dbURL := os.Getenv("VOICX_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable"
	}

	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment: %v", err)
	}

	s, err := store.New(dbURL, logger, 5, 1, time.Minute)
	if err != nil {
		t.Skipf("database unavailable, skipping: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Migrate(); err != nil {
		t.Skipf("migrate failed, skipping: %v", err)
	}

	sm := state.New(logger)
	mgr := New(s, sm, logger)
	mgr.CleanupDelay = testCleanupDelay
	t.Cleanup(func() { mgr.Close() })
	return mgr, s, sm
}

// createTestUser inserts a fresh user row and returns its id. Used as the
// CreatedBy value for channels that require a creator.
func createTestUser(t *testing.T, s *store.Store) int64 {
	t.Helper()
	const q = `INSERT INTO users (unique_id, nickname, created_at)
	          VALUES ($1, $2, NOW()) RETURNING id`
	var id int64
	err := s.DB().QueryRowContext(context.Background(), q,
		fmt.Sprintf("uid-%d", time.Now().UnixNano()),
		fmt.Sprintf("chan-test-%d", time.Now().UnixNano()),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return id
}

// createParentChannel inserts a permanent parent channel directly in the DB and
// registers it in the state manager. Returns its id.
func createParentChannel(t *testing.T, mgr *ChannelManager, sm *state.Manager, userID int64) int64 {
	t.Helper()
	id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      fmt.Sprintf("parent-%d", time.Now().UnixNano()),
		Type:      ChannelTypePermanent,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("create parent channel: %v", err)
	}
	return id
}

// channelExistsInDB reports whether a channel row with the given id exists.
func channelExistsInDB(t *testing.T, s *store.Store, id int64) bool {
	t.Helper()
	var exists bool
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1)`, id,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("channelExistsInDB: %v", err)
	}
	return exists
}

// channelTypeInDB returns the channel_type column for the given id.
func channelTypeInDB(t *testing.T, s *store.Store, id int64) int16 {
	t.Helper()
	var ct int16
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT channel_type FROM channels WHERE id = $1`, id,
	).Scan(&ct)
	if err != nil {
		t.Fatalf("channelTypeInDB: %v", err)
	}
	return ct
}

// pollCondition calls fn repeatedly until it returns true or the timeout
// elapses, failing the test on timeout.
func pollCondition(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func activeCleanupToken(t *testing.T, mgr *ChannelManager, channelID int64) *cleanupTimer {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	token := mgr.timers[channelID]
	if token == nil {
		t.Fatalf("channel %d has no active cleanup token", channelID)
	}
	return token
}

func hasCleanupTimer(mgr *ChannelManager, channelID int64) bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	_, ok := mgr.timers[channelID]
	return ok
}

// TestCreateChannel_AllTypes verifies that CreateChannel inserts each channel
// type into the DB and registers it in the state manager.
func TestCreateChannel_AllTypes(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	parentID := createParentChannel(t, mgr, sm, userID)

	cases := []struct {
		name string
		typ  ChannelType
	}{
		{"temporary", ChannelTypeTemporary},
		{"semi-permanent", ChannelTypeSemiPermanent},
		{"permanent", ChannelTypePermanent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Cancel any cleanup timer the parent or prior iterations may have
			// left running for this channel by using a unique name.
			id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
				Name:       tc.name + "-" + fmt.Sprintf("%d", time.Now().UnixNano()),
				Topic:      "test topic",
				ParentID:   parentID,
				Type:       tc.typ,
				MaxClients: 10,
				CreatedBy:  userID,
			})
			if err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			if id == 0 {
				t.Fatal("expected non-zero channel id")
			}

			if !channelExistsInDB(t, s, id) {
				t.Fatalf("channel %d not in DB", id)
			}
			if channelTypeInDB(t, s, id) != int16(tc.typ) {
				t.Fatalf("channel type mismatch in DB")
			}
			ch, ok := sm.GetChannel(id)
			if !ok {
				t.Fatalf("channel %d not in state manager", id)
			}
			if ch.ChannelType != int(tc.typ) {
				t.Fatalf("state channel type = %d, want %d", ch.ChannelType, tc.typ)
			}

			// Clean up: delete the channel so it doesn't linger. For temporary
			// channels, cancel the timer first to avoid races with the cleanup
			// goroutine.
			mgr.CancelCleanup(id)
			if err := mgr.DeleteChannel(context.Background(), id); err != nil {
				t.Fatalf("DeleteChannel cleanup: %v", err)
			}
		})
	}
}

// TestCreateChannel_InvalidSpec verifies that an invalid spec is rejected.
func TestCreateChannel_InvalidSpec(t *testing.T) {
	mgr, _, _ := testEnv(t)

	cases := []struct {
		name string
		spec ChannelSpec
	}{
		{"empty name", ChannelSpec{Name: "", Type: ChannelTypePermanent}},
		{"invalid type", ChannelSpec{Name: "x", Type: ChannelType(99)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mgr.CreateChannel(context.Background(), tc.spec)
			if err == nil {
				t.Fatal("expected error for invalid spec, got nil")
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("expected ErrInvalidSpec, got %v", err)
			}
		})
	}
}

// TestCreateChannel_BadParent verifies that a non-existent parent is rejected.
func TestCreateChannel_BadParent(t *testing.T) {
	mgr, _, _ := testEnv(t)

	_, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:     "orphan",
		Type:     ChannelTypePermanent,
		ParentID: 99999999,
	})
	if err == nil {
		t.Fatal("expected error for non-existent parent, got nil")
	}
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("expected ErrChannelNotFound, got %v", err)
	}
}

// TestDeleteChannel verifies that DeleteChannel removes the channel from the DB
// and the state manager.
func TestDeleteChannel(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)

	id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "to-delete",
		Type:      ChannelTypePermanent,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if !channelExistsInDB(t, s, id) {
		t.Fatal("channel not created")
	}
	if _, ok := sm.GetChannel(id); !ok {
		t.Fatal("channel not in state")
	}

	if err := mgr.DeleteChannel(context.Background(), id); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if channelExistsInDB(t, s, id) {
		t.Fatal("channel still in DB after delete")
	}
	if _, ok := sm.GetChannel(id); ok {
		t.Fatal("channel still in state after delete")
	}

	// Deleting again returns ErrChannelNotFound.
	if err := mgr.DeleteChannel(context.Background(), id); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("expected ErrChannelNotFound on second delete, got %v", err)
	}
}

func TestDeleteChannel_RemovesEntireSubtreeFromStateAndTimers(t *testing.T) {
	mgr, s, sm := testEnv(t)
	mgr.SetCleanupDelay(time.Hour)
	userID := createTestUser(t, s)

	create := func(name string, parentID int64, channelType ChannelType) int64 {
		t.Helper()
		id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
			Name:      name,
			ParentID:  parentID,
			Type:      channelType,
			CreatedBy: userID,
		})
		if err != nil {
			t.Fatalf("CreateChannel(%q): %v", name, err)
		}
		return id
	}

	rootID := create("subtree-root", 0, ChannelTypePermanent)
	childID := create("subtree-child", rootID, ChannelTypePermanent)
	grandchildID := create("subtree-grandchild", childID, ChannelTypePermanent)
	temporaryID := create("subtree-temporary", childID, ChannelTypeTemporary)

	sm.AddClient(&state.Client{ClientID: "subtree-client-a"})
	sm.AddClient(&state.Client{ClientID: "subtree-client-b"})
	if _, err := mgr.MoveClient("subtree-client-a", childID); err != nil {
		t.Fatalf("move client a: %v", err)
	}
	if _, err := mgr.MoveClient("subtree-client-b", grandchildID); err != nil {
		t.Fatalf("move client b: %v", err)
	}
	sm.SetSpeaking("subtree-client-a", true)
	sm.Subscribe("subtree-client-a", []int64{temporaryID})
	if got := mgr.CleanupTimersCount(); got != 1 {
		t.Fatalf("cleanup timers before delete = %d, want 1", got)
	}

	if err := mgr.DeleteChannel(context.Background(), rootID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	for _, id := range []int64{rootID, childID, grandchildID, temporaryID} {
		if channelExistsInDB(t, s, id) {
			t.Errorf("channel %d remains in database", id)
		}
		if _, ok := sm.GetChannel(id); ok {
			t.Errorf("channel %d remains in state", id)
		}
	}
	for _, clientID := range []string{"subtree-client-a", "subtree-client-b"} {
		channelID, _, ok := sm.ClientChannelState(clientID)
		if !ok || channelID != 0 {
			t.Errorf("client %q state = (%d, %v), want channel 0 and present", clientID, channelID, ok)
		}
	}
	if sm.IsSpeaking("subtree-client-a") {
		t.Error("descendant speaking state remains after subtree delete")
	}
	if sm.IsSubscribed("subtree-client-a", temporaryID) {
		t.Error("descendant subscription remains after subtree delete")
	}
	if got := mgr.CleanupTimersCount(); got != 0 {
		t.Fatalf("cleanup timers after subtree delete = %d, want 0", got)
	}
}

func TestDeleteChannelSubtree_RepairsStateAfterExternalDatabaseDelete(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	rootID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name: "externally-deleted-root", Type: ChannelTypePermanent, CreatedBy: userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name: "externally-deleted-child", ParentID: rootID, Type: ChannelTypePermanent, CreatedBy: userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientID := "external-delete-client"
	sm.AddClient(&state.Client{ClientID: clientID})
	if _, err := mgr.MoveClient(clientID, childID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(context.Background(), `DELETE FROM channels WHERE id = $1`, rootID); err != nil {
		t.Fatal(err)
	}

	result, err := mgr.DeleteChannelSubtree(context.Background(), rootID)
	if err != nil {
		t.Fatalf("repairing stale subtree: %v", err)
	}
	if result.RootID != rootID || len(result.ChannelIDs) != 2 || len(result.Members) != 1 {
		t.Fatalf("repair result = %+v", result)
	}
	if result.Members[0].ClientID != clientID || result.Members[0].ChannelID != childID {
		t.Fatalf("displaced members = %+v", result.Members)
	}
	if _, ok := sm.GetChannel(rootID); ok {
		t.Fatal("stale root remains in state")
	}
	if _, ok := sm.GetChannel(childID); ok {
		t.Fatal("stale child remains in state")
	}
	channelID, _, ok := sm.ClientChannelState(clientID)
	if !ok || channelID != 0 {
		t.Fatalf("displaced client state = (%d, %v), want channel 0", channelID, ok)
	}
}

func TestDeleteChannelSubtree_ReconcilesAppliedExecError(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)
	execErr := errors.New("delete result lost")
	mgr.testHooks.deleteChannel = func(ctx context.Context, id int64) (sql.Result, error) {
		result, err := s.DB().ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, id)
		if err != nil {
			return result, err
		}
		return result, execErr
	}

	result, err := mgr.DeleteChannelSubtree(context.Background(), channelID)
	if err != nil {
		t.Fatalf("DeleteChannelSubtree after applied Exec error: %v", err)
	}
	if result.RootID != channelID || len(result.ChannelIDs) != 1 || result.ChannelIDs[0] != channelID {
		t.Fatalf("delete result = %+v", result)
	}
	if channelExistsInDB(t, s, channelID) {
		t.Fatal("ambiguously committed delete remains in database")
	}
	if _, ok := sm.GetChannel(channelID); ok {
		t.Fatal("ambiguously committed delete remains in state")
	}
}

// TestSetChannelType_Transitions verifies type transitions adjust the cleanup
// timer appropriately.
func TestSetChannelType_Transitions(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)

	// temporary -> permanent cancels the timer.
	tempID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "temp-to-perm",
		Type:      ChannelTypeTemporary,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// A freshly-created empty temporary channel has a cleanup timer running.
	pollCondition(t, time.Second, func() bool {
		return mgr.CleanupTimersCount() >= 1
	}, "expected at least one cleanup timer after creating empty temp channel")

	if err := mgr.SetChannelType(context.Background(), tempID, ChannelTypePermanent); err != nil {
		t.Fatalf("SetChannelType temp->perm: %v", err)
	}
	if channelTypeInDB(t, s, tempID) != int16(ChannelTypePermanent) {
		t.Fatalf("DB type not updated")
	}
	if ch, ok := sm.GetChannel(tempID); !ok || ch.ChannelType != int(ChannelTypePermanent) {
		t.Fatalf("state type not updated")
	}
	if mgr.CleanupTimersCount() != 0 {
		t.Fatalf("expected 0 cleanup timers after temp->perm, got %d", mgr.CleanupTimersCount())
	}
	if err := mgr.DeleteChannel(context.Background(), tempID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}

	// permanent -> temporary when empty starts the timer.
	permID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "perm-to-temp",
		Type:      ChannelTypePermanent,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if mgr.CleanupTimersCount() != 0 {
		t.Fatalf("expected 0 timers for permanent channel, got %d", mgr.CleanupTimersCount())
	}
	if err := mgr.SetChannelType(context.Background(), permID, ChannelTypeTemporary); err != nil {
		t.Fatalf("SetChannelType perm->temp: %v", err)
	}
	if mgr.CleanupTimersCount() != 1 {
		t.Fatalf("expected 1 timer after perm->temp (empty), got %d", mgr.CleanupTimersCount())
	}
	// Cancel the timer so it doesn't delete the channel mid-test.
	mgr.CancelCleanup(permID)
	if err := mgr.DeleteChannel(context.Background(), permID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
}

func TestSetChannelType_SerializesCommitStateAndTimer(t *testing.T) {
	mgr, s, sm := testEnv(t)
	mgr.SetCleanupDelay(time.Hour)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)

	firstCommitted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var first atomic.Bool
	mgr.testHooks.afterSetTypeCommit = func(id int64) {
		if id != channelID || !first.CompareAndSwap(false, true) {
			return
		}
		// Exercise post-commit reconciliation while the first operation still
		// owns the lifecycle lock.
		sm.RemoveChannel(channelID)
		close(firstCommitted)
		<-releaseFirst
	}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- mgr.SetChannelType(context.Background(), channelID, ChannelTypeTemporary)
	}()
	select {
	case <-firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("first type update did not reach its post-commit barrier")
	}

	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondResult <- mgr.SetChannelType(context.Background(), channelID, ChannelTypePermanent)
	}()
	<-secondStarted
	select {
	case err := <-secondResult:
		t.Fatalf("second type update crossed the first operation's state/timer boundary: %v", err)
	default:
	}

	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatalf("temporary update: %v", err)
	}
	if err := <-secondResult; err != nil {
		t.Fatalf("permanent update: %v", err)
	}

	if got := channelTypeInDB(t, s, channelID); got != int16(ChannelTypePermanent) {
		t.Fatalf("database type = %d, want permanent", got)
	}
	channel, ok := sm.GetChannel(channelID)
	if !ok || channel.ChannelType != int(ChannelTypePermanent) {
		t.Fatalf("state channel = %+v, present=%v; want permanent", channel, ok)
	}
	if got := mgr.CleanupTimersCount(); got != 0 {
		t.Fatalf("cleanup timers = %d, want 0 for final permanent type", got)
	}
}

func TestSetChannelType_ReconcilesAppliedCommitError(t *testing.T) {
	mgr, s, sm := testEnv(t)
	mgr.SetCleanupDelay(time.Hour)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)
	commitErr := errors.New("commit result lost")
	mgr.testHooks.commitSetType = func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return commitErr
	}

	if err := mgr.SetChannelType(context.Background(), channelID, ChannelTypeTemporary); err != nil {
		t.Fatalf("SetChannelType after applied commit error: %v", err)
	}
	if got := channelTypeInDB(t, s, channelID); got != int16(ChannelTypeTemporary) {
		t.Fatalf("database type = %d, want temporary", got)
	}
	channel, ok := sm.GetChannel(channelID)
	if !ok || channel.ChannelType != int(ChannelTypeTemporary) {
		t.Fatalf("state channel = %+v, present=%v", channel, ok)
	}
	if !hasCleanupTimer(mgr, channelID) {
		t.Fatal("confirmed temporary commit did not reconcile cleanup timer")
	}
}

func TestCleanupCallback_ClaimsExactTimerToken(t *testing.T) {
	mgr, s, sm := testEnv(t)
	mgr.SetCleanupDelay(time.Hour)
	userID := createTestUser(t, s)
	channelID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "timer-token",
		Type:      ChannelTypeTemporary,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	oldToken := activeCleanupToken(t, mgr, channelID)
	mgr.StartCleanupWatcher(channelID)
	replacement := activeCleanupToken(t, mgr, channelID)
	if replacement == oldToken || replacement.generation <= oldToken.generation {
		t.Fatalf("replacement token = %+v, old = %+v", replacement, oldToken)
	}

	// Model a stopped timer whose callback had already begun running.
	mgr.cleanupCallback(channelID, oldToken)
	if got := activeCleanupToken(t, mgr, channelID); got != replacement {
		t.Fatalf("stale callback claimed replacement token: got %p, want %p", got, replacement)
	}
	if !channelExistsInDB(t, s, channelID) {
		t.Fatal("stale callback deleted the channel")
	}
	if _, ok := sm.GetChannel(channelID); !ok {
		t.Fatal("stale callback removed the state channel")
	}
}

func TestMoveClient_LinearizableWithTemporaryCleanup(t *testing.T) {
	t.Run("cleanup wins", func(t *testing.T) {
		mgr, s, sm := testEnv(t)
		mgr.SetCleanupDelay(time.Hour)
		userID := createTestUser(t, s)
		channelID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
			Name:      "cleanup-wins",
			Type:      ChannelTypeTemporary,
			CreatedBy: userID,
		})
		if err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		clientID := "cleanup-wins-client"
		sm.AddClient(&state.Client{ClientID: clientID})
		token := activeCleanupToken(t, mgr, channelID)

		deleteReady := make(chan struct{})
		releaseDelete := make(chan struct{})
		mgr.testHooks.beforeCleanupDelete = func(id int64) {
			if id == channelID {
				close(deleteReady)
				<-releaseDelete
			}
		}
		cleanupDone := make(chan struct{})
		go func() {
			mgr.cleanupCallback(channelID, token)
			close(cleanupDone)
		}()
		select {
		case <-deleteReady:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup did not reach its pre-delete barrier")
		}

		moveStarted := make(chan struct{})
		moveResult := make(chan error, 1)
		go func() {
			close(moveStarted)
			_, err := mgr.MoveClient(clientID, channelID)
			moveResult <- err
		}()
		<-moveStarted
		select {
		case err := <-moveResult:
			t.Fatalf("move completed while cleanup owned the channel lock: %v", err)
		default:
		}

		close(releaseDelete)
		<-cleanupDone
		if err := <-moveResult; !errors.Is(err, state.ErrChannelNotFound) {
			t.Fatalf("move after cleanup error = %v, want state.ErrChannelNotFound", err)
		}
		if channelExistsInDB(t, s, channelID) {
			t.Fatal("cleanup-winning channel remains in database")
		}
		if _, ok := sm.GetChannel(channelID); ok {
			t.Fatal("cleanup-winning channel remains in state")
		}
	})

	t.Run("join wins", func(t *testing.T) {
		mgr, s, sm := testEnv(t)
		mgr.SetCleanupDelay(time.Hour)
		userID := createTestUser(t, s)
		channelID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
			Name:      "join-wins",
			Type:      ChannelTypeTemporary,
			CreatedBy: userID,
		})
		if err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		clientID := "join-wins-client"
		sm.AddClient(&state.Client{ClientID: clientID})
		token := activeCleanupToken(t, mgr, channelID)

		if oldChannelID, err := mgr.MoveClient(clientID, channelID); err != nil || oldChannelID != 0 {
			t.Fatalf("MoveClient = (%d, %v), want (0, nil)", oldChannelID, err)
		}
		mgr.cleanupCallback(channelID, token)

		if !channelExistsInDB(t, s, channelID) {
			t.Fatal("join-winning channel was deleted")
		}
		if _, ok := sm.GetChannel(channelID); !ok {
			t.Fatal("join-winning channel missing from state")
		}
		members := sm.ChannelMembers(channelID)
		if len(members) != 1 || members[0].ClientID != clientID {
			t.Fatalf("members after winning join = %+v", members)
		}
		if got := mgr.CleanupTimersCount(); got != 0 {
			t.Fatalf("cleanup timers after winning join = %d, want 0", got)
		}
	})
}

func TestClientLeaveAndRemovalSerializeWithMove(t *testing.T) {
	setup := func(t *testing.T) (*ChannelManager, *state.Manager, int64, int64, string) {
		t.Helper()
		mgr, store, sm := testEnv(t)
		mgr.SetCleanupDelay(time.Hour)
		userID := createTestUser(t, store)
		sourceID := createParentChannel(t, mgr, sm, userID)
		targetID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
			Name:      "serialized-move-target",
			Type:      ChannelTypeTemporary,
			CreatedBy: userID,
		})
		if err != nil {
			t.Fatal(err)
		}
		clientID := "serialized-client"
		sm.AddClient(&state.Client{ClientID: clientID})
		if _, err := mgr.MoveClient(clientID, sourceID); err != nil {
			t.Fatal(err)
		}
		return mgr, sm, sourceID, targetID, clientID
	}

	t.Run("leave then move", func(t *testing.T) {
		mgr, sm, sourceID, targetID, clientID := setup(t)
		leaveReady := make(chan struct{})
		releaseLeave := make(chan struct{})
		mgr.testHooks.beforeLeaveClient = func(string) {
			close(leaveReady)
			<-releaseLeave
		}
		leaveResult := make(chan error, 1)
		go func() {
			oldChannelID, err := mgr.LeaveClient(clientID)
			if err == nil && oldChannelID != sourceID {
				err = fmt.Errorf("old channel = %d, want %d", oldChannelID, sourceID)
			}
			leaveResult <- err
		}()
		<-leaveReady
		moveResult := make(chan error, 1)
		go func() {
			_, err := mgr.MoveClient(clientID, targetID)
			moveResult <- err
		}()
		select {
		case err := <-moveResult:
			t.Fatalf("move crossed leave lifecycle lock: %v", err)
		default:
		}
		close(releaseLeave)
		if err := <-leaveResult; err != nil {
			t.Fatal(err)
		}
		if err := <-moveResult; err != nil {
			t.Fatal(err)
		}
		channelID, _, ok := sm.ClientChannelState(clientID)
		if !ok || channelID != targetID {
			t.Fatalf("final client state = (%d, %v), want target %d", channelID, ok, targetID)
		}
		if got := mgr.CleanupTimersCount(); got != 0 {
			t.Fatalf("target cleanup timer survived successful move: %d", got)
		}
	})

	t.Run("remove then move", func(t *testing.T) {
		mgr, _, sourceID, targetID, clientID := setup(t)
		removeReady := make(chan struct{})
		releaseRemove := make(chan struct{})
		mgr.testHooks.beforeRemoveClient = func(string) {
			close(removeReady)
			<-releaseRemove
		}
		removeResult := make(chan error, 1)
		go func() {
			removed, err := mgr.RemoveClient(clientID)
			if err == nil && (removed == nil || removed.ChannelID != sourceID) {
				err = fmt.Errorf("removed client = %+v, want source %d", removed, sourceID)
			}
			removeResult <- err
		}()
		<-removeReady
		moveResult := make(chan error, 1)
		go func() {
			_, err := mgr.MoveClient(clientID, targetID)
			moveResult <- err
		}()
		select {
		case err := <-moveResult:
			t.Fatalf("move crossed remove lifecycle lock: %v", err)
		default:
		}
		close(releaseRemove)
		if err := <-removeResult; err != nil {
			t.Fatal(err)
		}
		if err := <-moveResult; !errors.Is(err, state.ErrClientNotFound) {
			t.Fatalf("move after removal error = %v, want client not found", err)
		}
		if got := mgr.CleanupTimersCount(); got != 1 {
			t.Fatalf("empty target cleanup timer count = %d, want 1", got)
		}
	})
}

// TestCleanupTimer_DeletesEmptyTemporary verifies that the cleanup goroutine
// deletes an empty temporary channel after the cleanup delay.
func TestCleanupTimer_DeletesEmptyTemporary(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	deleted := make(chan DeleteResult, 1)
	mgr.SetCleanupDeleteHandler(func(result DeleteResult) { deleted <- result })

	id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "auto-cleanup",
		Type:      ChannelTypeTemporary,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Simulate a client joining then leaving.
	clientID := "test-client-cleanup"
	sm.AddClient(&state.Client{ClientID: clientID})
	if _, err := mgr.MoveClient(clientID, id); err != nil {
		t.Fatalf("MoveClient: %v", err)
	}
	if mgr.CleanupTimersCount() != 0 {
		t.Fatalf("expected 0 timers after join, got %d", mgr.CleanupTimersCount())
	}

	if oldChannelID, err := mgr.LeaveClient(clientID); err != nil || oldChannelID != id {
		t.Fatalf("LeaveClient = (%d, %v), want (%d, nil)", oldChannelID, err, id)
	}
	if mgr.CleanupTimersCount() != 1 {
		t.Fatalf("expected 1 timer after leave, got %d", mgr.CleanupTimersCount())
	}

	// Wait for the cleanup goroutine to fire and delete the channel.
	pollCondition(t, 2*time.Second, func() bool {
		return !channelExistsInDB(t, s, id)
	}, "channel still in DB after cleanup delay")
	if _, ok := sm.GetChannel(id); ok {
		t.Fatal("channel still in state after cleanup")
	}
	if mgr.CleanupTimersCount() != 0 {
		t.Fatalf("expected 0 timers after cleanup, got %d", mgr.CleanupTimersCount())
	}
	select {
	case result := <-deleted:
		if result.RootID != id || len(result.ChannelIDs) != 1 || result.ChannelIDs[0] != id {
			t.Fatalf("cleanup deletion result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup deletion was not published to the side-effect sink")
	}
}

func TestTemporaryParentSurvivesUntilItsLastChildIsDeleted(t *testing.T) {
	mgr, s, _ := testEnv(t)
	mgr.SetCleanupDelay(40 * time.Millisecond)
	userID := createTestUser(t, s)
	parentID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "temporary-parent",
		Type:      ChannelTypeTemporary,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "permanent-child",
		ParentID:  parentID,
		Type:      ChannelTypePermanent,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.CleanupTimersCount(); got != 0 {
		t.Fatalf("cleanup timers with child present = %d, want 0", got)
	}
	time.Sleep(120 * time.Millisecond)
	if !channelExistsInDB(t, s, parentID) || !channelExistsInDB(t, s, childID) {
		t.Fatal("temporary parent cleanup cascaded through an existing child")
	}

	if err := mgr.DeleteChannel(context.Background(), childID); err != nil {
		t.Fatal(err)
	}
	if got := mgr.CleanupTimersCount(); got != 1 {
		t.Fatalf("cleanup timers after last child deletion = %d, want 1", got)
	}
	pollCondition(t, time.Second, func() bool {
		return !channelExistsInDB(t, s, parentID)
	}, "temporary parent was not cleaned after becoming an empty leaf")
}

// TestMoveClient_CancelsCleanup verifies that joining through the lifecycle
// manager cancels pending cleanup so the channel survives past the delay.
func TestMoveClient_CancelsCleanup(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)

	id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "join-cancels",
		Type:      ChannelTypeTemporary,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Leave -> timer starts (channel is empty already, but simulate the leave
	// path by adding/leaving a client).
	clientID := "test-client-join-cancel"
	sm.AddClient(&state.Client{ClientID: clientID})
	if _, err := mgr.MoveClient(clientID, id); err != nil {
		t.Fatalf("MoveClient: %v", err)
	}
	if _, err := mgr.LeaveClient(clientID); err != nil {
		t.Fatalf("LeaveClient: %v", err)
	}
	if mgr.CleanupTimersCount() != 1 {
		t.Fatalf("expected 1 timer after leave, got %d", mgr.CleanupTimersCount())
	}

	// Re-join before the delay elapses -> timer cancels.
	if _, err := mgr.MoveClient(clientID, id); err != nil {
		t.Fatalf("MoveClient rejoin: %v", err)
	}
	if mgr.CleanupTimersCount() != 0 {
		t.Fatalf("expected 0 timers after rejoin, got %d", mgr.CleanupTimersCount())
	}

	// Wait well past the cleanup delay; the channel must still exist.
	time.Sleep(testCleanupDelay * 3)
	if !channelExistsInDB(t, s, id) {
		t.Fatal("channel was deleted despite a client being present")
	}
	if _, ok := sm.GetChannel(id); !ok {
		t.Fatal("channel missing from state despite a client being present")
	}

	// Clean up: leave and let the timer fire, then ensure deletion.
	if _, err := mgr.LeaveClient(clientID); err != nil {
		t.Fatalf("LeaveClient final: %v", err)
	}
	pollCondition(t, 2*time.Second, func() bool {
		return !channelExistsInDB(t, s, id)
	}, "channel not deleted after final leave")
}

// TestClose_CancelsAllTimers verifies that Close cancels all pending timers.
func TestClose_CancelsAllTimers(t *testing.T) {
	mgr, s, sm := testEnv(t)
	const closeCleanupDelay = 2 * time.Second
	mgr.SetCleanupDelay(closeCleanupDelay)
	userID := createTestUser(t, s)

	// Create several temporary channels (each starts a cleanup timer).
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
			Name:      fmt.Sprintf("close-%d-%d", i, time.Now().UnixNano()),
			Type:      ChannelTypeTemporary,
			CreatedBy: userID,
		})
		if err != nil {
			t.Fatalf("CreateChannel %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	pollCondition(t, time.Second, func() bool {
		return mgr.CleanupTimersCount() == 3
	}, "expected 3 cleanup timers")

	// Close should cancel all timers.
	mgr.Close()
	if mgr.CleanupTimersCount() != 0 {
		t.Fatalf("expected 0 timers after Close, got %d", mgr.CleanupTimersCount())
	}

	// Wait past the cleanup delay; channels must still exist because Close
	// cancelled the timers.
	time.Sleep(closeCleanupDelay + 100*time.Millisecond)
	for _, id := range ids {
		if !channelExistsInDB(t, s, id) {
			t.Fatalf("channel %d was deleted despite Close", id)
		}
		// Clean up the DB rows directly.
		if _, err := s.DB().ExecContext(context.Background(),
			`DELETE FROM channels WHERE id = $1`, id); err != nil {
			t.Logf("cleanup delete %d: %v", id, err)
		}
	}
	_ = sm
}

// TestChannelTypeString verifies the String method.
func TestChannelTypeString(t *testing.T) {
	cases := []struct {
		t    ChannelType
		want string
	}{
		{ChannelTypeTemporary, "temporary"},
		{ChannelTypeSemiPermanent, "semi-permanent"},
		{ChannelTypePermanent, "permanent"},
		{ChannelType(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("ChannelType(%d).String() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// TestCreateChannel_WithPassword verifies that a channel password is hashed
// and stored.
func TestCreateChannel_WithPassword(t *testing.T) {
	mgr, s, _ := testEnv(t)
	userID := createTestUser(t, s)

	id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      "pw-chan",
		Type:      ChannelTypePermanent,
		Password:  "secret",
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	var hash sql.NullString
	err = s.DB().QueryRowContext(context.Background(),
		`SELECT password_hash FROM channels WHERE id = $1`, id,
	).Scan(&hash)
	if err != nil {
		t.Fatalf("query password_hash: %v", err)
	}
	if !hash.Valid || hash.String == "" {
		t.Fatal("expected non-empty password_hash")
	}

	// Clean up.
	if err := mgr.DeleteChannel(context.Background(), id); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
}

// insertChannelRow inserts a channel row directly into the DB (bypassing
// CreateChannel, so the state manager stays empty) and returns its id.
func insertChannelRow(t *testing.T, s *store.Store, name string, parentID int64, channelType ChannelType, maxClients int) int64 {
	t.Helper()
	var parent any
	if parentID != 0 {
		parent = parentID
	}
	var maxc any
	if maxClients > 0 {
		maxc = maxClients
	}
	var id int64
	const q = `INSERT INTO channels (parent_id, name, topic, order_index, channel_type, max_clients)
	          VALUES ($1, $2, $3, 0, $4, $5) RETURNING id`
	err := s.DB().QueryRowContext(context.Background(), q,
		parent, name, name+" topic", int16(channelType), maxc,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertChannelRow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(context.Background(), `DELETE FROM channels WHERE id = $1`, id)
	})
	return id
}

// TestLoadIntoState verifies that persisted channels are loaded into the
// state manager with their type, parent, topic, and max_clients, and that an
// empty temporary channel gets a cleanup watcher (which eventually deletes
// it, matching the temp-cleanup semantics).
func TestLoadIntoState(t *testing.T) {
	mgr, s, sm := testEnv(t)

	if _, err := s.DB().Exec("DELETE FROM channels"); err != nil {
		t.Fatalf("failed to clean channels table: %v", err)
	}

	rootID := insertChannelRow(t, s, "LoadRoot", 0, ChannelTypePermanent, 10)
	childID := insertChannelRow(t, s, "LoadChild", rootID, ChannelTypeSemiPermanent, 0)
	tempID := insertChannelRow(t, s, "LoadTemp", 0, ChannelTypeTemporary, 0)

	count, err := mgr.LoadIntoState(context.Background())
	if err != nil {
		t.Fatalf("LoadIntoState: %v", err)
	}
	if count != 3 {
		t.Fatalf("LoadIntoState count = %d, want 3", count)
	}

	root, ok := sm.GetChannel(rootID)
	if !ok {
		t.Fatalf("root channel %d not in state", rootID)
	}
	if root.Name != "LoadRoot" || root.ChannelType != int(ChannelTypePermanent) ||
		root.MaxClients != 10 || root.Topic != "LoadRoot topic" {
		t.Fatalf("root channel = %+v", root)
	}

	child, ok := sm.GetChannel(childID)
	if !ok {
		t.Fatalf("child channel %d not in state", childID)
	}
	if child.ParentID != rootID || child.ChannelType != int(ChannelTypeSemiPermanent) {
		t.Fatalf("child channel = %+v, want parent %d semi-permanent", child, rootID)
	}

	if _, ok := sm.GetChannel(tempID); !ok {
		t.Fatalf("temp channel %d not in state", tempID)
	}

	// Only the (empty) temporary channel gets a cleanup watcher.
	if got := mgr.CleanupTimersCount(); got != 1 {
		t.Fatalf("cleanup timers = %d, want 1", got)
	}

	// The watcher fires after the test cleanup delay and deletes the empty
	// temporary channel from both state and the DB.
	pollCondition(t, 5*time.Second, func() bool {
		_, ok := sm.GetChannel(tempID)
		return !ok && !channelExistsInDB(t, s, tempID)
	}, "temporary channel was not cleaned up after LoadIntoState")
}

// TestUpdateChannel_Persistence verifies UpdateChannel writes non-nil fields
// to the DB and mirrors them into state, leaving nil fields untouched, and
// rejects invalid values.
func TestUpdateChannel_Persistence(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)

	topic := "music room"
	bitrate := 128000
	stereo := true
	if err := mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{
		Topic:       &topic,
		OpusBitrate: &bitrate,
		OpusStereo:  &stereo,
	}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}

	// State reflects the edit.
	ch, ok := sm.GetChannel(channelID)
	if !ok {
		t.Fatalf("channel %d not in state", channelID)
	}
	if ch.Topic != "music room" || ch.OpusBitrate != 128000 || !ch.OpusStereo || ch.OpusFEC || ch.OpusDTX {
		t.Fatalf("state channel = %+v, want edited values", ch)
	}

	// The DB row reflects the edit.
	var (
		dbTopic   string
		dbBitrate int
		dbFEC     bool
		dbStereo  bool
	)
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT COALESCE(topic, ''), opus_bitrate, opus_fec, opus_stereo FROM channels WHERE id = $1`,
		channelID,
	).Scan(&dbTopic, &dbBitrate, &dbFEC, &dbStereo)
	if err != nil {
		t.Fatalf("query channel row: %v", err)
	}
	if dbTopic != "music room" || dbBitrate != 128000 || dbStereo != true || dbFEC != false {
		t.Fatalf("db row = (%q, %d, %v, %v), want (music room, 128000, false, true)",
			dbTopic, dbBitrate, dbFEC, dbStereo)
	}

	// max_clients round-trips through NULL for unlimited.
	zero := 0
	if err := mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{MaxClients: &zero}); err != nil {
		t.Fatalf("UpdateChannel max_clients=0: %v", err)
	}
	var maxClients *int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT max_clients FROM channels WHERE id = $1`, channelID,
	).Scan(&maxClients); err != nil {
		t.Fatalf("query max_clients: %v", err)
	}
	if maxClients != nil {
		t.Fatalf("max_clients = %v, want NULL (unlimited)", *maxClients)
	}

	// Invalid values are rejected; unknown channels report not-found.
	bad := -1
	if err := mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{OpusBitrate: &bad}); err == nil {
		t.Fatal("negative bitrate accepted")
	}
	if err := mgr.UpdateChannel(context.Background(), 999999, ChannelUpdate{Topic: &topic}); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("unknown channel: %v, want ErrChannelNotFound", err)
	}
}

func TestUpdateChannel_SerializesCommitAndStateMirror(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)

	firstCommitted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var first atomic.Bool
	mgr.testHooks.afterUpdateCommit = func(id int64) {
		if id == channelID && first.CompareAndSwap(false, true) {
			close(firstCommitted)
			<-releaseFirst
		}
	}

	firstTopic := "first committed topic"
	secondTopic := "second committed topic"
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{Topic: &firstTopic})
	}()
	select {
	case <-firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("first update did not reach its post-commit barrier")
	}

	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondResult <- mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{Topic: &secondTopic})
	}()
	<-secondStarted
	select {
	case err := <-secondResult:
		t.Fatalf("second update crossed the first operation's state boundary: %v", err)
	default:
	}

	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatalf("first UpdateChannel: %v", err)
	}
	if err := <-secondResult; err != nil {
		t.Fatalf("second UpdateChannel: %v", err)
	}

	var dbTopic string
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT COALESCE(topic, '') FROM channels WHERE id = $1`, channelID,
	).Scan(&dbTopic); err != nil {
		t.Fatalf("query final topic: %v", err)
	}
	if dbTopic != secondTopic {
		t.Fatalf("database topic = %q, want %q", dbTopic, secondTopic)
	}
	channel, ok := sm.GetChannel(channelID)
	if !ok || channel.Topic != secondTopic {
		t.Fatalf("state channel = %+v, present=%v; want topic %q", channel, ok, secondTopic)
	}
}

func TestUpdateChannel_ConcurrentReparentsReconcileActualParents(t *testing.T) {
	mgr, s, sm := testEnv(t)
	mgr.SetCleanupDelay(time.Hour)
	userID := createTestUser(t, s)
	create := func(name string, parentID int64, channelType ChannelType) int64 {
		t.Helper()
		id, err := mgr.CreateChannel(context.Background(), ChannelSpec{
			Name: name, ParentID: parentID, Type: channelType, CreatedBy: userID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	parentA := create("reparent-a", 0, ChannelTypeTemporary)
	parentB := create("reparent-b", 0, ChannelTypeTemporary)
	parentC := create("reparent-c", 0, ChannelTypeTemporary)
	childID := create("reparent-child", parentA, ChannelTypePermanent)

	firstCommitted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var first atomic.Bool
	mgr.testHooks.afterUpdateCommit = func(id int64) {
		if id == childID && first.CompareAndSwap(false, true) {
			close(firstCommitted)
			<-releaseFirst
		}
	}
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- mgr.UpdateChannel(context.Background(), childID, ChannelUpdate{ParentID: &parentB})
	}()
	<-firstCommitted
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- mgr.UpdateChannel(context.Background(), childID, ChannelUpdate{ParentID: &parentC})
	}()
	select {
	case err := <-secondResult:
		t.Fatalf("second reparent crossed first tree mutation: %v", err)
	default:
	}
	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}

	channel, ok := sm.GetChannel(childID)
	if !ok || channel.ParentID != parentC {
		t.Fatalf("final state channel = %+v, present=%v; want parent %d", channel, ok, parentC)
	}
	var databaseParent int64
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT parent_id FROM channels WHERE id = $1`, childID,
	).Scan(&databaseParent); err != nil {
		t.Fatal(err)
	}
	if databaseParent != parentC {
		t.Fatalf("database parent = %d, want %d", databaseParent, parentC)
	}
	for _, parentID := range []int64{parentA, parentB} {
		if !hasCleanupTimer(mgr, parentID) {
			t.Errorf("empty former parent %d has no cleanup timer", parentID)
		}
	}
	if hasCleanupTimer(mgr, parentC) {
		t.Errorf("occupied final parent %d retained cleanup timer", parentC)
	}
	if got := mgr.CleanupTimersCount(); got != 2 {
		t.Fatalf("cleanup timer count = %d, want 2 former parents", got)
	}
}

func TestUpdateChannel_ReconcilesAppliedCommitError(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)
	topic := "committed despite lost result"
	commitErr := errors.New("update commit result lost")
	mgr.testHooks.commitUpdate = func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return commitErr
	}

	if err := mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{Topic: &topic}); err != nil {
		t.Fatalf("UpdateChannel after applied commit error: %v", err)
	}
	var databaseTopic string
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT topic FROM channels WHERE id = $1`, channelID,
	).Scan(&databaseTopic); err != nil {
		t.Fatal(err)
	}
	channel, ok := sm.GetChannel(channelID)
	if databaseTopic != topic || !ok || channel.Topic != topic {
		t.Fatalf("database topic=%q state=%+v present=%v, want %q", databaseTopic, channel, ok, topic)
	}
}

func TestUpdateChannel_ReconcilesMissingStateAfterCommit(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)
	description := "durable description"

	var removed atomic.Bool
	mgr.testHooks.afterUpdateCommit = func(id int64) {
		if id == channelID && removed.CompareAndSwap(false, true) {
			sm.RemoveChannel(channelID)
		}
	}
	if err := mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{Description: &description}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	channel, ok := sm.GetChannel(channelID)
	if !ok {
		t.Fatal("committed channel was not reloaded into state")
	}
	if channel.Description != description || channel.Name == "" || channel.ChannelType != int(ChannelTypePermanent) {
		t.Fatalf("reconciled state channel = %+v", channel)
	}
}

// TestUpdateChannel_Description verifies the description column (migration
// 008) persists and loads back into state (112/113).
func TestUpdateChannel_Description(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)

	desc := "the **quiet** room — rules inside"
	if err := mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{Description: &desc}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if ch, _ := sm.GetChannel(channelID); ch.Description != desc {
		t.Fatalf("state description = %q, want %q", ch.Description, desc)
	}
	var dbDesc string
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT description FROM channels WHERE id = $1`, channelID,
	).Scan(&dbDesc); err != nil {
		t.Fatalf("query description: %v", err)
	}
	if dbDesc != desc {
		t.Fatalf("db description = %q, want %q", dbDesc, desc)
	}

	// And it survives a fresh LoadIntoState.
	if _, err := mgr.LoadIntoState(context.Background()); err != nil {
		t.Fatalf("LoadIntoState: %v", err)
	}
	if ch, _ := sm.GetChannel(channelID); ch.Description != desc {
		t.Fatalf("description after reload = %q, want %q", ch.Description, desc)
	}
}

func TestMoveClientWithLifecycleOrdersConsequencesBeforeDeletion(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)
	channelID := createParentChannel(t, mgr, sm, userID)
	const clientID = "lifecycle-move-client"
	sm.AddClient(&state.Client{ClientID: clientID})

	callbackEntered := make(chan struct{})
	allowCallback := make(chan struct{})
	moveDone := make(chan error, 1)
	var consequenceErr error
	go func() {
		_, err := mgr.MoveClientWithLifecycle(clientID, channelID, func(oldChannelID int64) {
			if oldChannelID != 0 {
				consequenceErr = fmt.Errorf("old channel = %d, want 0", oldChannelID)
			}
			current, _, ok := sm.ClientChannelState(clientID)
			if !ok || current != channelID {
				consequenceErr = fmt.Errorf("callback state = (%d, %v), want (%d, true)", current, ok, channelID)
			}
			close(callbackEntered)
			<-allowCallback
		})
		moveDone <- err
	}()
	<-callbackEntered

	deleteStarted := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		close(deleteStarted)
		deleteDone <- mgr.DeleteChannel(context.Background(), channelID)
	}()
	<-deleteStarted
	select {
	case err := <-deleteDone:
		t.Fatalf("deletion crossed lifecycle callback: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowCallback)
	if err := <-moveDone; err != nil {
		t.Fatalf("MoveClientWithLifecycle: %v", err)
	}
	if consequenceErr != nil {
		t.Fatal(consequenceErr)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	current, _, ok := sm.ClientChannelState(clientID)
	if !ok || current != 0 {
		t.Fatalf("client state after deletion = (%d, %v), want (0, true)", current, ok)
	}
}

func TestDeleteChannelSubtreeUsesAuthoritativeDatabaseParent(t *testing.T) {
	mgr, s, sm := testEnv(t)
	mgr.SetCleanupDelay(time.Hour)
	userID := createTestUser(t, s)
	parentID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      fmt.Sprintf("authoritative-parent-%d", time.Now().UnixNano()),
		Type:      ChannelTypeTemporary,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("create temporary parent: %v", err)
	}
	childID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      fmt.Sprintf("state-missing-child-%d", time.Now().UnixNano()),
		ParentID:  parentID,
		Type:      ChannelTypePermanent,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if hasCleanupTimer(mgr, parentID) {
		t.Fatal("temporary parent retained a cleanup timer while it had a child")
	}

	// Simulate state loss after the child was committed. The database remains
	// authoritative for both the deletion target and its parent edge.
	sm.RemoveChannel(childID)
	result, err := mgr.DeleteChannelSubtree(context.Background(), childID)
	if err != nil {
		t.Fatalf("DeleteChannelSubtree: %v", err)
	}
	if len(result.ChannelIDs) != 1 || result.ChannelIDs[0] != childID {
		t.Fatalf("deleted IDs = %v, want [%d]", result.ChannelIDs, childID)
	}
	if !hasCleanupTimer(mgr, parentID) {
		t.Fatal("database parent was not reconciled after deleting its final child")
	}
}

func TestDeleteChannelSubtreeReloadsLiveStateOnlyDescendant(t *testing.T) {
	mgr, s, sm := testEnv(t)
	mgr.SetCleanupDelay(time.Hour)
	userID := createTestUser(t, s)
	deletedParentID := createParentChannel(t, mgr, sm, userID)
	liveParentID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      fmt.Sprintf("live-parent-%d", time.Now().UnixNano()),
		Type:      ChannelTypeTemporary,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("create live parent: %v", err)
	}
	childID, err := mgr.CreateChannel(context.Background(), ChannelSpec{
		Name:      fmt.Sprintf("ambiguously-moved-child-%d", time.Now().UnixNano()),
		ParentID:  deletedParentID,
		Type:      ChannelTypePermanent,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if !hasCleanupTimer(mgr, liveParentID) {
		t.Fatal("temporary destination parent should begin with a cleanup timer")
	}

	// Model a reparent whose commit succeeded but whose state reconciliation
	// failed: the row is live beneath liveParentID while state still points at
	// the channel that is about to be deleted.
	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE channels SET parent_id = $1 WHERE id = $2`, liveParentID, childID,
	); err != nil {
		t.Fatalf("move child directly in database: %v", err)
	}

	result, err := mgr.DeleteChannelSubtree(context.Background(), deletedParentID)
	if err != nil {
		t.Fatalf("DeleteChannelSubtree: %v", err)
	}
	if len(result.ChannelIDs) != 1 || result.ChannelIDs[0] != deletedParentID {
		t.Fatalf("deleted IDs = %v, want only [%d]", result.ChannelIDs, deletedParentID)
	}
	if !channelExistsInDB(t, s, childID) {
		t.Fatal("live reparented child was deleted from the database")
	}
	child, ok := sm.GetChannel(childID)
	if !ok || child.ParentID != liveParentID {
		t.Fatalf("reloaded child = %+v, found=%v, want parent %d", child, ok, liveParentID)
	}
	if hasCleanupTimer(mgr, liveParentID) {
		t.Fatal("live temporary parent retained a cleanup timer after its child was reloaded")
	}
}

func TestChannelUpdatesPropagateRowsAffectedErrors(t *testing.T) {
	t.Run("set type", func(t *testing.T) {
		mgr, s, sm := testEnv(t)
		userID := createTestUser(t, s)
		channelID := createParentChannel(t, mgr, sm, userID)
		injected := errors.New("injected rows affected failure")
		mgr.testHooks.rowsAffected = func(sql.Result) (int64, error) {
			return 0, injected
		}

		err := mgr.SetChannelType(context.Background(), channelID, ChannelTypeTemporary)
		if !errors.Is(err, injected) {
			t.Fatalf("SetChannelType error = %v, want injected row-count error", err)
		}
		if got := channelTypeInDB(t, s, channelID); got != int16(ChannelTypePermanent) {
			t.Fatalf("database type = %d, want rolled-back permanent type", got)
		}
		channel, ok := sm.GetChannel(channelID)
		if !ok || channel.ChannelType != int(ChannelTypePermanent) {
			t.Fatalf("state channel = %+v, found=%v, want permanent", channel, ok)
		}
	})

	t.Run("general update", func(t *testing.T) {
		mgr, s, sm := testEnv(t)
		userID := createTestUser(t, s)
		channelID := createParentChannel(t, mgr, sm, userID)
		injected := errors.New("injected rows affected failure")
		mgr.testHooks.rowsAffected = func(sql.Result) (int64, error) {
			return 0, injected
		}
		updatedTopic := "must roll back"

		err := mgr.UpdateChannel(context.Background(), channelID, ChannelUpdate{Topic: &updatedTopic})
		if !errors.Is(err, injected) {
			t.Fatalf("UpdateChannel error = %v, want injected row-count error", err)
		}
		var databaseTopic string
		if err := s.DB().QueryRowContext(context.Background(),
			`SELECT COALESCE(topic, '') FROM channels WHERE id = $1`, channelID,
		).Scan(&databaseTopic); err != nil {
			t.Fatalf("query topic: %v", err)
		}
		if databaseTopic != "" {
			t.Fatalf("database topic = %q, want rollback", databaseTopic)
		}
		channel, ok := sm.GetChannel(channelID)
		if !ok || channel.Topic != "" {
			t.Fatalf("state channel = %+v, found=%v, want original topic", channel, ok)
		}
	})
}

func TestDeleteChannelSubtreeMissingPreimageSkipsDelete(t *testing.T) {
	mgr, _, _ := testEnv(t)
	deleteCalled := false
	mgr.testHooks.deleteChannel = func(context.Context, int64) (sql.Result, error) {
		deleteCalled = true
		return nil, errors.New("delete hook must not run")
	}

	_, err := mgr.DeleteChannelSubtree(context.Background(), 9_000_000_000)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("missing delete error = %v, want ErrChannelNotFound", err)
	}
	if deleteCalled {
		t.Fatal("database delete ran without a database or state preimage")
	}
}
