package permissions

import (
	"database/sql"
	"testing"
	"time"
)

type panicStoreBackend struct{}

func (panicStoreBackend) DB() *sql.DB {
	panic("database access on a fresh cache hit")
}

func TestLoaderReturnsFreshCachedPermissions(t *testing.T) {
	t.Parallel()

	loader := NewLoader(panicStoreBackend{}, nil)
	key := cacheKey{userID: 7, channelID: 11}
	want := NewTieredPermissions()
	set := NewPermissionSet()
	set.Set(&Permission{Key: PermissionKeyClientBan, Type: PermissionTypeBoolean, Value: 1})
	want.Set(TierClientSpecific, set)
	loader.cache.Store(key, cacheEntry{tp: want, expires: time.Now().Add(time.Hour)})

	got, err := loader.LoadForClient(t.Context(), key.userID, key.channelID)
	if err != nil {
		t.Fatalf("LoadForClient: %v", err)
	}
	gotSet, ok := got.Get(TierClientSpecific)
	if !ok {
		t.Fatal("cached result is missing client-specific tier")
	}
	permission, ok := gotSet.Get(PermissionKeyClientBan)
	if !ok || permission.Value != 1 {
		t.Fatalf("cached permission = %+v, want granted ban permission", permission)
	}
}

func TestLoaderInvalidationIsScoped(t *testing.T) {
	t.Parallel()

	loader := NewLoader(panicStoreBackend{}, nil)
	first := cacheKey{userID: 1, channelID: 10}
	second := cacheKey{userID: 2, channelID: 20}
	entry := cacheEntry{tp: NewTieredPermissions(), expires: time.Now().Add(time.Hour)}
	loader.cache.Store(first, entry)
	loader.cache.Store(second, entry)

	loader.Invalidate(first.userID, first.channelID)
	if _, ok := loader.cache.Load(first); ok {
		t.Fatal("Invalidate left the selected cache entry present")
	}
	if _, ok := loader.cache.Load(second); !ok {
		t.Fatal("Invalidate removed an unrelated cache entry")
	}

	loader.InvalidateAll()
	if _, ok := loader.cache.Load(second); ok {
		t.Fatal("InvalidateAll left a cache entry present")
	}
}

func TestInferTypeDefaultsUnknownKeysToInteger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want PermissionType
	}{
		{name: "boolean", key: "b_client_ban", want: PermissionTypeBoolean},
		{name: "integer", key: "i_client_ban_power", want: PermissionTypeInteger},
		{name: "unknown prefix fails safe", key: "custom_permission", want: PermissionTypeInteger},
		{name: "empty key fails safe", want: PermissionTypeInteger},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := inferType(test.key); got != test.want {
				t.Errorf("inferType(%q) = %v, want %v", test.key, got, test.want)
			}
		})
	}
}
