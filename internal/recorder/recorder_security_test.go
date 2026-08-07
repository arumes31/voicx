package recorder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"go.uber.org/zap"

	"voicx/internal/webrtc"
)

func waitForSessionCount(t *testing.T, recorder *Recorder, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recorder.SessionCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session count = %d, want %d", recorder.SessionCount(), want)
}

func assertRecordingDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read recording directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed startup left recording artifacts: %v", entries)
	}
}

func releaseAndStop(t *testing.T, recorder *Recorder, cmd *fakeCommand, channelID int64) {
	t.Helper()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cmd.release()
	}()
	if err := recorder.Stop(channelID); err != nil {
		t.Fatalf("Stop(%d): %v", channelID, err)
	}
}

func TestConcurrentStartReservesChannelAtomically(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := &fakeTapRouter{}

	type result struct {
		session *Session
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			<-start
			session, err := recorder.Start(context.Background(), 77, router)
			results <- result{session: session, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var started, rejected int
	for got := range results {
		switch {
		case got.err == nil && got.session != nil:
			started++
		case errors.Is(got.err, ErrAlreadyRecording):
			rejected++
		default:
			t.Fatalf("unexpected concurrent Start result: session=%v err=%v", got.session, got.err)
		}
	}
	if started != 1 || rejected != 1 || recorder.SessionCount() != 1 {
		t.Fatalf("started/rejected/sessions = %d/%d/%d", started, rejected, recorder.SessionCount())
	}
	releaseAndStop(t, recorder, exec.cmd, 77)
}

type multiExec struct {
	mu       sync.Mutex
	commands []*fakeCommand
}

func (e *multiExec) run(_ context.Context, _ string, args ...string) Command {
	output := ""
	if len(args) > 0 {
		output = args[len(args)-1]
	}
	command := newFakeCommand(output)
	e.mu.Lock()
	e.commands = append(e.commands, command)
	e.mu.Unlock()
	return command
}

func TestConcurrentRecordingLimitIsAtomic(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.MaxConcurrent = 3
	recorder := New(cfg, zap.NewNop())
	exec := &multiExec{}
	recorder.Exec = exec.run
	router := &fakeTapRouter{}

	start := make(chan struct{})
	errs := make(chan error, 12)
	var workers sync.WaitGroup
	workers.Add(12)
	for index := range 12 {
		go func() {
			defer workers.Done()
			<-start
			_, err := recorder.Start(context.Background(), int64(100+index), router)
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errs)

	started, rejected := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			started++
		case errors.Is(err, ErrCapacity):
			rejected++
		default:
			t.Fatalf("unexpected capped Start error: %v", err)
		}
	}
	if started != 3 || rejected != 9 || recorder.SessionCount() != 3 {
		t.Fatalf("started/rejected/sessions = %d/%d/%d, want 3/9/3", started, rejected, recorder.SessionCount())
	}
	exec.mu.Lock()
	commands := append([]*fakeCommand(nil), exec.commands...)
	exec.mu.Unlock()
	if len(commands) != 3 {
		t.Fatalf("launched commands = %d, want 3", len(commands))
	}
	for _, command := range commands {
		command.release()
	}
	waitForSessionCount(t, recorder, 0)
}

type contextExec struct {
	mu  sync.Mutex
	ctx context.Context
	cmd *fakeCommand
}

func (e *contextExec) run(ctx context.Context, _ string, args ...string) Command {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ctx = ctx
	output := ""
	if len(args) > 0 {
		output = args[len(args)-1]
	}
	e.cmd = newFakeCommand(output)
	return e.cmd
}

func TestRecordingLifetimeOutlivesRequestContext(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	exec := &contextExec{}
	recorder.Exec = exec.run
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	if _, err := recorder.Start(requestCtx, 8, &fakeTapRouter{}); err != nil {
		t.Fatal(err)
	}
	cancelRequest()

	exec.mu.Lock()
	processCtx := exec.ctx
	cmd := exec.cmd
	exec.mu.Unlock()
	select {
	case <-processCtx.Done():
		t.Fatalf("process context followed request cancellation: %v", processCtx.Err())
	case <-time.After(50 * time.Millisecond):
	}
	if recorder.SessionCount() != 1 {
		t.Fatal("request cancellation removed active recording")
	}
	releaseAndStop(t, recorder, cmd, 8)
}

func TestUnexpectedProcessExitCleansSessionAndArtifacts(t *testing.T) {
	dir := t.TempDir()
	recorder := New(testConfig(dir), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := &fakeTapRouter{}
	session, err := recorder.Start(context.Background(), 9, router)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session.FilePath, []byte("recording"), 0o666); err != nil {
		t.Fatal(err)
	}
	exec.cmd.release()
	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
		t.Fatal("unexpected process exit was not observed")
	}
	waitForSessionCount(t, recorder, 0)
	if _, err := os.Stat(session.sdpPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SDP artifact still exists: %v", err)
	}
	info, err := os.Stat(session.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("output mode = %o, want no group/other access", info.Mode().Perm())
	}
	waitForRouterRemovals(t, router, 1)
}

