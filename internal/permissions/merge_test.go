package permissions

import (
	"reflect"
	"testing"
)

// merge_test.go contains table-driven tests for the within-tier merge
// logic in merge.go. These tests are pure in-memory and do not touch
// the database, so they always run (no skip-when-no-DB guard).

// perm is a small helper that builds a *Permission with the given
// fields. It keeps the table-driven tests compact.
func perm(key PermissionKey, typ PermissionType, value, grant int, skip, negate bool) *Permission {
	return &Permission{
		Key:    key,
		Type:   typ,
		Value:  value,
		Grant:  grant,
		Skip:   skip,
		Negate: negate,
	}
}

// set builds a PermissionSet from the given entries.
func set(entries ...*Permission) PermissionSet {
	s := NewPermissionSet()
	for _, e := range entries {
		s.Set(e)
	}
	return s
}

func TestMergeSet_SingleSetPassesThrough(t *testing.T) {
	in := set(
		perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 50, 50, false, false),
		perm(PermissionKeyChannelDelete, PermissionTypeBoolean, 1, 0, false, false),
	)
	out := MergeSet(in)
	if len(out) != len(in) {
		t.Fatalf("expected %d entries, got %d", len(in), len(out))
	}
	for _, k := range in.Keys() {
		got, ok := out.Get(k)
		if !ok {
			t.Fatalf("missing key %q in merged set", k)
		}
		want := in[k]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("key %q: expected %+v, got %+v", k, want, got)
		}
	}
}

func TestMergeSet_DifferentKeysUnion(t *testing.T) {
	a := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 50, 50, false, false))
	b := set(perm(PermissionKeyChannelDelete, PermissionTypeBoolean, 1, 0, false, false))
	out := MergeSet(a, b)
	if !out.Has(PermissionKeyChannelJoinPower) || !out.Has(PermissionKeyChannelDelete) {
		t.Fatalf("expected union of keys; got keys %v", out.Keys())
	}
}

func TestMergeSet_SameKeyMaxValueWins(t *testing.T) {
	a := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 50, 50, false, false))
	b := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 75, 75, false, false))
	out := MergeSet(a, b)
	p, ok := out.Get(PermissionKeyChannelJoinPower)
	if !ok {
		t.Fatal("missing key")
	}
	if p.Value != 75 {
		t.Fatalf("expected max value 75, got %d", p.Value)
	}
	if p.Grant != 75 {
		t.Fatalf("expected max grant 75, got %d", p.Grant)
	}
	if p.Negate {
		t.Fatal("expected Negate=false")
	}
}

func TestMergeSet_NegateWinsOverValue(t *testing.T) {
	a := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 100, 100, false, false))
	b := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 0, 0, false, true))
	out := MergeSet(a, b)
	p, ok := out.Get(PermissionKeyChannelJoinPower)
	if !ok {
		t.Fatal("missing key")
	}
	if !p.Negate {
		t.Fatal("expected Negate=true")
	}
	if p.Value != 0 {
		t.Fatalf("expected Value=0 when negated, got %d", p.Value)
	}
}

func TestMergeSet_SkipIsORed(t *testing.T) {
	a := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 50, 50, false, false))
	b := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 60, 60, true, false))
	out := MergeSet(a, b)
	p, ok := out.Get(PermissionKeyChannelJoinPower)
	if !ok {
		t.Fatal("missing key")
	}
	if !p.Skip {
		t.Fatal("expected Skip=true when any contributor sets it")
	}
	if p.Value != 60 {
		t.Fatalf("expected max value 60, got %d", p.Value)
	}
}

func TestMergeSet_GrantMaxWins(t *testing.T) {
	a := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 50, 80, false, false))
	b := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 75, 30, false, false))
	out := MergeSet(a, b)
	p, ok := out.Get(PermissionKeyChannelJoinPower)
	if !ok {
		t.Fatal("missing key")
	}
	if p.Grant != 80 {
		t.Fatalf("expected max grant 80, got %d", p.Grant)
	}
	if p.Value != 75 {
		t.Fatalf("expected max value 75, got %d", p.Value)
	}
}

func TestMergeSet_BooleanAnyTrueWins(t *testing.T) {
	a := set(perm(PermissionKeyChannelDelete, PermissionTypeBoolean, 0, 0, false, false))
	b := set(perm(PermissionKeyChannelDelete, PermissionTypeBoolean, 1, 0, false, false))
	out := MergeSet(a, b)
	p, ok := out.Get(PermissionKeyChannelDelete)
	if !ok {
		t.Fatal("missing key")
	}
	if p.Value != 1 {
		t.Fatalf("expected boolean any-true → 1, got %d", p.Value)
	}
}

