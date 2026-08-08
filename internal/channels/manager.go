package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/safecast"
	"voicx/internal/state"
	"voicx/internal/store"
)

// DefaultCleanupDelay is the grace period after a temporary channel becomes
// empty before it is automatically deleted. It can be overridden per-manager
// via the CleanupDelay field (e.g. set low in tests).
const DefaultCleanupDelay = 60 * time.Second

// ErrChannelNotFound is returned when an operation references an unknown
// channel.
var ErrChannelNotFound = errors.New("channel not found")

// ErrInvalidSpec is returned when a ChannelSpec fails validation.
var ErrInvalidSpec = errors.New("invalid channel spec")

// ErrInvalidMove is returned when a re-parent would corrupt the tree (168):
// a channel may not become its own parent or descend from itself.
var ErrInvalidMove = errors.New("invalid channel move")

// DeleteResult describes every client-visible consequence of a cascaded
// channel deletion. SubscriberIDs identifies clients whose explicit channel
// subscription snapshot must be refreshed.
type DeleteResult struct {
	RootID        int64
	ChannelIDs    []int64
	SubscriberIDs []string
	Members       []DeletedMember
}

// DeletedMember identifies a client whose voice/control membership was reset
// because its channel was part of a deleted subtree.
type DeletedMember struct {
	ClientID  string
	ChannelID int64
}

// ChannelAdminGroupName is the channel group a channel's creator is assigned
// to on that channel (156). The group is seeded by the group bootstrap; when
// it does not exist the assignment is skipped.
const ChannelAdminGroupName = "Channel Admin"

// maxChannelDepth caps the recursive ancestor walk used by the cycle guard.
const maxChannelDepth = 64

// cleanupTimer wraps a *time.Timer used to schedule the deletion of an empty
// temporary channel. The timer is stored so it can be cancelled (e.g. when a
// client joins the channel before the grace period elapses).
type cleanupTimer struct {
	timer      *time.Timer
	generation uint64
}

// keyedMutexPool provides bounded-lifetime locks for individual channel and
// client identities. Entries include waiters in their reference count, so an
// entry is removed only after no goroutine can still acquire its mutex.
type keyedMutexPool[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

func (p *keyedMutexPool[K]) lock(key K) func() {
	p.mu.Lock()
	if p.entries == nil {
		p.entries = make(map[K]*keyedMutexEntry)
	}
	entry := p.entries[key]
	if entry == nil {
		entry = new(keyedMutexEntry)
		p.entries[key] = entry
	}
	entry.refs++
	p.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		p.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(p.entries, key)
		}
		p.mu.Unlock()
	}
}

type channelManagerHooks struct {
	afterUpdateCommit   func(int64)
	afterSetTypeCommit  func(int64)
	beforeCleanupDelete func(int64)
	beforeLeaveClient   func(string)
	beforeRemoveClient  func(string)
	commitUpdate        func(*sql.Tx) error
	commitSetType       func(*sql.Tx) error
	deleteChannel       func(context.Context, int64) (sql.Result, error)
	rowsAffected        func(sql.Result) (int64, error)
}

// ChannelManager coordinates the lifecycle of channels across the database
// (store.Store) and the in-memory state (state.Manager). It owns the automatic
// cleanup of temporary channels: when a temporary channel becomes empty a
// timer is started; if it remains empty for the cleanup delay the channel is
// deleted from both the database and the state manager.
type ChannelManager struct {
	store  *store.Store
	state  *state.Manager
	logger *zap.Logger

	mu                    sync.Mutex
	timers                map[int64]*cleanupTimer
	nextCleanupGeneration uint64

	// treeMu is shared by every supported channel lifecycle operation. Normal
	// per-channel work takes a read lock; subtree deletion takes the write lock
	// so discovery, the cascading database delete, timer cancellation, and the
	// in-memory removal form one observable operation.
	treeMu       sync.RWMutex
	channelLocks keyedMutexPool[int64]
	clientLocks  keyedMutexPool[string]
	testHooks    channelManagerHooks

	cleanupHandlerMu sync.RWMutex
	cleanupHandler   func(DeleteResult)

	// CleanupDelay is the grace period after a temporary channel becomes empty
	// before it is deleted. Defaults to DefaultCleanupDelay when zero. It is
	// exported so tests can set a short delay.
	CleanupDelay time.Duration
}

// New constructs a ChannelManager wired to the given store, state manager,
// and logger. The cleanup delay defaults to DefaultCleanupDelay.
func New(s *store.Store, sm *state.Manager, logger *zap.Logger) *ChannelManager {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ChannelManager{
		store:        s,
		state:        sm,
		logger:       logger,
		timers:       make(map[int64]*cleanupTimer),
		CleanupDelay: DefaultCleanupDelay,
	}
}

// SetCleanupDeleteHandler installs the side-effect sink for automatic
// temporary-channel deletion. The TCP server uses it to invalidate connected
// clients and voice routing after the manager has committed the deletion.
func (m *ChannelManager) SetCleanupDeleteHandler(handler func(DeleteResult)) {
	m.cleanupHandlerMu.Lock()
	m.cleanupHandler = handler
	m.cleanupHandlerMu.Unlock()
}

func (m *ChannelManager) notifyCleanupDelete(result DeleteResult) {
	m.cleanupHandlerMu.RLock()
	handler := m.cleanupHandler
	m.cleanupHandlerMu.RUnlock()
	if handler != nil {
		handler(result)
	}
}

// cleanupDelayLocked returns the configured cleanup delay, defaulting to
// DefaultCleanupDelay when unset. The caller must hold m.mu.
func (m *ChannelManager) cleanupDelayLocked() time.Duration {
	if m.CleanupDelay <= 0 {
		return DefaultCleanupDelay
	}
	return m.CleanupDelay
}

