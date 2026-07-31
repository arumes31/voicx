// Package permissions implements the TeamSpeak 3-style permission model
// for voicx. It defines the permission keys, the five-tier evaluation
// hierarchy, and the resolver that determines the effective value of a
// permission for a client in a given channel context.
//
// This package (Steps 201-220) covers ONLY the in-memory permission model
// and the single-client/single-channel resolver. The multi-group merge
// logic within a single tier (Steps 221-240), the DB-backed loading
// (Steps 221-260), the TCP middleware (Steps 241-260), the join_power vs
// needed_join_power enforcement (Steps 261-280), and the extensive
// edge-case test suite (Steps 281-300) are intentionally out of scope.
package permissions

// PermissionKey identifies a single permission by its TeamSpeak 3-style
// name. Using a distinct string type prevents accidental confusion with
// arbitrary strings and enables type-safe map keys.
type PermissionKey string

// Core TeamSpeak 3-style permission keys.
//
// Naming conventions follow TS3:
//   - Prefix "i_" denotes an integer (power-level) permission.
//   - Prefix "b_" denotes a boolean permission (0 = denied, 1 = granted).
//   - Suffix "_power" denotes the power a client wields to perform an action.
//   - Suffix "_needed_..._power" denotes the power required to perform an
//     action against the holder (e.g. a channel's needed join power).
const (
	// --- Channel join / subscribe ---
	PermissionKeyChannelJoinPower            PermissionKey = "i_channel_join_power"
	PermissionKeyChannelNeededJoinPower      PermissionKey = "i_channel_needed_join_power"
	PermissionKeyChannelJoinIgnorePassword   PermissionKey = "b_channel_join_ignore_password"
	PermissionKeyChannelSubscribePower       PermissionKey = "i_channel_subscribe_power"
	PermissionKeyChannelNeededSubscribePower PermissionKey = "i_channel_needed_subscribe_power"

	// --- Channel creation ---
	PermissionKeyChannelCreateChild         PermissionKey = "b_channel_create_child"
	PermissionKeyChannelCreatePermanent     PermissionKey = "b_channel_create_permanent"
	PermissionKeyChannelCreateSemiPermanent PermissionKey = "b_channel_create_semi_permanent"
	PermissionKeyChannelCreateTemporary     PermissionKey = "b_channel_create_temporary"

	// --- Channel delete / modify ---
	PermissionKeyChannelDelete            PermissionKey = "b_channel_delete"
	PermissionKeyChannelModify            PermissionKey = "b_channel_modify"
	PermissionKeyChannelModifyPower       PermissionKey = "i_channel_modify_power"
	PermissionKeyChannelNeededModifyPower PermissionKey = "i_channel_needed_modify_power"

	// --- Kick ---
	PermissionKeyClientKickFromChannelPower       PermissionKey = "i_client_kick_from_channel_power"
	PermissionKeyClientNeededKickFromChannelPower PermissionKey = "i_client_needed_kick_from_channel_power"
	PermissionKeyClientKickFromServerPower        PermissionKey = "i_client_kick_from_server_power"
	PermissionKeyClientNeededKickFromServerPower  PermissionKey = "i_client_needed_kick_from_server_power"

	// --- Ban ---
	PermissionKeyClientBan            PermissionKey = "b_client_ban"
	PermissionKeyClientBanPower       PermissionKey = "i_client_ban_power"
	PermissionKeyClientNeededBanPower PermissionKey = "i_client_needed_ban_power"

	// --- Move ---
	PermissionKeyClientMovePower       PermissionKey = "i_client_move_power"
	PermissionKeyClientNeededMovePower PermissionKey = "i_client_needed_move_power"

	// --- Talk / video / command / antiflood ---
	PermissionKeyClientUseChannelCommand  PermissionKey = "b_client_use_channel_command"
	PermissionKeyClientTalkPower          PermissionKey = "i_client_talk_power"
	PermissionKeyClientNeededTalkPower    PermissionKey = "i_client_needed_talk_power"
	PermissionKeyClientWhisperPower       PermissionKey = "i_client_whisper_power"
	PermissionKeyClientNeededWhisperPower PermissionKey = "i_client_needed_whisper_power"
	PermissionKeyClientVideoPublish       PermissionKey = "b_client_video_publish"
	PermissionKeyClientIgnoreAntiflood    PermissionKey = "b_client_ignore_antiflood"
	PermissionKeyClientRequestTalker      PermissionKey = "b_client_request_talker"

	// --- File transfer ---
	PermissionKeyFTFileUploadPower         PermissionKey = "i_ft_file_upload_power"
	PermissionKeyFTNeededFileUploadPower   PermissionKey = "i_ft_needed_file_upload_power"
	PermissionKeyFTFileDownloadPower       PermissionKey = "i_ft_file_download_power"
	PermissionKeyFTNeededFileDownloadPower PermissionKey = "i_ft_needed_file_download_power"

	// --- Poke ---
	PermissionKeyClientPokePower       PermissionKey = "i_client_poke_power"
	PermissionKeyClientNeededPokePower PermissionKey = "i_client_needed_poke_power"
	PermissionKeyClientPoke            PermissionKey = "b_client_poke"

	// --- ServerQuery / visibility ---
	PermissionKeyClientServerQueryView PermissionKey = "b_client_serverquery_view"

	// --- Permission management ---
	PermissionKeyPermissionModifyPower  PermissionKey = "b_permission_modify_power"
	PermissionKeyPermissionModifyPowerI PermissionKey = "i_permission_modify_power"

	// --- Virtual server ---
	PermissionKeyVirtualserverSelect             PermissionKey = "b_virtualserver_select"
	PermissionKeyVirtualserverInfoView           PermissionKey = "b_virtualserver_info_view"
	PermissionKeyVirtualserverConnectionInfoView PermissionKey = "b_virtualserver_connectioninfo_view"
	PermissionKeyVirtualserverRecording          PermissionKey = "b_virtualserver_recording"
	PermissionKeyVirtualserverTokenList          PermissionKey = "b_virtualserver_token_list"
	PermissionKeyVirtualserverTokenAdd           PermissionKey = "b_virtualserver_token_add"
	PermissionKeyVirtualserverTokenUse           PermissionKey = "b_virtualserver_token_use"
	PermissionKeyVirtualserverTokenDelete        PermissionKey = "b_virtualserver_token_delete"
)

