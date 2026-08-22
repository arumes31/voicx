package recorder

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"go.uber.org/zap"

	"voicx/internal/webrtc"
)

// fakeCommand is a fake OS process for lifecycle tests.
type fakeCommand struct {
	mu            sync.Mutex
	stdin         *fakeWriteCloser
	started       bool
	killed        bool
	waitDone      chan struct{}
	quitRequested chan struct{}
	quitOnce      sync.Once
	output        string
}

type fakeWriteCloser struct {
	mu      sync.Mutex
	data    []byte
	onWrite func([]byte)
}

func (w *fakeWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.data = append(w.data, p...)
	onWrite := w.onWrite
	w.mu.Unlock()
	if onWrite != nil {
		onWrite(p)
	}
	return len(p), nil
}
func (w *fakeWriteCloser) Close() error { return nil }

func newFakeCommand(output string) *fakeCommand {
	command := &fakeCommand{
		waitDone:      make(chan struct{}),
		quitRequested: make(chan struct{}),
		output:        output,
	}
	command.stdin = &fakeWriteCloser{onWrite: func(p []byte) {
		if string(p) == "q" {
			command.quitOnce.Do(func() { close(command.quitRequested) })
		}
	}}
	return command
}

func (c *fakeCommand) StdinPipe() (io.WriteCloser, error) { return c.stdin, nil }
func (c *fakeCommand) BindRecordingRoot(*os.Root, string, string, string, string) error {
	return nil
}
func (c *fakeCommand) CloseBeforeStart() error { return nil }
func (c *fakeCommand) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.output != "" {
		if err := os.WriteFile(c.output, []byte("recording"), 0o600); err != nil {
			return err
		}
	}
	c.started = true
	return nil
}

// Wait blocks until the test releases the process (or Kill is called).
func (c *fakeCommand) Wait() error {
	<-c.waitDone
	return nil
}

type errorWaitCommand struct {
	*fakeCommand
	waitErr error
}

func (c *errorWaitCommand) Wait() error {
	<-c.waitDone
	return c.waitErr
}

type blockingStartErrorCommand struct {
	stdin   *fakeWriteCloser
	entered chan struct{}
	release chan struct{}
}

func (c *blockingStartErrorCommand) StdinPipe() (io.WriteCloser, error) { return c.stdin, nil }
func (*blockingStartErrorCommand) BindRecordingRoot(*os.Root, string, string, string, string) error {
	return nil
}
func (*blockingStartErrorCommand) CloseBeforeStart() error { return nil }
func (c *blockingStartErrorCommand) Start() error {
	close(c.entered)
	<-c.release
	return errors.New("delayed start failure")
}
func (*blockingStartErrorCommand) Wait() error { return errors.New("unexpected Wait") }
func (*blockingStartErrorCommand) Kill() error { return nil }

func (c *fakeCommand) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.killed = true
	select {
	case <-c.waitDone:
	default:
		close(c.waitDone)
	}
	return nil
}

// release lets Wait return (simulating a graceful exit).
func (c *fakeCommand) release() {
	select {
	case <-c.waitDone:
	default:
		close(c.waitDone)
	}
}

func (c *fakeCommand) waitForQuit(t *testing.T) {
	t.Helper()
	select {
	case <-c.quitRequested:
	case <-time.After(time.Second):
		t.Fatal("recorder did not request the ffmpeg quit command")
	}
}

func releaseAndWaitStop(t *testing.T, recorder *Recorder, cmd *fakeCommand, channelID int64) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- recorder.Stop(channelID) }()
	cmd.waitForQuit(t)
	cmd.release()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("recorder Stop did not return after ffmpeg exited")
		return nil
	}
}

func releaseAndStop(t *testing.T, recorder *Recorder, cmd *fakeCommand, channelID int64) {
	t.Helper()
	if err := releaseAndWaitStop(t, recorder, cmd, channelID); err != nil {
		t.Fatalf("Stop(%d): %v", channelID, err)
	}
}