// lockChannels acquires channel lifecycle locks in ascending ID order. A
// client move needs both its source and target locks; sorting and de-duplicating
// here gives every multi-channel operation the same deadlock-free order.
func (m *ChannelManager) lockChannels(channelIDs ...int64) func() {
	ids := make([]int64, 0, len(channelIDs))
	seen := make(map[int64]struct{}, len(channelIDs))
	for _, channelID := range channelIDs {
		if channelID <= 0 {
			continue
		}
		if _, ok := seen[channelID]; ok {
			continue
		}
		seen[channelID] = struct{}{}
		ids = append(ids, channelID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	unlocks := make([]func(), 0, len(ids))
	for _, channelID := range ids {
		unlocks = append(unlocks, m.channelLocks.lock(channelID))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

func (m *ChannelManager) resultRowsAffected(result sql.Result) (int64, error) {
	if hook := m.testHooks.rowsAffected; hook != nil {
		return hook(result)
	}
	return result.RowsAffected()
}

// SetCleanupDelay sets the grace period an empty temporary channel survives
// before deletion (165), so a brief gap between the last leave and the next
// join does not destroy the channel. A non-positive value restores the
// default. Timers already scheduled keep their old delay.
func (m *ChannelManager) SetCleanupDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d <= 0 {
		d = DefaultCleanupDelay
	}
	m.CleanupDelay = d
}

// CreateChannel validates the spec, hashes the password if non-empty, inserts
// the channel into the database, registers it in the state manager, and — for
// temporary channels — registers a cleanup watcher. It returns the new
// channel's ID.
func (m *ChannelManager) CreateChannel(ctx context.Context, spec ChannelSpec) (int64, error) {
	if err := spec.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()

	// Verify the parent channel exists if specified.
	if spec.ParentID != 0 {
		if err := m.channelExists(ctx, spec.ParentID); err != nil {
			return 0, fmt.Errorf("checking parent channel: %w", err)
		}
	}

	// Hash the password if provided.
	var passwordHash sql.NullString
	if spec.Password != "" {
		hash, err := auth.HashPassword(spec.Password)
		if err != nil {
			return 0, fmt.Errorf("hashing channel password: %w", err)
		}
		passwordHash = sql.NullString{String: hash, Valid: true}
	}

	// max_clients is stored as NULL when 0 or negative (unlimited).
	var maxClients sql.NullInt32
	if spec.MaxClients > 0 {
		value, err := safecast.IntToInt32(spec.MaxClients)
		if err != nil {
			return 0, fmt.Errorf("%w: max clients is out of range: %v", ErrInvalidSpec, err)
		}
		maxClients = sql.NullInt32{Int32: value, Valid: true}
	}

	var parentID sql.NullInt64
	if spec.ParentID != 0 {
		parentID = sql.NullInt64{Int64: spec.ParentID, Valid: true}
	}

	var createdBy sql.NullInt64
	if spec.CreatedBy != 0 {
		createdBy = sql.NullInt64{Int64: spec.CreatedBy, Valid: true}
	}

	const q = `INSERT INTO channels
	          (parent_id, name, topic, order_index, channel_type, max_clients, password_hash, created_by, needed_join_power,
	           opus_bitrate, opus_fec, opus_dtx, opus_stereo)
	          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	          RETURNING id, created_at`
	var (
		channelID int64
		createdAt time.Time
	)
	err := m.store.DB().QueryRowContext(ctx, q,
		parentID,
		spec.Name,
		spec.Topic,
		spec.OrderIndex,
		int16(spec.Type),
		maxClients,
		passwordHash,
		createdBy,
		spec.NeededJoinPower,
		spec.OpusBitrate,
		spec.OpusFEC,
		spec.OpusDTX,
		spec.OpusStereo,
	).Scan(&channelID, &createdAt)
	if err != nil {
		return 0, fmt.Errorf("inserting channel: %w", err)
	}

	// Register in the in-memory state manager.
	m.state.AddChannel(&state.Channel{
		ChannelID:       channelID,
		ParentID:        spec.ParentID,
		Name:            spec.Name,
		Topic:           spec.Topic,
		OrderIndex:      spec.OrderIndex,
		ChannelType:     int(spec.Type),
		MaxClients:      spec.MaxClients,
		CreatedAt:       createdAt,
		PasswordHash:    passwordHash.String,
		NeededJoinPower: spec.NeededJoinPower,
		OpusBitrate:     spec.OpusBitrate,
		OpusFEC:         spec.OpusFEC,
		OpusDTX:         spec.OpusDTX,
		OpusStereo:      spec.OpusStereo,
	})

	// The creator administers what they created (156).
	m.assignChannelAdmin(ctx, spec.CreatedBy, channelID)

	m.logger.Info("channel created",
		zap.Int64("channel_id", channelID),
		zap.String("name", spec.Name),
		zap.String("type", spec.Type.String()),
		zap.Int64("parent_id", spec.ParentID),
	)

	// Reconcile both sides of the new tree edge. A child prevents automatic
	// deletion of a temporary parent; a new empty temporary leaf gets a timer.
	unlock := m.lockChannels(channelID, spec.ParentID)
	m.reconcileCleanupWatcherLocked(channelID)
	if spec.ParentID != 0 {
		m.reconcileCleanupWatcherLocked(spec.ParentID)
	}
	unlock()

	return channelID, nil
}

// LoadIntoState reads all channels from the database and registers them in
// the state manager, returning the number of channels loaded. It is intended
// to be called once at startup before the server accepts clients, so the
// in-memory channel tree reflects the persisted one. Temporary channels are
// empty by definition at startup (no clients are connected yet), so each gets
// a cleanup watcher — matching the semantics of a temp channel that just
// became empty.
func (m *ChannelManager) LoadIntoState(ctx context.Context) (int, error) {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()

	const q = `SELECT id, COALESCE(parent_id, 0), name, COALESCE(topic, ''),
	          order_index, channel_type, COALESCE(max_clients, 0), created_at,
	          COALESCE(password_hash, ''), COALESCE(needed_join_power, 0),
	          opus_bitrate, opus_fec, opus_dtx, opus_stereo, slow_mode_seconds,
	          COALESCE(description, ''), inherit_permissions
	          FROM channels ORDER BY id`
	rows, err := m.store.DB().QueryContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("querying channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	var temporaryIDs []int64
	for rows.Next() {
		var ch state.Channel
		var channelType int16
		if err := rows.Scan(&ch.ChannelID, &ch.ParentID, &ch.Name, &ch.Topic,
			&ch.OrderIndex, &channelType, &ch.MaxClients, &ch.CreatedAt,
			&ch.PasswordHash, &ch.NeededJoinPower,
			&ch.OpusBitrate, &ch.OpusFEC, &ch.OpusDTX, &ch.OpusStereo, &ch.SlowModeSeconds,
			&ch.Description, &ch.InheritPermissions); err != nil {
			return count, fmt.Errorf("scanning channel row: %w", err)
		}
		parsedType, err := ParseChannelType(int(channelType))
		if err != nil {
			return count, fmt.Errorf("channel %d has invalid stored type %d: %w", ch.ChannelID, channelType, err)
		}
		ch.ChannelType = int(parsedType)
		m.state.AddChannel(&ch)
		count++

		if parsedType == ChannelTypeTemporary {
			temporaryIDs = append(temporaryIDs, ch.ChannelID)
		}
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("iterating channel rows: %w", err)
	}
	// The full tree must be present before deciding whether a temporary channel
	// is an empty leaf. Starting timers while rows stream could schedule a
	// parent before its persisted descendants are loaded.
	for _, channelID := range temporaryIDs {
		unlock := m.lockChannels(channelID)
		m.reconcileCleanupWatcherLocked(channelID)
		unlock()
	}

	m.logger.Info("channels loaded into state", zap.Int("count", count))
	return count, nil
}

// DeleteChannel atomically removes a channel subtree from the database, the
// state manager, and cleanup-timer ownership.
func (m *ChannelManager) DeleteChannel(ctx context.Context, channelID int64) error {
	_, err := m.DeleteChannelSubtree(ctx, channelID)
	return err
}

// DeleteChannelSubtree removes a channel and returns the complete cascaded ID
// set so protocol clients can invalidate the same subtree as the server.
func (m *ChannelManager) DeleteChannelSubtree(ctx context.Context, channelID int64) (DeleteResult, error) {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	return m.deleteChannelLocked(ctx, channelID)
}

// deleteChannelLocked removes one database-cascaded subtree while treeMu's
// write lock is held. It discovers IDs from both the database and state so it
// also repairs an already-stale in-memory descendant.
func (m *ChannelManager) deleteChannelLocked(ctx context.Context, channelID int64) (DeleteResult, error) {
	stateParentID := int64(0)
	if channel, ok := m.state.GetChannel(channelID); ok {
		stateParentID = channel.ParentID
	}
	databaseSubtree, err := m.loadChannelSubtree(ctx, channelID)
	if err != nil {
		return DeleteResult{}, err
	}
	stateIDs := m.stateChannelSubtreeIDs(channelID)
	if len(databaseSubtree.IDs) == 0 && len(stateIDs) == 0 {
		return DeleteResult{}, ErrChannelNotFound
	}
	missingStateIDs, err := m.reconcileStateOnlyDescendants(ctx, databaseSubtree.IDs, stateIDs)
	if err != nil {
		return DeleteResult{}, err
	}
	if len(databaseSubtree.IDs) == 0 && len(missingStateIDs) == 0 {
		// The only state descendants were live database rows whose authoritative
		// parents have now been repaired. The requested root itself never existed.
		return DeleteResult{}, ErrChannelNotFound
	}
	channelIDs := mergeChannelIDs(databaseSubtree.IDs, missingStateIDs)
	parentID := databaseSubtree.RootParentID
	if len(databaseSubtree.IDs) == 0 {
		parentID = stateParentID
	}

	var res sql.Result
	if hook := m.testHooks.deleteChannel; hook != nil {
		res, err = hook(ctx, channelID)
	} else {
		const q = `DELETE FROM channels WHERE id = $1`
		res, err = m.store.DB().ExecContext(ctx, q, channelID)
	}
	deleteConfirmed := false
	if err != nil {
		confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		remaining, confirmErr := m.loadChannelSubtree(confirmCtx, channelID)
		cancel()
		if confirmErr != nil {
			return DeleteResult{}, errors.Join(
				fmt.Errorf("deleting channel: %w", err),
				fmt.Errorf("confirming ambiguous channel deletion: %w", confirmErr),
			)
		}
		if len(remaining.IDs) != 0 {
			return DeleteResult{}, fmt.Errorf("deleting channel: %w", err)
		}
		deleteConfirmed = true
		m.logger.Warn("channel deletion returned an error but was confirmed committed",
			zap.Int64("channel_id", channelID),
			zap.Error(err),
		)
	}
	rows := int64(1)
	if !deleteConfirmed {
		rows, err = res.RowsAffected()
		if err != nil {
			return DeleteResult{}, fmt.Errorf("reading deleted channel row count: %w", err)
		}
	}
	for _, id := range channelIDs {
		m.cancelCleanupLocked(id)
	}
	removedState := m.state.RemoveChannels(channelIDs)
	result := DeleteResult{
		RootID:        channelID,
		ChannelIDs:    channelIDs,
		SubscriberIDs: removedState.SubscriberIDs,
		Members:       make([]DeletedMember, 0, len(removedState.Members)),
	}
	for _, member := range removedState.Members {
		result.Members = append(result.Members, DeletedMember{
			ClientID:  member.ClientID,
			ChannelID: member.ChannelID,
		})
	}
	if parentID != 0 {
		m.reconcileCleanupWatcherLocked(parentID)
	}
	if rows == 0 {
		// A pre-delete database or stale-state snapshot established the root's
		// existence. A zero row count therefore means another writer already
		// achieved the database postcondition; still publish state consequences.
		m.logger.Warn("reconciled channel subtree after concurrent database deletion",
			zap.Int64("channel_id", channelID),
			zap.Int("state_channels_removed", len(missingStateIDs)),
		)
		return result, nil
	}

	m.logger.Info("channel subtree deleted",
		zap.Int64("channel_id", channelID),
		zap.Int("channels_removed", len(channelIDs)),
	)
	return result, nil
}

// SetChannelType updates the channel's type in the database and the state
// manager, and adjusts the cleanup timer accordingly:
//
//   - Changing to Temporary when the channel is currently empty starts the
//     cleanup timer.
//   - Changing away from Temporary cancels any pending cleanup timer.
func (m *ChannelManager) SetChannelType(ctx context.Context, channelID int64, newType ChannelType) (retErr error) {
	if !newType.Valid() {
		return fmt.Errorf("%w: invalid channel type %d", ErrInvalidSpec, newType)
	}
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlock := m.lockChannels(channelID)
	defer unlock()

	tx, err := m.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning channel type update: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rolling back channel type update: %w", err))
		}
	}()

	// Lock the database row as well as the in-process lifecycle entry. The row
	// lock protects against writers that do not share this manager instance.
	var currentType int16
	err = tx.QueryRowContext(ctx,
		`SELECT channel_type FROM channels WHERE id = $1 FOR UPDATE`, channelID,
	).Scan(&currentType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrChannelNotFound
		}
		return fmt.Errorf("querying channel type: %w", err)
	}
	parsedCurrentType, err := ParseChannelType(int(currentType))
	if err != nil {
		return fmt.Errorf("channel %d has invalid stored type %d: %w", channelID, currentType, err)
	}
	if parsedCurrentType != newType {
		res, err := tx.ExecContext(ctx,
			`UPDATE channels SET channel_type = $1 WHERE id = $2`,
			int16(newType), channelID,
		)
		if err != nil {
			return fmt.Errorf("updating channel type: %w", err)
		}
		rows, rowsErr := m.resultRowsAffected(res)
		if rowsErr != nil {
			return fmt.Errorf("reading updated channel type row count: %w", rowsErr)
		}
		if rows == 0 {
			return ErrChannelNotFound
		}
	}
	commit := tx.Commit
	if hook := m.testHooks.commitSetType; hook != nil {
		commit = func() error { return hook(tx) }
	}
	if err := commit(); err != nil {
		commitErr := fmt.Errorf("committing channel type update: %w", err)
		channel, reconcileErr := m.reconcilePersistedChannel(ctx, channelID)
		if reconcileErr != nil {
			m.cancelCleanupLocked(channelID)
			return errors.Join(commitErr, fmt.Errorf("reconciling ambiguous channel type commit: %w", reconcileErr))
		}
		m.reconcileCleanupWatcherLocked(channelID)
		if channel.ChannelType != int(newType) {
			return commitErr
		}
		m.logger.Warn("channel type commit returned an error but was confirmed committed",
			zap.Int64("channel_id", channelID),
			zap.Error(err),
		)
		return nil
	}
	if hook := m.testHooks.afterSetTypeCommit; hook != nil {
		hook(channelID)
	}

	channelType := int(newType)
	if _, err := m.mirrorChannelUpdate(ctx, channelID, state.ChannelUpdate{ChannelType: &channelType}); err != nil {
		// A stale cleanup timer is more dangerous than a missed cleanup while
		// state is being repaired, so fail safe by cancelling it.
		m.cancelCleanupLocked(channelID)
		return fmt.Errorf("channel type committed but state reconciliation failed: %w", err)
	}

	// Mirror the timer while the same channel lock is still held. This also
	// repairs an inconsistent timer when the persisted type did not change.
	m.reconcileCleanupWatcherLocked(channelID)

	if parsedCurrentType != newType {
		m.logger.Info("channel type changed",
			zap.Int64("channel_id", channelID),
			zap.String("from", parsedCurrentType.String()),
			zap.String("to", newType.String()),
		)
	}
	return nil
}

// UpdateChannel applies the non-nil fields of upd to the channel in the
// database and the in-memory state. It returns ErrChannelNotFound for unknown
// channels and validates the resulting values (bitrate must be non-negative,
// max clients non-negative).
func (m *ChannelManager) UpdateChannel(ctx context.Context, channelID int64, upd ChannelUpdate) (retErr error) {
	if upd.OpusBitrate != nil && *upd.OpusBitrate < 0 {
		return fmt.Errorf("%w: opus bitrate must be >= 0", ErrInvalidSpec)
	}
	if upd.MaxClients != nil && *upd.MaxClients < 0 {
		return fmt.Errorf("%w: max clients must be >= 0", ErrInvalidSpec)
	}
	if upd.NeededJoinPower != nil && *upd.NeededJoinPower < 0 {
		return fmt.Errorf("%w: needed join power must be >= 0", ErrInvalidSpec)
	}
	// Build the SET clause from the non-nil fields only.
	var sets []string
	var args []any
	add := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if upd.Topic != nil {
		add("topic", *upd.Topic)
	}
	if upd.MaxClients != nil {
		if *upd.MaxClients > 0 {
			add("max_clients", *upd.MaxClients)
		} else {
			add("max_clients", nil)
		}
	}
	if upd.OpusBitrate != nil {
		add("opus_bitrate", *upd.OpusBitrate)
	}
	if upd.OpusFEC != nil {
		add("opus_fec", *upd.OpusFEC)
	}
	if upd.OpusDTX != nil {
		add("opus_dtx", *upd.OpusDTX)
	}
	if upd.OpusStereo != nil {
		add("opus_stereo", *upd.OpusStereo)
	}
	if upd.SlowModeSeconds != nil {
		if *upd.SlowModeSeconds < 0 {
			return fmt.Errorf("%w: slow mode must be >= 0", ErrInvalidSpec)
		}
		add("slow_mode_seconds", *upd.SlowModeSeconds)
	}
	if upd.Description != nil {
		add("description", *upd.Description)
	}
	if upd.NeededJoinPower != nil {
		add("needed_join_power", *upd.NeededJoinPower)
	}
	if upd.OrderIndex != nil {
		add("order_index", *upd.OrderIndex)
	}
	if upd.ParentID != nil {
		if *upd.ParentID == 0 {
			add("parent_id", nil)
		} else {
			add("parent_id", *upd.ParentID)
		}
	}
	if upd.InheritPermissions != nil {
		add("inherit_permissions", *upd.InheritPermissions)
	}
	if len(sets) == 0 {
		return nil // nothing to do
	}
	var unlockTree func()
	if upd.ParentID != nil {
		// Reparenting changes cleanup eligibility for two parents. Serialize the
		// parent snapshot, DB commit, state mirror, and timer reconciliation as
		// one tree mutation so a second reparent cannot act on a stale old parent.
		m.treeMu.Lock()
		unlockTree = m.treeMu.Unlock
	} else {
		m.treeMu.RLock()
		unlockTree = m.treeMu.RUnlock
	}
	defer unlockTree()
	oldParentID := int64(0)
	lockedChannelIDs := []int64{channelID}
	if upd.ParentID != nil {
		if current, ok := m.state.GetChannel(channelID); ok {
			oldParentID = current.ParentID
		}
		lockedChannelIDs = append(lockedChannelIDs, oldParentID, *upd.ParentID)
	}
	unlock := m.lockChannels(lockedChannelIDs...)
	defer unlock()

	tx, err := m.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning channel update: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rolling back channel update: %w", err))
		}
	}()
	// Validate and write under the same transaction. validateMoveTx locks the
	// moved channel and each prospective ancestor before reading its parent, so
	// a concurrent move cannot validate against a stale chain.
	if upd.ParentID != nil {
		if err := m.validateMoveTx(ctx, tx, channelID, *upd.ParentID); err != nil {
			return err
		}
	}

	args = append(args, channelID)
	// #nosec G201 -- every column name is selected from fixed literals above;
	// all caller-controlled values remain positional query parameters.
	q := fmt.Sprintf("UPDATE channels SET %s WHERE id = $%d",
		strings.Join(sets, ", "), len(args))
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("updating channel: %w", err)
	}
	rows, rowsErr := m.resultRowsAffected(res)
	if rowsErr != nil {
		return fmt.Errorf("reading updated channel row count: %w", rowsErr)
	}
	if rows == 0 {
		return ErrChannelNotFound
	}
	commit := tx.Commit
	if hook := m.testHooks.commitUpdate; hook != nil {
		commit = func() error { return hook(tx) }
	}
	if err := commit(); err != nil {
		commitErr := fmt.Errorf("committing channel update: %w", err)
		channel, reconcileErr := m.reconcilePersistedChannel(ctx, channelID)
		if reconcileErr != nil {
			return errors.Join(commitErr, fmt.Errorf("reconciling ambiguous channel update commit: %w", reconcileErr))
		}
		m.reconcileCleanupWatcherLocked(channelID)
		if upd.ParentID != nil {
			for _, parentID := range mergeChannelIDs(
				[]int64{oldParentID, *upd.ParentID, channel.ParentID},
			) {
				m.reconcileCleanupWatcherLocked(parentID)
			}
		}
		if !channelMatchesUpdate(channel, upd) {
			return commitErr
		}
		m.logger.Warn("channel update commit returned an error but was confirmed committed",
			zap.Int64("channel_id", channelID),
			zap.Error(err),
		)
		return nil
	}
	if hook := m.testHooks.afterUpdateCommit; hook != nil {
		hook(channelID)
	}

	// Mirror the change into the in-memory state through its ownership
	// boundary; GetChannel returns a snapshot and is never a mutation handle.
	reloaded, err := m.mirrorChannelUpdate(ctx, channelID, state.ChannelUpdate{
		ParentID:           upd.ParentID,
		Topic:              upd.Topic,
		OrderIndex:         upd.OrderIndex,
		MaxClients:         upd.MaxClients,
		NeededJoinPower:    upd.NeededJoinPower,
		OpusBitrate:        upd.OpusBitrate,
		OpusFEC:            upd.OpusFEC,
		OpusDTX:            upd.OpusDTX,
		OpusStereo:         upd.OpusStereo,
		SlowModeSeconds:    upd.SlowModeSeconds,
		Description:        upd.Description,
		InheritPermissions: upd.InheritPermissions,
	})
	if err != nil {
		return fmt.Errorf("channel update committed but state reconciliation failed: %w", err)
	}
	if reloaded {
		m.reconcileCleanupWatcherLocked(channelID)
	}
	if upd.ParentID != nil {
		if oldParentID != 0 && oldParentID != *upd.ParentID {
			m.reconcileCleanupWatcherLocked(oldParentID)
		}
		if *upd.ParentID != 0 {
			m.reconcileCleanupWatcherLocked(*upd.ParentID)
		}
	}

	m.logger.Info("channel updated",
		zap.Int64("channel_id", channelID),
		zap.Int("fields", len(sets)),
	)
	return nil
}

