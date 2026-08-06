// identity.go implements the client's Ed25519 identity (TS3 model: the
// identity IS the key pair) and the multiple-identities manager (351).
// Identities live one file per identity in <UserConfigDir>/voicx/identities/
// with 0600 permissions; settings.ActiveIdentity records which one is
// current. The legacy single <UserConfigDir>/voicx/identity.json is migrated
// in on first use.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/nacl/box"

	"voicx/internal/auth"
)

// identity is the client's key material, PEM/base64-encoded: the Ed25519
// signing pair (the TS3-style identity) plus an X25519 pair for E2EE chat
// (wave 4b). The X25519 fields generate automatically on first load of an
// older identity file.
type identity struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
	// X25519 encryption pair (base64, 32 bytes each) for E2EE direct
	// messages and unsealing channel chat keys.
	X25519Public  string `json:"x25519_public,omitempty"`
	X25519Private string `json:"x25519_private,omitempty"`

	// (351) per-identity metadata. The file is the unit of backup, so the
	// label, the backup marker and the proof-of-work travel with the key.
	Name      string `json:"name,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"` // unix
	// (353) unix time of the last export; 0 means this key has never been
	// backed up and losing the machine loses the account on every server.
	ExportedAt int64 `json:"exported_at,omitempty"`
	// (352) TS3-style proof of work over the public key: Counter is the
	// nonce, SecurityLevel the leading zero bits it reaches.
	Counter       uint64 `json:"security_counter,omitempty"`
	SecurityLevel int    `json:"security_level,omitempty"`
	// (354) how PrivateKey is stored on disk: "" = plaintext, "dpapi" = the
	// file's private_key carries a dpapiPrefix blob. In memory PrivateKey is
	// always plaintext.
	Protection string `json:"protection,omitempty"`
}

const (
	// dpapiPrefix marks a private_key value as a DPAPI blob (354). It sits in
	// the same field so an older client still sees a non-empty private key
	// and reports a bad key instead of an empty identity.
	dpapiPrefix = "dpapi:v1:"
	// protectionDPAPI is the Protection value for a DPAPI-protected file.
	protectionDPAPI = "dpapi"
	// defaultIdentityID is the file stem the legacy identity migrates into.
	defaultIdentityID = "default"
)

// errProtectedUnreadable is returned when a DPAPI-protected key cannot be
// unlocked here — the usual cause is the file being copied from another
// machine or Windows account (354). It must never lead to regenerating the
// key: the file is intact, only unreadable on this box.
var errProtectedUnreadable = errors.New("identity key is protected by " + keyProtectionName +
	" and cannot be unlocked here (it was protected on another machine or Windows account) — " +
	"import a plaintext export of it instead")

// x25519 returns the identity's X25519 key pair, decoding the base64 fields.
func (id *identity) x25519() (pub, priv [32]byte, err error) {
	pubRaw, err := base64.StdEncoding.DecodeString(id.X25519Public)
	if err != nil || len(pubRaw) != 32 {
		return pub, priv, fmt.Errorf("invalid x25519 public key in identity")
	}
	privRaw, err := base64.StdEncoding.DecodeString(id.X25519Private)
	if err != nil || len(privRaw) != 32 {
		return pub, priv, fmt.Errorf("invalid x25519 private key in identity")
	}
	copy(pub[:], pubRaw)
	copy(priv[:], privRaw)
	return pub, priv, nil
}

// uniqueID derives the client's unique ID from its public key (stable across
// runs, TS3-style).
func (id *identity) uniqueID() (string, error) {
	return auth.UniqueIDFromPublicKey(id.PublicKey)
}

// --- storage layout (351) ------------------------------------------------------

// identityRootOverride redirects the identity store away from the real
// <UserConfigDir>/voicx. Tests set it so no test can create, switch or delete
// an identity in the developer's own config directory.
var identityRootOverride string

// identityRoot returns the directory holding the identity store.
func identityRoot() (string, error) {
	if identityRootOverride != "" {
		return identityRootOverride, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "voicx"), nil
}

// identitiesDir returns <root>/identities.
func identitiesDir() (string, error) {
	root, err := identityRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "identities"), nil
}

// legacyIdentityPath returns the pre-351 single identity file.
func legacyIdentityPath() (string, error) {
	root, err := identityRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "identity.json"), nil
}