func TestMissingRecordingOutputIsReported(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	command := newFakeCommand("")
	recorder.Exec = func(context.Context, string, ...string) Command { return command }
	if _, err := recorder.Start(context.Background(), 15, &fakeTapRouter{}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		command.release()
	}()
	err := recorder.Stop(15)
	if err == nil || !strings.Contains(err.Error(), "recording output is missing") {
		t.Fatalf("Stop with missing output = %v", err)
	}
}

func TestReturnedSessionMutationCannotRedirectCleanup(t *testing.T) {
	dir := t.TempDir()
	recorder := New(testConfig(dir), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	session, err := recorder.Start(context.Background(), 16, &fakeTapRouter{})
	if err != nil {
		t.Fatal(err)
	}
	originalSDP := session.sdpPath
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("leave me"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}

	session.ChannelID = 999
	session.FilePath = victim
	session.StartedAt = time.Time{}
	releaseAndStop(t, recorder, exec.cmd, 16)
	if recorder.SessionCount() != 0 {
		t.Fatalf("mutated snapshot stranded %d sessions", recorder.SessionCount())
	}
	if _, err := os.Stat(originalSDP); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned SDP was not removed: %v", err)
	}
	after, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "leave me" {
		t.Fatalf("victim contents changed to %q", contents)
	}
	if runtime.GOOS != "windows" && before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("victim mode changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestRecordingPathsAreUniqueAndNeverOverwrite(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	fixed := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	recorder.now = func() time.Time { return fixed }
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := &fakeTapRouter{}

	first, err := recorder.Start(context.Background(), 10, router)
	if err != nil {
		t.Fatal(err)
	}
	firstCmd := exec.cmd
	releaseAndStop(t, recorder, firstCmd, 10)
	second, err := recorder.Start(context.Background(), 10, router)
	if err != nil {
		t.Fatal(err)
	}
	secondCmd := exec.cmd
	if first.FilePath == second.FilePath {
		t.Fatalf("recording paths collided: %q", first.FilePath)
	}
	exec.mu.Lock()
	args := strings.Join(exec.args, " ")
	exec.mu.Unlock()
	if !strings.Contains(args, " -n ") || strings.Contains(args, " -y ") {
		t.Fatalf("unsafe ffmpeg overwrite args: %q", args)
	}
	releaseAndStop(t, recorder, secondCmd, 10)
}

type failingStartCommand struct {
	stdin *fakeWriteCloser
}

func (c *failingStartCommand) StdinPipe() (io.WriteCloser, error) { return c.stdin, nil }
func (c *failingStartCommand) BindRecordingRoot(*os.Root, string, string, string, string) error {
	return nil
}
func (c *failingStartCommand) CloseBeforeStart() error { return nil }
func (c *failingStartCommand) Start() error            { return errors.New("start failed") }
func (c *failingStartCommand) Wait() error             { return errors.New("unexpected Wait") }
func (c *failingStartCommand) Kill() error             { return nil }

var (
	errBindRecordingRoot = errors.New("bind recording root failed")
	errCloseBoundRoot    = errors.New("close bound root failed")
)

type failingBindCommand struct {
	closeCalled bool
}

func (*failingBindCommand) BindRecordingRoot(*os.Root, string, string, string, string) error {
	return errBindRecordingRoot
}
func (c *failingBindCommand) CloseBeforeStart() error {
	c.closeCalled = true
	return errCloseBoundRoot
}
func (*failingBindCommand) StdinPipe() (io.WriteCloser, error) {
	return nil, errors.New("unexpected StdinPipe")
}
func (*failingBindCommand) Start() error { return errors.New("unexpected Start") }
func (*failingBindCommand) Wait() error  { return errors.New("unexpected Wait") }
func (*failingBindCommand) Kill() error  { return errors.New("unexpected Kill") }

func TestBindRecordingRootFailureClosesCommandResources(t *testing.T) {
	dir := t.TempDir()
	command := &failingBindCommand{}
	recorder := New(testConfig(dir), zap.NewNop())
	recorder.Exec = func(context.Context, string, ...string) Command { return command }

	_, err := recorder.Start(context.Background(), 60, &fakeTapRouter{})
	if !errors.Is(err, errBindRecordingRoot) || !errors.Is(err, errCloseBoundRoot) {
		t.Fatalf("Start error = %v, want joined bind and cleanup errors", err)
	}
	if !command.closeCalled {
		t.Fatal("BindRecordingRoot failure did not call CloseBeforeStart")
	}
	assertRecordingDirEmpty(t, dir)
}

func TestStartFailureRemovesSensitiveSDP(t *testing.T) {
	dir := t.TempDir()
	recorder := New(testConfig(dir), zap.NewNop())
	recorder.Exec = func(context.Context, string, ...string) Command {
		return &failingStartCommand{stdin: &fakeWriteCloser{}}
	}
	if _, err := recorder.Start(context.Background(), 11, &fakeTapRouter{}); err == nil {
		t.Fatal("Start succeeded with failing process")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("startup failure left artifacts: %v", entries)
	}
}

func TestRecorderValidatesDirectConstructionAndClosedState(t *testing.T) {
	valid := New(testConfig(t.TempDir()), zap.NewNop())
	//nolint:staticcheck // This explicitly verifies the exported method's nil-context guard.
	if _, err := valid.Start(nil, 1, &fakeTapRouter{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil context error = %v", err)
	}
	if _, err := valid.Start(context.Background(), 0, &fakeTapRouter{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid channel error = %v", err)
	}
	if _, err := valid.Start(context.Background(), 1, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil router error = %v", err)
	}
	var typedNilRouter *fakeTapRouter
	if _, err := valid.Start(context.Background(), 1, typedNilRouter); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("typed-nil router error = %v", err)
	}
	if err := valid.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := valid.Start(context.Background(), 1, &fakeTapRouter{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after Close error = %v", err)
	}

	invalid := New(Config{Enabled: true, Dir: t.TempDir(), Format: "mkv"}, zap.NewNop())
	if _, err := invalid.Start(context.Background(), 1, &fakeTapRouter{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid format error = %v", err)
	}
}

func TestConcurrentStopIsIdempotent(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := &fakeTapRouter{}
	if _, err := recorder.Start(context.Background(), 13, router); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			errs <- recorder.Stop(13)
		}()
	}
	close(start)
	time.Sleep(20 * time.Millisecond)
	exec.cmd.release()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Stop: %v", err)
		}
	}
	waitForRouterRemovals(t, router, 1)
}

