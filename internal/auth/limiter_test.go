package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginFailureLimiterThresholdClearAndExpiry(t *testing.T) {
	start := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     3,
		LockoutDuration: time.Minute,
		FailureTTL:      30 * time.Second,
		MaxEntries:      4,
		Now:             func() time.Time { return now },
	})
	scope := LoginFailureScope("192.0.2.10", "Admin")

	for range 2 {
		limiter.RecordFailure(scope)
	}
	if !limiter.Allowed(scope) {
		t.Fatal("scope locked before threshold")
	}

	limiter.RecordFailure(scope)
	if limiter.Allowed(scope) {
		t.Fatal("scope allowed after threshold")
	}
	now = now.Add(time.Minute)
	if !limiter.Allowed(scope) {
		t.Fatal("scope remains locked after lockout expiry")
	}
	if len(limiter.entries) != 0 {
		t.Fatalf("expired lockout entries = %d, want 0", len(limiter.entries))
	}

	limiter.RecordFailure(scope)
	limiter.Clear(scope)
	if !limiter.Allowed(scope) {
		t.Fatal("scope remains locked after clear")
	}

	limiter.RecordFailure(scope)
	now = now.Add(31 * time.Second)
	if !limiter.Allowed(scope) {
		t.Fatal("expired failure streak still blocks login")
	}
	if len(limiter.entries) != 0 {
		t.Fatalf("expired entries = %d, want 0", len(limiter.entries))
	}
}

func TestLoginFailureLimiterEvictionPreservesActiveLockouts(t *testing.T) {
	start := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     2,
		LockoutDuration: time.Minute,
		FailureTTL:      time.Hour,
		MaxEntries:      2,
		Now:             func() time.Time { return now },
	})
	oldest := LoginFailureScope("tcp", "oldest")
	newer := LoginFailureScope("tcp", "newer")
	third := LoginFailureScope("tcp", "third")

	limiter.RecordFailure(oldest)
	now = now.Add(time.Second)
	limiter.RecordFailure(newer)
	now = now.Add(time.Second)
	limiter.RecordFailure(third)
	if _, ok := limiter.entries[oldest]; ok {
		t.Fatal("oldest incomplete failure streak was not evicted at capacity")
	}
	if len(limiter.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(limiter.entries))
	}

	locked := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     1,
		LockoutDuration: time.Minute,
		FailureTTL:      time.Hour,
		MaxEntries:      1,
		Now:             func() time.Time { return now },
	})
	lockedScope := LoginFailureScope("tcp", "locked")
	locked.RecordFailure(lockedScope)
	locked.RecordFailure(LoginFailureScope("tcp", "new"))
	if locked.Allowed(lockedScope) {
		t.Fatal("active lockout was evicted under capacity pressure")
	}
	if len(locked.entries) != 1 {
		t.Fatalf("locked entries = %d, want 1", len(locked.entries))
	}
}

func TestLoginFailureScopeNormalizesComponents(t *testing.T) {
	if got, want := LoginFailureScope(" TCP ", "Admin"), LoginFailureScope("tcp", "Admin"); got != want {
		t.Fatalf("normalized scopes differ: %q != %q", got, want)
	}
	if LoginFailureScope("tcp", "Admin") == LoginFailureScope("tcp", "admin") {
		t.Fatal("case-sensitive principals share a failure scope")
	}
	if LoginFailureScope("tcp", "Admin") == LoginFailureScope("tcp", " Admin ") {
		t.Fatal("whitespace-distinct principals share a failure scope")
	}
	long := string(make([]byte, 300))
	if LoginFailureScope("tcp", long+"a") == LoginFailureScope("tcp", long+"b") {
		t.Fatal("long principals share a failure scope after truncation")
	}
	if LoginFailureScope("tcp", "admin") == LoginFailureScope("tcp", "other-admin") {
		t.Fatal("different principals share a failure scope")
	}
	if LoginFailureScope("tcp", "admin") == LoginFailureScope("grpc", "admin") {
		t.Fatal("different sources share a failure scope")
	}
}

func TestLoginFailureLimiterSaturationFailsClosedForNewScopes(t *testing.T) {
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     1,
		LockoutDuration: time.Minute,
		FailureTTL:      time.Hour,
		MaxEntries:      2,
		Now:             func() time.Time { return now },
	})
	limiter.RecordFailure(LoginFailureScope("tcp", "first"))
	limiter.RecordFailure(LoginFailureScope("tcp", "second"))
	newScope := LoginFailureScope("tcp", "third")
	if limiter.Allowed(newScope) {
		t.Fatal("new scope was allowed when every bounded entry was actively locked")
	}
	if _, ok := limiter.Reserve(newScope); ok {
		t.Fatal("new scope reserved KDF work when every bounded entry was actively locked")
	}
}

