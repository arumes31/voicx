package query

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startSSHQuery starts an SSH ServerQuery front end on an ephemeral port and
// returns its address.
func startSSHQuery(t *testing.T, backend Backend) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	base := New("", nil, backend)
	srv := NewSSH(addr, filepath.Join(t.TempDir(), "host.key"), base)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		<-errCh
	})
	return addr
}

// TestSSHCloseWithActiveSession verifies shutdown does not wait for an idle
// session's read timeout (224).
func TestSSHCloseWithActiveSession(t *testing.T) {
	backend := newFakeBackend()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	base := New("", nil, backend)
	srv := NewSSH(addr, filepath.Join(t.TempDir(), "host.key"), base)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(context.Background()) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "admin-uid",
		Auth:            []ssh.AuthMethod{ssh.Password("pw")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		_ = srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked on an idle session")
	}
	<-errCh
}

// dialSSH opens an authenticated SSH client connection.
func dialSSH(t *testing.T, addr, user, password string) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestSSHExec verifies a one-shot command over SSH runs pre-authenticated
// against the same backend as the TCP port (224).
func TestSSHExec(t *testing.T) {
	backend := newFakeBackend()
	addr := startSSHQuery(t, backend)

	client := dialSSH(t, addr, "admin-uid", "pw")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	out, err := sess.Output("clientlist")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "client_unique_identifier=admin-uid") {
		t.Fatalf("clientlist output = %q", text)
	}
	if !strings.Contains(text, "error id=0 msg=ok") {
		t.Fatalf("missing terminator: %q", text)
	}
}

// TestSSHShell verifies the interactive loop: banner, several commands, quit
// (224).
func TestSSHShell(t *testing.T) {
	backend := newFakeBackend()
	addr := startSSHQuery(t, backend)

	client := dialSSH(t, addr, "admin-uid", "pw")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	r := bufio.NewReader(stdout)
	if line := readLine(t, r); !strings.HasPrefix(line, "VOICX ServerQuery") {
		t.Fatalf("banner = %q", line)
	}
	_ = readLine(t, r) // hint

	if _, err := stdin.Write([]byte("serverinfo\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if line := readLine(t, r); !strings.Contains(line, "virtualserver_name=voicx\\stest") {
		t.Fatalf("serverinfo = %q", line)
	}
	if line := readLine(t, r); line != "error id=0 msg=ok" {
		t.Fatalf("serverinfo terminator = %q", line)
	}

	if _, err := stdin.Write([]byte("quit\n")); err != nil {
		t.Fatalf("write quit: %v", err)
	}
	if line := readLine(t, r); line != "error id=0 msg=ok" {
		t.Fatalf("quit = %q", line)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("session wait: %v", err)
	}
}

// TestSSHRejectsNonAdminAndBadPassword verifies the SSH transport enforces the
// same admin-only credential rule as the TCP port (224).
func TestSSHRejectsNonAdminAndBadPassword(t *testing.T) {
	backend := newFakeBackend()
	addr := startSSHQuery(t, backend)

	for _, tc := range []struct{ user, password string }{
		{"user-uid", "pw"},    // valid credentials, not an admin
		{"admin-uid", "nope"}, // wrong password
		{"ghost", "pw"},       // unknown account
	} {
		client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
			User:            tc.user,
			Auth:            []ssh.AuthMethod{ssh.Password(tc.password)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		})
		if err == nil {
			_ = client.Close()
			t.Fatalf("ssh login accepted for %s", tc.user)
		}
	}
}

// TestSSHHostKeyIsStable verifies the generated host key is reused, so clients
// do not see a changed host identity after a restart (224).
func TestSSHHostKeyIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.key")
	first, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("create host key: %v", err)
	}
	second, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("reload host key: %v", err)
	}
	if ssh.FingerprintSHA256(first.PublicKey()) != ssh.FingerprintSHA256(second.PublicKey()) {
		t.Fatal("host key changed between starts")
	}
}
