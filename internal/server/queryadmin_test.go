package server

import (
	"context"
	"errors"
	"testing"

	"voicx/internal/config"
	"voicx/internal/permissions"
)

func TestPermOverviewResolvesTierPrecedenceAndLoadArguments(t *testing.T) {
	tp := permissions.NewTieredPermissions()
	key := permissions.PermissionKey("i_test_power")
	high := permissions.NewPermissionSet()
	high.Set(&permissions.Permission{Key: key, Type: permissions.PermissionTypeInteger, Value: 4, Grant: 9})
	low := permissions.NewPermissionSet()
	low.Set(&permissions.Permission{Key: key, Type: permissions.PermissionTypeInteger, Value: 99, Grant: 99})
	tp.Set(permissions.TierServerGroup, high)
	tp.Set(permissions.TierChannel, low)
	var loadedUser, loadedChannel int64
	env := startTestEnvDeps(t, &tp, nil, func(deps *Deps) {
		deps.Perms.(*fakePerms).loadForClientFn = func(_ context.Context, userID, channelID int64) (permissions.TieredPermissions, error) {
			loadedUser, loadedChannel = userID, channelID
			return tp, nil
		}
	})
	defer env.stop()
	env.auth.users["user-uid"].IsAdmin = true

	got, admin, err := env.srv.PermOverview(context.Background(), "user-uid", 42)
	if err != nil {
		t.Fatalf("PermOverview: %v", err)
	}
	if loadedUser != 2 || loadedChannel != 42 {
		t.Fatalf("LoadForClient arguments = (%d, %d), want (2, 42)", loadedUser, loadedChannel)
	}
	if !admin {
		t.Fatal("PermOverview did not return the authoritative admin flag")
	}
	if len(got) != 1 || got[0].Key != string(key) || got[0].Value != 4 || got[0].Grant != 9 || got[0].Tier != "server_group" {
		t.Fatalf("PermOverview = %+v, want server-group winning permission", got)
	}
}

func TestPermOverviewReturnsLookupAndLoadErrors(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	if _, _, err := env.srv.PermOverview(context.Background(), "missing", 0); err == nil {
		t.Fatal("PermOverview succeeded for a missing user")
	}
	loadErr := errors.New("injected permission load failure")
	env.perms.loadForClientFn = func(context.Context, int64, int64) (permissions.TieredPermissions, error) {
		return permissions.TieredPermissions{}, loadErr
	}
	if _, _, err := env.srv.PermOverview(context.Background(), "user-uid", 0); !errors.Is(err, loadErr) {
		t.Fatalf("PermOverview load error = %v, want injected error", err)
	}
}

func TestEffectiveMaxClientsUsesOnlyValidNonNegativeOverride(t *testing.T) {
	env := startTestEnvFull(t, nil, func(cfg *config.Config) { cfg.MaxClients = 9 })
	defer env.stop()
	ctx := context.Background()
	if got := env.srv.EffectiveMaxClients(ctx); got != 9 {
		t.Fatalf("default max clients = %d, want 9", got)
	}
	for _, test := range []struct {
		value string
		want  int
	}{
		{value: "24", want: 24},
		{value: "-1", want: 9},
		{value: "not-a-number", want: 9},
	} {
		t.Run(test.value, func(t *testing.T) {
			if err := env.chat.SetServerSetting(ctx, "max_clients_override", test.value, 0); err != nil {
				t.Fatalf("set override: %v", err)
			}
			if got := env.srv.EffectiveMaxClients(ctx); got != test.want {
				t.Fatalf("EffectiveMaxClients(%q) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}
