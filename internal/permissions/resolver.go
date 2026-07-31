package permissions

import (
	"errors"
)

// ErrPermissionNotSet is returned by Resolve when no tier contains an
// entry for the requested permission key. It is a typed sentinel so
// callers can distinguish "explicitly denied" (Negate) from "not set"
// with errors.Is.
var ErrPermissionNotSet = errors.New("permissions: permission not set for this client/channel context")

// noTier is the grantedBy value returned when no tier provided the key.
// It is distinct from every valid Tier constant (which are 0..4).
const noTier Tier = -1

// Resolver computes the effective value of a permission for a single
// client in a single channel context, given the TieredPermissions that
// apply to that context.
//
// The Resolver is stateless apart from an optional logger; it is safe
// to share a single Resolver across goroutines.
type Resolver struct {
	// logger may be nil. When set, the resolver emits debug-level logs
	// describing which tier granted each resolved permission. The
	// logger integration is wired in later steps; for now the field is
	// retained so the resolver signature is stable.
	// logger *zap.Logger // intentionally omitted to avoid a hard dep here
}

// NewResolver returns a Resolver. The resolver holds no mutable state.
func NewResolver() *Resolver {
	return &Resolver{}
}

// Resolve determines the effective Permission for the given key across
// the five tiers in tp.
//
// ALGORITHM (single-client, single-channel; see tiers.go for the full
// precedence documentation):
//
//  1. Iterate tiers in priority order: ServerGroup → ClientSpecific →
//     ChannelSpecific → ChannelGroup → ChannelClient.
//  2. For each tier, look up the key in that tier's PermissionSet.
//  3. If the entry is found AND Negate is true → return a denied result
//     (Value 0, Negate true) grantedBy that tier. Stop.
//  4. If the entry is found AND Skip is true → return this entry's
//     value, grantedBy that tier. Stop. (In the single-tier-wins model
//     this is equivalent to the normal "found" case because lower tiers
//     are never consulted anyway; Skip is honored explicitly so the
//     behavior remains correct when the multi-group merge of
//     Steps 221-240 starts feeding Skip-bearing entries into Resolve.)
//  5. If the entry is found and neither Skip nor Negate → return this
//     entry's value, grantedBy that tier. Stop. (Highest tier with an
//     entry wins; lower tiers are NOT consulted further for this key.)
//  6. If no tier has the key → return (nil, noTier, ErrPermissionNotSet).
//
// The returned *Permission is a copy of the winning entry with the
// resolved Value applied; callers may mutate it without affecting tp.
// When Negate wins, the returned Permission has Value 0 and Negate true
// regardless of the stored Value, matching TS3's "negate denies"
// semantics.
func (r *Resolver) Resolve(tp TieredPermissions, key PermissionKey) (result *Permission, grantedBy Tier, err error) {
	for _, tier := range tierOrder {
		set, ok := tp.Get(tier)
		if !ok || set == nil {
			continue
		}
		p, ok := set.Get(key)
		if !ok || p == nil {
			continue
		}

		// Found the first (highest-priority) tier that has the key.
		// Make a defensive copy so callers cannot mutate the source set.
		resolved := *p

		if p.Negate {
			// Negate explicitly denies regardless of stored value.
			resolved.Value = 0
			resolved.Negate = true
			return &resolved, tier, nil
		}

		// Skip or plain value: this tier's entry wins. Skip is honored
		// here for forward-compatibility with the multi-group merge.
		return &resolved, tier, nil
	}

	return nil, noTier, ErrPermissionNotSet
}

// ResolveBool resolves a boolean permission and returns whether it is
// granted (Value != 0). It is a convenience wrapper around Resolve for
// b_* permissions.
//
// Returns:
//   - granted: true if the resolved Value is non-zero (and not negated).
//   - grantedBy: the tier that supplied the value, or noTier (-1) if unset.
//   - err: ErrPermissionNotSet if no tier has the key, nil otherwise.
//     A negated entry is NOT an error; it simply yields granted=false.
func (r *Resolver) ResolveBool(tp TieredPermissions, key PermissionKey) (bool, Tier, error) {
	p, tier, err := r.Resolve(tp, key)
	if err != nil {
		return false, noTier, err
	}
	return p.Value != 0, tier, nil
}

// ResolveInt resolves an integer (power-level) permission and returns
// its Value. It is a convenience wrapper around Resolve for i_*
// permissions.
//
// Returns:
//   - value: the resolved power level (0 if negated).
//   - grantedBy: the tier that supplied the value, or noTier (-1) if unset.
//   - err: ErrPermissionNotSet if no tier has the key, nil otherwise.
func (r *Resolver) ResolveInt(tp TieredPermissions, key PermissionKey) (int, Tier, error) {
	p, tier, err := r.Resolve(tp, key)
	if err != nil {
		return 0, noTier, err
	}
	return p.Value, tier, nil
}

// IsGranted resolves the integer permission `key` and reports whether
// the resolved value is greater than or equal to `required`. This is
// the standard TS3 "power vs needed power" comparison used by checks
// such as join_power vs needed_join_power.
//
// Returns:
//   - ok: true if the resolved value >= required.
//   - grantedBy: the tier that supplied the value, or noTier (-1) if the
//     key was not set in any tier.
//   - err: ErrPermissionNotSet if no tier has the key, nil otherwise.
//
// Note: the actual pairing of a client's *_power against a channel's
// needed_*_power (e.g. reading needed_join_power from the channel and
// comparing it to the client's join_power) is implemented in
// Steps 261-280. IsGranted here only performs the numeric comparison
// against an externally-supplied required value.
func (r *Resolver) IsGranted(tp TieredPermissions, key PermissionKey, required int) (bool, Tier, error) {
	value, tier, err := r.ResolveInt(tp, key)
	if err != nil {
		return false, noTier, err
	}
	return value >= required, tier, nil
}
