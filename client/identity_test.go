package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// identityTestApp builds an App whose identity store and settings file both
// live under t.TempDir(). Nothing here may reach the developer's own
// <UserConfigDir>/voicx — an overwritten identity file is unrecoverable.
func identityTestApp(t *testing.T, protection string) *App {
	t.Helper()
	root := t.TempDir()
	oldRoot, oldProt := identityRootOverride, keyProtectionSetting
	identityRootOverride = root
	keyProtectionSetting = func() string { return protection }
	t.Cleanup(func() {
		identityRootOverride = oldRoot
		keyProtectionSetting = oldProt
	})
	return &App{
		settings:     DefaultSettings(),
		hotkeys:      map[string]*hotkeyReg{},
		settingsPath: filepath.Join(root, "settings.json"),
	}
}

// TestIdentityRoundTrip verifies first-run generation, persistence, stable
// reload, and a stable derived unique ID.
func TestIdentityRoundTrip(t *testing.T) {
	identityTestApp(t, "off")
	path := filepath.Join(t.TempDir(), "voicx", "identity.json")

	id1, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if id1.PublicKey == "" || id1.PrivateKey == "" {
		t.Fatal("generated identity has empty keys")
	}

	// File exists with restrictive content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("identity file not written: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("identity file is empty")
	}

	// Second load returns the SAME key pair.
	id2, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id1.PublicKey != id2.PublicKey || id1.PrivateKey != id2.PrivateKey {
		t.Fatal("reloaded identity differs from generated one")
	}

	// Unique ID derives and is stable.
	uid1, err := id1.uniqueID()
	if err != nil {
		t.Fatalf("uniqueID: %v", err)
	}
	uid2, err := id2.uniqueID()
	if err != nil {
		t.Fatalf("uniqueID (reloaded): %v", err)
	}
	if uid1 == "" || uid1 != uid2 {
		t.Fatalf("unique IDs differ: %q vs %q", uid1, uid2)
	}

	// A different path gets a DIFFERENT identity.
	id3, err := loadOrCreateIdentityAt(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatalf("second path generate: %v", err)
	}
	if id3.PublicKey == id1.PublicKey {
		t.Fatal("different paths produced the same identity")
	}
}

