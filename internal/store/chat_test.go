// chat_test.go DB-backed tests for ciphertext-at-rest (91-135, migration
// 012). They follow the repository's skip pattern: without a reachable
// Postgres they skip. The tests that need a pre-012 database create a scratch
// one (see store_test.go), because the shared dev database is already
// migrated and cannot prove what a migration does on upgrade.
package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

func TestReactionCacheExpiresAndIsBounded(t *testing.T) {
	s := &Store{}
	s.cacheReactions(1, map[string]int{"👍": 1})
	value, ok := s.reactionCache.Load(int64(1))
	if !ok {
		t.Fatal("reaction cache entry missing")
	}
	value.(*reactionCacheEntry).expiresAt.Store(time.Now().Add(-time.Second).UnixNano())
	if _, ok := s.cachedReactions(1); ok {
		t.Fatal("expired reaction cache entry returned")
	}
	for id := int64(1); id <= maxReactionCacheEntries+1; id++ {
		s.cacheReactions(id, map[string]int{})
	}
	if s.reactionCacheSize > maxReactionCacheEntries {
		t.Fatalf("reaction cache size = %d, max %d", s.reactionCacheSize, maxReactionCacheEntries)
	}
}

// testScopeKey is deterministic per fill byte so a test can seal and open
// without a key manager.
func testScopeKey(fill byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = fill + byte(i)
	}
	return k
}

// sealTest produces the exact at-rest form the pipeline stores:
// base64(nonce[24] || secretbox(plain, scopeKey)).
func sealTest(t *testing.T, key [32]byte, plain string) string {
	t.Helper()
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return base64.StdEncoding.EncodeToString(secretbox.Seal(nonce[:24:24], []byte(plain), &nonce, &key))
}

// openTest reverses sealTest.
func openTest(t *testing.T, key [32]byte, blob string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		t.Fatalf("body_enc is not base64: %v", err)
	}
	if len(raw) <= 24 {
		t.Fatalf("body_enc is %d bytes, too short to be sealed", len(raw))
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	out, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		t.Fatal("body_enc did not open under the scope key")
	}
	return string(out)
}

// testChatScope returns a channel id no real channel uses, so a test never
// collides with dev data or another test.
func testChatScope() int64 { return time.Now().UnixNano() }