// fakeExec captures the binary and args and returns a canned fakeCommand.
type fakeExec struct {
	mu   sync.Mutex
	name string
	args []string
	cmd  *fakeCommand
}

func (e *fakeExec) run(_ context.Context, name string, args ...string) Command {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.name = name
	e.args = args
	output := ""
	if len(args) > 0 {
		output = args[len(args)-1]
	}
	e.cmd = newFakeCommand(output)
	return e.cmd
}

// fakeTapRouter records AddTap/RemoveTap calls.
type fakeTapRouter struct {
	mu      sync.Mutex
	added   []string
	removed []string
	channel int64
}

func (f *fakeTapRouter) AddTap(channelID int64, tapID string, _, _ webrtc.TrackWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, tapID)
	f.channel = channelID
}

func (f *fakeTapRouter) RemoveTap(tapID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, tapID)
}

func waitForRouterRemovals(t *testing.T, router *fakeTapRouter, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		router.mu.Lock()
		got := len(router.removed)
		router.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	router.mu.Lock()
	got := append([]string(nil), router.removed...)
	router.mu.Unlock()
	t.Fatalf("removed taps = %v, want %d", got, want)
}

func testLogger() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

func testConfig(dir string) Config {
	return Config{Enabled: true, Dir: dir, WindowsACLReady: true}
}

