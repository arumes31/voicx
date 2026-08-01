// handlers_test.go exercises the TCP control handlers end-to-end over real
// TCP connections using fake backends, so no database is required.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/auth"
	"voicx/internal/broadcast"
	"voicx/internal/channels"
	"voicx/internal/chatcrypto"
	"voicx/internal/config"
	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/state"
	"voicx/internal/store"
)

// --- fakes -----------------------------------------------------------------

// fakeAuth implements AuthBackend with in-memory credentials, public keys,
// and bans.
type fakeAuth struct {
	passwords   map[string]string // uniqueID -> password
	pubkeys     map[string]string // uniqueID -> PEM public key
	users       map[string]*auth.User
	nicknames   map[string]*auth.User // nickname -> user
	pubkeyIndex map[string]*auth.User // PEM public key -> user
	bans        map[string]*auth.Ban  // uniqueID or IP -> active ban

	mu       sync.Mutex
	bindings [][2]any // (userID, publicKey) recorded by BindPublicKey
	e2eKeys  map[int64]string
	e2eByUID map[string]string
}

func (f *fakeAuth) AuthenticatePassword(_ context.Context, uniqueID, password string) (bool, error) {
	pw, ok := f.passwords[uniqueID]
	if !ok {
		return false, auth.ErrUserNotFound
	}
	return pw == password, nil
}

func (f *fakeAuth) AuthenticateChallenge(_ context.Context, uniqueID string, challenge, signature []byte) (bool, error) {
	pub, ok := f.pubkeys[uniqueID]
	if !ok {
		return false, auth.ErrUserNotFound
	}
	if err := auth.VerifyChallenge(pub, challenge, signature); err != nil {
		return false, nil
	}
	return true, nil
}

func (f *fakeAuth) AuthenticateNickname(_ context.Context, nickname, password string) (*auth.User, error) {
	u, ok := f.nicknames[nickname]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	if f.passwords[u.UniqueID] != password {
		return nil, nil
	}
	return u, nil
}

func (f *fakeAuth) LookupUser(_ context.Context, uniqueID string) (*auth.User, error) {
	u, ok := f.users[uniqueID]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeAuth) LookupUserByPublicKey(_ context.Context, publicKey string) (*auth.User, error) {
	u, ok := f.pubkeyIndex[publicKey]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeAuth) BindPublicKey(_ context.Context, userID int64, publicKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings = append(f.bindings, [2]any{userID, publicKey})
	return nil
}

// e2eKeys records published X25519 keys per user ID.
func (f *fakeAuth) SetE2EPublicKey(_ context.Context, userID int64, publicKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.e2eKeys == nil {
		f.e2eKeys = map[int64]string{}
	}
	if f.e2eByUID == nil {
		f.e2eByUID = map[string]string{}
	}
	f.e2eKeys[userID] = publicKey
	for _, u := range f.users {
		if u.ID == userID {
			f.e2eByUID[u.UniqueID] = publicKey
		}
	}
	return nil
}

// GetE2EPublicKey resolves a published key by unique ID.
func (f *fakeAuth) GetE2EPublicKey(_ context.Context, uniqueID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.e2eByUID == nil {
		return "", auth.ErrUserNotFound
	}
	key, ok := f.e2eByUID[uniqueID]
	if !ok {
		return "", auth.ErrUserNotFound
	}
	return key, nil
}

func (f *fakeAuth) LookupActiveBan(_ context.Context, uniqueID, ip string) (*auth.Ban, error) {
	if b, ok := f.bans[uniqueID]; ok {
		return b, nil
	}
	if b, ok := f.bans[ip]; ok {
		return b, nil
	}
	return nil, nil
}

// fakeChannels implements ChannelBackend against an in-memory state manager,
// mirroring what the real channels.ChannelManager does to state.
type fakeChannels struct {
	state *state.Manager

	mu      sync.Mutex
	nextID  int64
	created []channels.ChannelSpec
	deleted []int64
	joinLog []int64
	leftLog []int64
}

