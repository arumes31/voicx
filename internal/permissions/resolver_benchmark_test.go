package permissions

import (
	"errors"
	"fmt"
	"testing"
)

func benchmarkTieredPermissions(keysPerTier int, targetTier Tier, includeTarget bool) TieredPermissions {
	tp := NewTieredPermissions()
	for tierIndex, tier := range tierOrder {
		set := NewPermissionSet()
		for keyIndex := 0; keyIndex < keysPerTier; keyIndex++ {
			key := PermissionKey(fmt.Sprintf("i_benchmark_resolve_%d_%04d", tierIndex, keyIndex))
			set.Set(&Permission{Key: key, Type: PermissionTypeInteger, Value: keyIndex})
		}
		if includeTarget && tier == targetTier {
			set.Set(&Permission{Key: PermissionKeyChannelJoinPower, Type: PermissionTypeInteger, Value: 75})
		}
		tp.Set(tier, set)
	}
	return tp
}

func BenchmarkResolverResolve(b *testing.B) {
	for _, test := range []struct {
		name          string
		keysPerTier   int
		targetTier    Tier
		includeTarget bool
		wantErr       bool
	}{
		{name: "small_first_tier_hit", keysPerTier: 1, targetTier: TierServerGroup, includeTarget: true},
		{name: "medium_last_tier_hit", keysPerTier: 64, targetTier: TierChannelClient, includeTarget: true},
		{name: "large_dense_miss", keysPerTier: 256, targetTier: noTier, wantErr: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			tp := benchmarkTieredPermissions(test.keysPerTier, test.targetTier, test.includeTarget)
			resolver := NewResolver()
			b.ReportAllocs()

			var (
				permission *Permission
				tier       Tier
				err        error
			)
			for b.Loop() {
				permission, tier, err = resolver.Resolve(tp, PermissionKeyChannelJoinPower)
			}
			if test.wantErr {
				if !errors.Is(err, ErrPermissionNotSet) || permission != nil || tier != noTier {
					b.Fatalf("Resolve miss = (%+v, %v, %v)", permission, tier, err)
				}
				return
			}
			if err != nil || permission == nil || permission.Value != 75 || tier != test.targetTier {
				b.Fatalf("Resolve hit = (%+v, %v, %v)", permission, tier, err)
			}
		})
	}
}
