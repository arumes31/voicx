package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"voicx/internal/store"
)

// ErrUserExists is returned by RegisterUser when a user with the same unique ID
// or nickname already exists in the database.
var ErrUserExists = errors.New("user already exists")

// ErrPasswordTooShort is returned for new registrations below the minimum
// password length. Existing short password hashes remain login-compatible.
var ErrPasswordTooShort = errors.New("password must be at least 12 bytes")

const minRegistrationPasswordBytes = 12

// ErrUserNotFound is returned by the Authenticate* methods when no user with the
// given unique ID exists in the database.
var ErrUserNotFound = errors.New("user not found")

// AuthService provides user registration and authentication (password and
// challenge-response) backed by the PostgreSQL store. Passwords are verified
// on every call: there is deliberately NO positive-result cache — a cache
// keyed only by unique ID would let any password through after one
// successful login (see the BUG-1 regression).
type AuthService struct {
	store            *store.Store
	logger           *zap.Logger
	verifyPassword   func(password, encodedHash string) error
	lookupIdentifier func(context.Context, string) (passwordCredential, error)
}

type passwordCredential struct {
	user User
	hash string
}

// New constructs an AuthService backed by the given store and logger.
func New(s *store.Store, logger *zap.Logger) *AuthService {
	return &AuthService{store: s, logger: logger, verifyPassword: VerifyPassword}
}

// RecordLastIP encrypts the address at column level before persistence.
func (a *AuthService) RecordLastIP(ctx context.Context, userID int64, ip string) error {
	return a.store.SetUserPII(ctx, userID, nil, &ip)
}

// RegisterUser generates a new Ed25519 identity key pair, derives the unique
// ID, hashes the password with Argon2id, and inserts a new row into the users
// table. It returns the unique ID of the newly created user. If a user with the
// same unique ID or nickname already exists, ErrUserExists is returned.
func (a *AuthService) RegisterUser(ctx context.Context, nickname, password string) (string, error) {
	if err := validateRegistrationPassword(password); err != nil {
		return "", err
	}

	pubPEM, _, err := GenerateIdentityKeyPair()
	if err != nil {
		return "", fmt.Errorf("generating identity key pair: %w", err)
	}

	uniqueID, err := UniqueIDFromPublicKey(pubPEM)
	if err != nil {
		return "", fmt.Errorf("deriving unique id: %w", err)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}

	const q = `INSERT INTO users (unique_id, nickname, password_hash, public_key, created_at)
	          VALUES ($1, $2, $3, $4, NOW())`
	_, err = a.store.DB().ExecContext(ctx, q, uniqueID, nickname, hash, pubPEM)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrUserExists
		}
		return "", fmt.Errorf("inserting user: %w", err)
	}

	if a.logger != nil {
		a.logger.Info("user registered",
			zap.String("unique_id", uniqueID),
			zap.String("nickname", nickname),
		)
	}
	return uniqueID, nil
}

func validateRegistrationPassword(password string) error {
	if len(password) < minRegistrationPasswordBytes {
		return ErrPasswordTooShort
	}
	if len(password) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

// AuthenticatePassword looks up the user by unique ID and verifies the supplied
// password against the stored Argon2id hash. It returns true on success and
// false (with a nil error) on a password mismatch. Every call verifies.
func (a *AuthService) AuthenticatePassword(ctx context.Context, uniqueID, password string) (bool, error) {
	var hash string
	const q = `SELECT password_hash FROM users WHERE unique_id = $1`
	err := a.store.DB().QueryRowContext(ctx, q, uniqueID).Scan(&hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.verifyUnknownPassword(password)
			return false, ErrUserNotFound
		}
		return false, fmt.Errorf("querying user password hash: %w", err)
	}
	return a.verifyCredential(password, hash), nil
}