func (f *fakeChannels) CreateChannel(_ context.Context, spec channels.ChannelSpec) (int64, error) {
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	var passwordHash string
	if spec.Password != "" {
		hash, err := auth.HashPassword(spec.Password)
		if err != nil {
			return 0, err
		}
		passwordHash = hash
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.created = append(f.created, spec)
	f.state.AddChannel(&state.Channel{
		ChannelID:       f.nextID,
		ParentID:        spec.ParentID,
		Name:            spec.Name,
		Topic:           spec.Topic,
		ChannelType:     int(spec.Type),
		MaxClients:      spec.MaxClients,
		CreatedAt:       time.Now(),
		PasswordHash:    passwordHash,
		NeededJoinPower: spec.NeededJoinPower,
	})
	return f.nextID, nil
}

func (f *fakeChannels) DeleteChannel(_ context.Context, channelID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.state.GetChannel(channelID); !ok {
		return channels.ErrChannelNotFound
	}
	f.state.RemoveChannel(channelID)
	f.deleted = append(f.deleted, channelID)
	return nil
}

func (f *fakeChannels) OnClientJoinedChannel(channelID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joinLog = append(f.joinLog, channelID)
}

// UpdateChannel applies the non-nil fields of upd to the in-memory channel.
func (f *fakeChannels) UpdateChannel(_ context.Context, channelID int64, upd channels.ChannelUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.state.GetChannel(channelID)
	if !ok {
		return channels.ErrChannelNotFound
	}
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
	return nil
}

func (f *fakeChannels) OnClientLeftChannel(channelID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leftLog = append(f.leftLog, channelID)
}

func (f *fakeChannels) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

// fakeChat implements ChatStore in memory.
type fakeChat struct {
	mu        sync.Mutex
	nextID    int64
	messages  map[int64]*store.ChatMessage
	order     []int64
	pins      map[int64]map[int64]store.PinnedMessage // channel -> message -> pin
	reactions map[int64]map[string]map[string]bool    // message -> emoji -> uid -> present
	settings  map[string]string
	settingID map[string]uint32
	legacy    []store.LegacyChatRow
	validated bool

	// storeErr, when set, fails every StoreChatMessage — the constraint
	// violation a CHECK produces in production (91).
	storeErr error
}

func newFakeChat() *fakeChat {
	return &fakeChat{
		messages:  map[int64]*store.ChatMessage{},
		pins:      map[int64]map[int64]store.PinnedMessage{},
		reactions: map[int64]map[string]map[string]bool{},
		settings:  map[string]string{},
		settingID: map[string]uint32{},
	}
}

// failStores makes every subsequent StoreChatMessage fail.
func (f *fakeChat) failStores(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.storeErr = err
}

// messageCount reports how many messages were stored.
func (f *fakeChat) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

func (f *fakeChat) StoreChatMessage(_ context.Context, channelID int64, fromUniqueID, fromNickname, bodyEnc string, keyID uint32) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return 0, f.storeErr
	}
	f.nextID++
	m := &store.ChatMessage{
		ID:           f.nextID,
		ChannelID:    channelID,
		FromUniqueID: fromUniqueID,
		FromNickname: fromNickname,
		BodyEnc:      bodyEnc,
		KeyID:        keyID,
		SentAt:       time.Now(),
	}
	f.messages[f.nextID] = m
	f.order = append(f.order, f.nextID)
	return f.nextID, nil
}

func (f *fakeChat) ChatHistory(_ context.Context, channelID, beforeID int64, limit int) ([]store.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []store.ChatMessage
	for i := len(f.order) - 1; i >= 0 && len(out) < limit; i-- {
		m := f.messages[f.order[i]]
		if m.ChannelID != channelID {
			continue
		}
		if beforeID > 0 && m.ID >= beforeID {
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

func (f *fakeChat) GetChatMessage(_ context.Context, id int64) (*store.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.messages[id], nil
}

func (f *fakeChat) EditChatMessage(_ context.Context, id int64, bodyEnc string, keyID uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[id]
	if !ok || m.DeletedAt != nil {
		return fmt.Errorf("chat message not found or deleted")
	}
	m.BodyEnc = bodyEnc
	m.KeyID = keyID
	now := time.Now()
	m.EditedAt = &now
	return nil
}

func (f *fakeChat) DeleteChatMessage(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[id]
	if !ok || m.DeletedAt != nil {
		return fmt.Errorf("chat message not found or already deleted")
	}
	m.BodyEnc = ""
	m.KeyID = 0
	now := time.Now()
	m.DeletedAt = &now
	return nil
}

func (f *fakeChat) PinChatMessage(_ context.Context, channelID, messageID int64, pinnedBy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pins[channelID] == nil {
		f.pins[channelID] = map[int64]store.PinnedMessage{}
	}
	f.pins[channelID][messageID] = store.PinnedMessage{MessageID: messageID, PinnedBy: pinnedBy, PinnedAt: time.Now()}
	return nil
}

func (f *fakeChat) UnpinChatMessage(_ context.Context, channelID, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pins[channelID], messageID)
	return nil
}

func (f *fakeChat) ChatPins(_ context.Context, channelID int64) ([]store.PinnedMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.PinnedMessage
	for _, p := range f.pins[channelID] {
		p.Message = f.messages[p.MessageID]
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeChat) ToggleReaction(_ context.Context, messageID int64, uniqueID, emoji string) (map[string]int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reactions[messageID] == nil {
		f.reactions[messageID] = map[string]map[string]bool{}
	}
	if f.reactions[messageID][emoji] == nil {
		f.reactions[messageID][emoji] = map[string]bool{}
	}
	added := true
	if f.reactions[messageID][emoji][uniqueID] {
		delete(f.reactions[messageID][emoji], uniqueID)
		added = false
	} else {
		f.reactions[messageID][emoji][uniqueID] = true
	}
	return f.reactionCounts(messageID), added, nil
}

func (f *fakeChat) reactionCounts(messageID int64) map[string]int {
	out := map[string]int{}
	for emoji, users := range f.reactions[messageID] {
		if len(users) > 0 {
			out[emoji] = len(users)
		}
	}
	return out
}

func (f *fakeChat) ReactionsFor(_ context.Context, ids []int64) (map[int64]map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]map[string]int{}
	for _, id := range ids {
		out[id] = f.reactionCounts(id)
	}
	return out, nil
}

func (f *fakeChat) SetServerSetting(_ context.Context, key, value string, keyID uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[key] = value
	f.settingID[key] = keyID
	return nil
}

func (f *fakeChat) GetServerSetting(_ context.Context, key string) (string, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings[key], f.settingID[key], nil
}

// seedLegacy adds a pre-012 plaintext row for the backfill tests.
func (f *fakeChat) seedLegacy(id, channelID int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.legacy = append(f.legacy, store.LegacyChatRow{ID: id, ChannelID: channelID, Body: body})
}

func (f *fakeChat) LegacyPlaintextPage(_ context.Context, afterID int64, limit int) ([]store.LegacyChatRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.LegacyChatRow
	for _, r := range f.legacy {
		if r.ID > afterID && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeChat) SetChatCiphertext(_ context.Context, id int64, bodyEnc string, keyID uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.legacy {
		if r.ID != id {
			continue
		}
		f.messages[id] = &store.ChatMessage{
			ID: id, ChannelID: r.ChannelID, BodyEnc: bodyEnc, KeyID: keyID, SentAt: time.Now(),
		}
		f.order = append(f.order, id)
		f.legacy = append(f.legacy[:i], f.legacy[i+1:]...)
		return nil
	}
	return fmt.Errorf("legacy chat row %d not found", id)
}

func (f *fakeChat) PurgeLegacyPlaintext(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int64(len(f.legacy))
	f.legacy = nil
	return n, nil
}

func (f *fakeChat) CountPlaintextBodies(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.legacy)), nil
}

func (f *fakeChat) ValidateChatNoPlaintext(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validated = true
	return nil
}

type fakeScopeKeyEntry struct {
	scopeID int64
	key     *store.ScopeKey
}

type fakeScopeKeys struct {
	mu      sync.Mutex
	nextID  uint32
	keys    map[string]*fakeScopeKeyEntry
	inserts int
}

func newFakeScopeKeys() *fakeScopeKeys {
	return &fakeScopeKeys{keys: map[string]*fakeScopeKeyEntry{}}
}

// insertCount reports how many generations were ever persisted — the
// disk-exhaustion counter the mint DoS guard asserts on (91).
func (f *fakeScopeKeys) insertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inserts
}