func TestLoginFailureLimiterOverflowClearsWhenReservationMakesSpaceEvictable(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:      2,
		LockoutDuration:  time.Minute,
		FailureTTL:       time.Hour,
		MaxEntries:       2,
		MaxConcurrentKDF: 3,
		Now:              func() time.Time { return now },
	})
	firstScope := LoginFailureScope("tcp:192.0.2.1", "first")
	secondScope := LoginFailureScope("tcp:192.0.2.2", "second")
	thirdScope := LoginFailureScope("tcp:192.0.2.3", "third")

	first, ok := limiter.Reserve(firstScope)
	if !ok {
		t.Fatal("first reservation rejected")
	}
	second, ok := limiter.Reserve(secondScope)
	if !ok {
		t.Fatal("second reservation rejected")
	}
	if _, ok := limiter.Reserve(thirdScope); ok {
		t.Fatal("third reservation bypassed capacity protected by active reservations")
	}
	if limiter.overflowUntil.IsZero() {
		t.Fatal("capacity overflow was not recorded")
	}

	first.Fail() // Below the threshold, but now evictable for a new scope.
	if !limiter.overflowUntil.IsZero() {
		t.Fatal("overflow remained set after a reservation became evictable")
	}
	third, ok := limiter.Reserve(thirdScope)
	if !ok {
		t.Fatal("overflow remained sticky after a reservation became evictable")
	}
	third.Cancel()
	second.Cancel()
}

func TestLoginFailureLimiterReserveProtectsRequestedScopesFromEviction(t *testing.T) {
	start := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	now := start
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:      3,
		LockoutDuration:  time.Minute,
		FailureTTL:       time.Hour,
		MaxEntries:       2,
		MaxConcurrentKDF: 2,
		Now:              func() time.Time { return now },
	})
	sourceScope := LoginFailureScope("tcp:192.0.2.10", "")
	fillerScope := LoginFailureScope("tcp:192.0.2.11", "")
	principalScope := LoginFailureScope("", "admin")

	limiter.RecordFailure(sourceScope)
	now = now.Add(time.Second)
	limiter.RecordFailure(fillerScope)

	attempt, ok := limiter.Reserve(sourceScope, principalScope)
	if !ok {
		t.Fatal("reservation rejected despite an unrelated evictable entry")
	}
	if got := len(limiter.entries); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
	source, ok := limiter.entries[sourceScope]
	if !ok || source.failures != 1 || !source.inFlight {
		t.Fatalf("source entry = %+v, want failures=1 inFlight=true", source)
	}
	if _, ok := limiter.entries[fillerScope]; ok {
		t.Fatal("unrelated filler was not evicted")
	}

	attempt.Cancel()
	source, ok = limiter.entries[sourceScope]
	if !ok || source.failures != 1 || source.inFlight {
		t.Fatalf("source after cancel = %+v, want failures=1 inFlight=false", source)
	}
	if _, ok := limiter.entries[principalScope]; ok {
		t.Fatal("principal reservation remained after cancel")
	}
	if got := len(limiter.entries); got > 2 {
		t.Fatalf("entries = %d, exceeds cap 2", got)
	}
}

func TestLoginFailureLimiterSuccessClearsOnlyAuthenticatedScopes(t *testing.T) {
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     2,
		LockoutDuration: time.Minute,
		FailureTTL:      time.Hour,
		MaxEntries:      8,
	})
	sourceScope := LoginFailureScope("tcp:192.0.2.10", "")
	principalScope := LoginFailureScope("", "authenticated-user")

	limiter.RecordFailure(sourceScope)
	attempt, ok := limiter.Reserve(sourceScope, principalScope)
	if !ok {
		t.Fatal("successful login reservation rejected")
	}
	attempt.Succeed(principalScope)

	if _, ok := limiter.entries[sourceScope]; !ok {
		t.Fatal("successful principal login cleared source failure history")
	}
	if _, ok := limiter.entries[principalScope]; ok {
		t.Fatal("successful principal scope was retained")
	}
}

