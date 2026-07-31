package permissions

import (
	"errors"
	"testing"
)

// newPerm is a small test helper that builds a *Permission with sensible
// defaults. Tests override only the fields they care about.
func newPerm(key PermissionKey, typ PermissionType, value int) *Permission {
	return &Permission{
		Key:   key,
		Type:  typ,
		Value: value,
	}
}

// setOf builds a PermissionSet from the given entries.
func setOf(perms ...*Permission) PermissionSet {
	s := NewPermissionSet()
	for _, p := range perms {
		s.Set(p)
	}
	return s
}

// TestResolve_TableDriven covers the core tier-precedence rules of the
// resolver. Each case constructs a TieredPermissions, resolves a single
// key, and asserts on the returned value, grantedBy tier, and error.
func TestResolve_TableDriven(t *testing.T) {
	const intKey = PermissionKeyClientTalkPower
	const boolKey = PermissionKeyChannelDelete

	cases := []struct {
		name       string
		key        PermissionKey
		tp         TieredPermissions
		wantValue  int
		wantTier   Tier
		wantErr    error
		wantNegate bool
	}{
		{
			name: "set only in ServerGroup returns that value",
			key:  intKey,
			tp: func() TieredPermissions {
				tp := NewTieredPermissions()
				tp.Set(TierServerGroup, setOf(newPerm(intKey, PermissionTypeInteger, 50)))
				return tp
			}(),
			wantValue: 50,
			wantTier:  TierServerGroup,
		},
		{
			name: "set in ServerGroup and ChannelGroup returns ServerGroup value",
			key:  intKey,
			tp: func() TieredPermissions {
				tp := NewTieredPermissions()
				tp.Set(TierServerGroup, setOf(newPerm(intKey, PermissionTypeInteger, 50)))
				tp.Set(TierChannelGroup, setOf(newPerm(intKey, PermissionTypeInteger, 99)))
				return tp
			}(),
			wantValue: 50,
			wantTier:  TierServerGroup,
		},
		{
			name: "set only in ChannelClient returns that value",
			key:  intKey,
			tp: func() TieredPermissions {
				tp := NewTieredPermissions()
				tp.Set(TierChannelClient, setOf(newPerm(intKey, PermissionTypeInteger, 7)))
				return tp
			}(),
			wantValue: 7,
			wantTier:  TierChannelClient,
		},
		{
			name: "negate in higher tier denies regardless of lower tiers",
			key:  boolKey,
			tp: func() TieredPermissions {
				tp := NewTieredPermissions()
				tp.Set(TierServerGroup, setOf(&Permission{
					Key:    boolKey,
					Type:   PermissionTypeBoolean,
					Value:  1,
					Negate: true,
				}))
				tp.Set(TierChannelGroup, setOf(newPerm(boolKey, PermissionTypeBoolean, 1)))
				return tp
			}(),
			wantValue:  0,
			wantTier:   TierServerGroup,
			wantNegate: true,
		},
		{
			name: "skip in ServerGroup returns ServerGroup value (lower tiers not consulted)",
			key:  intKey,
			tp: func() TieredPermissions {
				tp := NewTieredPermissions()
				tp.Set(TierServerGroup, setOf(&Permission{
					Key:   intKey,
					Type:  PermissionTypeInteger,
					Value: 42,
					Skip:  true,
				}))
				tp.Set(TierChannelSpecific, setOf(newPerm(intKey, PermissionTypeInteger, 100)))
				return tp
			}(),
			wantValue: 42,
			wantTier:  TierServerGroup,
		},
		{
			name:     "no permission set returns ErrPermissionNotSet",
			key:      intKey,
			tp:       NewTieredPermissions(),
			wantErr:  ErrPermissionNotSet,
			wantTier: noTier,
		},
		{
			name: "ClientSpecific wins over ChannelGroup",
			key:  intKey,
			tp: func() TieredPermissions {
				tp := NewTieredPermissions()
				tp.Set(TierClientSpecific, setOf(newPerm(intKey, PermissionTypeInteger, 30)))
				tp.Set(TierChannelGroup, setOf(newPerm(intKey, PermissionTypeInteger, 60)))
				return tp
			}(),
			wantValue: 30,
			wantTier:  TierClientSpecific,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := NewResolver()
			got, tier, err := r.Resolve(tc.tp, tc.key)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Resolve err = %v, want %v", err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("Resolve returned non-nil permission for error case: %+v", got)
				}
				if tier != tc.wantTier {
					t.Fatalf("Resolve grantedBy = %v, want %v", tier, tc.wantTier)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve unexpected err = %v", err)
			}
			if got == nil {
				t.Fatalf("Resolve returned nil permission")
			}
			if got.Value != tc.wantValue {
				t.Errorf("Resolve value = %d, want %d", got.Value, tc.wantValue)
			}
			if tier != tc.wantTier {
				t.Errorf("Resolve grantedBy = %v, want %v", tier, tc.wantTier)
			}
			if got.Negate != tc.wantNegate {
				t.Errorf("Resolve negate = %v, want %v", got.Negate, tc.wantNegate)
			}
		})
	}
}