// countFor reports how many generations exist for one scope.
func (f *fakeScopeKeys) countFor(scope int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.keys {
		if e.scopeID == scope {
			n++
		}
	}
	return n
}

func (f *fakeScopeKeys) keyStr(scope int64, id uint32) string {
	return fmt.Sprintf("%d:%d", scope, id)
}

func (f *fakeScopeKeys) CountScopeKeys(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.keys)), nil
}

func (f *fakeScopeKeys) AllocScopeKeyID(_ context.Context, _ int64) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return f.nextID, nil
}

func (f *fakeScopeKeys) CurrentScopeKey(_ context.Context, scope int64) (*store.ScopeKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var cur *store.ScopeKey
	for _, e := range f.keys {
		if e.scopeID == scope {
			if cur == nil || e.key.KeyID > cur.KeyID {
				cur = e.key
			}
		}
	}
	return cur, nil
}

func (f *fakeScopeKeys) GetScopeKey(_ context.Context, scope int64, keyID uint32) (*store.ScopeKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.keys[f.keyStr(scope, keyID)]
	if e == nil {
		return nil, nil
	}
	return e.key, nil
}

func (f *fakeScopeKeys) InsertScopeKey(_ context.Context, scope int64, keyID uint32, wrapped []byte, kekID uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := &store.ScopeKey{
		KeyID:     keyID,
		Wrapped:   wrapped,
		KEKID:     kekID,
		CreatedAt: time.Now(),
	}
	if _, dup := f.keys[f.keyStr(scope, keyID)]; dup {
		return fmt.Errorf("scope key %d/%d already exists", scope, keyID)
	}
	f.keys[f.keyStr(scope, keyID)] = &fakeScopeKeyEntry{scopeID: scope, key: k}
	f.inserts++
	return nil
}

func (f *fakeScopeKeys) RotateScopeKey(ctx context.Context, scope int64, newKeyID uint32, wrapped []byte, kekID uint16) error {
	return f.InsertScopeKey(ctx, scope, newKeyID, wrapped, kekID)
}

// fakePerms implements PermLoader, returning the same tiered permissions for
// every client.
type fakePerms struct {
	tp permissions.TieredPermissions

	// groupSet, when non-nil, is the canned set served by
	// LoadGroupPermissions (guest-group tests).
	groupSet permissions.PermissionSet

	mu            sync.Mutex
	invalidations [][2]int64
}

func (f *fakePerms) LoadForClient(context.Context, int64, int64) (permissions.TieredPermissions, error) {
	return f.tp, nil
}

// Invalidate records a cache-invalidation call.
func (f *fakePerms) Invalidate(userID, channelID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidations = append(f.invalidations, [2]int64{userID, channelID})
}

// InvalidateAll records a full invalidation.
func (f *fakePerms) InvalidateAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidations = append(f.invalidations, [2]int64{-1, -1})
}

// LoadGroupPermissions returns the canned group set.
func (f *fakePerms) LoadGroupPermissions(context.Context, int64) (permissions.PermissionSet, error) {
	if f.groupSet != nil {
		return f.groupSet, nil
	}
	return permissions.NewPermissionSet(), nil
}

// fakeSpool implements SpoolStore in memory.
type fakeSpool struct {
	mu        sync.Mutex
	nextID    int64
	pending   []spooledEntry
	delivered []int64
}

// spooledEntry tracks the recipient alongside the message.
type spooledEntry struct {
	store.SpooledMessage
	toUserID int64
}

func (f *fakeSpool) SpoolMessage(_ context.Context, fromUserID, toUserID int64, fromUniqueID, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.pending = append(f.pending, spooledEntry{
		SpooledMessage: store.SpooledMessage{
			ID:           f.nextID,
			FromUserID:   fromUserID,
			FromUniqueID: fromUniqueID,
			FromName:     fmt.Sprintf("user-%d", fromUserID),
			Message:      message,
			SentAt:       time.Now(),
		},
		toUserID: toUserID,
	})
	return nil
}

func (f *fakeSpool) PendingMessages(_ context.Context, toUserID int64) ([]store.SpooledMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.SpooledMessage
	for _, m := range f.pending {
		if m.toUserID == toUserID {
			out = append(out, m.SpooledMessage)
		}
	}
	return out, nil
}

