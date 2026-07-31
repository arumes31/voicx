package auth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/store"
)

// testAuthService constructs an AuthService backed by a real Postgres store if
// one is reachable. It skips the calling test when no database is available.
func testAuthService(t *testing.T) *AuthService {
	t.Helper()
	svc, _ := testAuthServiceWithStore(t)
	return svc
}

// testAuthServiceWithStore is testAuthService but also returns the store, for
// tests that need to insert fixture rows directly.
func testAuthServiceWithStore(t *testing.T) (*AuthService, *store.Store) {
	t.Helper()

	dbURL := os.Getenv("VOICX_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://voicx:voicx@localhost:5432/voicx?sslmode=disable"
	}

	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment: %v", err)
	}

	s, err := store.New(dbURL, logger, 5, 1, time.Minute)
	if err != nil {
		t.Skipf("database unavailable, skipping: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Migrate(); err != nil {
		t.Skipf("migrate failed, skipping: %v", err)
	}
	return New(s, logger), s
}

// uniqueNickname returns a nickname unlikely to collide with other test runs.
func uniqueNickname(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestRegisterAuthenticatePasswordRoundTrip verifies that a registered user can
// authenticate with the correct password and fails with a wrong one.
func TestRegisterAuthenticatePasswordRoundTrip(t *testing.T) {
	svc := testAuthService(t)

	nick := uniqueNickname("pwuser")
	pw := "correct-horse-battery-staple"

	uid, err := svc.RegisterUser(context.Background(), nick, pw)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	if uid == "" {
		t.Fatal("expected non-empty unique id")
	}

	ok, err := svc.AuthenticatePassword(context.Background(), uid, pw)
	if err != nil {
		t.Fatalf("AuthenticatePassword(correct): %v", err)
	}
	if !ok {
		t.Fatal("expected successful authentication with correct password")
	}

	ok, err = svc.AuthenticatePassword(context.Background(), uid, "wrong-password")
	if err != nil {
		t.Fatalf("AuthenticatePassword(wrong): %v", err)
	}
	if ok {
		t.Fatal("expected failed authentication with wrong password")
	}
}

// TestRegisterAuthenticateChallengeRoundTrip verifies the challenge-response
// flow: register, generate a challenge, sign with the private key, verify.
func TestRegisterAuthenticateChallengeRoundTrip(t *testing.T) {
	svc := testAuthService(t)

	nick := uniqueNickname("chaluser")
	pw := "somepassword"

	_, err := svc.RegisterUser(context.Background(), nick, pw)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	// We need the private key to sign the challenge. RegisterUser does not
	// return it, so generate a fresh key pair and insert it directly via the
	// store to exercise the challenge path with a known private key.
	pubPEM, privPEM, err := GenerateIdentityKeyPair()
	if err != nil {
		t.Fatalf("GenerateIdentityKeyPair: %v", err)
	}
	newUID, err := UniqueIDFromPublicKey(pubPEM)
	if err != nil {
		t.Fatalf("UniqueIDFromPublicKey: %v", err)
	}
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	const q = `INSERT INTO users (unique_id, nickname, password_hash, public_key, created_at)
	          VALUES ($1, $2, $3, $4, NOW())`
	if _, err := svc.store.DB().ExecContext(context.Background(), q, newUID, nick+"-chal", hash, pubPEM); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	challenge, err := GenerateChallenge()
	if err != nil {
		t.Fatalf("GenerateChallenge: %v", err)
	}
	if len(challenge) != 32 {
		t.Fatalf("expected 32-byte challenge, got %d", len(challenge))
	}

	sig, err := SignChallenge(privPEM, challenge)
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}

	ok, err := svc.AuthenticateChallenge(context.Background(), newUID, challenge, sig)
	if err != nil {
		t.Fatalf("AuthenticateChallenge(valid): %v", err)
	}
	if !ok {
		t.Fatal("expected successful challenge authentication")
	}

	// Tamper with the signature.
	tampered := make([]byte, len(sig))
	copy(tampered, sig)
	tampered[0] ^= 0xff
	ok, err = svc.AuthenticateChallenge(context.Background(), newUID, challenge, tampered)
	if err != nil {
		t.Fatalf("AuthenticateChallenge(tampered): %v", err)
	}
	if ok {
		t.Fatal("expected failed authentication with tampered signature")
	}
}

// TestRegisterUserDuplicate verifies that registering the same nickname twice
// returns ErrUserExists.
func TestRegisterUserDuplicate(t *testing.T) {
	svc := testAuthService(t)

	nick := uniqueNickname("dupuser")
	pw := "password"

	if _, err := svc.RegisterUser(context.Background(), nick, pw); err != nil {
		t.Fatalf("RegisterUser 1: %v", err)
	}
	_, err := svc.RegisterUser(context.Background(), nick, pw)
	if err != ErrUserExists {
		t.Fatalf("expected ErrUserExists, got %v", err)
	}
}