// TestChatMessageRowsHoldNoPlaintext is the at-rest guarantee: after every
// write path (store, edit, delete) the plaintext column is empty and the row
// holds only ciphertext the database cannot read.
func TestChatMessageRowsHoldNoPlaintext(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	canary := "canary-7f3a-" + fmt.Sprint(time.Now().UnixNano())
	scope := testChatScope()
	key := testScopeKey(11)
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM chat_messages WHERE channel_id = $1`, scope)
	})

	id, _, err := s.StoreChatMessage(ctx, scope, "u1", "nick", sealTest(t, key, canary), 3, 0, "")
	if err != nil {
		t.Fatalf("StoreChatMessage: %v", err)
	}

	read := func() (body, bodyEnc string, keyID int64) {
		t.Helper()
		if err := s.DB().QueryRowContext(ctx,
			`SELECT body, body_enc, key_id FROM chat_messages WHERE id = $1`, id).Scan(&body, &bodyEnc, &keyID); err != nil {
			t.Fatalf("reading row: %v", err)
		}
		return
	}

	body, bodyEnc, keyID := read()
	if body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
	if strings.Contains(bodyEnc, canary) {
		t.Fatal("body_enc holds the canary in the clear")
	}
	if keyID != 3 {
		t.Fatalf("key_id = %d, want 3", keyID)
	}
	if got := openTest(t, key, bodyEnc); got != canary {
		t.Fatalf("stored ciphertext opens to %q, want the canary", got)
	}

	// Reads return the ciphertext verbatim; there is no plaintext field to
	// return at all.
	msgs, err := s.ChatHistory(ctx, scope, 0, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ChatHistory = %d rows, err=%v", len(msgs), err)
	}
	if msgs[0].BodyEnc != bodyEnc || msgs[0].KeyID != 3 {
		t.Fatalf("history row = (%q, %d), want the stored ciphertext", msgs[0].BodyEnc, msgs[0].KeyID)
	}
	got, err := s.GetChatMessage(ctx, id)
	if err != nil || got == nil || got.BodyEnc != bodyEnc || got.KeyID != 3 {
		t.Fatalf("GetChatMessage = %+v, err=%v", got, err)
	}

	// An edit re-seals under a possibly newer generation and never writes back
	// into the legacy column.
	edited := sealTest(t, key, canary+"-edited")
	if _, err := s.EditChatMessage(ctx, id, edited, 4, 0); err != nil {
		t.Fatalf("EditChatMessage: %v", err)
	}
	body, bodyEnc, keyID = read()
	if body != "" || keyID != 4 {
		t.Fatalf("after edit: body = %q, key_id = %d", body, keyID)
	}
	if openTest(t, key, bodyEnc) != canary+"-edited" {
		t.Fatal("edited ciphertext does not open to the new text")
	}

	// A tombstone keeps nothing at all.
	if err := s.DeleteChatMessage(ctx, id); err != nil {
		t.Fatalf("DeleteChatMessage: %v", err)
	}
	body, bodyEnc, keyID = read()
	if body != "" || bodyEnc != "" || keyID != 0 {
		t.Fatalf("tombstone = (%q, %q, %d), want everything cleared", body, bodyEnc, keyID)
	}
}

// TestEditRejectedOnLegacyPlaintextRow pins the ordering hazard: an upgraded
// deployment whose backfill has not reached a row yet must still be editable.
// The UPDATE has to blank body itself, because the CHECK is evaluated against
// every updated row, not only the columns the statement names.
func TestEditRejectedOnLegacyPlaintextRow(t *testing.T) {
	s := testScratchStore(t)
	ctx := context.Background()
	applyPreLedgerMigrations(t, s, "012")

	var id int64
	if err := s.DB().QueryRowContext(ctx,
		`INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body)
		 VALUES (1, 42, 'u1', 'nick', 'legacy plaintext') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seeding legacy row: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	key := testScopeKey(21)
	if _, err := s.EditChatMessage(ctx, id, sealTest(t, key, "edited"), 1, 0); err != nil {
		t.Fatalf("editing a not-yet-backfilled row: %v", err)
	}
	var body, bodyEnc string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT body, body_enc FROM chat_messages WHERE id = $1`, id).Scan(&body, &bodyEnc); err != nil {
		t.Fatalf("reading row: %v", err)
	}
	if body != "" {
		t.Fatalf("body = %q after edit, want the legacy plaintext gone", body)
	}
	if openTest(t, key, bodyEnc) != "edited" {
		t.Fatal("edited row does not open to the new text")
	}
}

// TestPlaintextWriteRejectedByConstraint is the production half of the
// invariant: code that writes a plaintext body fails at the database, not
// only in CI.
func TestPlaintextWriteRejectedByConstraint(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	scope := testChatScope()
	key := testScopeKey(31)
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM chat_messages WHERE channel_id = $1`, scope)
	})

	id, _, err := s.StoreChatMessage(ctx, scope, "u1", "nick", sealTest(t, key, "hello"), 1, 0, "")
	if err != nil {
		t.Fatalf("StoreChatMessage: %v", err)
	}

	_, err = s.DB().ExecContext(ctx, `UPDATE chat_messages SET body = 'leak' WHERE id = $1`, id)
	if err == nil {
		t.Fatal("the database accepted a plaintext body")
	}
	if !strings.Contains(err.Error(), "chat_messages_no_plaintext") {
		t.Fatalf("error does not name the constraint: %v", err)
	}

	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO chat_messages (scope, channel_id, from_unique_id, body, body_enc, key_id)
		 VALUES (1, $1, 'u1', 'leak', 'x', 1)`, scope)
	if err == nil || !strings.Contains(err.Error(), "chat_messages_no_plaintext") {
		t.Fatalf("plaintext INSERT = %v, want a chat_messages_no_plaintext violation", err)
	}

	// The other half: a live row with no ciphertext at all is refused too, so
	// "encrypted" cannot degrade into "stored nowhere".
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO chat_messages (scope, channel_id, from_unique_id, body_enc, key_id)
		 VALUES (1, $1, 'u1', '', 0)`, scope)
	if err == nil || !strings.Contains(err.Error(), "chat_messages_sealed") {
		t.Fatalf("unsealed INSERT = %v, want a chat_messages_sealed violation", err)
	}

	var n int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM chat_messages WHERE channel_id = $1 AND body <> ''`, scope).Scan(&n); err != nil {
		t.Fatalf("auditing: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d plaintext bodies survived the rejected writes", n)
	}
}

// TestEmptyBodyRowSurvivesMigration covers the row a validated
// chat_messages_sealed would have aborted on: an encrypted message whose
// plaintext was the empty string is stored today with an empty body and no
// deleted_at, and it must migrate and then seal like any other.
func TestEmptyBodyRowSurvivesMigration(t *testing.T) {
	s := testScratchStore(t)
	ctx := context.Background()
	applyPreLedgerMigrations(t, s, "012")

	seed := func(body string, deleted bool) int64 {
		t.Helper()
		q := `INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body)
		      VALUES (1, 7, 'u1', 'nick', $1) RETURNING id`
		if deleted {
			q = `INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body, deleted_at)
			     VALUES (1, 7, 'u1', 'nick', $1, NOW()) RETURNING id`
		}
		var id int64
		if err := s.DB().QueryRowContext(ctx, q, body).Scan(&id); err != nil {
			t.Fatalf("seeding row: %v", err)
		}
		return id
	}
	empty := seed("", false)
	filled := seed("legacy text", false)
	tomb := seed("", true)

	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate over a row with an empty body: %v", err)
	}

	// The backfill's page must include the empty-plaintext row and exclude the
	// tombstone.
	page, err := s.LegacyPlaintextPage(ctx, 0, 500)
	if err != nil {
		t.Fatalf("LegacyPlaintextPage: %v", err)
	}
	seen := map[int64]string{}
	for _, r := range page {
		seen[r.ID] = r.Body
	}
	if _, ok := seen[empty]; !ok {
		t.Fatal("the empty-plaintext row is invisible to the backfill and would stay unsealed")
	}
	if seen[filled] != "legacy text" {
		t.Fatalf("legacy row body = %q, want the original text", seen[filled])
	}
	if _, ok := seen[tomb]; ok {
		t.Fatal("a tombstone was offered to the backfill")
	}

	key := testScopeKey(41)
	for id, body := range seen {
		if err := s.SetChatCiphertext(ctx, id, sealTest(t, key, body), 1); err != nil {
			t.Fatalf("SetChatCiphertext(%d): %v", id, err)
		}
	}
	if err := s.ValidateChatNoPlaintext(ctx); err != nil {
		t.Fatalf("validating after backfill: %v", err)
	}
	var bodyEnc string
	var keyID int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT body_enc, key_id FROM chat_messages WHERE id = $1`, empty).Scan(&bodyEnc, &keyID); err != nil {
		t.Fatalf("reading sealed row: %v", err)
	}
	if bodyEnc == "" || keyID != 1 {
		t.Fatalf("empty-plaintext row = (%q, %d), want it sealed", bodyEnc, keyID)
	}
	if openTest(t, key, bodyEnc) != "" {
		t.Fatal("the sealed empty body does not open to the empty string")
	}
}

