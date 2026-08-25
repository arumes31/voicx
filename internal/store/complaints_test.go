package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// testDedicatedDBStore creates a fresh scratch database only when the
// operator explicitly supplied a test URL. It never falls back to the shared
// developer database, so broad operations such as DeleteAllComplaints stay
// isolated even when the supplied database has existing data.
func testDedicatedDBStore(t *testing.T) *Store {
	t.Helper()
	base := os.Getenv("VOICX_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("VOICX_TEST_DATABASE_URL is required for dedicated DB tests")
	}
	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("opening dedicated DB admin connection: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("pinging dedicated DB: %v", err)
	}
	name := fmt.Sprintf("voicx_9a_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		_ = admin.Close()
		t.Fatalf("creating dedicated scratch database: %v", err)
	}
	dropScratch := func() error {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dropCancel()
		_, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+name)
		return err
	}
	u, err := url.Parse(base)
	if err != nil {
		if dropErr := dropScratch(); dropErr != nil {
			t.Errorf("dropping dedicated scratch database after URL parse failure: %v", dropErr)
		}
		_ = admin.Close()
		t.Fatalf("parsing dedicated DB URL: %v", err)
	}
	u.Path = "/" + name
	u.RawPath = ""
	s, err := New(u.String(), testLogger(), 8, 2, time.Minute)
	if err != nil {
		if dropErr := dropScratch(); dropErr != nil {
			t.Errorf("dropping dedicated scratch database after open failure: %v", dropErr)
		}
		_ = admin.Close()
		t.Fatalf("opening dedicated scratch database: %v", err)
	}
	if err := s.Migrate(); err != nil {
		_ = s.Close()
		if dropErr := dropScratch(); dropErr != nil {
			t.Errorf("dropping dedicated scratch database after migration failure: %v", dropErr)
		}
		_ = admin.Close()
		t.Fatalf("migrating dedicated scratch database: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		if err := dropScratch(); err != nil {
			t.Errorf("dropping dedicated scratch database %s: %v", name, err)
		}
		_ = admin.Close()
	})
	return s
}

func TestComplaintsDedicatedDB(t *testing.T) {
	s := testDedicatedDBStore(t)
	ctx := context.Background()
	stamp := fmt.Sprintf("complaint-test-%d", time.Now().UnixNano())
	reporter := stamp + "-reporter"
	targets := []string{stamp + "-target-a", stamp + "-target-b"}
	t.Cleanup(func() {
		for _, target := range targets {
			if _, err := s.DeleteComplaintsAgainst(context.Background(), target, reporter); err != nil {
				t.Errorf("cleanup complaints for %q: %v", target, err)
			}
		}
	})

	if err := s.AddComplaint(ctx, reporter, targets[0], "first reason"); err != nil {
		t.Fatalf("add first complaint: %v", err)
	}
	if err := s.AddComplaint(ctx, reporter, targets[1], "second reason"); err != nil {
		t.Fatalf("add second complaint: %v", err)
	}
	complaints, err := s.ListComplaints(ctx)
	if err != nil {
		t.Fatalf("list complaints: %v", err)
	}
	var ours []Complaint
	for _, complaint := range complaints {
		if complaint.Reporter == reporter {
			ours = append(ours, complaint)
		}
	}
	if len(ours) != 2 {
		t.Fatalf("listed %d test complaints, want 2", len(ours))
	}
	if ours[0].ID >= ours[1].ID {
		t.Fatalf("complaints are not oldest first: ids %d, %d", ours[0].ID, ours[1].ID)
	}
	for index, want := range []struct{ target, reason string }{
		{targets[0], "first reason"},
		{targets[1], "second reason"},
	} {
		got := ours[index]
		if got.Reporter != reporter || got.Target != want.target || got.Reason != want.reason || got.CreatedAt.IsZero() {
			t.Fatalf("complaint %d = %+v, want reporter=%q target=%q reason=%q and created_at", index, got, reporter, want.target, want.reason)
		}
	}
	if err := s.DeleteComplaint(ctx, ours[0].ID); err != nil {
		t.Fatalf("delete complaint: %v", err)
	}
	complaints, err = s.ListComplaints(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	for _, complaint := range complaints {
		if complaint.ID == ours[0].ID {
			t.Fatal("deleted complaint remains listed")
		}
	}

	limitedReporter := stamp + "-limited"
	limitTargets := make([]string, 0, MaxOpenComplaints)
	for i := 0; i < MaxOpenComplaints; i++ {
		target := fmt.Sprintf("%s-limit-%d", stamp, i)
		limitTargets = append(limitTargets, target)
		if err := s.AddComplaint(ctx, limitedReporter, target, "limit"); err != nil {
			t.Fatalf("add complaint %d before limit: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for _, target := range limitTargets {
			if _, err := s.DeleteComplaintsAgainst(context.Background(), target, limitedReporter); err != nil {
				t.Errorf("cleanup limited complaints for %q: %v", target, err)
			}
		}
	})
	if err := s.AddComplaint(ctx, limitedReporter, stamp+"-one-too-many", "limit"); !errors.Is(err, ErrComplaintLimit) {
		t.Fatalf("open complaint limit error = %v, want ErrComplaintLimit", err)
	}
}

func TestDeleteAllComplaintsDedicatedDB(t *testing.T) {
	s := testDedicatedDBStore(t)
	ctx := context.Background()
	for _, reporter := range []string{"delete-all-first", "delete-all-second"} {
		if err := s.AddComplaint(ctx, reporter, "delete-all-target", "delete all"); err != nil {
			t.Fatalf("AddComplaint(%q): %v", reporter, err)
		}
	}
	if err := s.DeleteAllComplaints(ctx); err != nil {
		t.Fatalf("DeleteAllComplaints: %v", err)
	}
	complaints, err := s.ListComplaints(ctx)
	if err != nil {
		t.Fatalf("ListComplaints after DeleteAllComplaints: %v", err)
	}
	if len(complaints) != 0 {
		t.Fatalf("complaints after DeleteAllComplaints = %d, want 0", len(complaints))
	}
}

func TestComplaintLimitIsAtomicAcrossConcurrentCallsDedicatedDB(t *testing.T) {
	s := testDedicatedDBStore(t)
	ctx := context.Background()
	reporter := fmt.Sprintf("complaint-concurrent-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, err := s.DB().ExecContext(context.Background(), `DELETE FROM complaints WHERE reporter = $1`, reporter); err != nil {
			t.Errorf("cleaning concurrent complaints: %v", err)
		}
	})

	const attempts = MaxOpenComplaints * 3
	start := make(chan struct{})
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- s.AddComplaint(ctx, reporter, fmt.Sprintf("%s-target-%d", reporter, i), "concurrent limit")
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	accepted, limited := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrComplaintLimit):
			limited++
		default:
			t.Fatalf("concurrent AddComplaint: %v", err)
		}
	}
	if accepted != MaxOpenComplaints || limited != attempts-MaxOpenComplaints {
		t.Fatalf("concurrent complaint results accepted=%d limited=%d, want %d/%d", accepted, limited, MaxOpenComplaints, attempts-MaxOpenComplaints)
	}
	var persisted int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM complaints WHERE reporter = $1`, reporter).Scan(&persisted); err != nil {
		t.Fatalf("count persisted concurrent complaints: %v", err)
	}
	if persisted != MaxOpenComplaints {
		t.Fatalf("persisted concurrent complaints = %d, want %d", persisted, MaxOpenComplaints)
	}
}