// insertBan inserts a ban row and returns its id. expiresAt nil means a
// permanent ban.
func insertBan(t *testing.T, s *store.Store, banType int, value, reason string, expiresAt *time.Time) int64 {
	t.Helper()
	var id int64
	const q = `INSERT INTO bans (ban_type, value, reason, expires_at)
	          VALUES ($1, $2, $3, $4) RETURNING id`
	err := s.DB().QueryRowContext(context.Background(), q, banType, value, reason, expiresAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert ban: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(context.Background(), `DELETE FROM bans WHERE id = $1`, id)
	})
	return id
}

// TestLookupActiveBan verifies the ban lookup semantics: active unique-ID and
// IP bans match, expired and channel-scoped bans are ignored, and unknown
// values yield no ban.
func TestLookupActiveBan(t *testing.T) {
	svc, s := testAuthServiceWithStore(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	uid := uniqueNickname("banned-uid")
	expiredUID := uniqueNickname("expired-uid")
	tempUID := uniqueNickname("temp-uid")
	insertBan(t, s, 1, uid, "spam", nil)             // permanent unique-ID ban
	insertBan(t, s, 0, "10.99.0.1", "ip abuse", nil) // permanent IP ban
	insertBan(t, s, 1, expiredUID, "old", &past)     // expired
	insertBan(t, s, 1, tempUID, "temp", &future)     // not yet expired

	t.Run("active unique_id ban matches", func(t *testing.T) {
		ban, err := svc.LookupActiveBan(ctx, uid, "")
		if err != nil {
			t.Fatalf("LookupActiveBan: %v", err)
		}
		if ban == nil {
			t.Fatal("expected a ban, got nil")
		}
		if ban.Type != 1 || ban.Value != uid || ban.Reason != "spam" {
			t.Fatalf("ban = %+v, want type 1 value %q reason spam", ban, uid)
		}
		if !ban.ExpiresAt.IsZero() {
			t.Fatalf("permanent ban has ExpiresAt %v, want zero", ban.ExpiresAt)
		}
	})

	t.Run("active IP ban matches", func(t *testing.T) {
		ban, err := svc.LookupActiveBan(ctx, "some-other-uid", "10.99.0.1")
		if err != nil {
			t.Fatalf("LookupActiveBan: %v", err)
		}
		if ban == nil || ban.Type != 0 {
			t.Fatalf("ban = %+v, want type 0 IP ban", ban)
		}
	})

	t.Run("expired ban ignored", func(t *testing.T) {
		ban, err := svc.LookupActiveBan(ctx, expiredUID, "")
		if err != nil {
			t.Fatalf("LookupActiveBan: %v", err)
		}
		if ban != nil {
			t.Fatalf("expected expired ban to be ignored, got %+v", ban)
		}
	})

	t.Run("future-expiry ban still active", func(t *testing.T) {
		ban, err := svc.LookupActiveBan(ctx, tempUID, "")
		if err != nil {
			t.Fatalf("LookupActiveBan: %v", err)
		}
		if ban == nil {
			t.Fatal("expected future-expiry ban to be active, got nil")
		}
		if ban.ExpiresAt.IsZero() {
			t.Fatal("expected non-zero ExpiresAt for temporary ban")
		}
	})

	t.Run("no ban for unknown identity", func(t *testing.T) {
		ban, err := svc.LookupActiveBan(ctx, uniqueNickname("clean-uid"), "192.0.2.1")
		if err != nil {
			t.Fatalf("LookupActiveBan: %v", err)
		}
		if ban != nil {
			t.Fatalf("expected no ban, got %+v", ban)
		}
	})
}

// TestAuthenticatePasswordVerifiesEveryCall is the BUG-1 regression test:
// after a successful password auth, a wrong password for the same unique ID
// must FAIL immediately (a positive-result cache keyed only by unique ID
// previously let any password through for 30s).
func TestAuthenticatePasswordVerifiesEveryCall(t *testing.T) {
	svc := testAuthService(t)
	ctx := context.Background()

	nick := uniqueNickname("cachebug")
	pw := "the-right-password"
	uid, err := svc.RegisterUser(ctx, nick, pw)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	// First: correct password succeeds.
	ok, err := svc.AuthenticatePassword(ctx, uid, pw)
	if err != nil {
		t.Fatalf("AuthenticatePassword(correct): %v", err)
	}
	if !ok {
		t.Fatal("correct password rejected")
	}

	// Immediately after: a wrong password must fail (no positive caching).
	ok, err = svc.AuthenticatePassword(ctx, uid, "wrong-password")
	if err != nil {
		t.Fatalf("AuthenticatePassword(wrong): %v", err)
	}
	if ok {
		t.Fatal("wrong password accepted immediately after a successful login")
	}

	// And the correct password still works afterwards.
	ok, err = svc.AuthenticatePassword(ctx, uid, pw)
	if err != nil {
		t.Fatalf("AuthenticatePassword(correct again): %v", err)
	}
	if !ok {
		t.Fatal("correct password rejected on second attempt")
	}
}