// TestResolveBool covers the boolean convenience wrapper.
func TestResolveBool(t *testing.T) {
	r := NewResolver()
	const key = PermissionKeyChannelDelete

	t.Run("granted when value is 1", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierServerGroup, setOf(newPerm(key, PermissionTypeBoolean, 1)))
		ok, tier, err := r.ResolveBool(tp, key)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !ok {
			t.Fatal("expected granted=true")
		}
		if tier != TierServerGroup {
			t.Errorf("tier = %v, want %v", tier, TierServerGroup)
		}
	})

	t.Run("denied when value is 0", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierServerGroup, setOf(newPerm(key, PermissionTypeBoolean, 0)))
		ok, _, err := r.ResolveBool(tp, key)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if ok {
			t.Fatal("expected granted=false")
		}
	})

	t.Run("negated entry yields granted=false and no error", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierServerGroup, setOf(&Permission{
			Key:    key,
			Type:   PermissionTypeBoolean,
			Value:  1,
			Negate: true,
		}))
		ok, _, err := r.ResolveBool(tp, key)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if ok {
			t.Fatal("expected granted=false for negated entry")
		}
	})

	t.Run("unset returns ErrPermissionNotSet", func(t *testing.T) {
		ok, tier, err := r.ResolveBool(NewTieredPermissions(), key)
		if !errors.Is(err, ErrPermissionNotSet) {
			t.Fatalf("err = %v, want ErrPermissionNotSet", err)
		}
		if ok {
			t.Fatal("expected granted=false when unset")
		}
		if tier != noTier {
			t.Errorf("tier = %v, want noTier", tier)
		}
	})
}

// TestResolveInt covers the integer convenience wrapper.
func TestResolveInt(t *testing.T) {
	r := NewResolver()
	const key = PermissionKeyClientTalkPower

	t.Run("returns resolved power level", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierChannelGroup, setOf(newPerm(key, PermissionTypeInteger, 75)))
		v, tier, err := r.ResolveInt(tp, key)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if v != 75 {
			t.Errorf("value = %d, want 75", v)
		}
		if tier != TierChannelGroup {
			t.Errorf("tier = %v, want %v", tier, TierChannelGroup)
		}
	})

	t.Run("negated entry yields 0 and no error", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierServerGroup, setOf(&Permission{
			Key:    key,
			Type:   PermissionTypeInteger,
			Value:  100,
			Negate: true,
		}))
		v, _, err := r.ResolveInt(tp, key)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if v != 0 {
			t.Errorf("value = %d, want 0 for negated entry", v)
		}
	})

	t.Run("unset returns ErrPermissionNotSet", func(t *testing.T) {
		v, tier, err := r.ResolveInt(NewTieredPermissions(), key)
		if !errors.Is(err, ErrPermissionNotSet) {
			t.Fatalf("err = %v, want ErrPermissionNotSet", err)
		}
		if v != 0 {
			t.Errorf("value = %d, want 0 when unset", v)
		}
		if tier != noTier {
			t.Errorf("tier = %v, want noTier", tier)
		}
	})
}