func TestConcurrentForcedStopReturnsConsistentTimeout(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	recorder.stopGracePeriod = 20 * time.Millisecond
	recorder.killWait = time.Second
	exec := &fakeExec{}
	recorder.Exec = exec.run
	if _, err := recorder.Start(context.Background(), 14, &fakeTapRouter{}); err != nil {
		t.Fatal(err)
	}

	errResults := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			errResults <- recorder.Stop(14)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errResults; !errors.Is(err, ErrStopTimeout) {
			t.Fatalf("concurrent forced Stop = %v, want ErrStopTimeout", err)
		}
	}
}

type wedgedWriteCloser struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newWedgedWriteCloser() *wedgedWriteCloser {
	return &wedgedWriteCloser{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *wedgedWriteCloser) Write(_ []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return 0, net.ErrClosed
}

func (w *wedgedWriteCloser) Close() error { return nil }

type wedgedCommand struct {
	stdin       *wedgedWriteCloser
	output      string
	waitDone    chan struct{}
	killStarted chan struct{}
	killRelease chan struct{}
	killOnce    sync.Once
	waitOnce    sync.Once
}

func newWedgedCommand() *wedgedCommand {
	return &wedgedCommand{
		stdin:       newWedgedWriteCloser(),
		waitDone:    make(chan struct{}),
		killStarted: make(chan struct{}),
		killRelease: make(chan struct{}),
	}
}

func (c *wedgedCommand) StdinPipe() (io.WriteCloser, error) { return c.stdin, nil }
func (c *wedgedCommand) BindRecordingRoot(*os.Root, string, string, string, string) error {
	return nil
}
func (c *wedgedCommand) CloseBeforeStart() error { return nil }
func (c *wedgedCommand) Start() error {
	if c.output == "" {
		return nil
	}
	return os.WriteFile(c.output, []byte("recording"), 0o600)
}
func (c *wedgedCommand) Wait() error {
	<-c.waitDone
	return nil
}
func (c *wedgedCommand) Kill() error {
	c.killOnce.Do(func() {
		close(c.killStarted)
		<-c.killRelease
		c.waitOnce.Do(func() { close(c.waitDone) })
	})
	return nil
}

type wedgedRouter struct {
	removeStarted chan struct{}
	removeRelease chan struct{}
	removeOnce    sync.Once
}

func newWedgedRouter() *wedgedRouter {
	return &wedgedRouter{removeStarted: make(chan struct{}), removeRelease: make(chan struct{})}
}

func (*wedgedRouter) AddTap(int64, string, webrtc.TrackWriter, webrtc.TrackWriter) {}
func (r *wedgedRouter) RemoveTap(string) {
	r.removeOnce.Do(func() {
		close(r.removeStarted)
		<-r.removeRelease
	})
}

func TestRecorderShutdownIsBoundedWithWedgedCollaborators(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*Recorder) error
	}{
		{name: "Stop", stop: func(recorder *Recorder) error { return recorder.Stop(41) }},
		{name: "Close", stop: func(recorder *Recorder) error { return recorder.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := New(testConfig(t.TempDir()), zap.NewNop())
			recorder.stopGracePeriod = 20 * time.Millisecond
			recorder.killWait = 20 * time.Millisecond
			command := newWedgedCommand()
			recorder.Exec = func(_ context.Context, _ string, args ...string) Command {
				command.output = args[len(args)-1]
				return command
			}
			router := newWedgedRouter()
			t.Cleanup(func() {
				closeIfOpen(command.stdin.release)
				closeIfOpen(command.killRelease)
				closeIfOpen(router.removeRelease)
				command.waitOnce.Do(func() { close(command.waitDone) })
			})
			if _, err := recorder.Start(context.Background(), 41, router); err != nil {
				t.Fatal(err)
			}

			started := time.Now()
			err := tc.stop(recorder)
			elapsed := time.Since(started)
			if !errors.Is(err, ErrStopTimeout) {
				t.Fatalf("shutdown error = %v, want ErrStopTimeout", err)
			}
			if elapsed > 500*time.Millisecond {
				t.Fatalf("shutdown took %s despite 40ms combined deadline", elapsed)
			}
			if recorder.SessionCount() != 1 {
				t.Fatalf("session count = %d after bounded shutdown, want reservation retained", recorder.SessionCount())
			}

			for name, signal := range map[string]<-chan struct{}{
				"stdin write":  command.stdin.started,
				"process kill": command.killStarted,
				"tap removal":  router.removeStarted,
			} {
				select {
				case <-signal:
				case <-time.After(time.Second):
					t.Fatalf("%s was not attempted", name)
				}
			}

			_, startErr := recorder.Start(context.Background(), 41, router)
			if tc.name == "Close" {
				if !errors.Is(startErr, ErrClosed) {
					t.Fatalf("Start while closed = %v, want ErrClosed", startErr)
				}
			} else if !errors.Is(startErr, ErrAlreadyRecording) {
				t.Fatalf("replacement Start during late cleanup = %v, want ErrAlreadyRecording", startErr)
			}

			closeIfOpen(command.stdin.release)
			closeIfOpen(command.killRelease)
			closeIfOpen(router.removeRelease)
			waitForSessionCount(t, recorder, 0)
		})
	}
}