func (f *fakeSpool) MarkMessagesDelivered(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, ids...)
	keep := f.pending[:0]
	for _, m := range f.pending {
		delivered := false
		for _, id := range ids {
			if m.ID == id {
				delivered = true
				break
			}
		}
		if !delivered {
			keep = append(keep, m)
		}
	}
	f.pending = keep
	return nil
}

func (f *fakeSpool) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// tieredWith builds TieredPermissions with the given entries in the server
// group tier.
func tieredWith(perms ...*permissions.Permission) permissions.TieredPermissions {
	tp := permissions.NewTieredPermissions()
	set := permissions.NewPermissionSet()
	for _, p := range perms {
		set.Set(p)
	}
	tp.Set(permissions.TierServerGroup, set)
	return tp
}

func boolPerm(key permissions.PermissionKey, granted bool) *permissions.Permission {
	v := 0
	if granted {
		v = 1
	}
	return &permissions.Permission{Key: key, Type: permissions.PermissionTypeBoolean, Value: v}
}

// --- test harness ----------------------------------------------------------

// testEnv bundles a running server with its fake backends.
type testEnv struct {
	addr       string
	state      *state.Manager
	channels   *fakeChannels
	auth       *fakeAuth
	spool      *fakeSpool
	voice      *fakeVoice
	recorder   *fakeRecorder
	ft         *fakeFileTransfer
	perms      *fakePerms
	tokens     *fakeTokens
	complaints *fakeComplaints
	chat       *fakeChat
	groups     *fakeGroups
	banAdmin   *fakeBanAdmin
	deps       *Deps
	srv        *TCPServer
	stop       func()
}

// startTestEnv starts a TCP server with fake auth/channels/permissions
// backends. perms may be nil, in which case an empty TieredPermissions is
// served (nothing granted; admins still bypass).
func startTestEnv(t *testing.T, perms *permissions.TieredPermissions) *testEnv {
	t.Helper()
	return startTestEnvFull(t, perms, nil)
}

// startTestEnvFull is startTestEnv with a hook to adjust the server config
// (e.g. disable ChatAllowPlaintext for encryption tests).
func startTestEnvFull(t *testing.T, perms *permissions.TieredPermissions, mutateCfg func(*config.Config)) *testEnv {
	t.Helper()
	return startTestEnvDeps(t, perms, mutateCfg, nil)
}

// startTestEnvDeps is startTestEnvFull with an additional hook to adjust the
// server deps (e.g. default group IDs for wave-6a tests).
func startTestEnvDeps(t *testing.T, perms *permissions.TieredPermissions, mutateCfg func(*config.Config), mutateDeps func(*Deps)) *testEnv {
	t.Helper()
	return startTestEnvLogger(t, perms, mutateCfg, mutateDeps, nil)
}

// startTestEnvLogger is startTestEnvDeps with the server's logger supplied by
// the caller, so a test can assert on what the pipeline logs (91).
func startTestEnvLogger(t *testing.T, perms *permissions.TieredPermissions, mutateCfg func(*config.Config), mutateDeps func(*Deps), logger *zap.Logger) *testEnv {
	t.Helper()
	if logger == nil {
		logger = testLogger()
	}

	sm := state.New(testLogger())
	bc := broadcast.New(testLogger(), sm)
	fc := &fakeChannels{state: sm}

	tp := permissions.NewTieredPermissions()
	if perms != nil {
		tp = *perms
	}

	fa := &fakeAuth{
		passwords: map[string]string{"admin-uid": "pw", "user-uid": "pw"},
		pubkeys:   map[string]string{},
		users: map[string]*auth.User{
			"admin-uid": {ID: 1, UniqueID: "admin-uid", Nickname: "admin", IsAdmin: true},
			"user-uid":  {ID: 2, UniqueID: "user-uid", Nickname: "user", IsAdmin: false},
		},
		nicknames:   map[string]*auth.User{},
		pubkeyIndex: map[string]*auth.User{},
		bans:        map[string]*auth.Ban{},
	}
	fs := &fakeSpool{}
	fv := &fakeVoice{}
	fr := &fakeRecorder{}
	ft := &fakeFileTransfer{}
	ftk := &fakeTokens{}
	fcm := &fakeComplaints{}
	fchat := newFakeChat()
	fg := newFakeGroups()
	fba := &fakeBanAdmin{}

	fp := &fakePerms{tp: tp}

	fsk := newFakeScopeKeys()
	kek, err := chatcrypto.LoadKEKRing(filepath.Join(t.TempDir(), "kek.ring"), "", true)
	if err != nil {
		t.Fatalf("load kek ring: %v", err)
	}

	deps := &Deps{
		Auth:         fa,
		State:        sm,
		Channels:     fc,
		Broadcast:    bc,
		Perms:        fp,
		Resolver:     permissions.NewResolver(),
		Spool:        fs,
		Voice:        fv,
		Recorder:     fr,
		FileTransfer: ft,
		Tokens:       ftk,
		Complaints:   fcm,
		Chat:         fchat,
		Groups:       fg,
		BanAdmin:     fba,
		ScopeKeys:    fsk,
		ChatKEK:      kek,
	}
	if mutateDeps != nil {
		mutateDeps(deps)
	}

	addr := freePort(t)
	cfg := &config.Config{
		TCPAddr:            addr,
		HealthAddr:         ":9090",
		FileRoot:           t.TempDir(),
		TLSEnabled:         true,
		TLSDir:             t.TempDir(),
		ChatAllowPlaintext: true, // legacy chat tests send plaintext
		ChatMaxLength:      2000,
	}
	if mutateCfg != nil {
		mutateCfg(cfg)
	}
	srv := New(cfg, logger, deps)
	// Mirror the binary's boot order: the global generation is minted once,
	// eagerly, so nothing on a hot path ever mints (91).
	if err := srv.EnsureGlobalScopeKey(context.Background()); err != nil && deps.ScopeKeys != nil {
		t.Fatalf("ensure global scope key: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()

	conn := dialRetry(t, addr)
	_ = conn.Close() // connectivity probe only

	env := &testEnv{addr: addr, state: sm, channels: fc, auth: fa, spool: fs, voice: fv, recorder: fr, ft: ft, perms: fp, tokens: ftk, complaints: fcm, chat: fchat, groups: fg, banAdmin: fba, deps: deps, srv: srv}
	env.stop = func() {
		cancel()
		if err := <-startErr; err != nil {
			t.Errorf("server start returned error: %v", err)
		}
		_ = srv.Shutdown()
		bc.Close()
	}
	return env
}

// dialRetry dials addr with TLS (the test servers enable TLS with a
// self-signed cert) until the server accepts or the deadline passes.
// Certificate verification is skipped — test clients pin nothing.
func dialRetry(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	dialer := &net.Dialer{Timeout: 300 * time.Millisecond}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test client
	for time.Now().Before(deadline) {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err == nil {
			return conn
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dial tcp server at %s: timeout", addr)
	return nil
}

// send encodes and writes a message to the connection.
func send(t *testing.T, conn net.Conn, mt netproto.MessageType, msg any) {
	t.Helper()
	f, err := netproto.Encode(mt, msg)
	if err != nil {
		t.Fatalf("encode %s: %v", mt, err)
	}
	if err := netproto.WriteFrame(conn, f); err != nil {
		t.Fatalf("write %s: %v", mt, err)
	}
}

// readFrame reads a single frame with a deadline.
func readFrame(t *testing.T, conn net.Conn) *netproto.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(waitDeadline))
	f, err := netproto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

// readOfType reads frames until one of the wanted type arrives, skipping
// asynchronous event/snapshot frames.
func readOfType(t *testing.T, conn net.Conn, mt netproto.MessageType) *netproto.Frame {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		f := readFrame(t, conn)
		if netproto.MessageType(f.Type) == mt {
			return f
		}
	}
	t.Fatalf("no %s frame received before deadline", mt)
	return nil
}

// dialAuthed connects and authenticates, consuming the AuthResponse and the
// initial snapshot. It returns the connection and the assigned client ID.
func dialAuthed(t *testing.T, addr, uniqueID string) (net.Conn, string) {
	t.Helper()
	conn := dialRetry(t, addr)
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: uniqueID, Password: "pw"})

	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("authenticate %q failed: %s", uniqueID, resp.Reason)
	}
	// The snapshot frame must follow the auth response.
	readOfType(t, conn, netproto.MsgSnapshot)
	return conn, resp.ClientID
}

