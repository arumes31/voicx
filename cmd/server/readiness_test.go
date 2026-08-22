package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryOnceRetriesAnyFirstError(t *testing.T) {
	var calls atomic.Int32
	err := retryOnce(t.Context(), time.Millisecond, func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("arbitrary first failure")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryOnce: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("check calls = %d, want 2", got)
	}
}

func TestRetryOnceReturnsSecondFailureAndHonorsCancellation(t *testing.T) {
	var calls atomic.Int32
	err := retryOnce(t.Context(), time.Millisecond, func(context.Context) error {
		calls.Add(1)
		return errors.New("permanent failure")
	})
	if err == nil || err.Error() != "permanent failure" {
		t.Fatalf("retryOnce error = %v, want second permanent failure", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("permanent check calls = %d, want 2", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls.Store(0)
	err = retryOnce(ctx, time.Second, func(context.Context) error {
		calls.Add(1)
		return errors.New("first failure")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("cancelled check calls = %d, want 0", got)
	}
}

func TestReadinessCheckerRunsAllComponentsAndRequiresOnlyRequired(t *testing.T) {
	var postgresCalls, redisCalls, storageCalls atomic.Int32
	var order []string
	var observationsMu sync.Mutex
	observations := map[string]string{}
	checker := newReadinessChecker([]readinessComponent{
		{
			name: "postgres", required: true,
			check: func(ctx context.Context) error {
				order = append(order, "postgres")
				return retryOnce(ctx, time.Millisecond, func(context.Context) error {
					if postgresCalls.Add(1) == 1 {
						return errors.New("first postgres failure")
					}
					return nil
				})
			},
		},
		{
			name: "redis", required: false,
			check: func(context.Context) error {
				order = append(order, "redis")
				redisCalls.Add(1)
				return errors.New("redis unavailable")
			},
		},
		{
			name: "storage", required: true,
			check: func(context.Context) error {
				order = append(order, "storage")
				storageCalls.Add(1)
				return nil
			},
		},
	}, func(component, result string, _ time.Duration) {
		observationsMu.Lock()
		observations[component] = result
		observationsMu.Unlock()
	})
	if err := checker(t.Context()); err != nil {
		t.Fatalf("optional Redis failure made readiness fail: %v", err)
	}
	if postgresCalls.Load() != 2 || redisCalls.Load() != 1 || storageCalls.Load() != 1 {
		t.Fatalf("component calls = postgres %d, redis %d, storage %d", postgresCalls.Load(), redisCalls.Load(), storageCalls.Load())
	}
	if len(order) != 3 || order[0] != "postgres" || order[1] != "storage" || order[2] != "redis" {
		t.Fatalf("component order = %v, want postgres/storage/redis", order)
	}
	observationsMu.Lock()
	defer observationsMu.Unlock()
	if got := observations["postgres"]; got != "ok" {
		t.Fatalf("postgres result = %q, want ok", got)
	}
	if got := observations["redis"]; got != "error" {
		t.Fatalf("redis result = %q, want error", got)
	}
	if got := observations["storage"]; got != "ok" {
		t.Fatalf("storage result = %q, want ok", got)
	}
}

func TestReadinessCheckerReturnsRequiredErrorsAfterAllChecks(t *testing.T) {
	postgresErr := errors.New("postgres down")
	storageErr := errors.New("storage down")
	var storageCalls atomic.Int32
	checker := newReadinessChecker([]readinessComponent{
		{name: "postgres", required: true, check: func(context.Context) error { return postgresErr }},
		{name: "redis", required: false, check: func(context.Context) error { return errors.New("redis down") }},
		{name: "storage", required: true, check: func(context.Context) error {
			storageCalls.Add(1)
			return storageErr
		}},
	}, nil)
	err := checker(t.Context())
	if err == nil || !errors.Is(err, postgresErr) || !errors.Is(err, storageErr) {
		t.Fatalf("required readiness error = %v, want postgres and storage failures", err)
	}
	if storageCalls.Load() != 1 {
		t.Fatal("storage was not checked after required Postgres failure")
	}
}

func TestOptionalReadinessFailureCannotMakeHealthyRequirementsFail(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var order []string
	var observationsMu sync.Mutex
	observations := map[string]string{}
	checker := newReadinessChecker([]readinessComponent{
		{name: "postgres", required: true, check: func(context.Context) error {
			order = append(order, "postgres")
			return nil
		}},
		{name: "storage", required: true, check: func(context.Context) error {
			order = append(order, "storage")
			return nil
		}},
		{name: "redis", required: false, check: func(context.Context) error {
			order = append(order, "redis")
			cancel() // Simulate an optional probe exhausting the shared deadline.
			return ctx.Err()
		}},
	}, func(component, result string, _ time.Duration) {
		observationsMu.Lock()
		observations[component] = result
		observationsMu.Unlock()
	})
	if err := checker(ctx); err != nil {
		t.Fatalf("optional Redis deadline made readiness fail: %v", err)
	}
	if len(order) != 3 || order[0] != "postgres" || order[1] != "storage" || order[2] != "redis" {
		t.Fatalf("component order = %v, want postgres/storage/redis", order)
	}
	observationsMu.Lock()
	defer observationsMu.Unlock()
	if observations["storage"] != "ok" || observations["redis"] != "error" {
		t.Fatalf("optional Redis observations = %v, want storage ok and redis error", observations)
	}
}

func TestOptionalRedisReadinessRecoversWithoutReconstruction(t *testing.T) {
	var redisCalls atomic.Int32
	var observationsMu sync.Mutex
	var observations []string
	checker := newReadinessChecker([]readinessComponent{
		{name: "postgres", required: true, check: func(context.Context) error { return nil }},
		{name: "storage", required: true, check: func(context.Context) error { return nil }},
		{name: "redis", required: false, check: func(context.Context) error {
			if redisCalls.Add(1) == 1 {
				return errors.New("redis temporarily unavailable")
			}
			return nil
		}},
	}, func(component, result string, _ time.Duration) {
		if component != "redis" {
			return
		}
		observationsMu.Lock()
		observations = append(observations, result)
		observationsMu.Unlock()
	})
	if err := checker(t.Context()); err != nil {
		t.Fatalf("first optional Redis failure made readiness fail: %v", err)
	}
	if err := checker(t.Context()); err != nil {
		t.Fatalf("recovered optional Redis made readiness fail: %v", err)
	}
	if redisCalls.Load() != 2 {
		t.Fatalf("retained Redis probe calls = %d, want 2", redisCalls.Load())
	}
	observationsMu.Lock()
	defer observationsMu.Unlock()
	if len(observations) != 2 || observations[0] != "error" || observations[1] != "ok" {
		t.Fatalf("Redis observations = %v, want [error ok]", observations)
	}
}

func TestCachedProbeCachesOutcomesAndSingleflightsExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	var calls atomic.Int32
	probe := newCachedProbe(time.Second, func() time.Time { return now }, func() error {
		if calls.Add(1) == 1 {
			return errors.New("storage unavailable")
		}
		return nil
	})
	if err := probe.Check(t.Context()); err == nil {
		t.Fatal("first probe succeeded, want cached failure")
	}
	if err := probe.Check(t.Context()); err == nil || calls.Load() != 1 {
		t.Fatalf("cached failure err/calls = %v/%d, want failure/1", err, calls.Load())
	}
	now = now.Add(time.Second)
	if err := probe.Check(t.Context()); err != nil {
		t.Fatalf("expired probe recovery: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expired calls = %d, want 2", calls.Load())
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var concurrentCalls atomic.Int32
	concurrent := newCachedProbe(time.Second, time.Now, func() error {
		concurrentCalls.Add(1)
		close(entered)
		<-release
		return nil
	})
	leader := make(chan error, 1)
	go func() { leader <- concurrent.Check(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cache leader did not start")
	}
	cancelledCtx, cancel := context.WithCancel(t.Context())
	follower := make(chan error, 1)
	go func() { follower <- concurrent.Check(cancelledCtx) }()
	cancel()
	if err := <-follower; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled follower error = %v, want context.Canceled", err)
	}
	followers := make(chan error, 8)
	for range 8 {
		go func() { followers <- concurrent.Check(t.Context()) }()
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatalf("leader error: %v", err)
	}
	for range 8 {
		if err := <-followers; err != nil {
			t.Fatalf("follower error: %v", err)
		}
	}
	if got := concurrentCalls.Load(); got != 1 {
		t.Fatalf("concurrent storage checks = %d, want 1", got)
	}
}

func TestCachedProbeDoesNotStartOrReturnAfterContextCancellation(t *testing.T) {
	var calls atomic.Int32
	probe := newCachedProbe(time.Second, time.Now, func() error {
		calls.Add(1)
		return nil
	})
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := probe.Check(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cache miss = %v, want context.Canceled", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("cancelled cache miss check calls = %d, want 0", calls.Load())
	}
	if err := probe.Check(t.Context()); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if err := probe.Check(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cache hit = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cancelled cache hit check calls = %d, want 1", calls.Load())
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	blocked := newCachedProbe(time.Second, time.Now, func() error {
		close(entered)
		<-release
		return nil
	})
	ctx, cancelLeader := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() { leader <- blocked.Check(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cancelled leader did not start")
	}
	cancelLeader()
	close(release)
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled leader = %v, want context.Canceled", err)
	}
	if err := blocked.Check(t.Context()); err != nil {
		t.Fatalf("physical leader outcome was not cached: %v", err)
	}
}