func closeIfOpen(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

type blockingAddRouter struct {
	started  chan struct{}
	release  chan struct{}
	removed  chan struct{}
	once     sync.Once
	remove   sync.Once
	audioTap webrtc.TrackWriter
	videoTap webrtc.TrackWriter
}

func newBlockingAddRouter() *blockingAddRouter {
	return &blockingAddRouter{
		started: make(chan struct{}), release: make(chan struct{}), removed: make(chan struct{}),
	}
}

func (r *blockingAddRouter) AddTap(
	_ int64,
	_ string,
	audio, video webrtc.TrackWriter,
) {
	r.audioTap = audio
	r.videoTap = video
	r.once.Do(func() { close(r.started) })
	<-r.release
}

func (r *blockingAddRouter) RemoveTap(string) {
	r.remove.Do(func() { close(r.removed) })
}

func assertBlockedStartupResourcesStopped(
	t *testing.T,
	exec *fakeExec,
	router *blockingAddRouter,
) {
	t.Helper()
	exec.mu.Lock()
	command := exec.cmd
	exec.mu.Unlock()
	if command == nil {
		t.Fatal("startup did not create a command")
	}
	packet := &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte("closed")}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		command.mu.Lock()
		killed := command.killed
		command.mu.Unlock()
		waitReturned := false
		select {
		case <-command.waitDone:
			waitReturned = true
		default:
		}
		audioClosed := router.audioTap != nil && errors.Is(router.audioTap.WriteRTP(packet), net.ErrClosed)
		videoClosed := router.videoTap != nil && errors.Is(router.videoTap.WriteRTP(packet), net.ErrClosed)
		if killed && waitReturned && audioClosed && videoClosed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Close/Stop did not kill and reap ffmpeg or close both taps while AddTap remained blocked")
}

func TestCloseBoundsInflightStartAndCachesOutcome(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	recorder.killWait = 20 * time.Millisecond
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := newBlockingAddRouter()
	t.Cleanup(func() { closeIfOpen(router.release) })

	type startResult struct {
		session *Session
		err     error
	}
	started := make(chan startResult, 1)
	go func() {
		session, err := recorder.Start(context.Background(), 47, router)
		started <- startResult{session: session, err: err}
	}()
	select {
	case <-router.started:
	case <-time.After(time.Second):
		t.Fatal("Start did not reach blocking router")
	}

	begin := time.Now()
	firstErr := recorder.Close()
	if !errors.Is(firstErr, ErrStartDrainTimeout) {
		t.Fatalf("Close error = %v, want ErrStartDrainTimeout", firstErr)
	}
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("Close took %s despite bounded startup drain", elapsed)
	}
	secondErr := recorder.Close()
	if firstErr != secondErr {
		t.Fatalf("repeated Close returned a different cached outcome: first=%v second=%v", firstErr, secondErr)
	}
	if _, err := recorder.Start(context.Background(), 48, router); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
	select {
	case result := <-started:
		if result.session != nil || !errors.Is(result.err, ErrClosed) ||
			!errors.Is(result.err, ErrStartupCleanupTimeout) {
			t.Fatalf("in-flight Start result = session %v, err %v; want closed cleanup timeout", result.session, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight Start did not return within its cleanup deadline")
	}
	assertBlockedStartupResourcesStopped(t, exec, router)

	closeIfOpen(router.release)
	select {
	case <-recorder.startsDone:
	case <-time.After(time.Second):
		t.Fatal("startup reservation was not released after cleanup")
	}
	select {
	case <-router.removed:
	case <-time.After(time.Second):
		t.Fatal("late tap registration was not removed after AddTap returned")
	}
}

func TestStopCancelsInflightStartAndOwnsResources(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	recorder.killWait = 20 * time.Millisecond
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := newBlockingAddRouter()
	t.Cleanup(func() { closeIfOpen(router.release) })

	startResult := make(chan error, 1)
	go func() {
		_, err := recorder.Start(context.Background(), 52, router)
		startResult <- err
	}()
	select {
	case <-router.started:
	case <-time.After(time.Second):
		t.Fatal("Start did not reach blocking router")
	}

	begin := time.Now()
	stopErr := recorder.Stop(52)
	if !errors.Is(stopErr, ErrStopTimeout) {
		t.Fatalf("Stop during startup = %v, want ErrStopTimeout", stopErr)
	}
	if elapsed := time.Since(begin); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop during startup took %s despite bounded cleanup", elapsed)
	}
	assertBlockedStartupResourcesStopped(t, exec, router)
	if _, err := recorder.Start(context.Background(), 52, &fakeTapRouter{}); !errors.Is(err, ErrAlreadyRecording) {
		t.Fatalf("replacement during late AddTap reconciliation = %v, want ErrAlreadyRecording", err)
	}
	select {
	case err := <-startResult:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStartupCleanupTimeout) {
			t.Fatalf("canceled in-flight Start = %v, want context and cleanup timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled in-flight Start did not return")
	}

	closeIfOpen(router.release)
	select {
	case <-router.removed:
	case <-time.After(time.Second):
		t.Fatal("late AddTap was not reconciled by RemoveTap")
	}
	deadline := time.Now().Add(time.Second)
	for {
		recorder.mu.Lock()
		_, reserved := recorder.starting[52]
		recorder.mu.Unlock()
		if !reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup reservation remained after late AddTap reconciliation")
		}
		time.Sleep(time.Millisecond)
	}

	replacementExec := &fakeExec{}
	recorder.Exec = replacementExec.run
	if _, err := recorder.Start(context.Background(), 52, &fakeTapRouter{}); err != nil {
		t.Fatalf("replacement Start after cleanup: %v", err)
	}
	releaseAndStop(t, recorder, replacementExec.cmd, 52)
}

