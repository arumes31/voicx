package permissions

// Tier identifies one of the five permission tiers in the TeamSpeak 3
// hierarchy. The numeric value reflects the EVALUATION ORDER used by the
// resolver: lower numbers are evaluated first and have higher priority.
//
// ---------------------------------------------------------------------
// TS3 PERMISSION TIER PRECEDENCE (documented for future maintainers)
// ---------------------------------------------------------------------
//
// TeamSpeak 3 evaluates permissions for a single client in a single
// channel context across five tiers, in this priority order (highest
// priority first, exactly as TS3 does):
//
//  0. Server Group        — permissions granted via the client's server
//     group memberships (server-wide).
//  1. Client Specific      — permissions granted directly to the client
//     at the server level (client_permissions
//     with channel_id NULL).
//  2. Channel Specific     — permissions granted directly to the client
//     on a specific channel (client_permissions
//     with a non-NULL channel_id).
//  3. Channel Group        — permissions granted via the client's
//     channel group membership on the channel
//     (channel_group_members).
//  4. Channel Client       — permissions granted to a specific client
//     on a specific channel via
//     channel_client_permissions.
//
// RESOLUTION RULE (single-client, single-channel):
//
//	The resolver walks tiers from highest priority (ServerGroup) to
//	lowest (ChannelClient). The FIRST tier that contains an entry for
//	the requested key determines the result:
//
//	  • If that entry has Negate == true  → the permission is DENIED
//	    (Value 0, Negate true), grantedBy that tier.
//	  • Otherwise                          → the entry's Value is the
//	    effective value, grantedBy that tier.
//
//	Lower tiers are NOT consulted once a higher tier has provided a
//	value. This is the standard TS3 behavior: the highest tier that
//	has the entry wins by default.
//
// THE ROLE OF THE SKIP FLAG:
//
//	The Skip flag prevents a permission from being overridden by LOWER
//	tiers. Because the resolver already stops at the first (highest)
//	tier that has the key, Skip on the winning entry is effectively a
//	no-op in the single-client/single-channel case — there are no lower
//	tiers to skip past, since they were never going to be consulted.
//
//	Skip becomes meaningful in two situations handled by LATER steps:
//
//	  1. Multi-group merge within a single tier (Steps 221-240): when a
//	     client belongs to several server groups, the per-tier merge
//	     must decide which group's value wins. A group with Skip set
//	     on a key prevents other groups in the same tier (and lower
//	     tiers) from overriding that key.
//
//	  2. Channel-specific overrides of server-group permissions: in
//	     full TS3 semantics a channel-specific entry can override a
//	     server-group entry UNLESS the server-group entry set Skip.
//	     The single-tier-wins resolver implemented here models the
//	     common case where the server-group entry already wins; the
//	     Skip-aware override path is layered on in Steps 221-240 where
//	     the merge logic can choose to consult lower tiers when the
//	     higher tier did NOT set Skip.
//
//	For Steps 201-220 we implement the straightforward, well-defined
//	rule: highest tier with an entry wins; Negate denies; Skip is
//	accepted and stored but does not change the outcome of Resolve
//	in this single-client/single-channel model.
//
// THE ROLE OF THE NEGATE FLAG:
//
//	Negate explicitly denies a permission. When the winning entry has
//	Negate == true, the resolver returns a denied result (Value 0,
//	Negate true) regardless of the stored Value. Negate at a higher
//	tier therefore overrides any positive value at a lower tier,
//	because the lower tier is never reached.
type Tier int8

const (
	// TierServerGroup is the highest-priority tier: permissions granted
	// through the client's server group memberships.
	TierServerGroup Tier = 0
	// TierClientSpecific covers server-wide permissions granted directly
	// to a client (client_permissions with NULL channel).
	TierClientSpecific Tier = 1
	// TierChannelSpecific covers permissions granted directly to a client
	// on a specific channel (client_permissions with a channel id).
	TierChannelSpecific Tier = 2
	// TierChannelGroup covers permissions granted through the client's
	// channel group membership on the channel.
	TierChannelGroup Tier = 3
	// TierChannelClient is the lowest-priority tier: permissions granted
	// to a specific client on a specific channel via
	// channel_client_permissions.
	TierChannelClient Tier = 4
	// TierChannel covers permissions granted on the channel object itself
	// (channel_permissions, migration 009). It evaluates between
	// TierChannelSpecific and TierChannelGroup in tierOrder; the numeric
	// value stays out of the existing 0-4 range for compatibility.
	TierChannel Tier = 5
)

// String returns a human-readable name for the tier, useful in logs and
// test output.
func (t Tier) String() string {
	switch t {
	case TierServerGroup:
		return "server_group"
	case TierClientSpecific:
		return "client_specific"
	case TierChannelSpecific:
		return "channel_specific"
	case TierChannelGroup:
		return "channel_group"
	case TierChannelClient:
		return "channel_client"
	case TierChannel:
		return "channel"
	default:
		return "unknown"
	}
}

// tierOrder lists tiers in evaluation order (highest priority first).
// It is the canonical iteration order used by the resolver.
var tierOrder = []Tier{
	TierServerGroup,
	TierClientSpecific,
	TierChannelSpecific,
	TierChannel,
	TierChannelGroup,
	TierChannelClient,
}

// TieredPermissions holds the permission sets for all five tiers as they
// apply to a single client in a single channel context. Each tier maps to
// at most one PermissionSet (which itself may contain many keys).
//
// A nil or absent set for a tier is treated as "no entries at this tier"
// and is skipped during resolution.
type TieredPermissions struct {
	tiers map[Tier]PermissionSet
}

// NewTieredPermissions returns an empty, ready-to-use TieredPermissions.
func NewTieredPermissions() TieredPermissions {
	return TieredPermissions{tiers: make(map[Tier]PermissionSet)}
}

// Set stores (or replaces) the PermissionSet for the given tier.
func (tp *TieredPermissions) Set(tier Tier, set PermissionSet) {
	if tp.tiers == nil {
		tp.tiers = make(map[Tier]PermissionSet)
	}
	tp.tiers[tier] = set
}

// Get returns the PermissionSet for the given tier and whether one was
// present. A missing tier returns (nil, false).
func (tp *TieredPermissions) Get(tier Tier) (PermissionSet, bool) {
	set, ok := tp.tiers[tier]
	return set, ok
}