// TestBuildArgsDefaults verifies the default ffmpeg command line (copy
// codecs, SDP input, output file).
func TestBuildArgsDefaults(t *testing.T) {
	r := New(Config{Enabled: true, Dir: t.TempDir()}, testLogger())
	args := r.buildArgs("in.sdp", "out.webm")
	joined := strings.Join(args, " ")

	for _, want := range []string{"-protocol_whitelist file,udp,rtp", "-i in.sdp", "-c:a copy", "-c:v copy", "-n out.webm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}

// TestBuildArgsHardwareEncoder verifies operator-configured hardware codec
// args land in the command line.
func TestBuildArgsHardwareEncoder(t *testing.T) {
	r := New(Config{
		Enabled:   true,
		Dir:       t.TempDir(),
		VideoArgs: []string{"-c:v", "h264_nvenc", "-preset", "p1"},
		AudioArgs: []string{"-c:a", "libopus"},
		Format:    "mp4",
	}, testLogger())
	args := r.buildArgs("in.sdp", "out.mp4")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "-c:v h264_nvenc -preset p1") {
		t.Errorf("args %q missing nvenc options", joined)
	}
	if !strings.Contains(joined, "-c:a libopus") {
		t.Errorf("args %q missing audio options", joined)
	}
}

// TestBuildSDP verifies the generated SDP describes the two loopback streams.
func TestBuildSDP(t *testing.T) {
	sdp := buildSDP(40000, 40002)
	for _, want := range []string{"m=audio 40000 RTP/AVP 111", "a=rtpmap:111 opus/48000/2", "m=video 40002 RTP/AVP 96", "a=rtpmap:96 VP8/90000"} {
		if !strings.Contains(sdp, want) {
			t.Errorf("sdp missing %q:\n%s", want, sdp)
		}
	}
}

// TestStartStopLifecycle verifies a recording session starts ffmpeg with the
// expected command, registers router taps, and stops gracefully.
func TestStartStopLifecycle(t *testing.T) {
	dir := t.TempDir()
	r := New(testConfig(dir), testLogger())
	exec := &fakeExec{}
	r.Exec = exec.run
	router := &fakeTapRouter{}

	session, err := r.Start(context.Background(), 7, router)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if exec.name != "ffmpeg" {
		t.Fatalf("binary = %q, want ffmpeg", exec.name)
	}
	if !exec.cmd.started {
		t.Fatal("ffmpeg process not started")
	}
	if r.SessionCount() != 1 {
		t.Fatalf("SessionCount = %d, want 1", r.SessionCount())
	}
	if router.channel != 7 || len(router.added) != 1 {
		t.Fatalf("router taps = %+v, want one tap in channel 7", router)
	}

	// Double start is rejected.
	if _, err := r.Start(context.Background(), 7, router); err != ErrAlreadyRecording {
		t.Fatalf("second Start = %v, want ErrAlreadyRecording", err)
	}

	// Graceful stop: release the fake process only after the quit command.
	releaseAndStop(t, r, exec.cmd, 7)
	if exec.cmd.killed {
		t.Error("ffmpeg was killed despite graceful exit")
	}
	waitForRouterRemovals(t, router, 1)
	if r.SessionCount() != 0 {
		t.Fatalf("SessionCount = %d, want 0", r.SessionCount())
	}

	if session.FilePath == "" {
		t.Error("session has no output path")
	}
}

func TestRecorderCloseReportsStopFailureOnce(t *testing.T) {
	var observed []string
	var observedMu sync.Mutex
	r := New(testConfig(t.TempDir()), testLogger(), Observers{
		OnError: func(operation string) {
			observedMu.Lock()
			observed = append(observed, operation)
			observedMu.Unlock()
		},
	})
	command := &errorWaitCommand{
		fakeCommand: newFakeCommand(""),
		waitErr:     errors.New("ffmpeg exited with an error"),
	}
	r.Exec = func(_ context.Context, _ string, args ...string) Command {
		command.output = args[len(args)-1]
		return command
	}
	if _, err := r.Start(t.Context(), 24, &fakeTapRouter{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- r.Close() }()
	command.waitForQuit(t)
	command.release()
	if err := <-closeResult; err == nil {
		t.Fatal("Close succeeded despite process wait error")
	}
	observedMu.Lock()
	defer observedMu.Unlock()
	if len(observed) != 1 || observed[0] != "stop" {
		t.Fatalf("observed errors = %v, want exactly [stop]", observed)
	}
}

func TestRecorderRequestedStopDoesNotReportUnexpectedExit(t *testing.T) {
	var observed []string
	var observedMu sync.Mutex
	r := New(testConfig(t.TempDir()), testLogger(), Observers{
		OnError: func(operation string) {
			observedMu.Lock()
			observed = append(observed, operation)
			observedMu.Unlock()
		},
	})
	command := &errorWaitCommand{
		fakeCommand: newFakeCommand(""),
		waitErr:     errors.New("ffmpeg exited with an error"),
	}
	r.Exec = func(_ context.Context, _ string, args ...string) Command {
		command.output = args[len(args)-1]
		return command
	}
	if _, err := r.Start(t.Context(), 26, &fakeTapRouter{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- r.Stop(26) }()
	command.waitForQuit(t)
	command.release()
	if err := <-stopResult; err == nil {
		t.Fatal("Stop succeeded despite process wait error")
	}
	observedMu.Lock()
	defer observedMu.Unlock()
	if len(observed) != 1 || observed[0] != "stop" {
		t.Fatalf("observed errors = %v, want exactly [stop]", observed)
	}
}

func TestRecorderConcurrentStartCloseReportsStartFailureOnce(t *testing.T) {
	var observed []string
	var observedMu sync.Mutex
	r := New(testConfig(t.TempDir()), testLogger(), Observers{
		OnError: func(operation string) {
			observedMu.Lock()
			observed = append(observed, operation)
			observedMu.Unlock()
		},
	})
	r.killWait = time.Second
	command := &blockingStartErrorCommand{
		stdin:   &fakeWriteCloser{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	r.Exec = func(context.Context, string, ...string) Command { return command }
	startResult := make(chan error, 1)
	go func() {
		_, err := r.Start(t.Context(), 25, &fakeTapRouter{})
		startResult <- err
	}()
	select {
	case <-command.entered:
	case <-time.After(time.Second):
		t.Fatal("Start did not reach the process launcher")
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- r.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not mark the recorder closed")
		}
		time.Sleep(time.Millisecond)
	}
	close(command.release)
	if err := <-startResult; err == nil {
		t.Fatal("Start succeeded despite delayed process failure")
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("Close = %v, want nil because Start owns the launcher failure", err)
	}
	observedMu.Lock()
	defer observedMu.Unlock()
	if len(observed) != 1 || observed[0] != "start" {
		t.Fatalf("observed errors = %v, want exactly [start]", observed)
	}
}

// TestStopKillsStuckProcess verifies Stop kills ffmpeg when it does not exit
// within the recorder's grace period.
func TestStopKillsStuckProcess(t *testing.T) {
	r := New(testConfig(t.TempDir()), testLogger())
	r.stopGracePeriod = 25 * time.Millisecond
	r.killWait = time.Second
	exec := &fakeExec{}
	r.Exec = exec.run
	router := &fakeTapRouter{}

	if _, err := r.Start(context.Background(), 3, router); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The fake never releases, so Stop must take the kill path.
	done := make(chan error, 1)
	go func() { done <- r.Stop(3) }()

	// The process exits only when Kill closes its wait channel.
	select {
	case err := <-done:
		// Stop returned only after killing (grace period elapsed).
		if !errors.Is(err, ErrStopTimeout) {
			t.Fatalf("Stop error = %v, want ErrStopTimeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return within 10s")
	}
	exec.cmd.mu.Lock()
	killed := exec.cmd.killed
	exec.cmd.mu.Unlock()
	if !killed {
		t.Error("stuck ffmpeg process was not killed")
	}
}

// TestStartDisabled verifies recording is gated by the Enabled flag.
func TestStartDisabled(t *testing.T) {
	r := New(Config{Enabled: false, Dir: t.TempDir()}, testLogger())
	if _, err := r.Start(context.Background(), 1, &fakeTapRouter{}); err != ErrDisabled {
		t.Fatalf("Start on disabled recorder = %v, want ErrDisabled", err)
	}
}

// TestStopNotRecording verifies Stop on an unknown channel errors.
func TestStopNotRecording(t *testing.T) {
	r := New(testConfig(t.TempDir()), testLogger())
	if err := r.Stop(99); err != ErrNotRecording {
		t.Fatalf("Stop = %v, want ErrNotRecording", err)
	}
}

// TestTapWritesRTPOverUDP verifies the tap delivers marshaled RTP packets to
// the configured UDP destination.
func TestTapWritesRTPOverUDP(t *testing.T) {
	// Listener standing in for ffmpeg's UDP input.
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.LocalAddr().(*net.UDPAddr).Port

	tap, err := NewTap(port)
	if err != nil {
		t.Fatalf("NewTap: %v", err)
	}
	defer func() { _ = tap.Close() }()

	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: 42, SSRC: 9001},
		Payload: []byte{0xde, 0xad, 0xbe, 0xef},
	}
	if err := tap.WriteRTP(pkt); err != nil {
		t.Fatalf("WriteRTP: %v", err)
	}

	buf := make([]byte, 1500)
	_ = listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := listener.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP: %v", err)
	}
	got := &rtp.Packet{}
	if err := got.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SSRC != 9001 || got.SequenceNumber != 42 || string(got.Payload) != string(pkt.Payload) {
		t.Fatalf("received packet = %+v, want SSRC 9001 seq 42", got)
	}
}

func TestBufferedTapDropsWhenRingIsFull(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	b, err := NewBufferedTap(listener.LocalAddr().(*net.UDPAddr).Port, 1)
	if err != nil {
		t.Fatal(err)
	}
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte("larger than one byte")}
	if err := b.WriteRTP(pkt); err != nil {
		t.Fatal(err)
	}
	if got := b.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1", got)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}