func TestProcessExitDuringBlockedTapRegistrationPreventsPublication(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := newBlockingAddRouter()
	t.Cleanup(func() { closeIfOpen(router.release) })

	type result struct {
		session *Session
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		session, err := recorder.Start(context.Background(), 53, router)
		resultCh <- result{session: session, err: err}
	}()
	select {
	case <-router.started:
	case <-time.After(time.Second):
		t.Fatal("Start did not reach blocking router")
	}
	exec.mu.Lock()
	command := exec.cmd
	exec.mu.Unlock()
	recorder.mu.Lock()
	processDone := recorder.starting[53].processDone
	recorder.mu.Unlock()
	if processDone == nil {
		t.Fatal("startup reservation did not expose process observation")
	}
	command.release()
	select {
	case <-processDone:
	case <-time.After(time.Second):
		t.Fatal("recorder did not observe process exit")
	}
	closeIfOpen(router.release)

	select {
	case got := <-resultCh:
		if got.session != nil || got.err == nil || !strings.Contains(got.err.Error(), "ffmpeg exited while registering taps") {
			t.Fatalf("Start after process exit returned session=%t, err=%v", got.session != nil, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not observe process exit after AddTap returned")
	}
	if recorder.SessionCount() != 0 {
		t.Fatalf("dead startup published %d sessions", recorder.SessionCount())
	}
	select {
	case <-router.removed:
	case <-time.After(time.Second):
		t.Fatal("tap registered for dead process was not removed")
	}
}

func TestProcessExitWhileTapRegistrationIsWedgedReturnsBounded(t *testing.T) {
	dir := t.TempDir()
	recorder := New(testConfig(dir), zap.NewNop())
	recorder.killWait = 20 * time.Millisecond
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := newBlockingAddRouter()
	t.Cleanup(func() { closeIfOpen(router.release) })

	resultCh := make(chan error, 1)
	go func() {
		_, err := recorder.Start(context.Background(), 54, router)
		resultCh <- err
	}()
	select {
	case <-router.started:
	case <-time.After(time.Second):
		t.Fatal("Start did not reach blocking router")
	}
	exec.mu.Lock()
	command := exec.cmd
	exec.mu.Unlock()
	command.release()

	select {
	case err := <-resultCh:
		if err == nil || !strings.Contains(err.Error(), "ffmpeg exited while registering taps") ||
			!errors.Is(err, ErrStartupCleanupTimeout) {
			t.Fatalf("Start after process exit = %v, want exit and bounded cleanup errors", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start remained blocked behind AddTap after process exit")
	}
	assertRecordingDirEmpty(t, dir)
	if _, err := recorder.Start(context.Background(), 54, &fakeTapRouter{}); !errors.Is(err, ErrAlreadyRecording) {
		t.Fatalf("replacement during late AddTap cleanup = %v, want ErrAlreadyRecording", err)
	}

	closeIfOpen(router.release)
	select {
	case <-router.removed:
	case <-time.After(time.Second):
		t.Fatal("late AddTap was not removed")
	}
	deadline := time.Now().Add(time.Second)
	for {
		recorder.mu.Lock()
		_, reserved := recorder.starting[54]
		recorder.mu.Unlock()
		if !reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reservation remained after late AddTap reconciled")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRequestCancellationDuringTapRegistrationAbortsStartup(t *testing.T) {
	dir := t.TempDir()
	recorder := New(testConfig(dir), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := newBlockingAddRouter()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	t.Cleanup(func() { closeIfOpen(router.release) })

	result := make(chan error, 1)
	go func() {
		_, err := recorder.Start(requestCtx, 48, router)
		result <- err
	}()
	select {
	case <-router.started:
	case <-time.After(time.Second):
		t.Fatal("Start did not reach tap registration")
	}
	cancelRequest()
	closeIfOpen(router.release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled Start did not finish")
	}
	if recorder.SessionCount() != 0 {
		t.Fatalf("canceled Start published %d sessions", recorder.SessionCount())
	}
	assertRecordingDirEmpty(t, dir)
}

func TestStartupCleanupRetainsReservationUntilProcessIsReaped(t *testing.T) {
	dir := t.TempDir()
	recorder := New(testConfig(dir), zap.NewNop())
	recorder.killWait = 20 * time.Millisecond
	command := newWedgedCommand()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	recorder.Exec = func(_ context.Context, _ string, args ...string) Command {
		cancelRequest()
		command.output = args[len(args)-1]
		return command
	}
	t.Cleanup(func() {
		closeIfOpen(command.stdin.release)
		closeIfOpen(command.killRelease)
		command.waitOnce.Do(func() { close(command.waitDone) })
	})

	if _, err := recorder.Start(requestCtx, 49, &fakeTapRouter{}); !errors.Is(err, ErrStartupCleanupTimeout) {
		t.Fatalf("Start error = %v, want ErrStartupCleanupTimeout", err)
	}
	if _, err := recorder.Start(context.Background(), 49, &fakeTapRouter{}); !errors.Is(err, ErrAlreadyRecording) {
		t.Fatalf("replacement during startup cleanup = %v, want ErrAlreadyRecording", err)
	}
	if _, err := os.Stat(command.output); err != nil {
		t.Fatalf("partial output disappeared before process Wait returned: %v", err)
	}

	closeIfOpen(command.killRelease)
	deadline := time.Now().Add(time.Second)
	for {
		recorder.mu.Lock()
		_, reserved := recorder.starting[49]
		recorder.mu.Unlock()
		if !reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup reservation remained after process cleanup completed")
		}
		time.Sleep(time.Millisecond)
	}
	assertRecordingDirEmpty(t, dir)
}

type delayedRemovalRouter struct {
	mu                 sync.Mutex
	active             map[string]bool
	removeCalls        int
	firstRemoveStarted chan struct{}
	firstRemoveRelease chan struct{}
}

func newDelayedRemovalRouter() *delayedRemovalRouter {
	return &delayedRemovalRouter{
		active:             make(map[string]bool),
		firstRemoveStarted: make(chan struct{}),
		firstRemoveRelease: make(chan struct{}),
	}
}

func (r *delayedRemovalRouter) AddTap(_ int64, tapID string, _, _ webrtc.TrackWriter) {
	r.mu.Lock()
	r.active[tapID] = true
	r.mu.Unlock()
}

func (r *delayedRemovalRouter) RemoveTap(tapID string) {
	r.mu.Lock()
	call := r.removeCalls
	r.removeCalls++
	if call == 0 {
		close(r.firstRemoveStarted)
	}
	r.mu.Unlock()
	if call == 0 {
		<-r.firstRemoveRelease
	}
	r.mu.Lock()
	delete(r.active, tapID)
	r.mu.Unlock()
}

func TestDelayedOldUnregisterCannotRemoveReplacementTap(t *testing.T) {
	recorder := New(testConfig(t.TempDir()), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	router := newDelayedRemovalRouter()
	t.Cleanup(func() { closeIfOpen(router.firstRemoveRelease) })

	first, err := recorder.Start(context.Background(), 51, router)
	if err != nil {
		t.Fatal(err)
	}
	firstCommand := exec.cmd
	firstStop := make(chan error, 1)
	go func() { firstStop <- recorder.Stop(51) }()
	select {
	case <-router.firstRemoveStarted:
	case <-time.After(time.Second):
		t.Fatal("old tap removal did not start")
	}
	firstCommand.release()
	select {
	case err := <-firstStop:
		t.Fatalf("first Stop returned before old tap removal completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := recorder.Start(context.Background(), 51, router); !errors.Is(err, ErrAlreadyRecording) {
		t.Fatalf("replacement during old removal = %v, want ErrAlreadyRecording", err)
	}
	closeIfOpen(router.firstRemoveRelease)
	if err := <-firstStop; err != nil {
		t.Fatalf("stopping first session: %v", err)
	}

	second, err := recorder.Start(context.Background(), 51, router)
	if err != nil {
		t.Fatalf("starting replacement session: %v", err)
	}
	secondCommand := exec.cmd
	if first.tapID == second.tapID {
		t.Fatalf("replacement reused tap ID %q", first.tapID)
	}

	deadline := time.Now().Add(time.Second)
	for {
		router.mu.Lock()
		oldActive := router.active[first.tapID]
		newActive := router.active[second.tapID]
		router.mu.Unlock()
		if !oldActive && newActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("active taps after delayed removal: old=%t replacement=%t", oldActive, newActive)
		}
		time.Sleep(time.Millisecond)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		secondCommand.release()
	}()
	if err := recorder.Stop(51); err != nil {
		t.Fatalf("stopping replacement session: %v", err)
	}
}

func TestRecorderCopiesConfigArguments(t *testing.T) {
	videoArgs := []string{"-c:v", "h264_nvenc"}
	recorder := New(Config{Enabled: true, Dir: t.TempDir(), VideoArgs: videoArgs}, zap.NewNop())
	videoArgs[1] = "mutated"
	args := strings.Join(recorder.buildArgs("input.sdp", "output.webm"), " ")
	if !strings.Contains(args, "h264_nvenc") || strings.Contains(args, "mutated") {
		t.Fatalf("recorder retained caller-owned config slice: %q", args)
	}
}

func TestTapRejectsInvalidInputsAndCloseIsIdempotent(t *testing.T) {
	for _, port := range []int{-1, 0, 65536} {
		if _, err := NewTap(port); err == nil {
			t.Fatalf("NewTap(%d) succeeded", port)
		}
	}
	tap, err := NewTap(12345)
	if err != nil {
		t.Fatal(err)
	}
	if err := tap.WriteRTP(nil); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("nil packet error = %v", err)
	}
	if err := tap.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tap.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := (*Tap)(nil).Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestRecordingDirectoryRejectsSymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "recordings-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	recorder := New(testConfig(link), zap.NewNop())
	if _, err := recorder.Start(context.Background(), 12, &fakeTapRouter{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("symlink directory error = %v", err)
	}
}

func TestRecordingDirectoryRejectsFilesystemRootAndCurrentDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Clean(filepath.VolumeName(cwd) + string(filepath.Separator))
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{name: "filesystem root", dir: rootPath},
		{name: "current directory relative", dir: "."},
		{name: "current directory absolute", dir: cwd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statPath, absErr := filepath.Abs(tc.dir)
			if absErr != nil {
				t.Fatal(absErr)
			}
			before, statErr := os.Stat(statPath)
			if statErr != nil {
				t.Fatal(statErr)
			}
			recorder := New(testConfig(tc.dir), zap.NewNop())
			if _, err := recorder.Start(context.Background(), 56, &fakeTapRouter{}); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Start(%q) = %v, want ErrInvalidConfig", tc.dir, err)
			}
			after, statErr := os.Stat(statPath)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if runtime.GOOS != "windows" && before.Mode().Perm() != after.Mode().Perm() {
				t.Fatalf("rejected path %q changed mode from %o to %o", statPath, before.Mode().Perm(), after.Mode().Perm())
			}
		})
	}
}

func TestExistingRecordingDirectoryPermissionsAreValidatedNotChanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows recording access is enforced through the acknowledged NTFS DACL")
	}
	dir := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	recorder := New(testConfig(dir), zap.NewNop())
	if _, err := recorder.Start(context.Background(), 57, &fakeTapRouter{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Start with permissive existing dir = %v, want ErrInvalidConfig", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("rejected existing directory mode = %o, want unchanged 750", info.Mode().Perm())
	}
}

func TestRecorderCreatesRestrictedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows recording directories must be pre-provisioned with a restricted DACL")
	}
	dir := filepath.Join(t.TempDir(), "recordings")
	recorder := New(testConfig(dir), zap.NewNop())
	exec := &fakeExec{}
	recorder.Exec = exec.run
	if _, err := recorder.Start(context.Background(), 58, &fakeTapRouter{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("new recording directory mode = %o, want 700", info.Mode().Perm())
	}
	releaseAndStop(t, recorder, exec.cmd, 58)
}

