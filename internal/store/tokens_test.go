// tokens_test.go DB-backed tests for the privilege-token and complaint
// queries behind the token manager and complaint list (173/174). They follow
// the repository's skip pattern: without a reachable Postgres they skip.
package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestTokenMetaDB covers creation with metadata, listing, redemption
// bookkeeping (uses + used_by) and deletion.
func TestTokenMetaDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID := seedTestUser(t, s, suffix)

	gid, err := s.CreateGroup(ctx, "server", "tok_test_"+suffix, 1)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGroup(ctx, "server", gid, true) })

	key, err := s.CreateTokenWithMeta(ctx, 0, gid, 0, "desc "+suffix, 1)
	if err != nil {
		t.Fatalf("CreateTokenWithMeta: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteToken(ctx, key) })
	if key == "" {
		t.Fatal("empty token key")
	}

	find := func() *Token {
		t.Helper()
		rows, err := s.ListTokens(ctx)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		for i := range rows {
			if rows[i].Key == key {
				return &rows[i]
			}
		}
		return nil
	}

	tok := find()
	if tok == nil {
		t.Fatal("created token not listed")
	}
	if tok.GroupID != gid || tok.Description != "desc "+suffix {
		t.Fatalf("token = %+v", *tok)
	}
	if tok.UsedBy != "" || tok.Uses != 0 {
		t.Fatalf("fresh token already redeemed: %+v", *tok)
	}
	if tok.CreatedAt.IsZero() {
		t.Fatal("created_at not populated")
	}

	granted, err := s.UseToken(ctx, key, userID)
	if err != nil {
		t.Fatalf("UseToken: %v", err)
	}
	if granted != gid {
		t.Fatalf("granted group = %d, want %d", granted, gid)
	}
	tok = find()
	if tok == nil {
		t.Fatal("redeemed token vanished")
	}
	if tok.Uses != 1 || tok.UsedBy != "w6atest_"+suffix {
		t.Fatalf("redemption not recorded: %+v", *tok)
	}

	// A one-use token is spent.
	if _, err := s.UseToken(ctx, key, userID); !errors.Is(err, ErrTokenExhausted) {
		t.Fatalf("second UseToken err = %v, want ErrTokenExhausted", err)
	}

	if err := s.DeleteToken(ctx, key); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if err := s.DeleteToken(ctx, key); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("second DeleteToken err = %v, want ErrTokenNotFound", err)
	}
}

// TestGuestTokenPromotionDB verifies that guest promotion and redemption are
// atomic and that the promoted identity owns the resulting grant.
func TestGuestTokenPromotionDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	uniqueID := "guest_token_" + suffix
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM users WHERE unique_id = $1`, uniqueID)
	})

	if _, err := s.UseTokenForIdentity(ctx, "missing", 0, uniqueID, "guest"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("missing token error = %v, want ErrTokenNotFound", err)
	}
	var count int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE unique_id = $1`, uniqueID).Scan(&count); err != nil {
		t.Fatalf("counting guest rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("invalid token created %d guest rows", count)
	}

	key, err := s.CreateToken(ctx, 0, 0, 1)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteToken(ctx, key) })
	grant, err := s.UseTokenForIdentity(ctx, key, 0, uniqueID, "guest")
	if err != nil {
		t.Fatalf("UseTokenForIdentity: %v", err)
	}
	if grant.UserID == 0 || !grant.Admin || !grant.Promoted || grant.GroupID != 0 {
		t.Fatalf("guest grant = %+v", grant)
	}

	var admin bool
	if err := s.DB().QueryRowContext(ctx, `SELECT is_admin FROM users WHERE id = $1`, grant.UserID).Scan(&admin); err != nil {
		t.Fatalf("querying promoted guest: %v", err)
	}
	if !admin {
		t.Fatal("promoted guest did not receive admin grant")
	}
}

// TestDeleteComplaintsAgainstDB covers the targeted and blanket clears.
func TestDeleteComplaintsAgainstDB(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	target := "target_" + suffix
	a := "reporter_a_" + suffix
	b := "reporter_b_" + suffix
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM complaints WHERE target = $1`, target)
	})

	for _, reporter := range []string{a, b} {
		if err := s.AddComplaint(ctx, reporter, target, "reason"); err != nil {
			t.Fatalf("AddComplaint: %v", err)
		}
	}

	// Targeted: only reporter a's complaint goes.
	n, err := s.DeleteComplaintsAgainst(ctx, target, a)
	if err != nil {
		t.Fatalf("DeleteComplaintsAgainst: %v", err)
	}
	if n != 1 {
		t.Fatalf("targeted clear removed %d, want 1", n)
	}

	remaining := 0
	rows, err := s.ListComplaints(ctx)
	if err != nil {
		t.Fatalf("ListComplaints: %v", err)
	}
	for _, c := range rows {
		if c.Target == target {
			remaining++
			if c.Reporter != b {
				t.Fatalf("wrong complaint survived: %+v", c)
			}
		}
	}
	if remaining != 1 {
		t.Fatalf("remaining = %d, want 1", remaining)
	}

	// Blanket: an empty reporter clears everything against the target.
	n, err = s.DeleteComplaintsAgainst(ctx, target, "")
	if err != nil {
		t.Fatalf("DeleteComplaintsAgainst: %v", err)
	}
	if n != 1 {
		t.Fatalf("blanket clear removed %d, want 1", n)
	}
	if n, err = s.DeleteComplaintsAgainst(ctx, target, ""); err != nil || n != 0 {
		t.Fatalf("clearing nothing: n=%d err=%v", n, err)
	}
}
