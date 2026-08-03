package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
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

// TestCleanupTimer_DeletesEmptyTemporary verifies that the cleanup goroutine
// deletes an empty temporary channel after the cleanup delay.
func TestCleanupTimer_DeletesEmptyTemporary(t *testing.T) {
	mgr, s, sm := testEnv(t)
	userID := createTestUser(t, s)

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
	sm.AddClient(&state.Client{ClientID: clientID, ChannelID: id})
	if err := sm.JoinChannel(clientID, id); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	mgr.OnClientJoinedChannel(id)
	if mgr.CleanupTimersCount() != 0 {
		t.Fatalf("expected 0 timers after join, got %d", mgr.CleanupTimersCount())
	}

	if err := sm.LeaveChannel(clientID); err != nil {
		t.Fatalf("LeaveChannel: %v", err)
	}
	mgr.OnClientLeftChannel(id)
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
}

// TestOnClientJoinedChannel_CancelsCleanup verifies that joining a channel
// cancels a pending cleanup so the channel survives past the delay.
func TestOnClientJoinedChannel_CancelsCleanup(t *testing.T) {
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
	sm.AddClient(&state.Client{ClientID: clientID, ChannelID: id})
	if err := sm.JoinChannel(clientID, id); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	mgr.OnClientJoinedChannel(id)
	if err := sm.LeaveChannel(clientID); err != nil {
		t.Fatalf("LeaveChannel: %v", err)
	}
	mgr.OnClientLeftChannel(id)
	if mgr.CleanupTimersCount() != 1 {
		t.Fatalf("expected 1 timer after leave, got %d", mgr.CleanupTimersCount())
	}

	// Re-join before the delay elapses -> timer cancels.
	if err := sm.JoinChannel(clientID, id); err != nil {
		t.Fatalf("JoinChannel rejoin: %v", err)
	}
	mgr.OnClientJoinedChannel(id)
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
	if err := sm.LeaveChannel(clientID); err != nil {
		t.Fatalf("LeaveChannel final: %v", err)
	}
	mgr.OnClientLeftChannel(id)
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