// MoveClient atomically moves a client in state and updates cleanup ownership
// for both the source and target channels. Holding the target lifecycle lock
// across the state mutation and timer cancellation makes a successful join
// linearizable with temporary-channel deletion: either cleanup deletes first
// and the move fails, or the move cancels cleanup before deletion can claim.
func (m *ChannelManager) MoveClient(clientID string, targetChannelID int64) (int64, error) {
	return m.MoveClientWithLifecycle(clientID, targetChannelID, nil)
}

// MoveClientWithLifecycle moves a client and invokes afterMove before
// releasing the client, channel, and tree lifecycle locks. The callback is
// for non-reentrant external consequences (voice membership, key delivery,
// and event publication) that must be ordered before a competing subtree
// deletion. It must not call back into ChannelManager.
func (m *ChannelManager) MoveClientWithLifecycle(clientID string, targetChannelID int64, afterMove func(oldChannelID int64)) (int64, error) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlockClient := m.clientLocks.lock(clientID)
	defer unlockClient()

	oldChannelID, _, ok := m.state.ClientChannelState(clientID)
	if !ok {
		return 0, state.ErrClientNotFound
	}
	unlockChannels := m.lockChannels(oldChannelID, targetChannelID)
	defer unlockChannels()

	if err := m.state.MoveClient(clientID, targetChannelID); err != nil {
		return oldChannelID, err
	}
	m.cancelCleanupLocked(targetChannelID)
	if oldChannelID != 0 && oldChannelID != targetChannelID {
		m.reconcileCleanupWatcherLocked(oldChannelID)
	}
	if afterMove != nil {
		afterMove(oldChannelID)
	}
	return oldChannelID, nil
}