// PermissionType distinguishes boolean permissions from integer (power)
// permissions. The type is informational at the model level: boolean perms
// use Value 0 (denied) / 1 (granted), while integer perms use Value as a
// power level that is compared against a corresponding "needed" power.
type PermissionType int8

const (
	// PermissionTypeBoolean is a yes/no permission. Value is 0 or 1.
	PermissionTypeBoolean PermissionType = 0
	// PermissionTypeInteger is a power-level permission. Value is the power.
	PermissionTypeInteger PermissionType = 1
)

// Permission is a single permission entry as it applies to a client in a
// particular tier context.
//
// Fields mirror the columns of the permissions table introduced in
// migration 001_init.sql:
//
//   - Value: the effective value of this permission. For boolean perms this
//     is 0 (denied) or 1 (granted); for integer perms it is a power level.
//   - Grant: the maximum value this entry is allowed to grant to others via
//     permission modification. A client may not assign a Value or Grant
//     exceeding their own Grant for the same key. (Enforced in later steps.)
//   - Skip: if true, this permission entry is NOT overridden by lower-tier
//     entries for the same key. See tiers.go for the full precedence rules.
//   - Negate: if true, this permission is explicitly denied regardless of
//     any positive value. A negated entry takes precedence over a granted
//     entry at the same or lower tier.
type Permission struct {
	Key    PermissionKey
	Type   PermissionType
	Value  int
	Grant  int
	Skip   bool
	Negate bool
}

// PermissionSet is a collection of permission entries keyed by PermissionKey.
// At most one entry per key is expected within a single set; the multi-group
// merge that produces a single set per tier is handled in Steps 221-240.
type PermissionSet map[PermissionKey]*Permission

// NewPermissionSet returns an empty, non-nil PermissionSet.
func NewPermissionSet() PermissionSet {
	return make(PermissionSet)
}

// Get returns the permission entry for key and whether it was present.
func (s PermissionSet) Get(key PermissionKey) (*Permission, bool) {
	p, ok := s[key]
	return p, ok
}

// Set stores or replaces the entry for p.Key. Set panics if p is nil to
// guard against silent nil-insertion bugs.
func (s PermissionSet) Set(p *Permission) {
	if p == nil {
		panic("permissions: PermissionSet.Set called with nil permission")
	}
	s[p.Key] = p
}

// Has reports whether an entry for key exists in the set.
func (s PermissionSet) Has(key PermissionKey) bool {
	_, ok := s[key]
	return ok
}

// Keys returns the keys present in the set in a stable, sorted order. The
// sorted order makes resolver output deterministic, which simplifies testing
// and debugging of the tier precedence logic.
func (s PermissionSet) Keys() []PermissionKey {
	keys := make([]PermissionKey, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	// Simple insertion sort: key counts per set are small, so avoiding the
	// sort import keeps the package dependency-light.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
