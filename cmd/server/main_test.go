package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestPermissionAuditLoopRecoversEachInvocation(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time, 1)
	secondCall := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		defer close(done)
		runPermissionAuditLoop(ctx, zap.New(core), ticks, func(context.Context, time.Time) (int64, error) {
			if calls.Add(1) == 1 {
				panic(permissionAuditPanic{secret: "audit-panic-secret"})
			}
			close(secondCall)
			return 0, nil
		})
	}()
	ticks <- time.Now()
	select {
	case <-secondCall:
	case <-time.After(time.Second):
		t.Fatal("permission audit loop did not continue after panic")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("permission audit loop did not stop after cancellation")
	}
	entries := observed.FilterMessage("permission audit compression panic").All()
	if len(entries) != 1 {
		t.Fatalf("panic log count = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if got := fields["panic_type"]; got != "main.permissionAuditPanic" {
		t.Fatalf("panic type = %#v, want main.permissionAuditPanic", got)
	}
	if stack, ok := fields["stack"].(string); !ok || stack == "" {
		t.Fatalf("panic log is missing stack: %#v", fields)
	}
	if strings.Contains(entries[0].Message+fmt.Sprint(fields), "audit-panic-secret") {
		t.Fatalf("panic log leaked recovered value: %#v", fields)
	}
}

type permissionAuditPanic struct{ secret string }