// LeaveClient atomically removes a client from its current channel and
// reconciles temporary-channel cleanup under the same lifecycle locks used by
// MoveClient.
func (m *ChannelManager) LeaveClient(clientID string) (int64, error) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlockClient := m.clientLocks.lock(clientID)
	defer unlockClient()

	oldChannelID, _, ok := m.state.ClientChannelState(clientID)
	if !ok {
		return 0, state.ErrClientNotFound
	}
	if oldChannelID == 0 {
		return 0, state.ErrNotInChannel
	}
	unlockChannel := m.lockChannels(oldChannelID)
	defer unlockChannel()
	if hook := m.testHooks.beforeLeaveClient; hook != nil {
		hook(clientID)
	}
	if err := m.state.LeaveChannel(clientID); err != nil {
		return oldChannelID, err
	}
	m.reconcileCleanupWatcherLocked(oldChannelID)
	return oldChannelID, nil
}

// RemoveClient atomically removes a disconnected client from state and
// reconciles the actual channel it occupied at removal time.
func (m *ChannelManager) RemoveClient(clientID string) (*state.Client, error) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlockClient := m.clientLocks.lock(clientID)
	defer unlockClient()

	snapshot, ok := m.state.GetClient(clientID)
	if !ok {
		return nil, state.ErrClientNotFound
	}
	unlockChannel := m.lockChannels(snapshot.ChannelID)
	defer unlockChannel()
	if hook := m.testHooks.beforeRemoveClient; hook != nil {
		hook(clientID)
	}
	removed, ok := m.state.RemoveClient(clientID)
	if !ok {
		return nil, state.ErrClientNotFound
	}
	if removed.ChannelID != 0 {
		m.reconcileCleanupWatcherLocked(removed.ChannelID)
	}
	return removed, nil
}