// dialAuthedX25519 authenticates while presenting an encryption key. That is
// the path that receives the global generation and the sealed MOTD in the
// auth response itself, before Connect() would return (133).
func dialAuthedX25519(t *testing.T, addr, uniqueID string, pub [32]byte) (net.Conn, netproto.AuthResponse) {
	t.Helper()
	conn := dialRetry(t, addr)
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Username: uniqueID, Password: "pw", X25519PublicKey: b64e(pub[:]),
	})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("authenticate %q failed: %s", uniqueID, resp.Reason)
	}
	readOfType(t, conn, netproto.MsgSnapshot)
	return conn, resp
}

// decodeEvent unwraps a MsgEvent frame's {"type","data"} envelope.
func decodeEvent(t *testing.T, f *netproto.Frame) (string, json.RawMessage) {
	t.Helper()
	if netproto.MessageType(f.Type) != netproto.MsgEvent {
		t.Fatalf("frame type = %s, want Event", netproto.MessageType(f.Type))
	}
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(f.Payload, &env); err != nil {
		t.Fatalf("unmarshal event envelope: %v", err)
	}
	return env.Type, env.Data
}

// readEventOfType reads MsgEvent frames until one of the wanted event type
// arrives, skipping other asynchronous events (e.g. the client's own
// user_joined announcement).
func readEventOfType(t *testing.T, conn net.Conn, want string) json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		f := readOfType(t, conn, netproto.MsgEvent)
		typ, data := decodeEvent(t, f)
		if typ == want {
			return data
		}
	}
	t.Fatalf("no %q event received before deadline", want)
	return nil
}

// waitDeadline is how long the polling helpers give a condition. It is
// generous because these are wall-clock waits on real sockets: under a full
// `go test ./...` the whole suite competes for cores, and a tight budget turns
// scheduling latency into a flaky failure that hides real regressions.
const waitDeadline = 15 * time.Second

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- tests -----------------------------------------------------------------

// TestRequiresAuthentication verifies that commands other than Authenticate
// and Ping are rejected before authentication.
func TestRequiresAuthentication(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn := dialRetry(t, env.addr)
	defer conn.Close()

	send(t, conn, netproto.MsgChatSend, netproto.ChatSend{Text: "hello"})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeNotAuthenticated {
		t.Fatalf("error code = %d, want %d (not authenticated)", e.Code, errCodeNotAuthenticated)
	}

	// Ping must still work unauthenticated.
	send(t, conn, netproto.MsgPing, netproto.Ping{})
	readOfType(t, conn, netproto.MsgPong)
}

// TestAuthenticateBadCredentials verifies a wrong password yields OK=false.
func TestAuthenticateBadCredentials(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn := dialRetry(t, env.addr)
	defer conn.Close()

	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "wrong"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK {
		t.Fatal("authenticate with wrong password returned OK=true")
	}

	// An unknown user must fail as well.
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "no-such-user", Password: "pw"})
	f = readOfType(t, conn, netproto.MsgAuthResponse)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK {
		t.Fatal("authenticate with unknown user returned OK=true")
	}
}