func TestMergeSet_IntegerHighestValueWins(t *testing.T) {
	a := set(perm(PermissionKeyClientKickFromServerPower, PermissionTypeInteger, 10, 10, false, false))
	b := set(perm(PermissionKeyClientKickFromServerPower, PermissionTypeInteger, 200, 200, false, false))
	c := set(perm(PermissionKeyClientKickFromServerPower, PermissionTypeInteger, 50, 50, false, false))
	out := MergeSet(a, b, c)
	p, ok := out.Get(PermissionKeyClientKickFromServerPower)
	if !ok {
		t.Fatal("missing key")
	}
	if p.Value != 200 {
		t.Fatalf("expected highest value 200, got %d", p.Value)
	}
}

func TestMergeSet_NegateAndSkipTogether(t *testing.T) {
	// One contributor negates, another sets Skip. The merged entry must
	// be Negate=true, Value=0, AND Skip=true (Skip is OR'd regardless of
	// Negate).
	a := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 100, 100, true, false))
	b := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 0, 0, false, true))
	out := MergeSet(a, b)
	p, ok := out.Get(PermissionKeyChannelJoinPower)
	if !ok {
		t.Fatal("missing key")
	}
	if !p.Negate {
		t.Fatal("expected Negate=true")
	}
	if !p.Skip {
		t.Fatal("expected Skip=true (OR'd even with Negate)")
	}
	if p.Value != 0 {
		t.Fatalf("expected Value=0 when negated, got %d", p.Value)
	}
}

func TestMergeSet_EmptyInputs(t *testing.T) {
	out := MergeSet()
	if out == nil {
		t.Fatal("expected non-nil empty set")
	}
	if len(out) != 0 {
		t.Fatalf("expected empty set, got %d entries", len(out))
	}
}

func TestMergeSet_NilSetsSkipped(t *testing.T) {
	a := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 50, 50, false, false))
	out := MergeSet(nil, a, nil)
	p, ok := out.Get(PermissionKeyChannelJoinPower)
	if !ok {
		t.Fatal("missing key")
	}
	if p.Value != 50 {
		t.Fatalf("expected value 50, got %d", p.Value)
	}
}

func TestMergeTiered_AssemblesAllTiers(t *testing.T) {
	groupA := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 50, 50, false, false))
	groupB := set(perm(PermissionKeyChannelJoinPower, PermissionTypeInteger, 75, 75, false, false))
	clientServer := set(perm(PermissionKeyChannelDelete, PermissionTypeBoolean, 1, 0, false, false))
	channelSpecific := set(perm(PermissionKeyClientTalkPower, PermissionTypeInteger, 30, 30, false, false))
	channelGroup := set(perm(PermissionKeyChannelSubscribePower, PermissionTypeInteger, 10, 10, false, false))
	channelClient := set(perm(PermissionKeyClientPoke, PermissionTypeBoolean, 1, 0, false, false))

	tp := MergeTiered(
		[]PermissionSet{groupA, groupB},
		[]PermissionSet{clientServer},
		[]PermissionSet{channelSpecific},
		[]PermissionSet{channelGroup},
		[]PermissionSet{channelClient},
	)

	// ServerGroup tier: merged max value 75.
	sg, ok := tp.Get(TierServerGroup)
	if !ok || sg == nil {
		t.Fatal("missing ServerGroup tier")
	}
	p, ok := sg.Get(PermissionKeyChannelJoinPower)
	if !ok || p.Value != 75 {
		t.Fatalf("ServerGroup merge: expected join_power 75, got %+v", p)
	}

	// ClientSpecific tier.
	cs, ok := tp.Get(TierClientSpecific)
	if !ok || !cs.Has(PermissionKeyChannelDelete) {
		t.Fatal("missing ClientSpecific tier entry")
	}

	// ChannelSpecific tier.
	chSpec, ok := tp.Get(TierChannelSpecific)
	if !ok || !chSpec.Has(PermissionKeyClientTalkPower) {
		t.Fatal("missing ChannelSpecific tier entry")
	}

	// ChannelGroup tier.
	cg, ok := tp.Get(TierChannelGroup)
	if !ok || !cg.Has(PermissionKeyChannelSubscribePower) {
		t.Fatal("missing ChannelGroup tier entry")
	}

	// ChannelClient tier.
	cc, ok := tp.Get(TierChannelClient)
	if !ok || !cc.Has(PermissionKeyClientPoke) {
		t.Fatal("missing ChannelClient tier entry")
	}
}

func TestMergeTiered_EmptyTiersPresent(t *testing.T) {
	tp := MergeTiered(nil, nil, nil, nil, nil)
	for _, tier := range []Tier{TierServerGroup, TierClientSpecific, TierChannelSpecific, TierChannelGroup, TierChannelClient} {
		s, ok := tp.Get(tier)
		if !ok {
			t.Fatalf("tier %s: expected present (even if empty)", tier)
		}
		if s == nil {
			t.Fatalf("tier %s: expected non-nil set", tier)
		}
		if len(s) != 0 {
			t.Fatalf("tier %s: expected empty set, got %d entries", tier, len(s))
		}
	}
}
