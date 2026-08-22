package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCustomPropertiesDedicatedDB(t *testing.T) {
	s := testDedicatedDBStore(t)
	ctx := context.Background()
	stamp := fmt.Sprintf("custom-test-%d", time.Now().UnixNano())
	firstID := stamp + "-first"
	secondID := stamp + "-second"
	keys := []string{"alpha", "beta"}
	t.Cleanup(func() {
		for _, id := range []string{firstID, secondID} {
			for _, key := range keys {
				if err := s.CustomDel(context.Background(), id, key); err != nil {
					t.Errorf("cleanup custom property %q/%q: %v", id, key, err)
				}
			}
		}
	})

	empty, err := s.CustomInfo(ctx, firstID)
	if err != nil {
		t.Fatalf("CustomInfo empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("CustomInfo empty = %+v, want no entries", empty)
	}
	for _, entry := range []CustomEntry{{Key: "beta", Value: "first"}, {Key: "alpha", Value: "one"}, {Key: "beta", Value: "updated"}} {
		if err := s.CustomSet(ctx, firstID, entry.Key, entry.Value); err != nil {
			t.Fatalf("CustomSet(%q, %q): %v", entry.Key, entry.Value, err)
		}
	}
	if err := s.CustomSet(ctx, secondID, "alpha", "other user"); err != nil {
		t.Fatalf("CustomSet second user: %v", err)
	}
	got, err := s.CustomInfo(ctx, firstID)
	if err != nil {
		t.Fatalf("CustomInfo first user: %v", err)
	}
	want := []CustomEntry{{Key: "alpha", Value: "one"}, {Key: "beta", Value: "updated"}}
	if len(got) != len(want) {
		t.Fatalf("CustomInfo entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CustomInfo entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := s.CustomDel(ctx, firstID, "missing"); err != nil {
		t.Fatalf("first idempotent delete: %v", err)
	}
	if err := s.CustomDel(ctx, firstID, "missing"); err != nil {
		t.Fatalf("second idempotent delete: %v", err)
	}
	if err := s.CustomDel(ctx, firstID, "alpha"); err != nil {
		t.Fatalf("delete alpha: %v", err)
	}
	if err := s.CustomDel(ctx, firstID, "beta"); err != nil {
		t.Fatalf("delete beta: %v", err)
	}
	got, err = s.CustomInfo(ctx, firstID)
	if err != nil {
		t.Fatalf("CustomInfo after deletes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("CustomInfo after deletes = %+v, want no entries", got)
	}
	second, err := s.CustomInfo(ctx, secondID)
	if err != nil {
		t.Fatalf("CustomInfo second user: %v", err)
	}
	if len(second) != 1 || second[0] != (CustomEntry{Key: "alpha", Value: "other user"}) {
		t.Fatalf("CustomInfo second user = %+v, want isolated value", second)
	}
}
