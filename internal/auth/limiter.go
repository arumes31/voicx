package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultLoginFailureMaxFailures is the number of failed attempts before a
	// scope is temporarily locked.
	DefaultLoginFailureMaxFailures = 5
	// DefaultLoginFailureLockout is the duration of a login lockout.
	DefaultLoginFailureLockout = 5 * time.Minute
	// DefaultLoginFailureTTL is how long an incomplete failure streak remains
	// relevant.
	DefaultLoginFailureTTL = 5 * time.Minute
	// DefaultLoginFailureMaxEntries caps the in-memory state retained for
	// unauthenticated callers.
	DefaultLoginFailureMaxEntries = 4096
	// DefaultLoginFailureMaxConcurrentKDF caps concurrent expensive password
	// verifications across all unauthenticated login scopes.
	DefaultLoginFailureMaxConcurrentKDF = 8
)

// LoginFailureLimiterConfig configures a LoginFailureLimiter. Now is injected
// so expiration behavior is deterministic in tests.
type LoginFailureLimiterConfig struct {
	MaxFailures      int
	LockoutDuration  time.Duration
	FailureTTL       time.Duration
	MaxEntries       int
	MaxConcurrentKDF int
	Now              func() time.Time
}

// LoginFailureLimiter bounds failed-login state. It has no background
// goroutine: every operation prunes expired state while holding the mutex.
// Active lockouts are never evicted to make capacity pressure unable to bypass
// an existing lockout.
type LoginFailureLimiter struct {
	mu sync.Mutex

	maxFailures      int
	lockoutDuration  time.Duration
	failureTTL       time.Duration
	maxEntries       int
	maxConcurrentKDF int
	now              func() time.Time
	entries          map[string]loginFailureEntry
	inFlight         int
	overflowUntil    time.Time
}

type loginFailureEntry struct {
	failures    int
	lastFailure time.Time
	lockedUntil time.Time
	inFlight    bool
}

// LoginAttempt holds an atomic reservation for one password verification.
// Exactly one of Succeed, Fail, or Cancel must finish it; repeated calls are
// harmless. Reservations reject concurrent attempts for the same scope before
// they can all begin an expensive KDF.
type LoginAttempt struct {
	limiter *LoginFailureLimiter
	scopes  []string
	once    sync.Once
}

// NewLoginFailureLimiter constructs a bounded login-failure limiter.
func NewLoginFailureLimiter(cfg LoginFailureLimiterConfig) *LoginFailureLimiter {
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = DefaultLoginFailureMaxFailures
	}
	if cfg.LockoutDuration <= 0 {
		cfg.LockoutDuration = DefaultLoginFailureLockout
	}
	if cfg.FailureTTL <= 0 {
		cfg.FailureTTL = DefaultLoginFailureTTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultLoginFailureMaxEntries
	}
	if cfg.MaxConcurrentKDF <= 0 {
		cfg.MaxConcurrentKDF = DefaultLoginFailureMaxConcurrentKDF
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &LoginFailureLimiter{
		maxFailures:      cfg.MaxFailures,
		lockoutDuration:  cfg.LockoutDuration,
		failureTTL:       cfg.FailureTTL,
		maxEntries:       cfg.MaxEntries,
		maxConcurrentKDF: cfg.MaxConcurrentKDF,
		now:              cfg.Now,
		entries:          make(map[string]loginFailureEntry),
	}
}

// LoginFailureScope returns a fixed-size key scoped by transport source and a
// complete, exact principal. Hashing bounds retained attacker-controlled input
// and avoids storing identities or addresses in the limiter map. Principal
// values are case- and whitespace-sensitive because database identities are.
func LoginFailureScope(source, principal string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	digest := sha256.Sum256([]byte(source + "\x00" + principal))
	return hex.EncodeToString(digest[:])
}

