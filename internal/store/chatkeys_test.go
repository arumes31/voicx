// chatkeys_test.go DB-backed tests for the persisted scope key generations
// (91-135, migration 012). A bug in id allocation or in the rotation
// transaction orphans every ciphertext that references the generation, so
// each invariant is asserted against a real Postgres. Skips without one.
package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// testScope returns a scope id no channel uses, so tests never collide with
// dev data or each other.
func testScope(t *testing.T, s *Store) int64 {
	t.Helper()
	scope := time.Now().UnixNano()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM chat_scope_keys WHERE scope_id = $1`, scope)
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM chat_scope_seq WHERE scope_id = $1`, scope)
	})
	return scope
}

// wrappedFixture stands in for chatcrypto.Wrap output: only its bytes matter
// to the store.
func wrappedFixture(fill byte) []byte {
	out := make([]byte, 72)
	for i := range out {
		out[i] = fill + byte(i)
	}
	return out
}

// TestScopeKeyRoundTrip covers the whole persistence path a restart depends
// on: allocate, insert, read back as current, and read back by id.
func TestScopeKeyRoundTrip(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	scope := testScope(t, s)

	if k, err := s.CurrentScopeKey(ctx, scope); err != nil || k != nil {
		t.Fatalf("CurrentScopeKey on an unknown scope = %+v, err=%v, want (nil, nil)", k, err)
	}
	if k, err := s.GetScopeKey(ctx, scope, 1); err != nil || k != nil {
		t.Fatalf("GetScopeKey on an unknown generation = %+v, err=%v, want (nil, nil)", k, err)
	}

	before, err := s.CountScopeKeys(ctx)
	if err != nil {
		t.Fatalf("CountScopeKeys: %v", err)
	}

	id, err := s.AllocScopeKeyID(ctx, scope)
	if err != nil {
		t.Fatalf("AllocScopeKeyID: %v", err)
	}
	if id != 1 {
		t.Fatalf("first generation id = %d, want 1", id)
	}
	wrapped := wrappedFixture(3)
	if err := s.InsertScopeKey(ctx, scope, id, wrapped, 2); err != nil {
		t.Fatalf("InsertScopeKey: %v", err)
	}

	cur, err := s.CurrentScopeKey(ctx, scope)
	if err != nil || cur == nil {
		t.Fatalf("CurrentScopeKey = %+v, err=%v", cur, err)
	}
	if cur.KeyID != id || cur.KEKID != 2 || string(cur.Wrapped) != string(wrapped) {
		t.Fatalf("current key = %+v, want the inserted generation", cur)
	}
	if cur.RetiredAt != nil {
		t.Fatalf("freshly inserted key is retired at %v", cur.RetiredAt)
	}
	if cur.CreatedAt.IsZero() {
		t.Fatal("created_at was not returned")
	}
	got, err := s.GetScopeKey(ctx, scope, id)
	if err != nil || got == nil || string(got.Wrapped) != string(wrapped) {
		t.Fatalf("GetScopeKey = %+v, err=%v", got, err)
	}

	after, err := s.CountScopeKeys(ctx)
	if err != nil {
		t.Fatalf("CountScopeKeys: %v", err)
	}
	if after-before < 1 {
		t.Fatalf("CountScopeKeys did not grow: %d -> %d", before, after)
	}
}