// WithChannelLifecycle runs a bounded channel-specific operation while
// deletion and same-channel lifecycle mutation are excluded. Asset handlers
// use this to keep filesystem writes/reads linearizable with subtree removal.
// The callback must not re-enter ChannelManager.
func (m *ChannelManager) WithChannelLifecycle(channelID int64, operation func() error) error {
	if operation == nil {
		return fmt.Errorf("%w: channel lifecycle operation is nil", ErrInvalidSpec)
	}
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlock := m.lockChannels(channelID)
	defer unlock()
	if _, ok := m.state.GetChannel(channelID); !ok {
		return ErrChannelNotFound
	}
	return operation()
}

// OnClientLeftChannel starts a cleanup timer if the channel is temporary and
// now has zero members.
func (m *ChannelManager) OnClientLeftChannel(channelID int64) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlock := m.lockChannels(channelID)
	defer unlock()
	m.reconcileCleanupWatcherLocked(channelID)
}

// reconcileCleanupWatcherLocked mirrors the current state into timer
// ownership. The caller must hold the channel lifecycle lock.
func (m *ChannelManager) reconcileCleanupWatcherLocked(channelID int64) {
	ch, ok := m.state.GetChannel(channelID)
	if ok && ch.ChannelType == int(ChannelTypeTemporary) &&
		len(m.state.ChannelMembers(channelID)) == 0 &&
		len(m.stateChannelSubtreeIDs(channelID)) == 1 {
		m.startCleanupWatcherLocked(channelID)
		return
	}
	m.cancelCleanupLocked(channelID)
}