// AuthenticateChallenge looks up the user's public key by unique ID and
// verifies the Ed25519 signature over the challenge. It returns true on success
// and false (with a nil error) on a signature mismatch. Every call verifies.
func (a *AuthService) AuthenticateChallenge(ctx context.Context, uniqueID string, challenge, signature []byte) (bool, error) {
	var pubPEM string
	const q = `SELECT public_key FROM users WHERE unique_id = $1`
	err := a.store.DB().QueryRowContext(ctx, q, uniqueID).Scan(&pubPEM)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrUserNotFound
		}
		return false, fmt.Errorf("querying user public key: %w", err)
	}
	if pubPEM == "" {
		return false, nil
	}

	if err := VerifyChallenge(pubPEM, challenge, signature); err != nil {
		return false, nil
	}
	return true, nil
}

// User holds the identity fields the server needs after a successful
// authentication.
type User struct {
	ID       int64
	UniqueID string
	Nickname string
	IsAdmin  bool
}

// LookupUser returns the user row for the given unique ID. It returns
// ErrUserNotFound when no such user exists.
func (a *AuthService) LookupUser(ctx context.Context, uniqueID string) (*User, error) {
	var u User
	const q = `SELECT id, unique_id, COALESCE(nickname, ''), is_admin FROM users WHERE unique_id = $1`
	err := a.store.DB().QueryRowContext(ctx, q, uniqueID).
		Scan(&u.ID, &u.UniqueID, &u.Nickname, &u.IsAdmin)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("querying user: %w", err)
	}
	return &u, nil
}

// Ban describes an active ban row from the bans table. ExpiresAt is the zero
// time for permanent bans.
type Ban struct {
	ID        int64
	Type      int // 0=IP, 1=unique_id, 2=nickname
	Value     string
	Reason    string
	ExpiresAt time.Time
}

// LookupActiveBan returns the first active, server-wide ban matching the
// given unique ID (ban_type 1) or IP address (ban_type 0). Expired bans
// (expires_at in the past) and channel-scoped bans are ignored; a NULL
// expires_at means the ban is permanent. It returns (nil, nil) when no ban
// matches.
func (a *AuthService) LookupActiveBan(ctx context.Context, uniqueID, ip string) (*Ban, error) {
	const q = `SELECT id, ban_type, value, COALESCE(reason, ''), expires_at
	          FROM bans
	          WHERE channel_id IS NULL
	            AND ((ban_type = 1 AND value = $1) OR (ban_type = 0 AND value = $2))
	            AND (expires_at IS NULL OR expires_at > NOW())
	          ORDER BY id LIMIT 1`
	var b Ban
	var expires sql.NullTime
	err := a.store.DB().QueryRowContext(ctx, q, uniqueID, ip).
		Scan(&b.ID, &b.Type, &b.Value, &b.Reason, &expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying active ban: %w", err)
	}
	if expires.Valid {
		b.ExpiresAt = expires.Time
	}
	return &b, nil
}

// AuthenticateNickname looks up a user by nickname and verifies the supplied
// password against the stored Argon2id hash. It returns the user on success,
// (nil, nil) on a password mismatch, and ErrUserNotFound when no user has
// the nickname. Migration 023 enforces nickname uniqueness for registered
// accounts, so this query has one unambiguous result.
func (a *AuthService) AuthenticateNickname(ctx context.Context, nickname, password string) (*User, error) {
	var (
		u    User
		hash string
	)
	const q = `SELECT id, unique_id, COALESCE(nickname, ''), is_admin, COALESCE(password_hash, '')
	          FROM users WHERE nickname = $1`
	err := a.store.DB().QueryRowContext(ctx, q, nickname).
		Scan(&u.ID, &u.UniqueID, &u.Nickname, &u.IsAdmin, &hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.verifyUnknownPassword(password)
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("querying user by nickname: %w", err)
	}
	if !a.verifyCredential(password, hash) {
		return nil, nil
	}
	return &u, nil
}

// AuthenticateIdentifier authenticates either a unique ID or nickname with
// unique-ID precedence. It performs exactly one real or dummy Argon2
// verification for each external password attempt.
func (a *AuthService) AuthenticateIdentifier(ctx context.Context, identifier, password string) (*User, error) {
	lookup := a.lookupIdentifier
	if lookup == nil {
		lookup = a.lookupCredentialByIdentifier
	}
	credential, err := lookup(ctx, identifier)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.verifyUnknownPassword(password)
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("querying user credential: %w", err)
	}
	if !a.verifyCredential(password, credential.hash) {
		return nil, nil
	}
	return &credential.user, nil
}

