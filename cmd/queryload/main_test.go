package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEscape(t *testing.T) {
	if got, want := escape("a b|c/d\\e\n\t"), `a\sb\pc\/d\\e\n\t`; got != want {
		t.Fatalf("escape = %q, want %q", got, want)
	}
}

func TestReadResponse(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{name: "success after data", body: "clid=1\nerror id=0 msg=ok\n"},
		{name: "server error", body: "error id=256 msg=denied\n", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := readResponse(bufio.NewReader(strings.NewReader(test.body)))
			if (err != nil) != test.want {
				t.Fatalf("readResponse error = %v, want error=%t", err, test.want)
			}
		})
	}
}

func TestDialAndLoginEscapesCredentials(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	login := make(chan string, 1)
	go func() {
		defer close(login)
		_, _ = fmt.Fprintln(server, "banner")
		_, _ = fmt.Fprintln(server, "welcome")
		line, err := bufio.NewReader(server).ReadString('\n')
		if err != nil {
			return
		}
		login <- line
		_, _ = fmt.Fprintln(server, "error id=0 msg=ok")
	}()
	dialed := false
	conn, _, err := dialAndLoginWithDial(options{addr: "ignored", user: "u ser", password: "p|w"}, func(network, address string, timeout time.Duration) (net.Conn, error) {
		if network != "tcp" || address != "ignored" || timeout != 5*time.Second {
			t.Fatalf("dial arguments = %q %q %s", network, address, timeout)
		}
		dialed = true
		return client, nil
	})
	if err != nil {
		t.Fatalf("dialAndLoginWithDial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if !dialed {
		t.Fatal("dial function was not called")
	}
	if got, want := <-login, "login u\\sser p\\pw\n"; got != want {
		t.Fatalf("login = %q, want %q", got, want)
	}
}

func TestRunWithDepsClosesOpenedConnectionsOnSetupFailure(t *testing.T) {
	client, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	tracked := &closeTrackingConn{Conn: client}
	calls := 0
	err := runWithDeps(context.Background(), options{connections: 2, rate: 1, duration: time.Second}, queryloadDeps{
		dialAndLogin: func(options) (net.Conn, *bufio.Reader, error) {
			calls++
			if calls == 1 {
				return tracked, bufio.NewReader(tracked), nil
			}
			return nil, nil, errors.New("injected setup failure")
		},
		newTicker: func(time.Duration) queryloadTicker { t.Fatal("ticker created after setup failure"); return nil },
		now:       time.Now,
		report:    func(options, *result) { t.Fatal("report emitted after setup failure") },
	})
	if err == nil || !strings.Contains(err.Error(), "connection 1") {
		t.Fatalf("runWithDeps error = %v, want connection setup error", err)
	}
	if !tracked.closed.Load() {
		t.Fatal("first successful connection was not closed after later setup failure")
	}
}

func TestRunWithDepsUsesManualTicksAndReports(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	responseWritten := make(chan struct{})
	go func() {
		reader := bufio.NewReader(server)
		if _, err := reader.ReadString('\n'); err == nil {
			if _, err := fmt.Fprintln(server, "error id=0 msg=ok"); err == nil {
				close(responseWritten)
			}
		}
	}()
	ticker := &manualQueryloadTicker{ch: make(chan time.Time, 1)}
	var report bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDeps(ctx, options{connections: 1, rate: 1, duration: time.Hour, command: "clientlist"}, queryloadDeps{
			dialAndLogin: func(options) (net.Conn, *bufio.Reader, error) { return client, bufio.NewReader(client), nil },
			newTicker:    func(time.Duration) queryloadTicker { return ticker },
			now:          time.Now,
			report:       func(o options, r *result) { printReportTo(&report, o, r) },
		})
	}()
	ticker.ch <- time.Unix(0, 0)
	<-responseWritten
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runWithDeps: %v", err)
	}
	if !ticker.stopped.Load() {
		t.Fatal("ticker was not stopped")
	}
	if got := report.String(); !strings.Contains(got, "ok=1") || !strings.Contains(got, "failed=0") {
		t.Fatalf("report = %q, want one successful request", got)
	}
}

func TestRunWithDepsCancellationClosesBlockedConnections(t *testing.T) {
	client, server := net.Pipe()
	tracked := &closeTrackingConn{Conn: client}
	serverRelease := make(chan struct{})
	commandSeen := make(chan struct{})
	go func() {
		defer func() { _ = server.Close() }()
		reader := bufio.NewReader(server)
		if _, err := reader.ReadString('\n'); err == nil {
			close(commandSeen)
			<-serverRelease // Intentionally never write a query response.
		}
	}()
	t.Cleanup(func() { close(serverRelease) })

	ticker := &manualQueryloadTicker{ch: make(chan time.Time, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- runWithDeps(ctx, options{connections: 1, rate: 1, duration: time.Hour, command: "clientlist"}, queryloadDeps{
			dialAndLogin: func(options) (net.Conn, *bufio.Reader, error) { return tracked, bufio.NewReader(tracked), nil },
			newTicker:    func(time.Duration) queryloadTicker { return ticker },
			now:          time.Now,
			report:       func(options, *result) {},
		})
	}()
	ticker.ch <- time.Unix(0, 0)
	<-commandSeen
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWithDeps: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runWithDeps did not return after cancellation while a response was blocked")
	}
	if !tracked.closed.Load() {
		t.Fatal("blocked worker connection was not closed on cancellation")
	}
}

type closeTrackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

type manualQueryloadTicker struct {
	ch      chan time.Time
	stopped atomic.Bool
}

func (t *manualQueryloadTicker) Chan() <-chan time.Time { return t.ch }
func (t *manualQueryloadTicker) Stop()                  { t.stopped.Store(true) }