// Reserve atomically checks scopes, reserves their per-scope verification slots,
// and acquires a bounded global KDF slot. A false result means no expensive
// password work may be started for that attempt.
func (l *LoginFailureLimiter) Reserve(scopes ...string) (*LoginAttempt, bool) {
	scopes = uniqueLoginScopes(scopes)
	if l == nil || len(scopes) == 0 {
		return &LoginAttempt{}, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneExpiredLocked(now)
	protectedScopes := loginScopeSet(scopes)
	if l.inFlight >= l.maxConcurrentKDF || !l.scopesAvailableLocked(scopes, protectedScopes, now) {
		return nil, false
	}
	newScopes := 0
	for _, scope := range scopes {
		if _, ok := l.entries[scope]; !ok {
			newScopes++
		}
	}
	if !l.makeRoomLocked(now, newScopes, protectedScopes) {
		return nil, false
	}
	for _, scope := range scopes {
		entry := l.entries[scope]
		entry.inFlight = true
		l.entries[scope] = entry
	}
	l.inFlight++
	return &LoginAttempt{limiter: l, scopes: scopes}, true
}

// Succeed releases every reserved scope after a successful verification and
// clears only the scopes that authenticated successfully. Callers must pass
// the authenticated principal scope explicitly: retaining source-scope
// failure history prevents a successful unrelated login from bypassing a
// source-wide lockout.
func (a *LoginAttempt) Succeed(successScopes ...string) {
	a.finish(loginAttemptSuccess, uniqueLoginScopes(successScopes))
}

// Fail records the completed scope as a failed verification.
func (a *LoginAttempt) Fail() { a.finish(loginAttemptFailure, nil) }

// Cancel releases a reservation after a backend failure without counting it as
// a credential failure.
func (a *LoginAttempt) Cancel() { a.finish(loginAttemptCancel, nil) }

type loginAttemptResult uint8

const (
	loginAttemptSuccess loginAttemptResult = iota
	loginAttemptFailure
	loginAttemptCancel
)

func (a *LoginAttempt) finish(result loginAttemptResult, successScopes []string) {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if a.limiter == nil || len(a.scopes) == 0 {
			return
		}

		l := a.limiter
		l.mu.Lock()
		defer l.mu.Unlock()

		now := l.now()
		l.pruneExpiredLocked(now)
		succeeded := make(map[string]struct{}, len(successScopes))
		for _, scope := range successScopes {
			succeeded[scope] = struct{}{}
		}
		finished := false
		for _, scope := range a.scopes {
			entry, ok := l.entries[scope]
			if !ok || !entry.inFlight {
				continue
			}
			finished = true
			entry.inFlight = false

			switch result {
			case loginAttemptSuccess:
				if _, ok := succeeded[scope]; ok {
					delete(l.entries, scope)
				} else if entry.failures == 0 && entry.lockedUntil.IsZero() {
					delete(l.entries, scope)
				} else {
					l.entries[scope] = entry
				}
			case loginAttemptFailure:
				entry.failures++
				entry.lastFailure = now
				if entry.failures >= l.maxFailures {
					entry.failures = 0
					entry.lockedUntil = now.Add(l.lockoutDuration)
				}
				l.entries[scope] = entry
			case loginAttemptCancel:
				if entry.failures == 0 && entry.lockedUntil.IsZero() {
					delete(l.entries, scope)
				} else {
					l.entries[scope] = entry
				}
			}
		}
		if finished {
			l.inFlight--
			l.clearOverflowWhenEvictableLocked(now)
		}
	})
}

