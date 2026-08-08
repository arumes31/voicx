package permissions

import "testing"

func TestDetectConflictsFlagsDeniedTalkWithRequestEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		talk       *Permission
		request    *Permission
		wantCount  int
		wantWinner Tier
	}{
		{
			name:       "negated talk",
			talk:       &Permission{Key: PermissionKeyClientTalkPower, Type: PermissionTypeInteger, Value: 100, Negate: true},
			request:    &Permission{Key: PermissionKeyClientRequestTalker, Type: PermissionTypeBoolean, Value: 1},
			wantCount:  1,
			wantWinner: TierServerGroup,
		},
		{
			name:       "negative talk power",
			talk:       &Permission{Key: PermissionKeyClientTalkPower, Type: PermissionTypeInteger, Value: -1},
			request:    &Permission{Key: PermissionKeyClientRequestTalker, Type: PermissionTypeBoolean, Value: 1},
			wantCount:  1,
			wantWinner: TierServerGroup,
		},
		{
			name:      "request is also denied",
			talk:      &Permission{Key: PermissionKeyClientTalkPower, Type: PermissionTypeInteger, Negate: true},
			request:   &Permission{Key: PermissionKeyClientRequestTalker, Type: PermissionTypeBoolean, Value: 1, Negate: true},
			wantCount: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			tp := NewTieredPermissions()
			talkSet := NewPermissionSet()
			talkSet.Set(test.talk)
			tp.Set(TierServerGroup, talkSet)
			requestSet := NewPermissionSet()
			requestSet.Set(test.request)
			tp.Set(TierChannelGroup, requestSet)

			conflicts := DetectConflicts(tp)
			if len(conflicts) != test.wantCount {
				t.Fatalf("DetectConflicts() = %+v, want %d conflicts", conflicts, test.wantCount)
			}
			if test.wantCount > 0 && (conflicts[0].Key != PermissionKeyClientTalkPower || conflicts[0].WinningTier != test.wantWinner) {
				t.Errorf("conflict = %+v, want talk-power conflict won by %s", conflicts[0], test.wantWinner)
			}
		})
	}
}

func TestDetectConflictsDistinguishesExplicitNegation(t *testing.T) {
	t.Parallel()

	tp := NewTieredPermissions()
	winner := NewPermissionSet()
	winner.Set(&Permission{Key: PermissionKeyClientBan, Type: PermissionTypeBoolean, Value: 0})
	tp.Set(TierServerGroup, winner)
	shadowed := NewPermissionSet()
	shadowed.Set(&Permission{Key: PermissionKeyClientBan, Type: PermissionTypeBoolean, Value: 1, Negate: true})
	tp.Set(TierChannelClient, shadowed)

	conflicts := DetectConflicts(tp)
	if len(conflicts) != 1 {
		t.Fatalf("DetectConflicts() = %+v, want one explicit-negation conflict", conflicts)
	}
	if conflicts[0].WinningTier != TierServerGroup || conflicts[0].ShadowedTier != TierChannelClient {
		t.Errorf("conflict tiers = %s -> %s, want server_group -> channel_client", conflicts[0].WinningTier, conflicts[0].ShadowedTier)
	}
}