// TestAuthenticateBannedUniqueID verifies that a client whose unique ID has an
// active ban is rejected with a clear reason, before any credential check.
func TestAuthenticateBannedUniqueID(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	env.auth.bans["user-uid"] = &auth.Ban{ID: 1, Type: 1, Value: "user-uid", Reason: "spam"}

	conn := dialRetry(t, env.addr)
	defer conn.Close()

	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "pw"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK {
		t.Fatal("banned client authenticate returned OK=true")
	}
	if resp.Reason != "banned: spam" {
		t.Fatalf("reason = %q, want %q", resp.Reason, "banned: spam")
	}
}

// TestAuthenticateBannedIP verifies that a client connecting from a banned IP
// is rejected regardless of unique ID.
func TestAuthenticateBannedIP(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	// Tests connect over loopback, so 127.0.0.1 is the client IP.
	env.auth.bans["127.0.0.1"] = &auth.Ban{ID: 2, Type: 0, Value: "127.0.0.1"}

	conn := dialRetry(t, env.addr)
	defer conn.Close()

	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: "user-uid", Password: "pw"})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK {
		t.Fatal("IP-banned client authenticate returned OK=true")
	}
	if resp.Reason != "banned" {
		t.Fatalf("reason = %q, want %q", resp.Reason, "banned")
	}
}

// TestAuthenticateLifecycle verifies that a successful auth registers the
// client in state and that disconnecting removes it again.
func TestAuthenticateLifecycle(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, clientID := dialAuthed(t, env.addr, "user-uid")

	sc, ok := env.state.GetClient(clientID)
	if !ok {
		t.Fatalf("client %q not registered in state after auth", clientID)
	}
	if sc.UniqueID != "user-uid" || sc.Nickname != "user" {
		t.Fatalf("state client = %+v, want uniqueID user-uid / nickname user", sc)
	}

	_ = conn.Close()
	waitFor(t, "client removal from state", func() bool {
		_, ok := env.state.GetClient(clientID)
		return !ok
	})
}

// TestCreateChannelPermissionDenied verifies that a non-admin without the
// create permission gets a permission-denied error.
func TestCreateChannelPermissionDenied(t *testing.T) {
	env := startTestEnv(t, nil) // no permissions granted
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "nope", Type: 0})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d (permission denied)", e.Code, errCodePermissionDenied)
	}
	if got := env.channels.createdCount(); got != 0 {
		t.Fatalf("created channels = %d, want 0", got)
	}
}

// TestCreateChannelGranted verifies the resolver path: a non-admin with an
// explicit b_channel_create_temporary grant may create a temporary channel.
func TestCreateChannelGranted(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChannelCreateTemporary, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Temp", Type: 0})
	f := readOfType(t, conn, netproto.MsgChannelList)
	var list netproto.ChannelList
	if err := netproto.Decode(f, &list); err != nil {
		t.Fatalf("decode channel list: %v", err)
	}
	if len(list.Channels) != 1 || list.Channels[0].Name != "Temp" {
		t.Fatalf("channel list = %+v, want one channel named Temp", list.Channels)
	}
	if got := env.channels.createdCount(); got != 1 {
		t.Fatalf("created channels = %d, want 1", got)
	}
}

// TestCreateChannelAsAdminBroadcasts verifies that an admin can create any
// channel type and that other clients receive a channel_created event.
func TestCreateChannelAsAdminBroadcasts(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	data := readEventOfType(t, userConn, eventChannelCreated)
	var ce channelEvent
	if err := json.Unmarshal(data, &ce); err != nil {
		t.Fatalf("unmarshal channel event: %v", err)
	}
	if ce.Name != "Lobby" || ce.ChannelID != 1 {
		t.Fatalf("channel event = %+v, want id 1 name Lobby", ce)
	}
}

// TestJoinChannelAndChat verifies joining a channel and channel-scoped chat
// delivery to the channel's members.
func TestJoinChannelAndChat(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	// Admin creates a permanent channel.
	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	// Both clients join it.
	send(t, adminConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})

	waitFor(t, "both clients in channel", func() bool {
		return len(env.state.ChannelMembers(1)) == 2
	})

	// The user must observe the admin's move via a user_moved event.
	readEventOfType(t, userConn, eventUserMoved)

	// Channel chat from the user reaches the admin (a channel member).
	send(t, userConn, netproto.MsgChatSend, netproto.ChatSend{ChannelID: "1", Text: "hi admin"})
	data := readEventOfType(t, adminConn, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if chat.Text == "" || chat.From != "user" || chat.ChannelID != "1" {
		t.Fatalf("chat = %+v, want from user text 'hi admin' channel 1", chat)
	}
}

// TestDirectMessage verifies direct messages reach the target and echo to the
// sender.
func TestDirectMessage(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgChatSend, netproto.ChatSend{ToClientID: userID, Text: "psst"})

	data := readEventOfType(t, userConn, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if chat.Text != "psst" || chat.From != "admin" {
		t.Fatalf("chat = %+v, want from admin text 'psst'", chat)
	}
}

// TestMoveOtherClientDenied verifies a non-admin without move power cannot
// move another client.
func TestMoveOtherClientDenied(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, userConn, netproto.MsgMoveClient, netproto.MoveClient{ClientID: userID, ChannelID: 1})
	f := readOfType(t, userConn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d (permission denied)", e.Code, errCodePermissionDenied)
	}
}