// Allowed reports whether a scope may attempt another login. New code should
// use Reserve before starting password work so the check and reservation are
// atomic; this method remains for compatibility and state inspection.
func (l *LoginFailureLimiter) Allowed(scope string) bool {
	if l == nil || scope == "" {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneExpiredLocked(now)
	scopes := []string{scope}
	return l.inFlight < l.maxConcurrentKDF && l.scopesAvailableLocked(scopes, loginScopeSet(scopes), now)
}

// RecordFailure records one failed login attempt for scope. Reserve and
// LoginAttempt.Fail are preferred for password verification paths.
func (l *LoginFailureLimiter) RecordFailure(scopes ...string) {
	scopes = uniqueLoginScopes(scopes)
	if l == nil || len(scopes) == 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneExpiredLocked(now)
	for _, scope := range scopes {
		l.recordFailureLocked(scope, now)
	}
	l.clearOverflowWhenEvictableLocked(now)
}

func (l *LoginFailureLimiter) recordFailureLocked(scope string, now time.Time) {
	entry, ok := l.entries[scope]
	if !ok {
		if !l.makeRoomLocked(now, 1, nil) {
			return
		}
		entry = loginFailureEntry{}
	}

	if entry.inFlight {
		return
	}
	entry.failures++
	entry.lastFailure = now
	if entry.failures >= l.maxFailures {
		entry.failures = 0
		entry.lockedUntil = now.Add(l.lockoutDuration)
	}
	l.entries[scope] = entry
}

// Clear removes all login-failure state for scope after a successful login.
func (l *LoginFailureLimiter) Clear(scopes ...string) {
	scopes = uniqueLoginScopes(scopes)
	if l == nil || len(scopes) == 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, scope := range scopes {
		if entry, ok := l.entries[scope]; ok && entry.inFlight {
			continue
		}
		delete(l.entries, scope)
	}
	l.clearOverflowWhenEvictableLocked(l.now())
}

func (l *LoginFailureLimiter) scopesAvailableLocked(scopes []string, protectedScopes map[string]struct{}, now time.Time) bool {
	newScopes := 0
	for _, scope := range scopes {
		entry, ok := l.entries[scope]
		if ok {
			if entry.inFlight || entry.lockedUntil.After(now) {
				return false
			}
			continue
		}
		newScopes++
	}
	if newScopes == 0 {
		return true
	}
	available := l.maxEntries - len(l.entries)
	for scope, entry := range l.entries {
		if _, protected := protectedScopes[scope]; protected {
			continue
		}
		if !entry.inFlight && !entry.lockedUntil.After(now) {
			available++
		}
	}
	if available >= newScopes {
		l.overflowUntil = time.Time{}
		return true
	}
	// All bounded slots hold active lockouts or KDF work. Fail closed for new
	// scopes until a slot naturally becomes available.
	l.overflowUntil = now.Add(l.lockoutDuration)
	return false
}

func (l *LoginFailureLimiter) pruneExpiredLocked(now time.Time) {
	for scope, entry := range l.entries {
		if entry.inFlight || entry.lockedUntil.After(now) {
			continue
		}
		lockoutExpired := !entry.lockedUntil.IsZero()
		failureExpired := entry.lastFailure.IsZero() ||
			(!entry.lastFailure.After(now) && now.Sub(entry.lastFailure) >= l.failureTTL)
		if lockoutExpired || failureExpired {
			delete(l.entries, scope)
		}
	}
	l.clearOverflowWhenEvictableLocked(now)
}

func (l *LoginFailureLimiter) evictableCapacityLocked(now time.Time) int {
	available := l.maxEntries - len(l.entries)
	for _, entry := range l.entries {
		if !entry.inFlight && !entry.lockedUntil.After(now) {
			available++
		}
	}
	return available
}

func (l *LoginFailureLimiter) clearOverflowWhenEvictableLocked(now time.Time) {
	if l.evictableCapacityLocked(now) > 0 || !l.overflowUntil.After(now) {
		l.overflowUntil = time.Time{}
	}
}

func (l *LoginFailureLimiter) makeRoomLocked(now time.Time, needed int, protectedScopes map[string]struct{}) bool {
	for len(l.entries)+needed > l.maxEntries {
		var (
			oldestScope string
			oldestTime  time.Time
		)
		for scope, entry := range l.entries {
			if _, protected := protectedScopes[scope]; protected {
				continue
			}
			if entry.inFlight || entry.lockedUntil.After(now) {
				continue
			}
			if oldestScope == "" || entry.lastFailure.Before(oldestTime) {
				oldestScope = scope
				oldestTime = entry.lastFailure
			}
		}
		if oldestScope == "" {
			l.overflowUntil = now.Add(l.lockoutDuration)
			return false
		}
		delete(l.entries, oldestScope)
	}
	return true
}

func loginScopeSet(scopes []string) map[string]struct{} {
	if len(scopes) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		set[scope] = struct{}{}
	}
	return set
}

func uniqueLoginScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(scopes))
	unique := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		unique = append(unique, scope)
	}
	return unique
}