// StartCleanupWatcher schedules a cleanup timer for the given temporary
// channel. When the timer fires, the manager re-checks (under the lock) that
// the channel is still temporary and still empty, and only then deletes it.
// If a timer is already running for the channel it is cancelled and replaced.
func (m *ChannelManager) StartCleanupWatcher(channelID int64) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlock := m.lockChannels(channelID)
	defer unlock()
	m.reconcileCleanupWatcherLocked(channelID)
}

// startCleanupWatcherLocked replaces the active token for channelID. The
// caller must hold the channel lifecycle lock.
func (m *ChannelManager) startCleanupWatcherLocked(channelID int64) {
	m.mu.Lock()

	// Cancel any existing timer for this channel.
	if existing, ok := m.timers[channelID]; ok {
		existing.timer.Stop()
	}

	delay := m.cleanupDelayLocked()
	m.nextCleanupGeneration++
	entry := &cleanupTimer{generation: m.nextCleanupGeneration}
	m.timers[channelID] = entry
	entry.timer = time.AfterFunc(delay, func() {
		m.cleanupCallback(channelID, entry)
	})
	m.mu.Unlock()

	m.logger.Debug("cleanup watcher scheduled",
		zap.Int64("channel_id", channelID),
		zap.Duration("delay", delay),
		zap.Uint64("generation", entry.generation),
	)
}

// CancelCleanup cancels any pending cleanup timer for the channel.
func (m *ChannelManager) CancelCleanup(channelID int64) {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	unlock := m.lockChannels(channelID)
	defer unlock()
	m.cancelCleanupLocked(channelID)
}

// cancelCleanupLocked invalidates the exact active timer token. The caller
// must hold the channel lifecycle lock.
func (m *ChannelManager) cancelCleanupLocked(channelID int64) {
	m.mu.Lock()
	if existing, ok := m.timers[channelID]; ok {
		existing.timer.Stop()
		delete(m.timers, channelID)
		m.mu.Unlock()
		m.logger.Debug("cleanup watcher cancelled", zap.Int64("channel_id", channelID))
		return
	}
	m.mu.Unlock()
}

// cleanupCallback is invoked by a cleanup timer when the grace period elapses.
// It claims only its own token and keeps the tree write lock through the final
// leaf/emptiness check and database/state deletion. External notification runs
// only after that lock is released.
func (m *ChannelManager) cleanupCallback(channelID int64, expected *cleanupTimer) {
	result, err := func() (DeleteResult, error) {
		m.treeMu.Lock()
		defer m.treeMu.Unlock()

		m.mu.Lock()
		active, ok := m.timers[channelID]
		if expected == nil || !ok || active != expected || active.generation != expected.generation {
			m.mu.Unlock()
			return DeleteResult{}, nil
		}
		delete(m.timers, channelID)
		m.mu.Unlock()

		// A temporary parent is not an automatically deletable leaf. This avoids
		// cascading cleanup through permanent or occupied descendants.
		ch, ok := m.state.GetChannel(channelID)
		if !ok || ch.ChannelType != int(ChannelTypeTemporary) ||
			len(m.state.ChannelMembers(channelID)) != 0 ||
			len(m.stateChannelSubtreeIDs(channelID)) != 1 {
			return DeleteResult{}, nil
		}
		if hook := m.testHooks.beforeCleanupDelete; hook != nil {
			hook(channelID)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result, err := m.deleteChannelLocked(ctx, channelID)
		if err != nil && !errors.Is(err, ErrChannelNotFound) {
			// Keep an empty temporary leaf convergent after a transient database
			// failure by installing a fresh token for a later retry.
			m.reconcileCleanupWatcherLocked(channelID)
		}
		return result, err
	}()
	if err != nil {
		if !errors.Is(err, ErrChannelNotFound) {
			m.logger.Warn("cleanup watcher failed to delete channel",
				zap.Int64("channel_id", channelID),
				zap.Error(err),
			)
		}
		return
	}
	if result.RootID == 0 {
		return
	}
	m.logger.Info("cleanup watcher deleted empty temporary channel",
		zap.Int64("channel_id", channelID),
	)
	m.notifyCleanupDelete(result)
}

// Close cancels all pending cleanup timers. It should be called on server
// shutdown to avoid goroutine leaks.
func (m *ChannelManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.timers {
		t.timer.Stop()
		delete(m.timers, id)
	}
}

// CleanupTimersCount returns the number of currently pending cleanup timers,
// for observability.
func (m *ChannelManager) CleanupTimersCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.timers)
}

