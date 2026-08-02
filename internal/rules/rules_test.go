package rules

import (
	"context"
	"strings"
	"testing"
)

// fakeSettings is an in-memory server_settings table.
type fakeSettings struct{ values map[string]string }

func (f *fakeSettings) GetServerSetting(_ context.Context, key string) (string, uint32, error) {
	return f.values[key], 0, nil
}

func (f *fakeSettings) SetServerSetting(_ context.Context, key, value string, _ uint32) error {
	f.values[key] = value
	return nil
}

func newService() (*Service, *fakeSettings) {
	settings := &fakeSettings{values: map[string]string{}}
	// db stays nil: every path exercised here returns before touching it,
	// which is exactly the "no rules configured" contract.
	return New(settings, nil), settings
}

// TestHashIdentifiesTheWording verifies the acceptance key changes with the
// text and ignores blank rules (215).
func TestHashIdentifiesTheWording(t *testing.T) {
	if Hash("be nice") == Hash("be nicer") {
		t.Fatal("different wording shares an acceptance hash")
	}
	if Hash("be nice") != Hash("be nice") {
		t.Fatal("hash is not stable")
	}
	for _, blank := range []string{"", "   ", "\n\t"} {
		if Hash(blank) != "" {
			t.Fatalf("blank rules %q produced a hash", blank)
		}
	}
}

// TestTextAndSet verifies the rules round-trip through the settings row (215).
func TestTextAndSet(t *testing.T) {
	svc, settings := newService()
	ctx := context.Background()

	text, hash, err := svc.Text(ctx)
	if err != nil || text != "" || hash != "" {
		t.Fatalf("unset rules = %q/%q (err %v)", text, hash, err)
	}

	if err := svc.Set(ctx, "be nice"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if settings.values[SettingKey] != "be nice" {
		t.Fatalf("stored under %q: %v", SettingKey, settings.values)
	}
	text, hash, err = svc.Text(ctx)
	if err != nil || text != "be nice" || hash != Hash("be nice") {
		t.Fatalf("text = %q/%q (err %v)", text, hash, err)
	}
}

// TestPendingWithoutRules verifies that unset rules are never asked about, so
// a server that configures none shows no dialog (215).
func TestPendingWithoutRules(t *testing.T) {
	svc, _ := newService()
	text, hash, pending, err := svc.Pending(context.Background(), 1)
	if err != nil || pending || text != "" || hash != "" {
		t.Fatalf("pending = %q/%q/%t (err %v)", text, hash, pending, err)
	}
}

// TestAcceptRejectsStaleWording verifies an acceptance for wording that is no
// longer in force is refused (215).
func TestAcceptRejectsStaleWording(t *testing.T) {
	svc, _ := newService()
	ctx := context.Background()

	if err := svc.Accept(ctx, 1, ""); err == nil ||
		!strings.Contains(err.Error(), "no server rules") {
		t.Fatalf("accept without rules = %v", err)
	}

	if err := svc.Set(ctx, "be nice"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := svc.Accept(ctx, 1, Hash("be nicer")); err == nil ||
		!strings.Contains(err.Error(), "rules changed") {
		t.Fatalf("accept with a stale hash = %v", err)
	}
}
