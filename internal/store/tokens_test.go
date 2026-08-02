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