type channelSubtree struct {
	IDs          []int64
	RootParentID int64
}

func (m *ChannelManager) loadChannelSubtree(ctx context.Context, channelID int64) (channelSubtree, error) {
	const q = `WITH RECURSIVE subtree(id, root_parent_id) AS (
	              SELECT id, COALESCE(parent_id, 0) FROM channels WHERE id = $1
	              UNION
	              SELECT child.id, parent.root_parent_id
	              FROM channels child
	              JOIN subtree parent ON child.parent_id = parent.id
	          )
	          SELECT id, root_parent_id FROM subtree ORDER BY id`
	rows, err := m.store.DB().QueryContext(ctx, q, channelID)
	if err != nil {
		return channelSubtree{}, fmt.Errorf("discovering channel subtree: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var subtree channelSubtree
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id, &subtree.RootParentID); err != nil {
			return channelSubtree{}, fmt.Errorf("scanning channel subtree: %w", err)
		}
		subtree.IDs = append(subtree.IDs, id)
	}
	if err := rows.Err(); err != nil {
		return channelSubtree{}, fmt.Errorf("iterating channel subtree: %w", err)
	}
	return subtree, nil
}

// reconcileStateOnlyDescendants distinguishes genuinely deleted database rows
// from live rows whose in-memory parent is stale. A successful but ambiguously
// reported reparent can leave the latter behind; blindly unioning every state
// descendant into a later deletion would remove a live channel from state.
// The caller holds treeMu for writing, so these repairs and timer updates are
// serialized with every supported channel lifecycle operation.
func (m *ChannelManager) reconcileStateOnlyDescendants(
	ctx context.Context,
	databaseIDs, stateIDs []int64,
) ([]int64, error) {
	databaseSet := make(map[int64]struct{}, len(databaseIDs))
	for _, id := range databaseIDs {
		databaseSet[id] = struct{}{}
	}
	missingIDs := make([]int64, 0)
	timerIDs := make([]int64, 0)
	for _, id := range stateIDs {
		if _, inSubtree := databaseSet[id]; inSubtree {
			continue
		}
		before, _ := m.state.GetChannel(id)
		persisted, err := m.loadChannelState(ctx, id)
		if errors.Is(err, ErrChannelNotFound) {
			missingIDs = append(missingIDs, id)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking state-only channel %d before subtree deletion: %w", id, err)
		}
		m.mirrorPersistedChannel(persisted)
		timerIDs = append(timerIDs, id, persisted.ParentID)
		if before != nil {
			timerIDs = append(timerIDs, before.ParentID)
		}
	}
	for _, id := range mergeChannelIDs(timerIDs) {
		m.reconcileCleanupWatcherLocked(id)
	}
	return missingIDs, nil
}

func (m *ChannelManager) stateChannelSubtreeIDs(channelID int64) []int64 {
	children := make(map[int64][]int64)
	present := make(map[int64]struct{})
	for _, channel := range m.state.ListChannels() {
		present[channel.ChannelID] = struct{}{}
		children[channel.ParentID] = append(children[channel.ParentID], channel.ChannelID)
	}
	ids := make([]int64, 0, 1)
	queue := []int64{channelID}
	seen := make(map[int64]struct{})
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := present[id]; ok {
			ids = append(ids, id)
		}
		queue = append(queue, children[id]...)
	}
	return ids
}

