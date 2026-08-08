package permissions

import (
	"fmt"
	"testing"
)

func benchmarkPermissionSets(sourceCount, keyCount int) []PermissionSet {
	sets := make([]PermissionSet, sourceCount)
	keys := make([]PermissionKey, keyCount)
	for keyIndex := range keys {
		keys[keyIndex] = PermissionKey(fmt.Sprintf("i_benchmark_merge_%04d", keyIndex))
	}
	for source := range sets {
		set := NewPermissionSet()
		for keyIndex, key := range keys {
			set.Set(&Permission{
				Key:    key,
				Type:   PermissionTypeInteger,
				Value:  source + keyIndex,
				Grant:  source * 2,
				Skip:   source%7 == 0,
				Negate: source == sourceCount-1 && keyIndex%31 == 0,
			})
		}
		sets[source] = set
	}
	return sets
}

func BenchmarkMergeSet(b *testing.B) {
	for _, test := range []struct {
		name        string
		sourceCount int
		keyCount    int
	}{
		{name: "small_1_source_8_keys", sourceCount: 1, keyCount: 8},
		{name: "medium_4_sources_64_keys", sourceCount: 4, keyCount: 64},
		{name: "large_16_sources_256_keys", sourceCount: 16, keyCount: 256},
	} {
		b.Run(test.name, func(b *testing.B) {
			sets := benchmarkPermissionSets(test.sourceCount, test.keyCount)
			b.ReportAllocs()

			var merged PermissionSet
			for b.Loop() {
				merged = MergeSet(sets...)
			}
			if len(merged) != test.keyCount {
				b.Fatalf("merged key count = %d, want %d", len(merged), test.keyCount)
			}
		})
	}
}
