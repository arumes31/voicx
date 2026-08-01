package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
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

// cleanupTimer wraps a *time.Timer used to schedule the deletion of an empty
// temporary channel. The timer is stored so it can be cancelled (e.g. when a
// client joins the channel before the grace period elapses).
type cleanupTimer struct {
	timer *time.Timer
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

	mu     sync.Mutex
	timers map[int64]*cleanupTimer

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

// cleanupDelay returns the configured cleanup delay, defaulting to
// DefaultCleanupDelay when unset.
func (m *ChannelManager) cleanupDelay() time.Duration {
	if m.CleanupDelay <= 0 {
		return DefaultCleanupDelay
	}
	return m.CleanupDelay
}

// CreateChannel validates the spec, hashes the password if non-empty, inserts
// the channel into the database, registers it in the state manager, and — for
// temporary channels — registers a cleanup watcher. It returns the new
// channel's ID.
func (m *ChannelManager) CreateChannel(ctx context.Context, spec ChannelSpec) (int64, error) {
	if err := spec.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

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
		maxClients = sql.NullInt32{Int32: int32(spec.MaxClients), Valid: true}
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

	m.logger.Info("channel created",
		zap.Int64("channel_id", channelID),
		zap.String("name", spec.Name),
		zap.String("type", spec.Type.String()),
		zap.Int64("parent_id", spec.ParentID),
	)

	// Temporary channels start with a cleanup watcher so that if they are
	// created empty they will be cleaned up after the grace period.
	if spec.Type == ChannelTypeTemporary {
		m.StartCleanupWatcher(channelID)
	}

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
	const q = `SELECT id, COALESCE(parent_id, 0), name, COALESCE(topic, ''),
	          order_index, channel_type, COALESCE(max_clients, 0), created_at,
	          COALESCE(password_hash, ''), COALESCE(needed_join_power, 0),
	          opus_bitrate, opus_fec, opus_dtx, opus_stereo, slow_mode_seconds,
	          COALESCE(description, '')
	          FROM channels ORDER BY id`
	rows, err := m.store.DB().QueryContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("querying channels: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var ch state.Channel
		var channelType int16
		if err := rows.Scan(&ch.ChannelID, &ch.ParentID, &ch.Name, &ch.Topic,
			&ch.OrderIndex, &channelType, &ch.MaxClients, &ch.CreatedAt,
			&ch.PasswordHash, &ch.NeededJoinPower,
			&ch.OpusBitrate, &ch.OpusFEC, &ch.OpusDTX, &ch.OpusStereo, &ch.SlowModeSeconds,
			&ch.Description); err != nil {
			return count, fmt.Errorf("scanning channel row: %w", err)
		}
		ch.ChannelType = int(channelType)
		m.state.AddChannel(&ch)
		count++

		if ChannelType(channelType) == ChannelTypeTemporary {
			m.StartCleanupWatcher(ch.ChannelID)
		}
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("iterating channel rows: %w", err)
	}

	m.logger.Info("channels loaded into state", zap.Int("count", count))
	return count, nil
}

// DeleteChannel removes the channel from the database and the state manager,
// and cancels any pending cleanup timer. Child channels are removed by the
// database's ON DELETE CASCADE constraint.
func (m *ChannelManager) DeleteChannel(ctx context.Context, channelID int64) error {
	const q = `DELETE FROM channels WHERE id = $1`
	res, err := m.store.DB().ExecContext(ctx, q, channelID)
	if err != nil {
		return fmt.Errorf("deleting channel: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Channel may already have been deleted by a cleanup timer; cancel any
		// timer and remove from state for idempotency.
		m.CancelCleanup(channelID)
		m.state.RemoveChannel(channelID)
		return ErrChannelNotFound
	}

	m.CancelCleanup(channelID)
	m.state.RemoveChannel(channelID)

	m.logger.Info("channel deleted", zap.Int64("channel_id", channelID))
	return nil
}

// SetChannelType updates the channel's type in the database and the state
// manager, and adjusts the cleanup timer accordingly:
//
//   - Changing to Temporary when the channel is currently empty starts the
//     cleanup timer.
//   - Changing away from Temporary cancels any pending cleanup timer.
func (m *ChannelManager) SetChannelType(ctx context.Context, channelID int64, newType ChannelType) error {
	if !newType.Valid() {
		return fmt.Errorf("%w: invalid channel type %d", ErrInvalidSpec, newType)
	}

	// Read the current type so we know whether a transition is needed.
	var currentType int16
	err := m.store.DB().QueryRowContext(ctx,
		`SELECT channel_type FROM channels WHERE id = $1`, channelID,
	).Scan(&currentType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrChannelNotFound
		}
		return fmt.Errorf("querying channel type: %w", err)
	}
	if ChannelType(currentType) == newType {
		return nil // no change
	}

	_, err = m.store.DB().ExecContext(ctx,
		`UPDATE channels SET channel_type = $1 WHERE id = $2`,
		int16(newType), channelID,
	)
	if err != nil {
		return fmt.Errorf("updating channel type: %w", err)
	}

	// Update the in-memory state.
	if ch, ok := m.state.GetChannel(channelID); ok {
		ch.ChannelType = int(newType)
	}

	// Adjust the cleanup timer.
	if newType == ChannelTypeTemporary {
		// Changing to temporary: start the timer if the channel is empty.
		if len(m.state.ChannelMembers(channelID)) == 0 {
			m.StartCleanupWatcher(channelID)
		}
	} else {
		// Changing away from temporary: cancel any pending timer.
		m.CancelCleanup(channelID)
	}

	m.logger.Info("channel type changed",
		zap.Int64("channel_id", channelID),
		zap.String("from", ChannelType(currentType).String()),
		zap.String("to", newType.String()),
	)
	return nil
}

// UpdateChannel applies the non-nil fields of upd to the channel in the
// database and the in-memory state. It returns ErrChannelNotFound for unknown
// channels and validates the resulting values (bitrate must be non-negative,
// max clients non-negative).
func (m *ChannelManager) UpdateChannel(ctx context.Context, channelID int64, upd ChannelUpdate) error {
	if upd.OpusBitrate != nil && *upd.OpusBitrate < 0 {
		return fmt.Errorf("%w: opus bitrate must be >= 0", ErrInvalidSpec)
	}
	if upd.MaxClients != nil && *upd.MaxClients < 0 {
		return fmt.Errorf("%w: max clients must be >= 0", ErrInvalidSpec)
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
	if len(sets) == 0 {
		return nil // nothing to do
	}

	args = append(args, channelID)
	q := fmt.Sprintf("UPDATE channels SET %s WHERE id = $%d",
		strings.Join(sets, ", "), len(args))
	res, err := m.store.DB().ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("updating channel: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrChannelNotFound
	}

	// Mirror the change into the in-memory state.
	if ch, ok := m.state.GetChannel(channelID); ok {
		if upd.Topic != nil {
			ch.Topic = *upd.Topic
		}
		if upd.MaxClients != nil {
			ch.MaxClients = *upd.MaxClients
		}
		if upd.OpusBitrate != nil {
			ch.OpusBitrate = *upd.OpusBitrate
		}
		if upd.OpusFEC != nil {
			ch.OpusFEC = *upd.OpusFEC
		}
		if upd.OpusDTX != nil {
			ch.OpusDTX = *upd.OpusDTX
		}
		if upd.OpusStereo != nil {
			ch.OpusStereo = *upd.OpusStereo
		}
		if upd.SlowModeSeconds != nil {
			ch.SlowModeSeconds = *upd.SlowModeSeconds
		}
		if upd.Description != nil {
			ch.Description = *upd.Description
		}
	}

	m.logger.Info("channel updated",
		zap.Int64("channel_id", channelID),
		zap.Int("fields", len(sets)),
	)
	return nil
}

// OnClientJoinedChannel cancels any pending cleanup timer for the channel, as
// a client is now present and the channel should not be deleted.
func (m *ChannelManager) OnClientJoinedChannel(channelID int64) {
	m.CancelCleanup(channelID)
}

// OnClientLeftChannel starts a cleanup timer if the channel is temporary and
// now has zero members.
func (m *ChannelManager) OnClientLeftChannel(channelID int64) {
	ch, ok := m.state.GetChannel(channelID)
	if !ok {
		return
	}
	if ch.ChannelType != int(ChannelTypeTemporary) {
		return
	}
	if len(m.state.ChannelMembers(channelID)) == 0 {
		m.StartCleanupWatcher(channelID)
	}
}

// StartCleanupWatcher schedules a cleanup timer for the given temporary
// channel. When the timer fires, the manager re-checks (under the lock) that
// the channel is still temporary and still empty, and only then deletes it.
// If a timer is already running for the channel it is cancelled and replaced.
func (m *ChannelManager) StartCleanupWatcher(channelID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel any existing timer for this channel.
	if existing, ok := m.timers[channelID]; ok {
		existing.timer.Stop()
		delete(m.timers, channelID)
	}

	delay := m.cleanupDelay()
	t := time.AfterFunc(delay, func() {
		m.cleanupCallback(channelID)
	})
	m.timers[channelID] = &cleanupTimer{timer: t}

	m.logger.Debug("cleanup watcher scheduled",
		zap.Int64("channel_id", channelID),
		zap.Duration("delay", delay),
	)
}

// CancelCleanup cancels any pending cleanup timer for the channel.
func (m *ChannelManager) CancelCleanup(channelID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.timers[channelID]; ok {
		existing.timer.Stop()
		delete(m.timers, channelID)
		m.logger.Debug("cleanup watcher cancelled", zap.Int64("channel_id", channelID))
	}
}

// cleanupCallback is invoked by a cleanup timer when the grace period elapses.
// It re-checks emptiness and type under the lock before deleting the channel.
func (m *ChannelManager) cleanupCallback(channelID int64) {
	// Re-check under the lock that the timer is still the active one (it may
	// have been cancelled and replaced) and remove it from the map.
	m.mu.Lock()
	if _, ok := m.timers[channelID]; !ok {
		m.mu.Unlock()
		return
	}
	delete(m.timers, channelID)
	m.mu.Unlock()

	// Re-check that the channel is still temporary and still empty.
	ch, ok := m.state.GetChannel(channelID)
	if !ok {
		return
	}
	if ch.ChannelType != int(ChannelTypeTemporary) {
		return
	}
	if len(m.state.ChannelMembers(channelID)) != 0 {
		// A client joined before the timer fired; reschedule nothing — the
		// join handler will manage timers from here.
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.DeleteChannel(ctx, channelID); err != nil {
		if !errors.Is(err, ErrChannelNotFound) {
			m.logger.Warn("cleanup watcher failed to delete channel",
				zap.Int64("channel_id", channelID),
				zap.Error(err),
			)
		}
		return
	}
	m.logger.Info("cleanup watcher deleted empty temporary channel",
		zap.Int64("channel_id", channelID),
	)
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