func mergeChannelIDs(groups ...[]int64) []int64 {
	seen := make(map[int64]struct{})
	for _, group := range groups {
		for _, id := range group {
			if id > 0 {
				seen[id] = struct{}{}
			}
		}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// reconcilePersistedChannel reloads authoritative persisted fields after an
// indeterminate commit result without overwriting derived membership/icon
// state on an existing channel.
func (m *ChannelManager) reconcilePersistedChannel(ctx context.Context, channelID int64) (*state.Channel, error) {
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	channel, err := m.loadChannelState(reconcileCtx, channelID)
	if err != nil {
		return nil, err
	}
	m.mirrorPersistedChannel(channel)
	return channel, nil
}

func (m *ChannelManager) mirrorPersistedChannel(channel *state.Channel) {
	channelType := channel.ChannelType
	hasPassword := channel.PasswordHash != ""
	if !m.state.UpdateChannel(channel.ChannelID, state.ChannelUpdate{
		ParentID:           &channel.ParentID,
		Name:               &channel.Name,
		Topic:              &channel.Topic,
		OrderIndex:         &channel.OrderIndex,
		ChannelType:        &channelType,
		MaxClients:         &channel.MaxClients,
		PasswordHash:       &channel.PasswordHash,
		HasPassword:        &hasPassword,
		NeededJoinPower:    &channel.NeededJoinPower,
		OpusBitrate:        &channel.OpusBitrate,
		OpusFEC:            &channel.OpusFEC,
		OpusDTX:            &channel.OpusDTX,
		OpusStereo:         &channel.OpusStereo,
		SlowModeSeconds:    &channel.SlowModeSeconds,
		Description:        &channel.Description,
		InheritPermissions: &channel.InheritPermissions,
	}) {
		m.state.AddChannel(channel)
	}
}

func channelMatchesUpdate(channel *state.Channel, update ChannelUpdate) bool {
	if channel == nil {
		return false
	}
	return (update.ParentID == nil || channel.ParentID == *update.ParentID) &&
		(update.Topic == nil || channel.Topic == *update.Topic) &&
		(update.OrderIndex == nil || channel.OrderIndex == *update.OrderIndex) &&
		(update.MaxClients == nil || channel.MaxClients == *update.MaxClients) &&
		(update.NeededJoinPower == nil || channel.NeededJoinPower == *update.NeededJoinPower) &&
		(update.OpusBitrate == nil || channel.OpusBitrate == *update.OpusBitrate) &&
		(update.OpusFEC == nil || channel.OpusFEC == *update.OpusFEC) &&
		(update.OpusDTX == nil || channel.OpusDTX == *update.OpusDTX) &&
		(update.OpusStereo == nil || channel.OpusStereo == *update.OpusStereo) &&
		(update.SlowModeSeconds == nil || channel.SlowModeSeconds == *update.SlowModeSeconds) &&
		(update.Description == nil || channel.Description == *update.Description) &&
		(update.InheritPermissions == nil || channel.InheritPermissions == *update.InheritPermissions)
}

// mirrorChannelUpdate applies a patch to state and reloads the authoritative
// database row if state unexpectedly lacks the channel after the commit. The
// reconciliation uses a short context detached from request cancellation: the
// database write is already durable, so abandoning the state repair would
// knowingly leave the two representations divergent.
func (m *ChannelManager) mirrorChannelUpdate(
	ctx context.Context,
	channelID int64,
	update state.ChannelUpdate,
) (bool, error) {
	if m.state.UpdateChannel(channelID, update) {
		return false, nil
	}
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	channel, err := m.loadChannelState(reconcileCtx, channelID)
	if err != nil {
		return false, err
	}
	m.state.AddChannel(channel)
	if _, ok := m.state.GetChannel(channelID); !ok {
		return false, errors.New("reloaded channel is still absent from state")
	}
	return true, nil
}

func (m *ChannelManager) loadChannelState(ctx context.Context, channelID int64) (*state.Channel, error) {
	const q = `SELECT id, COALESCE(parent_id, 0), name, COALESCE(topic, ''),
	          order_index, channel_type, COALESCE(max_clients, 0), created_at,
	          COALESCE(password_hash, ''), COALESCE(needed_join_power, 0),
	          opus_bitrate, opus_fec, opus_dtx, opus_stereo, slow_mode_seconds,
	          COALESCE(description, ''), inherit_permissions
	          FROM channels WHERE id = $1`
	var (
		channel     state.Channel
		channelType int16
	)
	err := m.store.DB().QueryRowContext(ctx, q, channelID).Scan(
		&channel.ChannelID,
		&channel.ParentID,
		&channel.Name,
		&channel.Topic,
		&channel.OrderIndex,
		&channelType,
		&channel.MaxClients,
		&channel.CreatedAt,
		&channel.PasswordHash,
		&channel.NeededJoinPower,
		&channel.OpusBitrate,
		&channel.OpusFEC,
		&channel.OpusDTX,
		&channel.OpusStereo,
		&channel.SlowModeSeconds,
		&channel.Description,
		&channel.InheritPermissions,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, fmt.Errorf("loading committed channel: %w", err)
	}
	parsedType, err := ParseChannelType(int(channelType))
	if err != nil {
		return nil, fmt.Errorf("channel %d has invalid stored type %d: %w", channelID, channelType, err)
	}
	channel.ChannelType = int(parsedType)
	return &channel, nil
}

// validateMoveTx checks that channelID may be re-parented under newParentID
// (168). Parent 0 (root) is always legal; otherwise the parent must exist, and
// must not be the channel itself or one of its descendants — a cycle would cut
// the subtree out of the tree and out of the join-power inheritance chain.
func (m *ChannelManager) validateMoveTx(ctx context.Context, tx *sql.Tx, channelID, newParentID int64) error {
	var lockedID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE id = $1 FOR UPDATE`, channelID).Scan(&lockedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrChannelNotFound
		}
		return fmt.Errorf("locking channel: %w", err)
	}
	if newParentID == 0 {
		return nil
	}

	current := newParentID
	for depth := 0; ; depth++ {
		if current == channelID {
			return fmt.Errorf("%w: channel %d is an ancestor of %d", ErrInvalidMove, channelID, newParentID)
		}
		var parent sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT parent_id FROM channels WHERE id = $1 FOR UPDATE`, current).Scan(&parent)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) && depth == 0 {
				return fmt.Errorf("%w: parent channel %d does not exist", ErrInvalidMove, newParentID)
			}
			return fmt.Errorf("checking channel ancestry: %w", err)
		}
		if depth >= maxChannelDepth {
			return fmt.Errorf("%w: parent ancestry exceeds maximum depth %d", ErrInvalidMove, maxChannelDepth)
		}
		if !parent.Valid || parent.Int64 == 0 {
			return nil
		}
		current = parent.Int64
	}
}

// assignChannelAdmin gives the channel's creator the channel-admin group on
// the channel they just created (156). A missing group or a failed assignment
// is logged and ignored: channel creation must not fail because the group
// bootstrap has not seeded ChannelAdminGroupName.
func (m *ChannelManager) assignChannelAdmin(ctx context.Context, userID, channelID int64) {
	if userID == 0 || m.store == nil {
		return
	}
	g, err := m.store.FindGroupByName(ctx, "channel", ChannelAdminGroupName)
	if err != nil || g == nil {
		m.logger.Debug("channel admin group unavailable, skipping auto-assign",
			zap.Int64("channel_id", channelID),
			zap.Error(err),
		)
		return
	}
	if err := m.store.AssignChannelGroup(ctx, g.ID, userID, channelID); err != nil {
		m.logger.Warn("channel admin auto-assign failed",
			zap.Int64("channel_id", channelID),
			zap.Int64("user_id", userID),
			zap.Error(err),
		)
		return
	}
	m.logger.Info("channel admin assigned to creator",
		zap.Int64("channel_id", channelID),
		zap.Int64("user_id", userID),
		zap.Int64("channel_group_id", g.ID),
	)
}

// channelExists reports whether a channel with the given ID exists in the
// database.
func (m *ChannelManager) channelExists(ctx context.Context, channelID int64) error {
	var exists bool
	err := m.store.DB().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM channels WHERE id = $1)`, channelID,
	).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: parent channel %d", ErrChannelNotFound, channelID)
	}
	return nil
}