func TestWindowsRecordingDirectoryMustBePreprovisioned(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific DACL provisioning invariant")
	}
	dir := filepath.Join(t.TempDir(), "missing-recordings")
	recorder := New(testConfig(dir), zap.NewNop())
	if _, err := recorder.Start(context.Background(), 59, &fakeTapRouter{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Start with missing Windows recording directory = %v, want ErrInvalidConfig", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected Windows recording directory was created: %v", err)
	}
}

type delayedRootOpenCommand struct {
	stdin      *fakeWriteCloser
	root       *os.Root
	sdpName    string
	outputName string
	open       chan struct{}
}

func newDelayedRootOpenCommand() *delayedRootOpenCommand {
	return &delayedRootOpenCommand{stdin: &fakeWriteCloser{}, open: make(chan struct{})}
}

func (c *delayedRootOpenCommand) BindRecordingRoot(
	root *os.Root,
	_, _ string,
	sdpName, outputName string,
) error {
	c.root = root
	c.sdpName = sdpName
	c.outputName = outputName
	return nil
}

func (c *delayedRootOpenCommand) StdinPipe() (io.WriteCloser, error) { return c.stdin, nil }
func (*delayedRootOpenCommand) CloseBeforeStart() error              { return nil }
func (*delayedRootOpenCommand) Start() error                         { return nil }
func (c *delayedRootOpenCommand) Wait() error {
	<-c.open
	if c.root == nil {
		return errors.New("recording root was not bound")
	}
	sdp, err := c.root.Open(c.sdpName)
	if err != nil {
		return fmt.Errorf("opening bound sdp: %w", err)
	}
	contents, readErr := io.ReadAll(sdp)
	closeErr := sdp.Close()
	if readErr != nil || closeErr != nil || len(contents) == 0 {
		return errors.Join(readErr, closeErr, errors.New("bound sdp is empty"))
	}
	output, err := c.root.OpenFile(c.outputName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating bound output: %w", err)
	}
	_, writeErr := output.Write([]byte("bound recording"))
	return errors.Join(writeErr, output.Sync(), output.Close())
}
func (c *delayedRootOpenCommand) Kill() error {
	closeIfOpen(c.open)
	return nil
}

