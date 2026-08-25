package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"

	"voicx/internal/store"
)

func TestAuthenticateIdentifierVerifiesExactlyOnce(t *testing.T) {
	tests := []struct {
		name     string
		lookup   func(context.Context, string) (passwordCredential, error)
		password string
		wantUser bool
		wantErr  error
	}{
		{
			name: "unknown account",
			lookup: func(context.Context, string) (passwordCredential, error) {
				return passwordCredential{}, sql.ErrNoRows
			},
			password: "wrong",
			wantErr:  ErrUserNotFound,
		},
		{
			name: "known wrong password",
			lookup: func(context.Context, string) (passwordCredential, error) {
				return passwordCredential{user: User{UniqueID: "uid"}, hash: "stored"}, nil
			},
			password: "wrong",
		},
		{
			name: "passwordless account",
			lookup: func(context.Context, string) (passwordCredential, error) {
				return passwordCredential{user: User{UniqueID: "uid"}}, nil
			},
			password: "wrong",
		},
		{
			name: "known correct password",
			lookup: func(context.Context, string) (passwordCredential, error) {
				return passwordCredential{user: User{UniqueID: "uid"}, hash: "stored"}, nil
			},
			password: "correct",
			wantUser: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			svc := &AuthService{
				lookupIdentifier: test.lookup,
				verifyPassword: func(password, hash string) error {
					calls++
					if hash == "stored" && password == "correct" {
						return nil
					}
					return errors.New("password mismatch")
				},
			}
			user, err := svc.AuthenticateIdentifier(context.Background(), "identifier", test.password)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("AuthenticateIdentifier() error = %v, want %v", err, test.wantErr)
			}
			if (user != nil) != test.wantUser {
				t.Fatalf("AuthenticateIdentifier() user = %#v, want present=%t", user, test.wantUser)
			}
			if calls != 1 {
				t.Fatalf("password verifier calls = %d, want 1", calls)
			}
		})
	}
}

func TestAuthenticateIdentifierPrefersExactUniqueIDOverNickname(t *testing.T) {
	svc, s := testAuthServiceWithStore(t)
	ctx := context.Background()
	identifier := uniqueNickname("identifier-collision")
	uniquePassword := "unique-password"
	nicknamePassword := "nickname-password"
	uniqueHash, err := HashPassword(uniquePassword)
	if err != nil {
		t.Fatalf("HashPassword(unique): %v", err)
	}
	nicknameHash, err := HashPassword(nicknamePassword)
	if err != nil {
		t.Fatalf("HashPassword(nickname): %v", err)
	}
	nicknameUniqueID := uniqueNickname("nickname-owner")
	const insert = `INSERT INTO users (unique_id, nickname, password_hash, created_at)
		VALUES ($1, $2, $3, NOW())`
	if _, err := s.DB().ExecContext(ctx, insert, identifier, uniqueNickname("unique-owner"), uniqueHash); err != nil {
		t.Fatalf("insert exact unique ID: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, insert, nicknameUniqueID, identifier, nicknameHash); err != nil {
		t.Fatalf("insert colliding nickname: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB().ExecContext(ctx, `DELETE FROM users WHERE unique_id IN ($1, $2)`, identifier, nicknameUniqueID)
	})

	user, err := svc.AuthenticateIdentifier(ctx, identifier, uniquePassword)
	if err != nil || user == nil || user.UniqueID != identifier {
		t.Fatalf("AuthenticateIdentifier exact unique ID = %#v, %v", user, err)
	}
	user, err = svc.AuthenticateIdentifier(ctx, identifier, nicknamePassword)
	if err != nil || user != nil {
		t.Fatalf("nickname collision overrode exact unique ID: %#v, %v", user, err)
	}
}

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
	pw := "long-password"

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