// TestIsGranted covers the power-vs-needed comparison helper.
func TestIsGranted(t *testing.T) {
	r := NewResolver()
	const key = PermissionKeyClientTalkPower

	t.Run("value >= required returns true", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierServerGroup, setOf(newPerm(key, PermissionTypeInteger, 50)))
		ok, tier, err := r.IsGranted(tp, key, 50)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !ok {
			t.Fatal("expected IsGranted=true for value == required")
		}
		if tier != TierServerGroup {
			t.Errorf("tier = %v, want %v", tier, TierServerGroup)
		}
	})

	t.Run("value > required returns true", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierServerGroup, setOf(newPerm(key, PermissionTypeInteger, 75)))
		ok, _, err := r.IsGranted(tp, key, 50)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !ok {
			t.Fatal("expected IsGranted=true for value > required")
		}
	})

	t.Run("value < required returns false", func(t *testing.T) {
		tp := NewTieredPermissions()
		tp.Set(TierServerGroup, setOf(newPerm(key, PermissionTypeInteger, 25)))
		ok, _, err := r.IsGranted(tp, key, 50)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if ok {
			t.Fatal("expected IsGranted=false for value < required")
		}
	})

	t.Run("unset returns ErrPermissionNotSet", func(t *testing.T) {
		ok, tier, err := r.IsGranted(NewTieredPermissions(), key, 1)
		if !errors.Is(err, ErrPermissionNotSet) {
			t.Fatalf("err = %v, want ErrPermissionNotSet", err)
		}
		if ok {
			t.Fatal("expected IsGranted=false when unset")
		}
		if tier != noTier {
			t.Errorf("tier = %v, want noTier", tier)
		}
	})
}

// TestPermissionSet covers the PermissionSet helper methods.
func TestPermissionSet(t *testing.T) {
	const k1 = PermissionKey("i_test_one")
	const k2 = PermissionKey("i_test_two")

	t.Run("Set/Get/Has", func(t *testing.T) {
		s := NewPermissionSet()
		if s.Has(k1) {
			t.Fatal("empty set should not have k1")
		}
		s.Set(newPerm(k1, PermissionTypeInteger, 10))
		if !s.Has(k1) {
			t.Fatal("set should have k1 after Set")
		}
		p, ok := s.Get(k1)
		if !ok || p == nil {
			t.Fatal("Get should return the entry")
		}
		if p.Value != 10 {
			t.Errorf("value = %d, want 10", p.Value)
		}
	})

	t.Run("Keys returns sorted keys", func(t *testing.T) {
		s := NewPermissionSet()
		s.Set(newPerm(k2, PermissionTypeInteger, 2))
		s.Set(newPerm(k1, PermissionTypeInteger, 1))
		keys := s.Keys()
		if len(keys) != 2 {
			t.Fatalf("len(keys) = %d, want 2", len(keys))
		}
		if keys[0] != k1 || keys[1] != k2 {
			t.Errorf("keys = %v, want [%s, %s] (sorted)", keys, k1, k2)
		}
	})

	t.Run("Set panics on nil", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on nil Set")
			}
		}()
		s := NewPermissionSet()
		s.Set(nil)
	})
}

// TestTierString ensures the String method covers all tiers.
func TestTierString(t *testing.T) {
	cases := []struct {
		tier Tier
		want string
	}{
		{TierServerGroup, "server_group"},
		{TierClientSpecific, "client_specific"},
		{TierChannelSpecific, "channel_specific"},
		{TierChannelGroup, "channel_group"},
		{TierChannelClient, "channel_client"},
		{Tier(99), "unknown"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.tier.String(); got != tc.want {
				t.Errorf("tier.String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveDoesNotMutateSource verifies the resolver returns a defensive
// copy, so callers cannot accidentally mutate the source PermissionSet.
func TestResolveDoesNotMutateSource(t *testing.T) {
	r := NewResolver()
	const key = PermissionKeyClientTalkPower

	tp := NewTieredPermissions()
	tp.Set(TierServerGroup, setOf(newPerm(key, PermissionTypeInteger, 50)))

	got, _, err := r.Resolve(tp, key)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got.Value = 999 // mutate the returned copy

	src, _ := tp.Get(TierServerGroup)
	srcPerm, _ := src.Get(key)
	if srcPerm.Value != 50 {
		t.Errorf("source value mutated to %d, want 50", srcPerm.Value)
	}
}
