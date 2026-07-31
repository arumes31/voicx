package permissions

// merge.go implements the WITHIN-TIER merge of multiple PermissionSets
// according to TeamSpeak 3 semantics. This is the logic that runs when a
// single client contributes several entries to the SAME tier — most
// commonly because the client belongs to multiple server groups (each
// group contributes a PermissionSet to the ServerGroup tier) or, less
// commonly, multiple channel groups on the same channel.
//
// ---------------------------------------------------------------------
// WHY A MERGE IS NEEDED WITHIN A TIER
// ---------------------------------------------------------------------
//
// The five-tier resolver (see resolver.go and tiers.go) treats each tier
// as a single PermissionSet. But the raw data model lets a client have
// several sources for one tier:
//
//   - ServerGroup tier: one PermissionSet per server group the client
//     belongs to. A client in groups "Guest", "Normal" and "Admin"
//     contributes three sets to the ServerGroup tier.
//   - ChannelGroup tier: TS3 normally allows only one channel group per
//     client per channel, but the data model permits several rows and
//     the merge must still be well-defined.
//
// Before the resolver can run, those per-source sets must be collapsed
// into ONE PermissionSet per tier. That collapse is MergeSet.
//
// ---------------------------------------------------------------------
// THE MERGE ALGORITHM (TS3 semantics)
// ---------------------------------------------------------------------
//
// For each permission key that appears in ANY contributing set, the
// merged entry is computed from ALL contributing entries for that key
// as follows:
//
//  1. NEGATE WINS OVER EVERYTHING WITHIN A TIER.
//     If ANY contributing entry has Negate == true, the merged entry has
//     Negate == true and Value == 0. Rationale: TS3 treats Negate as an
//     explicit, hard denial. Within a single tier there is no notion of
//     "a higher group overrides a lower group's negate" — all groups a
//     client belongs to apply simultaneously, so a single negating
//     group forbids the permission for the client at this tier. The
//     Value is forced to 0 so that even if the resolver falls through to
//     this entry, the effective value is "denied".
//
//  2. OTHERWISE, VALUE IS THE MAXIMUM.
//     The merged Value is max(value) across all contributing entries.
//     - For integer (i_*) permissions this means the highest power
//       wins: a client in a group granting join_power 50 and a group
//       granting join_power 75 effectively has join_power 75.
//     - For boolean (b_*) permissions any non-zero (true) entry makes
//       the merged value 1, because max(0,1)=1. This matches TS3's
//       "granted by any group" behavior.
//     Rationale: TS3 grants are additive/optimistic within a tier — the
//     most permissive contributing value is what the client gets, unless
//     something explicitly negates.
//
//  3. GRANT IS THE MAXIMUM.
//     The merged Grant is max(grant) across all contributing entries.
//     Grant represents how much permission-modification power the
//     client wields for this key. The most granting power wins for the
//     same optimistic-additive reason as Value.
//
//  4. SKIP IS OR'd.
//     The merged Skip is true if ANY contributing entry has Skip == true.
//     Rationale: Skip means "do not let lower tiers override this key".
//     If even one contributing group says "skip lower tiers for this
//     key", the merged tier as a whole should skip lower tiers — because
//     that group's intent is to lock the key at this tier. OR-ing is the
//     conservative choice: it respects the strongest skip request and
//     never silently drops a skip. (Negate already forces Value=0, so
//     Skip+Negate together produce a denied entry that also blocks
//     lower tiers from un-denying it — exactly TS3's behavior.)
//
//  5. TYPE is taken from the first contributing entry for the key.
//     All entries for a given key share the same type (boolean vs
//     integer) by convention; if they disagree we keep the first seen,
//     which is deterministic given the ordered iteration over the input
//     sets.
//
// The result is a NEW PermissionSet; the inputs are not mutated.
//
// ---------------------------------------------------------------------
// IMPORTANT: THIS IS WITHIN-TIER ONLY
// ---------------------------------------------------------------------
//
// MergeSet does NOT implement cross-tier precedence. Cross-tier
// precedence (highest tier with the key wins, Negate denies, Skip
// blocks lower tiers) is the resolver's job — see resolver.go. MergeSet
// only collapses multiple sources for ONE tier into one set.

