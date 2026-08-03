package permissions

import (
	"fmt"
	"sort"
)

// Conflict describes two effective inputs that disagree. Resolution remains
// deterministic; this diagnostic exists so an administrator can see a
// shadowed allow/deny instead of assuming both entries are active.
type Conflict struct {
	Key          PermissionKey
	WinningTier  Tier
	ShadowedTier Tier
	Message      string
}

// DetectConflicts statically inspects all tiers for contradictory values and
// suspicious talk-power combinations.
func DetectConflicts(tp TieredPermissions) []Conflict {
	var out []Conflict
	keys := map[PermissionKey]bool{}
	for _, tier := range tierOrder {
		if set, ok := tp.Get(tier); ok {
			for _, key := range set.Keys() {
				keys[key] = true
			}
		}
	}
	orderedKeys := make([]PermissionKey, 0, len(keys))
	for key := range keys {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Slice(orderedKeys, func(i, j int) bool { return orderedKeys[i] < orderedKeys[j] })
	for _, key := range orderedKeys {
		var winner *Permission
		var winnerTier Tier
		for _, tier := range tierOrder {
			set, ok := tp.Get(tier)
			if !ok {
				continue
			}
			entry, ok := set.Get(key)
			if !ok || entry == nil {
				continue
			}
			if winner == nil {
				winner, winnerTier = entry, tier
				continue
			}
			winnerValue := winner.Value
			if winner.Negate {
				winnerValue = 0
			}
			entryValue := entry.Value
			if entry.Negate {
				entryValue = 0
			}
			if winnerValue != entryValue || winner.Negate != entry.Negate {
				out = append(out, Conflict{
					Key: key, WinningTier: winnerTier, ShadowedTier: tier,
					Message: fmt.Sprintf("%s from %s shadows a contradictory value from %s", key, winnerTier, tier),
				})
			}
		}
	}
	if talk, talkTier, err := NewResolver().Resolve(tp, PermissionKeyClientTalkPower); err == nil && (talk.Negate || talk.Value < 0) {
		if request, requestTier, err := NewResolver().Resolve(tp, PermissionKeyClientRequestTalker); err == nil && !request.Negate && request.Value != 0 {
			out = append(out, Conflict{
				Key: PermissionKeyClientTalkPower, WinningTier: talkTier, ShadowedTier: requestTier,
				Message: "talk power is denied while request-talker is enabled",
			})
		}
	}
	return out
}