// TestScopeKeyIDsNeverReused is the guarantee that makes stored ciphertext
// safe forever: ids come from chat_scope_seq, so no id can ever name two
// different key materials — not after a rotation, and not even after the row
// itself is gone.
func TestScopeKeyIDsNeverReused(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	scope := testScope(t, s)

	first, err := s.AllocScopeKeyID(ctx, scope)
	if err != nil {
		t.Fatalf("AllocScopeKeyID: %v", err)
	}
	if err := s.InsertScopeKey(ctx, scope, first, wrappedFixture(1), 1); err != nil {
		t.Fatalf("InsertScopeKey: %v", err)
	}
	second, err := s.AllocScopeKeyID(ctx, scope)
	if err != nil {
		t.Fatalf("AllocScopeKeyID (rotation): %v", err)
	}
	if second <= first {
		t.Fatalf("second generation id = %d, want strictly greater than %d", second, first)
	}
	if err := s.RotateScopeKey(ctx, scope, second, wrappedFixture(2), 1); err != nil {
		t.Fatalf("RotateScopeKey: %v", err)
	}

	// The retired generation is still readable — history predates rotation and
	// rotation is not forward secrecy over it.
	old, err := s.GetScopeKey(ctx, scope, first)
	if err != nil || old == nil {
		t.Fatalf("retired generation = %+v, err=%v, want it kept", old, err)
	}
	if old.RetiredAt == nil {
		t.Fatal("the previous generation was not retired")
	}
	if string(old.Wrapped) != string(wrappedFixture(1)) {
		t.Fatal("rotation rewrote the retired generation's key material")
	}
	cur, err := s.CurrentScopeKey(ctx, scope)
	if err != nil || cur == nil || cur.KeyID != second {
		t.Fatalf("current = %+v, err=%v, want generation %d", cur, err, second)
	}

	// Even after every row for the scope is deleted, the sequence does not
	// rewind: a re-minted id with different material would orphan history.
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM chat_scope_keys WHERE scope_id = $1`, scope); err != nil {
		t.Fatalf("clearing scope keys: %v", err)
	}
	third, err := s.AllocScopeKeyID(ctx, scope)
	if err != nil {
		t.Fatalf("AllocScopeKeyID after delete: %v", err)
	}
	if third <= second {
		t.Fatalf("id after deleting every row = %d, want greater than %d", third, second)
	}

	// Separate scopes number independently.
	other := testScope(t, s)
	if id, err := s.AllocScopeKeyID(ctx, other); err != nil || id != 1 {
		t.Fatalf("first id of a fresh scope = %d, err=%v, want 1", id, err)
	}
}

// TestInsertScopeKeyConflictIsAnError keeps a generation collision loud: two
// different key materials competing for one id is corruption, and swallowing
// it would leave messages sealed under a key nobody can name.
func TestInsertScopeKeyConflictIsAnError(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	scope := testScope(t, s)

	if err := s.InsertScopeKey(ctx, scope, 1, wrappedFixture(1), 1); err != nil {
		t.Fatalf("InsertScopeKey: %v", err)
	}
	err := s.InsertScopeKey(ctx, scope, 1, wrappedFixture(9), 1)
	if err == nil {
		t.Fatal("a duplicate generation id was accepted")
	}
	if !strings.Contains(err.Error(), "inserting scope key") {
		t.Fatalf("conflict error is not attributable: %v", err)
	}

	// The original material is untouched.
	got, err := s.GetScopeKey(ctx, scope, 1)
	if err != nil || got == nil || string(got.Wrapped) != string(wrappedFixture(1)) {
		t.Fatalf("stored key after the rejected insert = %+v, err=%v", got, err)
	}

	// A second live generation is refused by the partial unique index, so
	// "current" can never be ambiguous.
	if err := s.InsertScopeKey(ctx, scope, 2, wrappedFixture(2), 1); err == nil {
		t.Fatal("a second live generation was accepted for one scope")
	}
}

// TestRotateScopeKeyIsAtomic proves the retire and the insert share one
// transaction: a failing insert must leave the old generation live, never a
// scope with no current key.
func TestRotateScopeKeyIsAtomic(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	scope := testScope(t, s)

	first, err := s.AllocScopeKeyID(ctx, scope)
	if err != nil {
		t.Fatalf("AllocScopeKeyID: %v", err)
	}
	if err := s.InsertScopeKey(ctx, scope, first, wrappedFixture(1), 1); err != nil {
		t.Fatalf("InsertScopeKey: %v", err)
	}

	// Rotating onto an id that already exists fails at the INSERT, after the
	// UPDATE has already retired the current key inside the transaction.
	if err := s.RotateScopeKey(ctx, scope, first, wrappedFixture(5), 1); err == nil {
		t.Fatal("rotation onto an existing generation id succeeded")
	}

	var live int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM chat_scope_keys WHERE scope_id = $1 AND retired_at IS NULL`, scope).Scan(&live); err != nil {
		t.Fatalf("counting live generations: %v", err)
	}
	if live != 1 {
		t.Fatalf("%d live generations after the failed rotation, want exactly 1", live)
	}
	cur, err := s.CurrentScopeKey(ctx, scope)
	if err != nil || cur == nil || cur.KeyID != first {
		t.Fatalf("current after the failed rotation = %+v, err=%v, want generation %d", cur, err, first)
	}
	if string(cur.Wrapped) != string(wrappedFixture(1)) {
		t.Fatal("the failed rotation changed the live key material")
	}

	// A well-formed rotation still commits both halves.
	next, err := s.AllocScopeKeyID(ctx, scope)
	if err != nil {
		t.Fatalf("AllocScopeKeyID: %v", err)
	}
	if err := s.RotateScopeKey(ctx, scope, next, wrappedFixture(7), 3); err != nil {
		t.Fatalf("RotateScopeKey: %v", err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM chat_scope_keys WHERE scope_id = $1 AND retired_at IS NULL`, scope).Scan(&live); err != nil {
		t.Fatalf("counting live generations: %v", err)
	}
	if live != 1 {
		t.Fatalf("%d live generations after rotation, want exactly 1", live)
	}
	cur, err = s.CurrentScopeKey(ctx, scope)
	if err != nil || cur == nil || cur.KeyID != next || cur.KEKID != 3 {
		t.Fatalf("current after rotation = %+v, err=%v", cur, err)
	}
}