// TestKickFromChannel verifies an admin can kick a client out of its channel
// and the target observes a kicked event.
func TestKickFromChannel(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, adminID := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, adminConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	waitFor(t, "user in channel", func() bool {
		for _, c := range env.state.ChannelMembers(1) {
			if c.ClientID == userID {
				return true
			}
		}
		return false
	})

	send(t, adminConn, netproto.MsgKickClient, netproto.KickClient{ClientID: userID, Reason: "idle"})

	data := readEventOfType(t, userConn, eventKicked)
	var ke kickEvent
	if err := json.Unmarshal(data, &ke); err != nil {
		t.Fatalf("unmarshal kick event: %v", err)
	}
	if ke.ClientID != userID || ke.ByClientID != adminID || ke.FromServer {
		t.Fatalf("kick event = %+v, want channel kick of %s by %s", ke, userID, adminID)
	}

	waitFor(t, "user removed from channel", func() bool {
		for _, c := range env.state.ChannelMembers(1) {
			if c.ClientID == userID {
				return false
			}
		}
		return true
	})
}

// TestDeleteChannel verifies the admin delete flow broadcasts channel_deleted.
func TestDeleteChannel(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, adminConn, netproto.MsgDeleteChannel, netproto.DeleteChannel{ChannelID: 1})
	data := readEventOfType(t, userConn, eventChannelDeleted)
	var ce channelEvent
	if err := json.Unmarshal(data, &ce); err != nil {
		t.Fatalf("unmarshal channel event: %v", err)
	}
	if ce.ChannelID != 1 {
		t.Fatalf("deleted channel id = %d, want 1", ce.ChannelID)
	}
	if _, ok := env.state.GetChannel(1); ok {
		t.Fatal("channel 1 still in state after delete")
	}
}

// TestJoinUnknownChannel verifies joining a nonexistent channel fails.
func TestJoinUnknownChannel(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 999})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeNotFound {
		t.Fatalf("error code = %d, want %d (not found)", e.Code, errCodeNotFound)
	}
}

// TestSendErrorOnMalformed verifies malformed payloads are rejected.
func TestSendErrorOnMalformed(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	if err := netproto.WriteFrame(conn, &netproto.Frame{
		Type:    uint16(netproto.MsgCreateChannel),
		Payload: []byte("{not json"),
	}); err != nil {
		t.Fatalf("write malformed frame: %v", err)
	}
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodeMalformed {
		t.Fatalf("error code = %d, want %d (malformed)", e.Code, errCodeMalformed)
	}
}

// intPerm builds an integer (power) permission entry for the fake loader.
func intPerm(key permissions.PermissionKey, v int) *permissions.Permission {
	return &permissions.Permission{Key: key, Type: permissions.PermissionTypeInteger, Value: v}
}

// --- challenge-response auth ------------------------------------------------

// TestChallengeAuthHandshake exercises the full Ed25519 challenge-response
// handshake over TCP with a real key pair generated in-test.
func TestChallengeAuthHandshake(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	pubPEM, privPEM, err := auth.GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	uid, err := auth.UniqueIDFromPublicKey(pubPEM)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey: %v", err)
	}
	env.auth.pubkeys[uid] = pubPEM
	env.auth.users[uid] = &auth.User{ID: 3, UniqueID: uid, Nickname: "keyuser"}

	conn := dialRetry(t, env.addr)
	defer conn.Close()

	// Round 1: authenticate without a password -> challenge.
	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: uid})
	f := readOfType(t, conn, netproto.MsgAuthChallenge)
	var ch netproto.AuthChallenge
	if err := netproto.Decode(f, &ch); err != nil {
		t.Fatalf("decode auth challenge: %v", err)
	}
	if len(ch.Challenge) == 0 {
		t.Fatal("empty challenge")
	}

	// Round 2: sign the challenge and reply.
	sig, err := auth.SignChallenge(privPEM, ch.Challenge)
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}
	send(t, conn, netproto.MsgAuthSignature, netproto.AuthSignature{UniqueID: uid, Signature: sig})

	f = readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !resp.OK || resp.UniqueID != uid || resp.Nickname != "keyuser" {
		t.Fatalf("auth response = %+v, want OK uniqueID %q nickname keyuser", resp, uid)
	}
	readOfType(t, conn, netproto.MsgSnapshot)
}

// TestChallengeAuthBadSignature verifies that a wrong signature is rejected.
func TestChallengeAuthBadSignature(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	pubPEM, _, err := auth.GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	uid, err := auth.UniqueIDFromPublicKey(pubPEM)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey: %v", err)
	}
	env.auth.pubkeys[uid] = pubPEM
	env.auth.users[uid] = &auth.User{ID: 3, UniqueID: uid, Nickname: "keyuser"}

	conn := dialRetry(t, env.addr)
	defer conn.Close()

	send(t, conn, netproto.MsgAuthenticate, netproto.Authenticate{Username: uid})
	readOfType(t, conn, netproto.MsgAuthChallenge)

	// A garbage signature must be rejected.
	send(t, conn, netproto.MsgAuthSignature, netproto.AuthSignature{UniqueID: uid, Signature: []byte("bad")})
	f := readOfType(t, conn, netproto.MsgAuthResponse)
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK {
		t.Fatal("auth with bad signature returned OK=true")
	}

	// A signature without a pending challenge must also be rejected.
	send(t, conn, netproto.MsgAuthSignature, netproto.AuthSignature{UniqueID: uid, Signature: []byte("bad")})
	f = readOfType(t, conn, netproto.MsgAuthResponse)
	if err := netproto.Decode(f, &resp); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if resp.OK || resp.Reason != "no pending challenge" {
		t.Fatalf("auth response = %+v, want OK=false reason 'no pending challenge'", resp)
	}
}