func TestDelayedChildOpenCannotEscapeRenamedRecordingRoot(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "recordings")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	command := newDelayedRootOpenCommand()
	recorder := New(testConfig(dir), zap.NewNop())
	recorder.Exec = func(context.Context, string, ...string) Command { return command }
	session, err := recorder.Start(context.Background(), 55, &fakeTapRouter{})
	if err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(parent, "recordings-retained")
	renameErr := os.Rename(dir, moved)
	if runtime.GOOS == "windows" {
		close(command.open)
		waitForSessionCount(t, recorder, 0)
		if renameErr == nil {
			t.Fatal("Windows allowed replacement of a recording root whose retained handle must pin its path")
		}
		return
	}
	if renameErr != nil {
		t.Fatalf("rename recording root: %v", renameErr)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create replacement recording root: %v", err)
	}
	close(command.open)
	waitForSessionCount(t, recorder, 0)

	name := filepath.Base(session.FilePath)
	contents, err := os.ReadFile(filepath.Join(moved, name))
	if err != nil {
		t.Fatalf("retained-root output: %v", err)
	}
	if string(contents) != "bound recording" {
		t.Fatalf("retained-root output = %q", contents)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("replacement root received artifacts: entries=%v err=%v", entries, err)
	}
}

func TestDefaultCommandBindsPathsToRetainedRoot(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	sdpPath := filepath.Join(dir, "input.sdp")
	outputPath := filepath.Join(dir, "output.webm")
	command := defaultExec(context.Background(), "ffmpeg", "-i", sdpPath, outputPath).(*cmdWrapper)
	if err := command.BindRecordingRoot(root, sdpPath, outputPath, "input.sdp", "output.webm"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.closeBoundRoot() }()
	if runtime.GOOS == "windows" {
		if command.cmd.Args[2] != sdpPath || command.cmd.Args[3] != outputPath {
			t.Fatalf("Windows pinned paths unexpectedly changed: %v", command.cmd.Args)
		}
		return
	}
	prefix, err := childVisibleRootPrefix()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(command.cmd.Args, " ")
	if len(command.cmd.ExtraFiles) != 1 || strings.Contains(args, sdpPath) || strings.Contains(args, outputPath) ||
		!strings.Contains(args, prefix+"/3/input.sdp") || !strings.Contains(args, prefix+"/3/output.webm") {
		t.Fatalf("command is not bound to inherited root: args=%q extra_files=%d", args, len(command.cmd.ExtraFiles))
	}
}