// MergeSet merges several PermissionSets that all belong to the SAME
// tier into a single PermissionSet, following the TS3 within-tier
// rules documented above.
//
// The inputs are not mutated. A nil or empty input slice yields an
// empty (non-nil) PermissionSet. Nil individual sets are skipped.
func MergeSet(sets ...PermissionSet) PermissionSet {
	merged := NewPermissionSet()
	if len(sets) == 0 {
		return merged
	}

	// Collect, per key, every contributing entry. We use a slice rather
	// than overwriting so we can apply the max/negate/skip rules across
	// ALL contributors. The map is keyed by PermissionKey.
	type contrib struct {
		entries []*Permission
	}
	byKey := make(map[PermissionKey]*contrib)

	for _, set := range sets {
		if set == nil {
			continue
		}
		for key, p := range set {
			if p == nil {
				continue
			}
			c, ok := byKey[key]
			if !ok {
				c = &contrib{}
				byKey[key] = c
			}
			c.entries = append(c.entries, p)
		}
	}

	for key, c := range byKey {
		merged.Set(mergeEntries(key, c.entries))
	}
	return merged
}

// mergeEntries applies the within-tier rules to a single key's
// contributing entries and returns the merged *Permission. The caller
// must guarantee entries is non-empty and contains no nil pointers.
func mergeEntries(key PermissionKey, entries []*Permission) *Permission {
	// Seed from the first entry so we inherit Type deterministically.
	m := &Permission{
		Key:    key,
		Type:   entries[0].Type,
		Value:  entries[0].Value,
		Grant:  entries[0].Grant,
		Skip:   entries[0].Skip,
		Negate: entries[0].Negate,
	}

	for _, e := range entries[1:] {
		// Negate wins: as soon as ANY contributor negates, the merged
		// entry is negated and its value forced to 0. We still continue
		// iterating so that Skip (OR'd) and Grant (max) are computed
		// from every contributor, matching the documented algorithm.
		if e.Negate {
			m.Negate = true
		}
		if e.Skip {
			m.Skip = true
		}
		if e.Value > m.Value {
			m.Value = e.Value
		}
		if e.Grant > m.Grant {
			m.Grant = e.Grant
		}
	}

	// If any contributor negated, force Value to 0 regardless of the
	// max computed above. This is the "Negate wins over everything"
	// rule made concrete in the stored value.
	if m.Negate {
		m.Value = 0
	}

	return m
}

// MergeTiered is a convenience that merges each tier's slice of
// PermissionSets and assembles a TieredPermissions holding one merged
// set per tier.
//
// Parameters are the raw per-source sets for each tier:
//
//   - groupSets           → ServerGroup tier (one set per server group)
//   - clientServerSets    → ClientSpecific tier (server-wide client perms;
//     in practice at most one set, but a slice is accepted for symmetry)
//   - channelSpecificSets → ChannelSpecific tier (channel-scoped client
//     perms for the target channel; at most one set in practice)
//   - channelGroupSets    → ChannelGroup tier (one set per channel group)
//   - channelClientSets   → ChannelClient tier (channel_client_permissions
//     for the target channel; at most one set in practice)
//
// Empty/nil slices produce an empty set for that tier (the tier is
// still present in the result with an empty PermissionSet, which the
// resolver treats as "no entries"). The returned TieredPermissions is
// ready to be passed to Resolver.Resolve.
func MergeTiered(
	groupSets []PermissionSet,
	clientServerSets []PermissionSet,
	channelSpecificSets []PermissionSet,
	channelGroupSets []PermissionSet,
	channelClientSets []PermissionSet,
) TieredPermissions {
	tp := NewTieredPermissions()
	tp.Set(TierServerGroup, MergeSet(groupSets...))
	tp.Set(TierClientSpecific, MergeSet(clientServerSets...))
	tp.Set(TierChannelSpecific, MergeSet(channelSpecificSets...))
	tp.Set(TierChannelGroup, MergeSet(channelGroupSets...))
	tp.Set(TierChannelClient, MergeSet(channelClientSets...))
	return tp
}
