package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPIICipherBindsCiphertextToFieldAndUser(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := NewPIICipher(key)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := cipher.seal("person@example.test", piiAAD(7, "email"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cipher.open(blob, piiAAD(7, "email"))
	if err != nil || got != "person@example.test" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := cipher.open(blob, piiAAD(8, "email")); err == nil {
		t.Fatal("ciphertext opened for a different user")
	}
	if _, err := cipher.open(blob, piiAAD(7, "last_ip")); err == nil {
		t.Fatal("ciphertext opened in a different column")
	}
}

func TestUserPIIOmitAndClear(t *testing.T) {
	s := testDBStore(t)
	cipher, err := NewPIICipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.SetPIICipher(cipher)
	userID := seedTestUser(t, s, fmt.Sprint(time.Now().UnixNano()))
	ctx := context.Background()
	if _, _, err := s.UserPII(ctx, userID); !errors.Is(err, ErrNoPII) {
		t.Fatalf("missing PII error = %v", err)
	}
	email, ip := "person@example.test", "192.0.2.10"
	if err := s.SetUserPII(ctx, userID, &email, &ip); err != nil {
		t.Fatal(err)
	}
	newIP := "192.0.2.11"
	if err := s.SetUserPII(ctx, userID, nil, &newIP); err != nil {
		t.Fatal(err)
	}
	gotEmail, gotIP, err := s.UserPII(ctx, userID)
	if err != nil || gotEmail != email || gotIP != newIP {
		t.Fatalf("after omitted email = %q, %q, %v", gotEmail, gotIP, err)
	}
	clear := ""
	if err := s.SetUserPII(ctx, userID, &clear, nil); err != nil {
		t.Fatal(err)
	}
	gotEmail, gotIP, err = s.UserPII(ctx, userID)
	if err != nil || gotEmail != "" || gotIP != newIP {
		t.Fatalf("after clear = %q, %q, %v", gotEmail, gotIP, err)
	}
}

func TestLoadOrCreatePIICipherCreatesRestrictedDurableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "pii.key")
	if _, err := LoadOrCreatePIICipher(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 32 {
		t.Fatalf("key size = %d, want 32", info.Size())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := LoadOrCreatePIICipher(path); err != nil {
		t.Fatalf("reloading key: %v", err)
	}
}