func TestRecordingRootFDHelper(t *testing.T) {
	if os.Getenv("VOICX_RECORDER_ROOT_FD_HELPER") != "1" {
		return
	}
	if len(os.Args) < 2 {
		t.Fatal("recording-root helper paths are missing")
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	sdpPath := os.Args[len(os.Args)-2]
	outputPath := os.Args[len(os.Args)-1]
	contents, err := os.ReadFile(sdpPath)
	if err != nil || string(contents) != "bound sdp" {
		t.Fatalf("helper sdp = %q, err=%v", contents, err)
	}
	if err := os.WriteFile(outputPath, []byte("bound child output"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxChildTraversesInheritedRecordingRootAfterRename(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc/self/fd functional probe")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "recordings")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	sdpName, outputName := "input.sdp", "output.webm"
	sdpPath, outputPath := filepath.Join(dir, sdpName), filepath.Join(dir, outputName)
	sdp, err := root.OpenFile(sdpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdp.Write([]byte("bound sdp")); err != nil {
		_ = sdp.Close()
		t.Fatal(err)
	}
	if err := sdp.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(
		ctx,
		os.Args[0],
		"-test.run=^TestRecordingRootFDHelper$",
		"--",
		sdpPath,
		outputPath,
	)
	child.Env = append(os.Environ(), "VOICX_RECORDER_ROOT_FD_HELPER=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command := &cmdWrapper{cmd: child}
	if err := command.BindRecordingRoot(root, sdpPath, outputPath, sdpName, outputName); err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = command.CloseBeforeStart()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		_ = command.Kill()
		t.Fatalf("helper readiness = %q, err=%v", line, err)
	}
	moved := filepath.Join(parent, "recordings-retained")
	if err := os.Rename(dir, moved); err != nil {
		_ = command.Kill()
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		_ = command.Kill()
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "open\n"); err != nil {
		_ = command.Kill()
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		_ = command.Kill()
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(moved, outputName))
	if err != nil || string(contents) != "bound child output" {
		t.Fatalf("retained child output = %q, err=%v", contents, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("replacement root received child artifacts: entries=%v err=%v", entries, err)
	}
}

func TestBufferedTapReportsNilPacketsAndWriteFailures(t *testing.T) {
	buffered, err := NewBufferedTap(12345, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := buffered.WriteRTP(nil); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("nil packet error = %v", err)
	}
	if err := buffered.tap.Close(); err != nil {
		t.Fatal(err)
	}
	packet := &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte("write-failure")}
	if err := buffered.WriteRTP(packet); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for buffered.WriteErrors() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if buffered.WriteErrors() != 1 || buffered.QueuedBytes() != 0 {
		t.Fatalf("write errors/queued bytes = %d/%d, want 1/0", buffered.WriteErrors(), buffered.QueuedBytes())
	}
	if err := buffered.Close(); err != nil {
		t.Fatal(err)
	}
}