// TestOfflineDeleteThenConstraintOrder pins the statement order in 012: the
// pre-4b spool rows are removed BEFORE offline_messages_sealed is added, and
// that constraint is validated, so a deployment carrying such a row still
// migrates and is proven clean afterwards.
func TestOfflineDeleteThenConstraintOrder(t *testing.T) {
	s := testScratchStore(t)
	ctx := context.Background()
	applyPreLedgerMigrations(t, s, "012")
	uid := seedScratchUser(t, s, "spool")

	seed := func(fromUnique, message string, delivered bool) int64 {
		t.Helper()
		q := `INSERT INTO offline_messages (from_user_id, to_user_id, from_unique_id, message)
		      VALUES ($1, $1, $2, $3) RETURNING id`
		if delivered {
			q = `INSERT INTO offline_messages (from_user_id, to_user_id, from_unique_id, message, delivered_at)
			     VALUES ($1, $1, $2, $3, NOW()) RETURNING id`
		}
		var id int64
		if err := s.DB().QueryRowContext(ctx, q, uid, fromUnique, message).Scan(&id); err != nil {
			t.Fatalf("seeding spool row: %v", err)
		}
		return id
	}
	legacyPending := seed("", "pre-4b plaintext", false)
	legacyDelivered := seed("", "pre-4b delivered", true)
	sealed := seed("sender-unique", "base64-ciphertext", false)

	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate with pre-4b spool rows present: %v", err)
	}

	if rowExists(t, s, "offline_messages", legacyPending) {
		t.Fatal("the undelivered pre-4b row survived")
	}
	var message string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT message FROM offline_messages WHERE id = $1`, legacyDelivered).Scan(&message); err != nil {
		t.Fatalf("reading delivered legacy row: %v", err)
	}
	if message != "" {
		t.Fatalf("delivered legacy row still holds %q at rest", message)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT message FROM offline_messages WHERE id = $1`, sealed).Scan(&message); err != nil {
		t.Fatalf("reading sealed spool row: %v", err)
	}
	if message != "base64-ciphertext" {
		t.Fatalf("a real E2EE spool row was damaged: %q", message)
	}

	// The constraint is live and validated, so no new plaintext row can appear.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO offline_messages (from_user_id, to_user_id, from_unique_id, message)
		 VALUES ($1, $1, '', 'new plaintext')`, uid)
	if err == nil || !strings.Contains(err.Error(), "offline_messages_sealed") {
		t.Fatalf("plaintext spool INSERT = %v, want an offline_messages_sealed violation", err)
	}
	pending, err := s.PendingMessages(ctx, uid)
	if err != nil {
		t.Fatalf("PendingMessages: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != sealed {
		t.Fatalf("pending = %+v, want only the sealed row", pending)
	}
}

// TestLegacyBackfillHelpersSealAndBlank exercises the store half of the
// one-shot backfill end to end: every pre-012 body seals under a generation,
// the legacy column ends empty, both constraints validate, and a second pass
// finds nothing left to do.
func TestLegacyBackfillHelpersSealAndBlank(t *testing.T) {
	s := testScratchStore(t)
	ctx := context.Background()
	applyPreLedgerMigrations(t, s, "012")

	want := map[int64]string{}
	for i, body := range []string{"first canary-7f3a", "second", "", "third with unicode: schoen 🔒"} {
		var id int64
		if err := s.DB().QueryRowContext(ctx,
			`INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body)
			 VALUES ($1, $2, 'u1', 'nick', $3) RETURNING id`, i%2, int64(i), body).Scan(&id); err != nil {
			t.Fatalf("seeding legacy row: %v", err)
		}
		want[id] = body
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n, err := s.CountPlaintextBodies(ctx); err != nil || n != 3 {
		t.Fatalf("plaintext bodies before the backfill = %d (err %v), want 3", n, err)
	}

	key := testScopeKey(51)
	sealed := 0
	after := int64(0)
	for {
		page, err := s.LegacyPlaintextPage(ctx, after, 2)
		if err != nil {
			t.Fatalf("LegacyPlaintextPage: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if r.Body != want[r.ID] {
				t.Fatalf("row %d body = %q, want %q", r.ID, r.Body, want[r.ID])
			}
			if err := s.SetChatCiphertext(ctx, r.ID, sealTest(t, key, r.Body), 9); err != nil {
				t.Fatalf("SetChatCiphertext: %v", err)
			}
			sealed++
			after = r.ID
		}
	}
	if sealed != len(want) {
		t.Fatalf("sealed %d rows, want %d", sealed, len(want))
	}
	if n, err := s.CountPlaintextBodies(ctx); err != nil || n != 0 {
		t.Fatalf("CountPlaintextBodies after the backfill = %d (err %v), want 0", n, err)
	}
	if err := s.ValidateChatNoPlaintext(ctx); err != nil {
		t.Fatalf("ValidateChatNoPlaintext: %v", err)
	}
	if err := s.ValidateChatNoPlaintext(ctx); err != nil {
		t.Fatalf("validating twice must be safe: %v", err)
	}

	for id, body := range want {
		var bodyEnc string
		var keyID int64
		if err := s.DB().QueryRowContext(ctx,
			`SELECT body_enc, key_id FROM chat_messages WHERE id = $1`, id).Scan(&bodyEnc, &keyID); err != nil {
			t.Fatalf("reading row %d: %v", id, err)
		}
		if keyID != 9 {
			t.Fatalf("row %d key_id = %d, want 9", id, keyID)
		}
		if got := openTest(t, key, bodyEnc); got != body {
			t.Fatalf("row %d opens to %q, want %q", id, got, body)
		}
	}

	// A second run has nothing to do: the predicate is empty, which is what
	// makes the backfill idempotent and resumable after a crash.
	if page, err := s.LegacyPlaintextPage(ctx, 0, 500); err != nil || len(page) != 0 {
		t.Fatalf("second pass found %d rows (err %v), want none", len(page), err)
	}
}

// TestLegacyBackfillHelpersPurgeMode covers chat_legacy_history=purge: history
// is destroyed rather than re-sealed under a server-minted key, and the result
// still satisfies both constraints.
func TestLegacyBackfillHelpersPurgeMode(t *testing.T) {
	s := testScratchStore(t)
	ctx := context.Background()
	applyPreLedgerMigrations(t, s, "012")

	for _, body := range []string{"one", "two", ""} {
		if _, err := s.DB().ExecContext(ctx,
			`INSERT INTO chat_messages (scope, channel_id, from_unique_id, from_nickname, body)
			 VALUES (1, 5, 'u1', 'nick', $1)`, body); err != nil {
			t.Fatalf("seeding legacy row: %v", err)
		}
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	n, err := s.PurgeLegacyPlaintext(ctx)
	if err != nil {
		t.Fatalf("PurgeLegacyPlaintext: %v", err)
	}
	if n != 3 {
		t.Fatalf("purged %d rows, want 3", n)
	}
	if left, err := s.CountPlaintextBodies(ctx); err != nil || left != 0 {
		t.Fatalf("plaintext bodies after purge = %d (err %v), want 0", left, err)
	}
	var ciphertexts, live int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE body_enc <> ''), count(*) FILTER (WHERE deleted_at IS NULL)
		 FROM chat_messages`).Scan(&ciphertexts, &live); err != nil {
		t.Fatalf("auditing purged rows: %v", err)
	}
	if ciphertexts != 0 || live != 0 {
		t.Fatalf("after purge: %d ciphertexts, %d live rows, want 0 and 0", ciphertexts, live)
	}
	if err := s.ValidateChatNoPlaintext(ctx); err != nil {
		t.Fatalf("ValidateChatNoPlaintext after purge: %v", err)
	}
	if page, err := s.LegacyPlaintextPage(ctx, 0, 500); err != nil || len(page) != 0 {
		t.Fatalf("purge left %d rows for the backfill (err %v), want none", len(page), err)
	}
}