// TestIdentityKeyProtection covers both halves of 354: with protection on the
// stored private key is a DPAPI blob that still round-trips, and with it off
// the plaintext fallback keeps working.
func TestIdentityKeyProtection(t *testing.T) {
	for _, tc := range []struct{ mode string }{{"auto"}, {"off"}} {
		t.Run(tc.mode, func(t *testing.T) {
			identityTestApp(t, tc.mode)
			path := filepath.Join(t.TempDir(), "id.json")
			id, err := loadOrCreateIdentityAt(path)
			if err != nil {
				t.Fatalf("create: %v", err)
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var onDisk identity
			if err := json.Unmarshal(raw, &onDisk); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			wantProtected := tc.mode != "off" && keyProtectionAvailable()
			gotProtected := strings.HasPrefix(onDisk.PrivateKey, dpapiPrefix)
			if gotProtected != wantProtected {
				t.Fatalf("protected on disk = %v, want %v (protection=%q)", gotProtected, wantProtected, onDisk.Protection)
			}
			if wantProtected && strings.Contains(string(raw), "PRIVATE KEY") {
				t.Fatal("protected file still contains the plaintext PEM private key")
			}

			// The in-memory key is plaintext either way and survives a reload.
			back, err := loadOrCreateIdentityAt(path)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if back.PrivateKey != id.PrivateKey || back.PublicKey != id.PublicKey {
				t.Fatal("protected round trip lost the key pair")
			}
			if _, err := back.uniqueID(); err != nil {
				t.Fatalf("uniqueID after round trip: %v", err)
			}
		})
	}
}

// TestIdentityProtectedFromAnotherMachine verifies a key protected elsewhere
// fails with a clear message and, crucially, is NOT replaced by a fresh one
// (354): the file is intact, only unreadable here.
func TestIdentityProtectedFromAnotherMachine(t *testing.T) {
	identityTestApp(t, "auto")
	path := filepath.Join(t.TempDir(), "id.json")
	blob := dpapiPrefix + base64.StdEncoding.EncodeToString([]byte("not a real dpapi blob"))
	raw, err := json.Marshal(identity{PublicKey: "pub", PrivateKey: blob, Protection: protectionDPAPI})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = loadOrCreateIdentityAt(path)
	if err == nil {
		t.Fatal("an unreadable protected key loaded successfully")
	}
	if !errors.Is(err, errProtectedUnreadable) {
		t.Fatalf("error = %v, want errProtectedUnreadable", err)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil || string(after) != string(raw) {
		t.Fatal("an unreadable protected identity file was overwritten")
	}
}

// TestIdentityManagerLifecycle covers 351: create, list, switch, rename, and
// the refusal to delete the last identity.
func TestIdentityManagerLifecycle(t *testing.T) {
	a := identityTestApp(t, "off")

	list := a.ListIdentities()
	if len(list) != 1 || !list[0].Active {
		t.Fatalf("first run should materialise one active identity: %+v", list)
	}
	first := list[0]
	if first.UniqueID == "" || first.Path == "" {
		t.Fatalf("entry missing derived fields: %+v", first)
	}

	if err := a.CreateIdentity("Work Account"); err != "" {
		t.Fatalf("create: %s", err)
	}
	list = a.ListIdentities()
	if len(list) != 2 {
		t.Fatalf("want 2 identities, got %d", len(list))
	}
	var work IdentityEntry
	for _, e := range list {
		if e.ID != first.ID {
			work = e
		}
	}
	if work.ID != "work-account" {
		t.Fatalf("slug = %q, want work-account", work.ID)
	}
	if work.Active {
		t.Fatal("creating an identity must not switch to it")
	}
	if work.UniqueID == first.UniqueID {
		t.Fatal("new identity reused the existing key pair")
	}

	if err := a.SwitchIdentity(work.ID); err != "" {
		t.Fatalf("switch: %s", err)
	}
	if a.settings.ActiveIdentity != work.ID {
		t.Fatalf("active = %q, want %q", a.settings.ActiveIdentity, work.ID)
	}
	if loadSettingsAt(a.settingsPath).ActiveIdentity != work.ID {
		t.Fatal("switch did not persist active_identity")
	}
	// The active identity is what a fresh connect would pick up.
	_, path, err := a.resolveActive()
	if err != nil {
		t.Fatalf("resolveActive: %v", err)
	}
	active, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("load active: %v", err)
	}
	if uid, _ := active.uniqueID(); uid != work.UniqueID {
		t.Fatalf("active identity uid = %q, want %q", uid, work.UniqueID)
	}

	if err := a.RenameIdentity(work.ID, "Renamed"); err != "" {
		t.Fatalf("rename: %s", err)
	}
	for _, e := range a.ListIdentities() {
		if e.ID == work.ID && e.Name != "Renamed" {
			t.Fatalf("rename lost: %+v", e)
		}
	}

	if err := a.DeleteIdentity(work.ID); err != "" {
		t.Fatalf("delete: %s", err)
	}
	list = a.ListIdentities()
	if len(list) != 1 || list[0].ID != first.ID || !list[0].Active {
		t.Fatalf("deleting the active identity must fall back to a real one: %+v", list)
	}
	if err := a.DeleteIdentity(first.ID); err == "" {
		t.Fatal("deleting the last identity was allowed")
	}
}

// TestLegacyIdentityMigrates verifies the pre-351 single identity.json is
// adopted instead of a new key being minted — that key is the user's account
// on every server they have joined (351).
func TestLegacyIdentityMigrates(t *testing.T) {
	a := identityTestApp(t, "off")
	legacy, err := legacyIdentityPath()
	if err != nil {
		t.Fatalf("legacy path: %v", err)
	}
	original, err := loadOrCreateIdentityAt(legacy)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	wantUID, _ := original.uniqueID()

	list := a.ListIdentities()
	if len(list) != 1 || list[0].ID != defaultIdentityID {
		t.Fatalf("legacy identity not migrated: %+v", list)
	}
	if list[0].UniqueID != wantUID {
		t.Fatalf("migrated uid = %q, want %q", list[0].UniqueID, wantUID)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("migration removed the legacy identity file")
	}
}

// TestSecurityLevel covers 352: the level is deterministic and improving it
// raises it and persists the counter.
func TestSecurityLevel(t *testing.T) {
	a := identityTestApp(t, "off")

	if securityLevelOf("abc", 7) != securityLevelOf("abc", 7) {
		t.Fatal("security level is not deterministic")
	}
	if securityLevelOf("abc", 7) == securityLevelOf("abc", 8) &&
		securityLevelOf("abc", 9) == securityLevelOf("abc", 7) {
		t.Skip("degenerate hash sample")
	}

	id := a.ListIdentities()[0]
	res := a.ImproveIdentityLevel(id.ID, 8, 30)
	if res.Error != "" {
		t.Fatalf("improve: %s", res.Error)
	}
	if res.Level < 8 {
		t.Fatalf("level = %d, want >= 8", res.Level)
	}
	path, err := identityPathFor(id.ID)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	stored, err := loadOrCreateIdentityAt(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Counter != res.Counter || stored.SecurityLevel != res.Level {
		t.Fatalf("counter/level not persisted: %d/%d vs %d/%d",
			stored.Counter, stored.SecurityLevel, res.Counter, res.Level)
	}
	if securityLevel(stored.PublicKey, stored.Counter) != res.Level {
		t.Fatal("stored level does not match a recomputation")
	}
	if a.ListIdentities()[0].SecurityLevel != res.Level {
		t.Fatal("identity manager does not show the improved level")
	}
}

// TestIdentityBackupReminder covers 353: a fresh identity nags until it has
// actually been exported, and the marker is per identity.
func TestIdentityBackupReminder(t *testing.T) {
	a := identityTestApp(t, "off")
	entries := a.ListIdentities()
	if !a.IdentityBackupPending() {
		t.Fatal("a never-exported identity should be pending backup")
	}
	if entries[0].ExportedAt != "" {
		t.Fatalf("fresh identity claims a backup: %+v", entries[0])
	}

	src := entries[0].Path
	loaded, err := loadOrCreateIdentityAt(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "backup.json")
	if err := exportIdentityTo(dest, src, loaded); err != nil {
		t.Fatalf("export: %v", err)
	}
	if a.IdentityBackupPending() {
		t.Fatal("reminder still pending after an export")
	}

	// The export is portable: plaintext key, loadable on its own.
	backup, err := loadOrCreateIdentityAt(dest)
	if err != nil {
		t.Fatalf("reload export: %v", err)
	}
	if backup.PrivateKey != loaded.PrivateKey {
		t.Fatal("exported copy does not carry the usable private key")
	}
	rawBackup, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if strings.Contains(string(rawBackup), dpapiPrefix) {
		t.Fatal("export is machine-bound and would not restore elsewhere")
	}

	// A second identity carries its own marker.
	if err := a.CreateIdentity("second"); err != "" {
		t.Fatalf("create: %s", err)
	}
	if err := a.SwitchIdentity("second"); err != "" {
		t.Fatalf("switch: %s", err)
	}
	if !a.IdentityBackupPending() {
		t.Fatal("backup marker is global, not per identity")
	}
}

// TestImportIdentityAddsWithoutOverwriting verifies an import lands as a new
// identity and re-importing the same key selects it instead of duplicating
// (351).
func TestImportIdentityAddsWithoutOverwriting(t *testing.T) {
	a := identityTestApp(t, "off")
	before := a.ListIdentities()[0]

	external, err := newIdentity("external")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	external.CreatedAt = time.Now().Unix()
	if msg := a.adoptImportedIdentity(external, "external.json"); msg != "" {
		t.Fatalf("import: %s", msg)
	}
	list := a.ListIdentities()
	if len(list) != 2 {
		t.Fatalf("import should add an identity: %+v", list)
	}
	if a.settings.ActiveIdentity == before.ID {
		t.Fatal("import did not switch to the imported identity")
	}
	for _, e := range list {
		if e.ID == before.ID && e.UniqueID != before.UniqueID {
			t.Fatal("import overwrote the existing identity")
		}
	}

	again, err := newIdentity("copy")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	again.PublicKey, again.PrivateKey = external.PublicKey, external.PrivateKey
	if msg := a.adoptImportedIdentity(again, "copy.json"); msg != "" {
		t.Fatalf("re-import: %s", msg)
	}
	if got := len(a.ListIdentities()); got != 2 {
		t.Fatalf("re-importing the same key duplicated it: %d entries", got)
	}
}