// identityFileLocation validates that path names exactly one identity JSON
// file. Callers choose the parent store (the application config directory in
// production and a temporary directory in tests); os.Root then confines the
// operation to that store.
func identityFileLocation(path string) (dir, name string, err error) {
	path = filepath.Clean(path)
	name = filepath.Base(path)
	if filepath.Ext(name) != ".json" {
		return "", "", errors.New("identity path must name a .json file")
	}
	if err := validateIdentityID(strings.TrimSuffix(name, ".json")); err != nil {
		return "", "", err
	}
	return filepath.Dir(path), name, nil
}

func readIdentityAt(path string) ([]byte, error) {
	dir, name, err := identityFileLocation(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(name)
}

func writeIdentityAt(path string, raw []byte) error {
	dir, name, err := identityFileLocation(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(name, raw, 0o600)
}

// identityIDs lists the identity file stems in dir, sorted.
func identityIDs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if validateIdentityID(id) != nil {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// migrateLegacyIdentity copies the pre-351 identity.json into the per-identity
// store when the store is empty. The original is left in place on purpose:
// that key IS the user's account on every server they ever joined, so the
// migration must not be able to destroy it.
func migrateLegacyIdentity(dir string) {
	if len(identityIDs(dir)) > 0 {
		return
	}
	legacy, err := legacyIdentityPath()
	if err != nil {
		return
	}
	raw, err := readIdentityAt(legacy)
	if err != nil || len(raw) == 0 {
		return
	}
	if err := writeIdentityAt(filepath.Join(dir, defaultIdentityID+".json"), raw); err != nil {
		log.Printf("migrating legacy identity: %v", err)
		return
	}
	log.Printf("migrated legacy identity.json into identities/%s.json", defaultIdentityID)
}

// identityIDFromName turns a display name into a stable, unique file stem.
func identityIDFromName(dir, name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 40 {
		base = base[:40]
	}
	if base == "" {
		base = "identity"
	}
	taken := map[string]bool{}
	for _, id := range identityIDs(dir) {
		taken[id] = true
	}
	candidate := base
	for i := 2; taken[candidate]; i++ {
		candidate = base + "-" + strconv.Itoa(i)
	}
	return candidate
}

// identityPathFor returns the file backing an identity ID.
func identityPathFor(id string) (string, error) {
	if err := validateIdentityID(id); err != nil {
		return "", err
	}
	dir, err := identitiesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// validateIdentityID accepts only the file-stem character set emitted by
// identityIDFromName. In particular, path separators and traversal tokens are
// never valid identity IDs.
func validateIdentityID(id string) error {
	if id == "" {
		return errors.New("identity ID is required")
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return errors.New("invalid identity ID")
		}
	}
	return nil
}

// resolveActiveIn returns the active identity's ID and file path for a store
// directory, migrating the legacy file in first. A stale want falls back to an
// identity that exists rather than minting a new key, which would silently
// hand the user a different account on every server (351).
func resolveActiveIn(dir, want string) (string, string) {
	migrateLegacyIdentity(dir)
	ids := identityIDs(dir)
	for _, id := range ids {
		if id == want {
			return id, filepath.Join(dir, id+".json")
		}
	}
	if len(ids) > 0 {
		return ids[0], filepath.Join(dir, ids[0]+".json")
	}
	return defaultIdentityID, filepath.Join(dir, defaultIdentityID+".json")
}

// resolveActiveIdentity resolves the active identity with no App in hand:
// connManager.identity() runs on the read loop, so the setting has to be
// reachable straight from the settings file.
func resolveActiveIdentity() (string, string, error) {
	dir, err := identitiesDir()
	if err != nil {
		return "", "", err
	}
	id, path := resolveActiveIn(dir, loadSettings().ActiveIdentity)
	return id, path, nil
}

// resolveActive is resolveActiveIdentity against the App's in-memory setting,
// which is the authoritative one while the client runs.
func (a *App) resolveActive() (string, string, error) {
	dir, err := identitiesDir()
	if err != nil {
		return "", "", err
	}
	id, path := resolveActiveIn(dir, a.settings.ActiveIdentity)
	return id, path, nil
}

// identityPath returns the ACTIVE identity's file location.
func identityPath() (string, error) {
	_, path, err := resolveActiveIdentity()
	return path, err
}

// loadOrCreateIdentity loads the active identity, generating and persisting a
// new key pair on first run.
func loadOrCreateIdentity() (*identity, error) {
	path, err := identityPath()
	if err != nil {
		return nil, err
	}
	return loadOrCreateIdentityAt(path)
}

// decodeIdentity parses an identity file, unwrapping a DPAPI-protected
// private key (354) so the in-memory key is always plaintext.
func decodeIdentity(raw []byte) (*identity, error) {
	var id identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("not a valid identity file: %w", err)
	}
	if id.PublicKey == "" || id.PrivateKey == "" {
		return nil, errors.New("not a valid identity file: missing key material")
	}
	if strings.HasPrefix(id.PrivateKey, dpapiPrefix) {
		blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(id.PrivateKey, dpapiPrefix))
		if err != nil {
			return nil, fmt.Errorf("%w (%v)", errProtectedUnreadable, err)
		}
		plain, err := unprotectBytes(blob)
		if err != nil {
			return nil, fmt.Errorf("%w (%v)", errProtectedUnreadable, err)
		}
		id.PrivateKey = string(plain)
		id.Protection = protectionDPAPI
	} else {
		id.Protection = ""
	}
	return &id, nil
}

// loadOrCreateIdentityAt is loadOrCreateIdentity with an explicit path
// (tests). Older identity files without X25519 fields are upgraded in place.
// An existing but undecodable file is an ERROR, never a regeneration: the key
// is the user's account everywhere and overwriting it is unrecoverable.
func loadOrCreateIdentityAt(path string) (*identity, error) {
	data, err := readIdentityAt(path)
	if err == nil && len(data) > 0 {
		id, derr := decodeIdentity(data)
		if derr != nil {
			return nil, derr
		}
		if id.upgrade(strings.TrimSuffix(filepath.Base(path), ".json")) {
			if err := saveIdentityAt(path, id); err != nil {
				return nil, err
			}
		}
		return id, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	id, err := newIdentity(strings.TrimSuffix(filepath.Base(path), ".json"))
	if err != nil {
		return nil, err
	}
	if err := saveIdentityAt(path, id); err != nil {
		return nil, err
	}
	return id, nil
}

// newIdentity generates a fresh key pair labelled name.
func newIdentity(name string) (*identity, error) {
	pubPEM, privPEM, err := auth.GenerateIdentityKeyPair()
	if err != nil {
		return nil, err
	}
	id := &identity{PublicKey: pubPEM, PrivateKey: privPEM, Name: name, CreatedAt: time.Now().Unix()}
	if err := id.generateX25519(); err != nil {
		return nil, err
	}
	id.SecurityLevel = securityLevel(id.PublicKey, id.Counter)
	return id, nil
}

// upgrade fills in fields a file written by an older client lacks. It reports
// whether anything changed and the file needs rewriting.
func (id *identity) upgrade(fallbackName string) bool {
	changed := false
	if id.X25519Public == "" || id.X25519Private == "" {
		if err := id.generateX25519(); err == nil {
			changed = true
		}
	}
	if id.Name == "" {
		id.Name = fallbackName
		changed = true
	}
	if id.CreatedAt == 0 {
		id.CreatedAt = time.Now().Unix()
		changed = true
	}
	// (352) recompute rather than trust the stored level: a hand-edited or
	// imported file must not be able to claim a level it did not earn.
	if lvl := securityLevel(id.PublicKey, id.Counter); lvl != id.SecurityLevel {
		id.SecurityLevel = lvl
		changed = true
	}
	return changed
}

// generateX25519 creates a fresh X25519 pair for E2EE chat.
func (id *identity) generateX25519() error {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	id.X25519Public = base64.StdEncoding.EncodeToString(pub[:])
	id.X25519Private = base64.StdEncoding.EncodeToString(priv[:])
	return nil
}

// keyProtectionSetting resolves settings.IdentityKeyProtection. It is a
// variable so tests pin the mode instead of inheriting whatever the developer
// has configured.
var keyProtectionSetting = func() string { return loadSettings().IdentityKeyProtection }

// keyProtectionWanted reports whether the identity key should be protected at
// rest (354). "" means auto, so a settings file that predates the option keeps
// the protected default without a migration.
func keyProtectionWanted() bool { return keyProtectionSetting() != "off" }

// saveIdentityAt persists the identity with owner-only permissions, protecting
// the private key when DPAPI is available and wanted (354). A protection
// failure falls back to the plaintext file — refusing to write would cost the
// user a key they can never recover.
func saveIdentityAt(path string, id *identity) error {
	out := *id
	out.Protection = ""
	if keyProtectionWanted() && keyProtectionAvailable() {
		if blob, err := protectBytes([]byte(id.PrivateKey)); err == nil {
			out.PrivateKey = dpapiPrefix + base64.StdEncoding.EncodeToString(blob)
			out.Protection = protectionDPAPI
		} else {
			log.Printf("key protection unavailable, storing identity in plaintext: %v", err)
		}
	}
	// #nosec G117 -- an identity file is intentionally a serialized keypair;
	// it is owner-only and the private key is platform-protected when enabled.
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	if err := writeIdentityAt(path, raw); err != nil {
		return err
	}
	id.Protection = out.Protection
	return nil
}

// exportIdentityTo writes a PORTABLE copy: the private key in the clear, so
// the backup still opens on a new machine (353/354). It also stamps the
// backup marker on the live file so the reminder stops.
func exportIdentityTo(dest, src string, id *identity) error {
	out := *id
	out.Protection = ""
	out.ExportedAt = time.Now().Unix()
	// #nosec G117 -- a portable identity backup must contain its private key;
	// the native save-dialog destination is written with owner-only permissions.
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		return err
	}
	id.ExportedAt = out.ExportedAt
	return saveIdentityAt(src, id)
}

// --- security level (352) ------------------------------------------------------

// securityLevel returns the TS3-style proof-of-work level of a public key:
// the number of leading zero bits of sha256(base64(rawPublicKey) + counter).
func securityLevel(publicKeyPEM string, counter uint64) int {
	pub, err := auth.LoadPublicKey(publicKeyPEM)
	if err != nil {
		return 0
	}
	return securityLevelOf(base64.StdEncoding.EncodeToString(pub), counter)
}

// securityLevelOf is securityLevel over the already-encoded public key.
func securityLevelOf(pubB64 string, counter uint64) int {
	sum := sha256.Sum256([]byte(pubB64 + strconv.FormatUint(counter, 10)))
	bits := 0
	for _, b := range sum {
		if b != 0 {
			for mask := byte(0x80); mask > 0 && b&mask == 0; mask >>= 1 {
				bits++
			}
			break
		}
		bits += 8
	}
	return bits
}

// improveSecurityLevel searches for a counter reaching target, giving up at
// the deadline. It returns the best counter/level found (never worse than the
// one it started from).
func improveSecurityLevel(id *identity, target int, deadline time.Time) (uint64, int) {
	pub, err := auth.LoadPublicKey(id.PublicKey)
	if err != nil {
		return id.Counter, id.SecurityLevel
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	bestCounter, bestLevel := id.Counter, securityLevelOf(pubB64, id.Counter)
	for counter := id.Counter + 1; bestLevel < target; counter++ {
		if lvl := securityLevelOf(pubB64, counter); lvl > bestLevel {
			bestCounter, bestLevel = counter, lvl
		}
		// The deadline check is amortised: hashing is fast enough that
		// checking the clock every iteration would dominate the loop.
		if counter%20000 == 0 && time.Now().After(deadline) {
			break
		}
	}
	return bestCounter, bestLevel
}

// --- Wails bindings (351/352/353/354) ------------------------------------------

// IdentityEntry is one row of the identity manager (351).
type IdentityEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	UniqueID      string `json:"unique_id"`
	Active        bool   `json:"active"`
	CreatedAt     string `json:"created_at,omitempty"`
	ExportedAt    string `json:"exported_at,omitempty"` // (353) "" = never backed up
	SecurityLevel int    `json:"security_level"`        // (352)
	Protection    string `json:"protection"`            // (354) "dpapi" | "plaintext"
	Path          string `json:"path"`
	Error         string `json:"error,omitempty"` // (354) unreadable protected file
}

// IdentityLevelResult reports the outcome of a security-level search (352).
type IdentityLevelResult struct {
	Level   int    `json:"level"`
	Counter uint64 `json:"counter"`
	Error   string `json:"error,omitempty"`
}

// entryFor builds the manager row for one identity file.
func entryFor(dir, id, activeID string) IdentityEntry {
	path := filepath.Join(dir, id+".json")
	e := IdentityEntry{ID: id, Name: id, Path: path, Active: id == activeID, Protection: "plaintext"}
	raw, err := readIdentityAt(path)
	if err != nil {
		e.Error = err.Error()
		return e
	}
	// The header is readable even when the key is not, so a protected file
	// from another machine still shows up as a row instead of vanishing (354).
	var head identity
	if json.Unmarshal(raw, &head) == nil {
		if head.Name != "" {
			e.Name = head.Name
		}
		if head.CreatedAt > 0 {
			e.CreatedAt = time.Unix(head.CreatedAt, 0).Format("2006-01-02 15:04:05")
		}
		if head.ExportedAt > 0 {
			e.ExportedAt = time.Unix(head.ExportedAt, 0).Format("2006-01-02 15:04:05")
		}
		if strings.HasPrefix(head.PrivateKey, dpapiPrefix) {
			e.Protection = protectionDPAPI
		}
		if uid, err := auth.UniqueIDFromPublicKey(head.PublicKey); err == nil {
			e.UniqueID = uid
		}
	}
	loaded, err := decodeIdentity(raw)
	if err != nil {
		e.Error = err.Error()
		return e
	}
	e.SecurityLevel = securityLevel(loaded.PublicKey, loaded.Counter)
	return e
}

// ListIdentities returns every stored identity (351), migrating the legacy
// single identity file in on first call.
func (a *App) ListIdentities() []IdentityEntry {
	dir, err := identitiesDir()
	if err != nil {
		return []IdentityEntry{}
	}
	activeID, activePath, err := a.resolveActive()
	if err != nil {
		return []IdentityEntry{}
	}
	ids := identityIDs(dir)
	if len(ids) == 0 {
		// First run: materialise the active identity so the manager is never
		// empty and the unique ID shown matches the one servers will see.
		if _, err := loadOrCreateIdentityAt(activePath); err != nil {
			return []IdentityEntry{}
		}
		ids = identityIDs(dir)
	}
	out := make([]IdentityEntry, 0, len(ids))
	for _, id := range ids {
		out = append(out, entryFor(dir, id, activeID))
	}
	return out
}

// forgetCachedIdentity drops the connManager's cached key so the next connect
// picks up the new active identity.
func (a *App) forgetCachedIdentity(id *identity) {
	cm := a.cmLoad()
	if cm == nil {
		return
	}
	cm.mu.Lock()
	cm.id = id
	cm.mu.Unlock()
}

// CreateIdentity generates a new identity labelled name WITHOUT switching to
// it (351). It returns "" on success or the failure reason.
func (a *App) CreateIdentity(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "name is required"
	}
	dir, err := identitiesDir()
	if err != nil {
		return err.Error()
	}
	migrateLegacyIdentity(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err.Error()
	}
	id, err := newIdentity(name)
	if err != nil {
		return err.Error()
	}
	if err := saveIdentityAt(filepath.Join(dir, identityIDFromName(dir, name)+".json"), id); err != nil {
		return err.Error()
	}
	return ""
}

