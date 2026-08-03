package permissions

import (
	"errors"
	"testing"
	"testing/quick"
)

// TestResolverAllTierCombinations property-checks every presence/value/flag
// shape generated across all six tiers. The first populated tier must always
// win, and a negate must always reduce that winner to zero.
func TestResolverAllTierCombinations(t *testing.T) {
	property := func(present, values, flags uint8) bool {
		key := PermissionKeyClientTalkPower
		tp := NewTieredPermissions()
		var want *Permission
		var wantTier Tier
		for i, tier := range tierOrder {
			if present&(1<<i) == 0 {
				continue
			}
			entry := &Permission{Key: key, Type: PermissionTypeInteger, Value: int((values >> i) & 1), Negate: flags&(1<<i) != 0}
			set := NewPermissionSet()
			set.Set(entry)
			tp.Set(tier, set)
			if want == nil {
				copy := *entry
				if copy.Negate {
					copy.Value = 0
				}
				want, wantTier = &copy, tier
			}
		}
		got, gotTier, err := NewResolver().Resolve(tp, key)
		if want == nil {
			return got == nil && errors.Is(err, ErrPermissionNotSet)
		}
		return err == nil && gotTier == wantTier && got.Value == want.Value && got.Negate == want.Negate
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 10_000}); err != nil {
		t.Fatal(err)
	}
}

func TestDetectConflictingOverride(t *testing.T) {
	tp := NewTieredPermissions()
	for tier, value := range map[Tier]int{TierServerGroup: 1, TierChannelClient: 0} {
		set := NewPermissionSet()
		set.Set(&Permission{Key: PermissionKeyClientVideoPublish, Type: PermissionTypeBoolean, Value: value})
		tp.Set(tier, set)
	}
	if conflicts := DetectConflicts(tp); len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", conflicts)
	}
}

func TestDetectConflictsIsSorted(t *testing.T) {
	tp := NewTieredPermissions()
	server, channel := NewPermissionSet(), NewPermissionSet()
	for _, key := range []PermissionKey{"z_test_permission", "a_test_permission"} {
		server.Set(&Permission{Key: key, Type: PermissionTypeBoolean, Value: 1})
		channel.Set(&Permission{Key: key, Type: PermissionTypeBoolean, Value: 0})
	}
	tp.Set(TierServerGroup, server)
	tp.Set(TierChannelClient, channel)
	conflicts := DetectConflicts(tp)
	if len(conflicts) != 2 || conflicts[0].Key != "a_test_permission" || conflicts[1].Key != "z_test_permission" {
		t.Fatalf("conflicts are not sorted: %+v", conflicts)
	}
}