func (a *AuthService) lookupCredentialByIdentifier(ctx context.Context, identifier string) (passwordCredential, error) {
	var credential passwordCredential
	const q = `SELECT id, unique_id, COALESCE(nickname, ''), is_admin, COALESCE(password_hash, '')
	          FROM users
	          WHERE unique_id = $1 OR nickname = $1
	          ORDER BY CASE WHEN unique_id = $1 THEN 0 ELSE 1 END
	          LIMIT 1`
	err := a.store.DB().QueryRowContext(ctx, q, identifier).Scan(
		&credential.user.ID,
		&credential.user.UniqueID,
		&credential.user.Nickname,
		&credential.user.IsAdmin,
		&credential.hash,
	)
	return credential, err
}

func (a *AuthService) verifyCredential(password, hash string) bool {
	if hash == "" {
		a.verifyUnknownPassword(password)
		return false
	}
	return a.passwordVerifier()(password, hash) == nil
}

func (a *AuthService) verifyUnknownPassword(password string) {
	// A match is deliberately not accepted: the fixed hash only equalizes the
	// Argon2 work performed for unknown accounts and wrong passwords.
	_ = a.passwordVerifier()(password, dummyPasswordHash)
}

func (a *AuthService) passwordVerifier() func(password, encodedHash string) error {
	if a.verifyPassword == nil {
		return VerifyPassword
	}
	return a.verifyPassword
}

// LookupUserByPublicKey returns the user whose stored public key matches, or
// ErrUserNotFound. Used to resolve accounts whose identity key was bound via
// a nickname login (the account's unique ID stays canonical).
func (a *AuthService) LookupUserByPublicKey(ctx context.Context, publicKey string) (*User, error) {
	var u User
	const q = `SELECT id, unique_id, COALESCE(nickname, ''), is_admin FROM users WHERE public_key = $1`
	err := a.store.DB().QueryRowContext(ctx, q, publicKey).
		Scan(&u.ID, &u.UniqueID, &u.Nickname, &u.IsAdmin)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("querying user by public key: %w", err)
	}
	return &u, nil
}

// BindPublicKey records a client's identity public key on an account (after
// a successful nickname login), enabling future challenge-response logins
// with that key.
func (a *AuthService) BindPublicKey(ctx context.Context, userID int64, publicKey string) error {
	const q = `UPDATE users SET public_key = $2 WHERE id = $1`
	if _, err := a.store.DB().ExecContext(ctx, q, userID, publicKey); err != nil {
		return fmt.Errorf("binding public key: %w", err)
	}
	return nil
}

// SetE2EPublicKey stores a user's X25519 public key for E2EE (wave 4b).
func (a *AuthService) SetE2EPublicKey(ctx context.Context, userID int64, publicKey string) error {
	const q = `UPDATE users SET e2e_public_key = $2 WHERE id = $1`
	if _, err := a.store.DB().ExecContext(ctx, q, userID, publicKey); err != nil {
		return fmt.Errorf("storing e2e public key: %w", err)
	}
	return nil
}

// GetE2EPublicKey returns a user's X25519 public key ("" when never
// published, auth.ErrUserNotFound for unknown users).
func (a *AuthService) GetE2EPublicKey(ctx context.Context, uniqueID string) (string, error) {
	const q = `SELECT COALESCE(e2e_public_key, '') FROM users WHERE unique_id = $1`
	var key string
	err := a.store.DB().QueryRowContext(ctx, q, uniqueID).Scan(&key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrUserNotFound
		}
		return "", fmt.Errorf("loading e2e public key: %w", err)
	}
	return key, nil
}

// GenerateChallenge returns 32 cryptographically random bytes for use in
// challenge-response authentication.
func GenerateChallenge() ([]byte, error) {
	c := make([]byte, 32)
	if _, err := rand.Read(c); err != nil {
		return nil, fmt.Errorf("generating challenge: %w", err)
	}
	return c, nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation produced by the lib/pq driver.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		code := string(pqErr.Code)
		return code == "23505"
	}
	return false
}
