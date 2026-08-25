package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	readinessRetryDelay = 100 * time.Millisecond
	storageProbeTTL     = 5 * time.Second
)

// readinessComponent describes one dependency checked by /readyz. Optional
// components are still observed, but only required failures make the process
// unready.
type readinessComponent struct {
	name     string
	required bool
	check    func(context.Context) error
}

// readinessObserver records one bounded observation for each configured
// component. It deliberately carries no error value or dependency identity.
type readinessObserver func(component, result string, duration time.Duration)

// newReadinessChecker evaluates every configured component under the caller's
// context. It never short-circuits: an operator needs to see every dependency
// state from one readiness scrape, while only required failures are returned.
func newReadinessChecker(components []readinessComponent, observe readinessObserver) func(context.Context) error {
	components = append([]readinessComponent(nil), components...)
	ordered := make([]readinessComponent, 0, len(components))
	for _, component := range components {
		if component.required {
			ordered = append(ordered, component)
		}
	}
	for _, component := range components {
		if !component.required {
			ordered = append(ordered, component)
		}
	}
	components = ordered
	return func(ctx context.Context) error {
		var requiredErrors []error
		for _, component := range components {
			started := time.Now()
			var err error
			if component.check == nil {
				err = errors.New("readiness component is not configured")
			} else {
				err = component.check(ctx)
			}
			if observe != nil {
				result := "ok"
				if err != nil {
					result = "error"
				}
				observe(component.name, result, time.Since(started))
			}
			if err != nil && component.required {
				requiredErrors = append(requiredErrors, fmt.Errorf("%s: %w", component.name, err))
			}
		}
		return errors.Join(requiredErrors...)
	}
}

// retryOnce calls check once, waits after any error, then calls it once more.
// It uses the supplied readiness context directly: /readyz owns the only
// deadline, so retrying cannot extend a request beyond its three-second cap.
func retryOnce(ctx context.Context, delay time.Duration, check func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := check(ctx)
	if err == nil {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return err
		}
		return check(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cachedProbe memoizes successful and failed storage checks for a short TTL.
// At expiry, one caller performs the synchronous probe while followers wait
// for its result or leave when their readiness request is cancelled.
type cachedProbe struct {
	mu       sync.Mutex
	check    func() error
	now      func() time.Time
	ttl      time.Duration
	expires  time.Time
	cached   error
	hasCache bool
	inFlight chan struct{}
}

func newCachedProbe(ttl time.Duration, now func() time.Time, check func() error) *cachedProbe {
	if ttl <= 0 {
		ttl = storageProbeTTL
	}
	if now == nil {
		now = time.Now
	}
	return &cachedProbe{check: check, now: now, ttl: ttl}
}

// Check returns a cached probe outcome where possible. The leader invokes the
// synchronous check without holding the mutex; followers never start a second
// filesystem operation for the same expired cache entry.
func (p *cachedProbe) Check(ctx context.Context) error {
	if p == nil || p.check == nil {
		return errors.New("storage readiness probe is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.hasCache && p.now().Before(p.expires) {
		err := p.cached
		p.mu.Unlock()
		return err
	}
	if done := p.inFlight; done != nil {
		p.mu.Unlock()
		select {
		case <-done:
			return p.Check(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	p.inFlight = done
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		p.mu.Lock()
		p.inFlight = nil
		close(done)
		p.mu.Unlock()
		return err
	}

	err := p.check()
	p.mu.Lock()
	p.cached = err
	p.hasCache = true
	p.expires = p.now().Add(p.ttl)
	p.inFlight = nil
	close(done)
	p.mu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}