func TestLoginFailureLimiterSeparatesSourceAndPrincipalDimensions(t *testing.T) {
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     2,
		LockoutDuration: time.Minute,
		FailureTTL:      time.Hour,
		MaxEntries:      16,
	})

	source := LoginFailureScope("192.0.2.1", "")
	limiter.RecordFailure(source, LoginFailureScope("", "first-principal"))
	limiter.RecordFailure(source, LoginFailureScope("", "second-principal"))
	if _, ok := limiter.Reserve(source, LoginFailureScope("", "rotated-principal")); ok {
		t.Fatal("principal rotation bypassed source-wide lockout")
	}

	other := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     2,
		LockoutDuration: time.Minute,
		FailureTTL:      time.Hour,
		MaxEntries:      16,
	})
	principal := LoginFailureScope("", "shared-principal")
	other.RecordFailure(LoginFailureScope("192.0.2.1", ""), principal)
	other.RecordFailure(LoginFailureScope("192.0.2.2", ""), principal)
	if _, ok := other.Reserve(LoginFailureScope("192.0.2.3", ""), principal); ok {
		t.Fatal("source rotation bypassed principal-wide lockout")
	}
}

func TestLoginFailureLimiterReservationBoundsConcurrentKDF(t *testing.T) {
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:      100,
		LockoutDuration:  time.Minute,
		FailureTTL:       time.Minute,
		MaxEntries:       8,
		MaxConcurrentKDF: 2,
	})

	start := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var attempted atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			scope := LoginFailureScope("tcp", string(rune('a'+i)))
			attempt, ok := limiter.Reserve(scope)
			attempted.Add(1)
			if !ok {
				return
			}
			calls.Add(1) // This stands in for entering the password verifier.
			<-release
			attempt.Fail()
		}(i)
	}
	close(start)
	deadline := time.Now().Add(time.Second)
	for (calls.Load() < 2 || attempted.Load() < 16) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 2 || attempted.Load() != 16 {
		t.Fatalf("concurrent verifier calls = %d attempts = %d, want cap 2 across 16 attempts", got, attempted.Load())
	}
	close(release)
	wg.Wait()

	sameScope, ok := limiter.Reserve(LoginFailureScope("tcp", "admin"))
	if !ok {
		t.Fatal("first same-scope reservation rejected")
	}
	if _, ok := limiter.Reserve(LoginFailureScope("tcp", "admin")); ok {
		t.Fatal("same-scope concurrent reservation accepted")
	}
	sameScope.Succeed(LoginFailureScope("tcp", "admin"))
}

func TestLoginFailureLimiterBoundsKDFAcrossTransportScopes(t *testing.T) {
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:      100,
		LockoutDuration:  time.Minute,
		FailureTTL:       time.Minute,
		MaxEntries:       16,
		MaxConcurrentKDF: 2,
	})

	transports := []string{"tcp", "query", "ssh", "grpc", "ws"}
	start := make(chan struct{})
	release := make(chan struct{})
	var entered atomic.Int32
	var wg sync.WaitGroup
	for _, transport := range transports {
		wg.Add(1)
		go func(transport string) {
			defer wg.Done()
			<-start
			attempt, ok := limiter.Reserve(
				LoginFailureScope(transport+":192.0.2.10", ""),
				LoginFailureScope("", transport+"-principal"),
			)
			if !ok {
				return
			}
			entered.Add(1) // Stands in for entering the transport's verifier.
			<-release
			attempt.Cancel()
		}(transport)
	}
	close(start)
	deadline := time.Now().Add(time.Second)
	for entered.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := entered.Load(); got != 2 {
		t.Fatalf("concurrent verifier entries = %d, want process-wide cap 2", got)
	}
	close(release)
	wg.Wait()
}

func TestLoginFailureLimiterConcurrentAccessRemainsBounded(t *testing.T) {
	limiter := NewLoginFailureLimiter(LoginFailureLimiterConfig{
		MaxFailures:     1000,
		LockoutDuration: time.Minute,
		FailureTTL:      time.Minute,
		MaxEntries:      32,
	})

	var wg sync.WaitGroup
	for i := range 256 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			scope := LoginFailureScope("tcp", string(rune(i)))
			limiter.RecordFailure(scope)
			_ = limiter.Allowed(scope)
		}(i)
	}
	wg.Wait()
	if len(limiter.entries) > 32 {
		t.Fatalf("entries = %d, cap = 32", len(limiter.entries))
	}
}