// adoptImportedIdentity stores an already-decoded imported key as a new
// identity and switches to it (351). Re-importing a key that is already
// stored just selects it instead of creating a duplicate account row.
func (a *App) adoptImportedIdentity(id *identity, fallbackName string) string {
	dir, err := identitiesDir()
	if err != nil {
		return err.Error()
	}
	migrateLegacyIdentity(dir)
	for _, existing := range identityIDs(dir) {
		if e := entryFor(dir, existing, ""); e.UniqueID != "" {
			if uid, err := id.uniqueID(); err == nil && uid == e.UniqueID {
				return a.SwitchIdentity(existing)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err.Error()
	}
	if id.Name == "" {
		id.Name = strings.TrimSuffix(fallbackName, ".json")
	}
	if id.CreatedAt == 0 {
		id.CreatedAt = time.Now().Unix()
	}
	// (353) an imported key demonstrably exists in a second place already, so
	// it does not need the backup reminder.
	if id.ExportedAt == 0 {
		id.ExportedAt = time.Now().Unix()
	}
	if id.X25519Public == "" || id.X25519Private == "" {
		if err := id.generateX25519(); err != nil {
			return err.Error()
		}
	}
	id.SecurityLevel = securityLevel(id.PublicKey, id.Counter)
	newID := identityIDFromName(dir, id.Name)
	if err := saveIdentityAt(filepath.Join(dir, newID+".json"), id); err != nil {
		return err.Error()
	}
	return a.SwitchIdentity(newID)
}

// SwitchIdentity makes id the active identity (351). Live connections keep
// the key they authenticated with; the change applies on the next connect.
func (a *App) SwitchIdentity(id string) string {
	path, err := identityPathFor(id)
	if err != nil {
		return err.Error()
	}
	loaded, err := loadOrCreateIdentityAt(path)
	if err != nil {
		return err.Error()
	}
	a.settings.ActiveIdentity = id
	if err := a.save(); err != nil {
		return err.Error()
	}
	a.forgetCachedIdentity(loaded)
	a.emitSettingsUpdate()
	return ""
}

// RenameIdentity changes an identity's display label (351). The file stem is
// the stable ID and does not move.
func (a *App) RenameIdentity(id, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "name is required"
	}
	path, err := identityPathFor(id)
	if err != nil {
		return err.Error()
	}
	loaded, err := loadOrCreateIdentityAt(path)
	if err != nil {
		return err.Error()
	}
	loaded.Name = name
	if err := saveIdentityAt(path, loaded); err != nil {
		return err.Error()
	}
	return ""
}

// DeleteIdentity removes an identity file (351). The last identity cannot be
// deleted, and an identity that has never been exported needs the caller to
// confirm: the key is the user's account on every server that has seen it.
func (a *App) DeleteIdentity(id string, confirmUnexported bool) string {
	path, err := identityPathFor(id)
	if err != nil {
		return err.Error()
	}
	dir := filepath.Dir(path)
	ids := identityIDs(dir)
	if len(ids) <= 1 {
		return "the last identity cannot be deleted"
	}
	if _, err := os.Stat(path); err != nil {
		return "no such identity"
	}
	loaded, err := loadOrCreateIdentityAt(path)
	if err != nil {
		return err.Error()
	}
	if loaded.ExportedAt == 0 && !confirmUnexported {
		return "identity has never been exported; explicit confirmation is required"
	}
	if err := os.Remove(path); err != nil {
		return err.Error()
	}
	if a.settings.ActiveIdentity == id || a.settings.ActiveIdentity == "" {
		newActive, _, err := a.resolveActive()
		if err != nil {
			return err.Error()
		}
		a.settings.ActiveIdentity = newActive
		a.forgetCachedIdentity(nil)
	}
	if err := a.save(); err != nil {
		return err.Error()
	}
	a.emitSettingsUpdate()
	return ""
}

// ImproveIdentityLevel searches for a counter that raises the identity's
// security level (352), giving up after maxSeconds. The counter is persisted
// so the level survives a restart.
func (a *App) ImproveIdentityLevel(id string, target, maxSeconds int) IdentityLevelResult {
	if target < 1 || target > 40 {
		return IdentityLevelResult{Error: "target level must be 1..40"}
	}
	if maxSeconds < 1 || maxSeconds > 300 {
		maxSeconds = 30
	}
	path, err := identityPathFor(id)
	if err != nil {
		return IdentityLevelResult{Error: err.Error()}
	}
	loaded, err := loadOrCreateIdentityAt(path)
	if err != nil {
		return IdentityLevelResult{Error: err.Error()}
	}
	counter, level := improveSecurityLevel(loaded, target, time.Now().Add(time.Duration(maxSeconds)*time.Second))
	if level > loaded.SecurityLevel {
		loaded.Counter, loaded.SecurityLevel = counter, level
		if err := saveIdentityAt(path, loaded); err != nil {
			return IdentityLevelResult{Error: err.Error()}
		}
		a.forgetCachedIdentity(nil)
	}
	return IdentityLevelResult{Level: loaded.SecurityLevel, Counter: loaded.Counter}
}

// IdentityBackupPending reports whether the ACTIVE identity has never been
// exported (353). An unexported key dies with the machine, so the frontend
// nags until this turns false.
func (a *App) IdentityBackupPending() bool {
	_, path, err := a.resolveActive()
	if err != nil {
		return false
	}
	raw, err := readIdentityAt(path)
	if err != nil {
		return false // nothing generated yet: nothing to lose
	}
	var head identity
	if json.Unmarshal(raw, &head) != nil {
		return false
	}
	return head.ExportedAt == 0
}