// --- channel password / needed join power -----------------------------------

// TestJoinChannelPassword verifies the channel password gate: no password and
// a wrong password are denied, the correct password joins.
func TestJoinChannelPassword(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Vault", Type: 2, Password: "chanpw"})
	readOfType(t, adminConn, netproto.MsgChannelList)

	expectDenied := func(msg netproto.JoinChannel, why string) {
		t.Helper()
		send(t, userConn, netproto.MsgJoinChannel, msg)
		f := readOfType(t, userConn, netproto.MsgError)
		var e netproto.Error
		if err := netproto.Decode(f, &e); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if e.Code != errCodePermissionDenied {
			t.Fatalf("%s: error code = %d, want %d", why, e.Code, errCodePermissionDenied)
		}
	}

	expectDenied(netproto.JoinChannel{ChannelID: 1}, "join without password")
	expectDenied(netproto.JoinChannel{ChannelID: 1, Password: "wrong"}, "join with wrong password")

	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1, Password: "chanpw"})
	waitFor(t, "user in channel", func() bool {
		for _, c := range env.state.ChannelMembers(1) {
			if c.ClientID == userID {
				return true
			}
		}
		return false
	})
}

// TestJoinChannelIgnorePassword verifies that b_channel_join_ignore_password
// bypasses the channel password.
func TestJoinChannelIgnorePassword(t *testing.T) {
	perms := tieredWith(boolPerm(permissions.PermissionKeyChannelJoinIgnorePassword, true))
	env := startTestEnv(t, &perms)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Vault", Type: 2, Password: "chanpw"})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	waitFor(t, "user in channel", func() bool {
		for _, c := range env.state.ChannelMembers(1) {
			if c.ClientID == userID {
				return true
			}
		}
		return false
	})
}

// TestJoinChannelNeededPower verifies the i_channel_join_power vs
// needed_join_power check: below the needed power is denied, sufficient power
// joins.
func TestJoinChannelNeededPower(t *testing.T) {
	env := startTestEnv(t, nil) // no join power granted
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "HQ", Type: 2, NeededJoinPower: 50})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	f := readOfType(t, userConn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
}

// TestJoinChannelNeededPowerGranted verifies a client with sufficient
// i_channel_join_power joins a channel with a needed_join_power.
func TestJoinChannelNeededPowerGranted(t *testing.T) {
	perms := tieredWith(intPerm(permissions.PermissionKeyChannelJoinPower, 75))
	env := startTestEnv(t, &perms)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "HQ", Type: 2, NeededJoinPower: 50})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	waitFor(t, "user in channel", func() bool {
		for _, c := range env.state.ChannelMembers(1) {
			if c.ClientID == userID {
				return true
			}
		}
		return false
	})
}

// TestCreateChannelNeededPowerCap verifies a non-admin may not set a needed
// join power above their own join power.
func TestCreateChannelNeededPowerCap(t *testing.T) {
	perms := tieredWith(
		boolPerm(permissions.PermissionKeyChannelCreateTemporary, true),
		intPerm(permissions.PermissionKeyChannelJoinPower, 10),
	)
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer conn.Close()

	send(t, conn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "TooHigh", Type: 0, NeededJoinPower: 50})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
	if got := env.channels.createdCount(); got != 0 {
		t.Fatalf("created channels = %d, want 0", got)
	}

	// At or below the caller's own join power it succeeds.
	send(t, conn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Fine", Type: 0, NeededJoinPower: 10})
	readOfType(t, conn, netproto.MsgChannelList)
	if got := env.channels.createdCount(); got != 1 {
		t.Fatalf("created channels = %d, want 1", got)
	}
}

// --- offline message spool --------------------------------------------------

// TestDirectMessageSpooledOffline verifies that an encrypted direct message
// to an offline user is spooled instead of failing, and that a plaintext one
// is refused: the server has no DM key, so it could not seal it, and an
// unsealable body must never reach the spool (91).
func TestDirectMessageSpooledOffline(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer adminConn.Close()

	send(t, adminConn, netproto.MsgChatSend, netproto.ChatSend{
		ToUniqueID: "user-uid", Text: "see you later", Enc: true,
	})
	waitFor(t, "message spooled", func() bool {
		return env.spool.pendingCount() == 1
	})

	send(t, adminConn, netproto.MsgChatSend, netproto.ChatSend{ToUniqueID: "user-uid", Text: "in the clear"})
	f := readOfType(t, adminConn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("plaintext spool: error = %d, want permission denied", e.Code)
	}
	if env.spool.pendingCount() != 1 {
		t.Fatalf("plaintext DM reached the spool: %d pending", env.spool.pendingCount())
	}
}

// TestSpooledMessagesDeliveredOnLogin verifies spooled messages are delivered
// as offline chat events on the next login and then cleared.
func TestSpooledMessagesDeliveredOnLogin(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	// Seed the spool while the user is offline (user-uid has user ID 2).
	if err := env.spool.SpoolMessage(context.Background(), 1, 2, "", "while you were away"); err != nil {
		t.Fatalf("seed spool: %v", err)
	}

	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer userConn.Close()

	data := readEventOfType(t, userConn, eventChat)
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(data, &chat); err != nil {
		t.Fatalf("unmarshal chat: %v", err)
	}
	if !chat.Offline || chat.Text != "while you were away" {
		t.Fatalf("chat = %+v, want offline message 'while you were away'", chat)
	}

	waitFor(t, "spool cleared", func() bool {
		return env.spool.pendingCount() == 0
	})
}
