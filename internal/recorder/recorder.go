// Package recorder implements optional server-side recording of channel
// streams. It is an alternative to cgo FFmpeg bindings: an ffmpeg subprocess
// is started per recording session, fed with RTP over loopback UDP (the
// stream layout is described to ffmpeg by a generated SDP file), and remuxes
// or transcodes to WebM/MP4. The codec arguments are configurable so
// operators can plug in hardware encoders (e.g. -c:v h264_nvenc, h264_qsv,
// h264_vaapi).
//
// The media path is: SFU router -> Tap (TrackWriter) -> UDP -> ffmpeg.
// Recording taps register in the router like any other subscriber via the
// TapRouter interface (satisfied by webrtc.Voice).
//
// NOTE: media correctness of the RTP->ffmpeg piping (payload types in the
// generated SDP matching the negotiated streams) can only be validated with
// a live client and a real ffmpeg binary; the unit tests cover argument/SDP
// construction, process lifecycle, and the UDP tap.
package recorder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"go.uber.org/zap"

	"voicx/internal/webrtc"
)

// ErrDisabled is returned by Start when recording is not enabled in the
// configuration.
var ErrDisabled = errors.New("recorder: recording is disabled")

// ErrAlreadyRecording is returned by Start when a session for the channel
// already exists.
var ErrAlreadyRecording = errors.New("recorder: channel is already being recorded")

// ErrCapacity is returned when the configured process/memory budget has no
// slot for another starting or active recording.
var ErrCapacity = errors.New("recorder: concurrent recording limit reached")

// ErrNotRecording is returned by Stop when no session exists for the channel.
var ErrNotRecording = errors.New("recorder: channel is not being recorded")

// ErrClosed is returned when Start is called after Recorder.Close.
var ErrClosed = errors.New("recorder: recorder is closed")

// ErrInvalidConfig is returned when a Recorder was constructed directly with
// unsafe or incomplete settings instead of validated application config.
var ErrInvalidConfig = errors.New("recorder: invalid configuration")

// ErrStopTimeout reports that ffmpeg or a tracked cleanup collaborator did not
// finish within the graceful stop deadline.
var ErrStopTimeout = errors.New("recorder: recording stop deadline exceeded")

// ErrStartDrainTimeout reports that Close returned while an in-flight Start
// remained wedged in an external collaborator. The closed recorder will still
// reject that Start if it eventually reaches the publication point.
var ErrStartDrainTimeout = errors.New("recorder: startup drain deadline exceeded")

// ErrStartupCleanupTimeout reports that Start failed after ffmpeg had already
// launched and the subprocess did not finish its bounded cleanup window. The
// channel remains reserved until Wait and every launched cleanup operation
// finish, preventing an orphaned process from overlapping a replacement.
var ErrStartupCleanupTimeout = errors.New("recorder: startup cleanup deadline exceeded")

// ErrInvalidPacket is returned when a tap is asked to write a nil RTP packet.
var ErrInvalidPacket = errors.New("recorder: RTP packet is nil")

const (
	defaultStopGracePeriod = 5 * time.Second
	defaultKillWait        = 5 * time.Second
	defaultMaxConcurrent   = 4
	maxConcurrentLimit     = 64
)

// videoRingBytes absorbs short disk/ffmpeg stalls without back-pressuring the
// SFU read loop. Once full, new recorder packets are dropped; live voice/video
// forwarding remains the higher-priority workload.
const videoRingBytes = 16 * 1024 * 1024

// Config holds the recorder settings (populated from the "recording" config
// section).
type Config struct {
	// Enabled gates recording entirely.
	Enabled bool
	// Dir is where SDP and output files are written. Created if missing.
	Dir string
	// FFmpegPath is the ffmpeg binary to run ("ffmpeg" = PATH lookup).
	FFmpegPath string
	// Format is the output container/codec preset: "webm" (default) or "mp4".
	Format string
	// VideoArgs are the ffmpeg output options for the video stream, e.g.
	// ["-c:v", "copy"] (default) or ["-c:v", "h264_nvenc"] for hardware
	// encoding.
	VideoArgs []string
	// AudioArgs are the ffmpeg output options for the audio stream, e.g.
	// ["-c:a", "copy"] (default).
	AudioArgs []string
	// MaxConcurrent bounds starting plus active ffmpeg processes. Zero uses
	// the conservative default; values above maxConcurrentLimit are rejected.
	MaxConcurrent int
	// WindowsACLReady is an explicit operator assertion that Dir has a
	// restricted inheritable NTFS DACL. os.Chmod cannot establish that policy.
	WindowsACLReady bool
}

// withDefaults fills unset fields with their defaults.
func (c Config) withDefaults() Config {
	c.Dir = strings.TrimSpace(c.Dir)
	c.FFmpegPath = strings.TrimSpace(c.FFmpegPath)
	c.Format = strings.ToLower(strings.TrimSpace(c.Format))
	if c.FFmpegPath == "" {
		c.FFmpegPath = "ffmpeg"
	}
	if c.Format == "" {
		c.Format = "webm"
	}
	if c.VideoArgs == nil {
		c.VideoArgs = []string{"-c:v", "copy"}
	}
	if c.AudioArgs == nil {
		c.AudioArgs = []string{"-c:a", "copy"}
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}
	c.VideoArgs = append([]string(nil), c.VideoArgs...)
	c.AudioArgs = append([]string(nil), c.AudioArgs...)
	return c
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Dir) == "" {
		return fmt.Errorf("%w: recording directory is empty", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.FFmpegPath) == "" {
		return fmt.Errorf("%w: ffmpeg path is empty", ErrInvalidConfig)
	}
	switch c.Format {
	case "webm", "mp4":
	default:
		return fmt.Errorf("%w: unsupported output format %q", ErrInvalidConfig, c.Format)
	}
	if len(c.AudioArgs)+len(c.VideoArgs) > 128 {
		return fmt.Errorf("%w: too many ffmpeg arguments", ErrInvalidConfig)
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > maxConcurrentLimit {
		return fmt.Errorf("%w: max concurrent recordings must be between 1 and %d", ErrInvalidConfig, maxConcurrentLimit)
	}
	if runtime.GOOS == "windows" && !c.WindowsACLReady {
		return fmt.Errorf("%w: recording directory requires a restricted inheritable NTFS DACL and windows ACL acknowledgement", ErrInvalidConfig)
	}
	if strings.IndexByte(c.FFmpegPath, 0) >= 0 {
		return fmt.Errorf("%w: ffmpeg path contains NUL", ErrInvalidConfig)
	}
	for _, arg := range append(append([]string(nil), c.AudioArgs...), c.VideoArgs...) {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("%w: ffmpeg argument contains NUL", ErrInvalidConfig)
		}
		if len(arg) > 4096 {
			return fmt.Errorf("%w: ffmpeg argument exceeds 4096 bytes", ErrInvalidConfig)
		}
	}
	return nil
}

// Command abstracts an OS process so tests can fake exec.
type Command interface {
	// BindRecordingRoot replaces child-visible recording paths with paths tied
	// to the already-open root. Implementations must not resolve the ordinary
	// directory pathname again after this returns.
	BindRecordingRoot(root *os.Root, sdpPath, outputPath, sdpName, outputName string) error
	// CloseBeforeStart releases resources acquired before a failed Start. It is
	// never called after the subprocess has launched successfully.
	CloseBeforeStart() error
	// StdinPipe returns the process's standard input (used for the graceful
	// "q" quit command).
	StdinPipe() (io.WriteCloser, error)
	Start() error
	Wait() error
	Kill() error
}

// processWait makes a process result observable by both startup and the
// long-lived monitor without racing to consume a one-shot error channel.
// Closing done publishes err to every waiter.
type processWait struct {
	done chan struct{}
	err  error
}

// startReservation is published before startup touches external resources, so
// Stop and Close can cancel an in-flight Start even before a Session exists.
// done closes only after late tap registration and subprocess cleanup have
// reconciled; until then the channel remains reserved.
type startReservation struct {
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	processDone <-chan struct{}

	finishOnce sync.Once
	resultMu   sync.RWMutex
	cleanupErr error
}

func newStartReservation(parent context.Context) *startReservation {
	ctx, cancel := context.WithCancel(parent)
	return &startReservation{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (s *startReservation) finish() {
	s.finishOnce.Do(func() {
		s.cancel()
		close(s.done)
	})
}

func (s *startReservation) setCleanupError(err error) {
	if err == nil {
		return
	}
	s.resultMu.Lock()
	s.cleanupErr = errors.Join(s.cleanupErr, err)
	s.resultMu.Unlock()
}

func (s *startReservation) result() error {
	s.resultMu.RLock()
	defer s.resultMu.RUnlock()
	return s.cleanupErr
}

// ExecFunc starts a process. It matches the shape of exec.CommandContext.
type ExecFunc func(ctx context.Context, name string, args ...string) Command

// cmdWrapper adapts *exec.Cmd to Command.
type cmdWrapper struct {
	cmd *exec.Cmd

	boundRootOnce sync.Once
	boundRoot     *os.File
	boundRootErr  error
}

func (w *cmdWrapper) BindRecordingRoot(
	root *os.Root,
	sdpPath, outputPath, sdpName, outputName string,
) error {
	if root == nil {
		return errors.New("recording root is nil")
	}
	if !filepath.IsAbs(sdpPath) || !filepath.IsAbs(outputPath) {
		return errors.New("recording process paths must be absolute")
	}
	if runtime.GOOS == "windows" {
		// os.OpenRoot uses a directory handle without FILE_SHARE_DELETE on
		// Windows. Keeping root open pins the child-visible absolute path against
		// rename/replacement for the process lifetime.
		return nil
	}

	visiblePrefix, err := childVisibleRootPrefix()
	if err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("opening recording root for child: %w", err)
	}
	childFD := 3 + len(w.cmd.ExtraFiles)
	stableRoot := fmt.Sprintf("%s/%d", visiblePrefix, childFD)
	stableSDP := filepath.ToSlash(filepath.Join(stableRoot, sdpName))
	stableOutput := filepath.ToSlash(filepath.Join(stableRoot, outputName))
	foundSDP := replaceCommandArg(w.cmd.Args, sdpPath, stableSDP)
	foundOutput := replaceCommandArg(w.cmd.Args, outputPath, stableOutput)
	if !foundSDP || !foundOutput {
		return errors.Join(errors.New("recording paths are absent from ffmpeg arguments"), directory.Close())
	}
	w.cmd.ExtraFiles = append(w.cmd.ExtraFiles, directory)
	w.boundRoot = directory
	return nil
}

func childVisibleRootPrefix() (string, error) {
	switch runtime.GOOS {
	case "linux":
		return "/proc/self/fd", nil
	default:
		return "", fmt.Errorf("%w: stable child recording paths are unsupported on %s", ErrInvalidConfig, runtime.GOOS)
	}
}

func replaceCommandArg(args []string, oldValue, newValue string) bool {
	found := false
	for index := range args {
		if args[index] == oldValue {
			args[index] = newValue
			found = true
		}
	}
	return found
}

func (w *cmdWrapper) closeBoundRoot() error {
	w.boundRootOnce.Do(func() {
		if w.boundRoot != nil {
			w.boundRootErr = w.boundRoot.Close()
		}
	})
	return w.boundRootErr
}

func (w *cmdWrapper) CloseBeforeStart() error            { return w.closeBoundRoot() }
func (w *cmdWrapper) StdinPipe() (io.WriteCloser, error) { return w.cmd.StdinPipe() }
func (w *cmdWrapper) Start() error {
	if err := w.cmd.Start(); err != nil {
		return errors.Join(err, w.closeBoundRoot())
	}
	return nil
}
func (w *cmdWrapper) Wait() error { return errors.Join(w.cmd.Wait(), w.closeBoundRoot()) }
func (w *cmdWrapper) Kill() error {
	if w.cmd.Process == nil {
		return nil
	}
	return w.cmd.Process.Kill()
}

// defaultExec starts a real OS process.
func defaultExec(ctx context.Context, name string, args ...string) Command {
	// #nosec G204 -- the executable and arguments come from trusted server
	// configuration and are passed directly without invoking a shell.
	return &cmdWrapper{cmd: exec.CommandContext(ctx, name, args...)}
}

// TapRouter is the subset of the SFU voice facade the recorder needs to
// register its taps as channel subscribers. *webrtc.Voice satisfies it.
type TapRouter interface {
	AddTap(channelID int64, tapID string, audio, video webrtc.TrackWriter)
	RemoveTap(tapID string)
}

// Session is one active recording.
type Session struct {
	// Exported values are immutable snapshots for callers. Recorder lifecycle
	// code uses the private ownership fields below, so mutating a returned
	// Session cannot redirect cleanup or strand a channel reservation.
	ChannelID int64
	FilePath  string
	StartedAt time.Time

	channelID   int64
	filePath    string
	audioTap    *Tap
	videoTap    *BufferedTap
	root        *os.Root
	router      TapRouter
	tapID       string
	sdpPath     string
	sdpName     string
	outputName  string
	cmd         Command
	stdin       io.WriteCloser
	cancel      context.CancelFunc
	done        chan struct{}
	processDone <-chan struct{}

	stopping       atomic.Bool
	forcedStop     atomic.Bool
	hardTimedOut   atomic.Bool
	stopOnce       sync.Once
	doneOnce       sync.Once
	unregisterOnce sync.Once
	controlOnce    sync.Once
	closeTapsOnce  sync.Once
	abortTapsOnce  sync.Once
	finalizeOnce   sync.Once
	unregisterDone chan struct{}
	controlDone    chan struct{}
	tapsDone       chan struct{}
	abortDone      chan struct{}
	killDone       chan struct{}
	killMu         sync.Mutex
	killLaunched   bool
	killFinalized  bool
	resultMu       sync.RWMutex
	waitErr        error
	killErr        error
	tapCloseErr    error
	cleanupErr     error
}

// Recorder manages ffmpeg recording sessions, one per channel.
type Recorder struct {
	cfg    Config
	logger *zap.Logger

	// Exec starts the ffmpeg process. It defaults to os/exec and is exported
	// so tests can inject a fake.
	Exec ExecFunc

	mu            sync.Mutex
	sessions      map[int64]*Session
	starting      map[int64]*startReservation
	closed        bool
	startWG       sync.WaitGroup
	nextTap       atomic.Uint64
	startWaitOnce sync.Once
	startsDone    chan struct{}
	closeOnce     sync.Once
	closeErr      error

	stopGracePeriod time.Duration
	killWait        time.Duration
	now             func() time.Time
}

// New constructs a Recorder. cfg fields left at their zero value get
// defaults (see Config.withDefaults).
func New(cfg Config, logger *zap.Logger) *Recorder {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Recorder{
		cfg:             cfg.withDefaults(),
		logger:          logger,
		Exec:            defaultExec,
		sessions:        make(map[int64]*Session),
		starting:        make(map[int64]*startReservation),
		stopGracePeriod: defaultStopGracePeriod,
		killWait:        defaultKillWait,
		now:             time.Now,
		startsDone:      make(chan struct{}),
	}
}

// Start begins recording the given channel: it allocates loopback UDP ports,
// writes an SDP file describing the streams, starts ffmpeg, and registers
// taps in the router so a copy of the channel's audio and video reaches the
// process.
func (r *Recorder) Start(ctx context.Context, channelID int64, router TapRouter) (_ *Session, retErr error) {
	if !r.cfg.Enabled {
		return nil, ErrDisabled
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("recording request canceled: %w", err)
	}
	if channelID <= 0 {
		return nil, fmt.Errorf("%w: channel ID must be positive", ErrInvalidConfig)
	}
	if nilTapRouter(router) {
		return nil, fmt.Errorf("%w: tap router is nil", ErrInvalidConfig)
	}
	if err := r.cfg.validate(); err != nil {
		return nil, err
	}
	if r.Exec == nil {
		return nil, fmt.Errorf("%w: process launcher is nil", ErrInvalidConfig)
	}
	recordingDir, err := filepath.Abs(r.cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolving recording directory: %w", err)
	}
	recordingDir = filepath.Clean(recordingDir)

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	if _, active := r.sessions[channelID]; active {
		r.mu.Unlock()
		return nil, ErrAlreadyRecording
	}
	if _, starting := r.starting[channelID]; starting {
		r.mu.Unlock()
		return nil, ErrAlreadyRecording
	}
	if len(r.sessions)+len(r.starting) >= r.cfg.MaxConcurrent {
		r.mu.Unlock()
		return nil, ErrCapacity
	}
	startState := newStartReservation(ctx)
	r.starting[channelID] = startState
	r.startWG.Add(1)
	r.mu.Unlock()
	startReservationOwned := true
	defer func() {
		if startReservationOwned {
			r.releaseStart(channelID, startState)
		}
	}()

	root, err := openRecordingRoot(recordingDir)
	if err != nil {
		return nil, err
	}
	rootOwned := true
	defer func() {
		if rootOwned {
			retErr = errors.Join(retErr, root.Close())
		}
	}()
	audioPort, err := freeUDPPort()
	if err != nil {
		return nil, fmt.Errorf("allocating audio port: %w", err)
	}
	videoPort, err := freeUDPPort()
	if err != nil {
		return nil, fmt.Errorf("allocating video port: %w", err)
	}

	startedAt := r.now().UTC()
	sdpName, outputName, sdpPath, outPath, err := createRecordingPaths(
		root,
		recordingDir,
		channelID,
		startedAt,
		r.cfg.Format,
		buildSDP(audioPort, videoPort),
	)
	if err != nil {
		return nil, err
	}
	artifactsTransferred := false
	defer func() {
		if !artifactsTransferred {
			retErr = errors.Join(retErr, cleanupFailedRecordingFiles(root, sdpName, outputName))
		}
	}()
	if err := startState.ctx.Err(); err != nil {
		return nil, r.startCancellationError(fmt.Errorf("recording startup canceled: %w", err))
	}
	if err := verifyRecordingRoot(recordingDir, root); err != nil {
		return nil, err
	}

	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopStartupCancellation := context.AfterFunc(startState.ctx, cancel)
	cmd := r.Exec(processCtx, r.cfg.FFmpegPath, r.buildArgs(sdpPath, outPath)...)
	if cmd == nil {
		stopStartupCancellation()
		cancel()
		return nil, fmt.Errorf("%w: process launcher returned nil", ErrInvalidConfig)
	}
	if err := cmd.BindRecordingRoot(root, sdpPath, outPath, sdpName, outputName); err != nil {
		cancel()
		return nil, errors.Join(
			fmt.Errorf("binding ffmpeg to recording root: %w", err),
			cmd.CloseBeforeStart(),
		)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errors.Join(fmt.Errorf("ffmpeg stdin: %w", err), cmd.CloseBeforeStart())
	}
	if err := cmd.Start(); err != nil {
		pipeErr := stdin.Close()
		commandErr := cmd.CloseBeforeStart()
		cancel()
		return nil, errors.Join(fmt.Errorf("starting ffmpeg: %w", err), pipeErr, commandErr)
	}
	processResult := &processWait{done: make(chan struct{})}
	// Publish the process observation point before AddTap can notify external
	// collaborators. Stop/Close still wait on reservation.done, while tests and
	// future lifecycle coordination can distinguish an OS exit observed by the
	// recorder from a merely signaled fake process.
	startState.processDone = processResult.done
	go func() {
		processResult.err = cmd.Wait()
		close(processResult.done)
	}()
	abort := func(cause error, kill bool, extraResult func() error, extra ...<-chan struct{}) error {
		// After cmd.Start, the process cleanup path owns the retained directory
		// handle and both artifacts. Start may return on its bounded deadline, but
		// the files cannot be removed until Wait proves ffmpeg can no longer write.
		artifactsTransferred = true
		rootOwned = false
		cleanup := beginStartupCleanup(cmd, stdin, cancel, processResult.done, kill, func() error {
			cleanupErr := cleanupFailedRecordingFiles(root, sdpName, outputName)
			if err := root.Close(); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("closing recording root: %w", err))
			}
			return cleanupErr
		})
		cleanup = combineStartupCleanup(cleanup, extraResult, extra...)
		wait := r.killWait
		if wait <= 0 {
			wait = defaultKillWait
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case cleanupErr := <-cleanup:
			startState.setCleanupError(cleanupErr)
			return errors.Join(cause, cleanupErr)
		case <-timer.C:
			// Cleanup continues under the original channel reservation. This
			// bounds Start while ensuring a replacement cannot overlap an
			// ffmpeg process or collaborator we no longer own conclusively.
			startReservationOwned = false
			go func() {
				cleanupErr := <-cleanup
				startState.setCleanupError(cleanupErr)
				if cleanupErr != nil {
					r.logger.Warn("late recording startup cleanup failed",
						zap.Int64("channel_id", channelID),
						zap.Error(cleanupErr),
					)
				}
				r.releaseStart(channelID, startState)
			}()
			return errors.Join(cause, ErrStartupCleanupTimeout)
		}
	}
	if err := verifyRecordingRoot(recordingDir, root); err != nil {
		return nil, abort(fmt.Errorf("recording root changed while starting ffmpeg: %w", err), true, nil)
	}

	audioTap, err := NewTap(audioPort)
	if err != nil {
		return nil, abort(fmt.Errorf("audio tap: %w", err), true, nil)
	}
	videoTap, err := NewBufferedTap(videoPort, videoRingBytes)
	if err != nil {
		_ = audioTap.Close()
		return nil, abort(fmt.Errorf("video tap: %w", err), true, nil)
	}
	if err := startState.ctx.Err(); err != nil {
		_ = audioTap.Close()
		_ = videoTap.Abort()
		return nil, abort(r.startCancellationError(
			fmt.Errorf("recording request canceled during startup: %w", err)), true, nil)
	}
	select {
	case <-processResult.done:
		_ = audioTap.Close()
		_ = videoTap.Abort()
		cleanupErr := abort(nil, false, nil)
		waitErr := processResult.err
		if waitErr == nil {
			return nil, errors.Join(errors.New("ffmpeg exited during startup"), cleanupErr)
		}
		return nil, errors.Join(fmt.Errorf("ffmpeg exited during startup: %w", waitErr), cleanupErr)
	default:
	}

	s := &Session{
		ChannelID:      channelID,
		FilePath:       outPath,
		StartedAt:      startedAt,
		channelID:      channelID,
		filePath:       outPath,
		audioTap:       audioTap,
		videoTap:       videoTap,
		root:           root,
		router:         router,
		tapID:          tapID(channelID, r.nextTap.Add(1)),
		sdpPath:        sdpPath,
		sdpName:        sdpName,
		outputName:     outputName,
		cmd:            cmd,
		stdin:          stdin,
		cancel:         cancel,
		done:           make(chan struct{}),
		processDone:    processResult.done,
		unregisterDone: make(chan struct{}),
		controlDone:    make(chan struct{}),
		tapsDone:       make(chan struct{}),
		abortDone:      make(chan struct{}),
		killDone:       make(chan struct{}),
	}
	addDone := make(chan struct{})
	go func() {
		router.AddTap(channelID, s.tapID, audioTap, videoTap)
		close(addDone)
	}()
	select {
	case <-addDone:
	case <-processResult.done:
		s.unregisterAfter(addDone)
		s.abortTaps()
		cleanupErr := abort(
			nil,
			false,
			s.result,
			s.unregisterDone,
			s.tapsDone,
			s.abortDone,
		)
		if processResult.err == nil {
			return nil, errors.Join(errors.New("ffmpeg exited while registering taps"), cleanupErr)
		}
		return nil, errors.Join(
			fmt.Errorf("ffmpeg exited while registering taps: %w", processResult.err),
			cleanupErr,
		)
	case <-startState.ctx.Done():
		s.unregisterAfter(addDone)
		s.abortTaps()
		return nil, abort(
			r.startCancellationError(fmt.Errorf(
				"recording request canceled while registering taps: %w", startState.ctx.Err())),
			true,
			s.result,
			s.unregisterDone,
			s.tapsDone,
			s.abortDone,
		)
	}
	select {
	case <-processResult.done:
		s.unregister()
		s.abortTaps()
		cleanupErr := abort(
			nil,
			false,
			s.result,
			s.unregisterDone,
			s.tapsDone,
			s.abortDone,
		)
		if processResult.err == nil {
			return nil, errors.Join(errors.New("ffmpeg exited while registering taps"), cleanupErr)
		}
		return nil, errors.Join(
			fmt.Errorf("ffmpeg exited while registering taps: %w", processResult.err),
			cleanupErr,
		)
	default:
	}
	if err := startState.ctx.Err(); err != nil {
		s.unregister()
		s.abortTaps()
		return nil, abort(
			r.startCancellationError(fmt.Errorf(
				"recording request canceled after registering taps: %w", err)),
			true,
			s.result,
			s.unregisterDone,
			s.tapsDone,
			s.abortDone,
		)
	}
	// The request context owns startup only. Once taps are registered, the
	// recording deliberately outlives cancellation of the initiating request.
	// A false result means cancellation already won the handoff and may have
	// canceled the process, so publishing would expose a doomed session.
	if !stopStartupCancellation() {
		s.unregister()
		s.abortTaps()
		return nil, abort(
			r.startCancellationError(fmt.Errorf(
				"recording request canceled at publication: %w", startState.ctx.Err())),
			true,
			s.result,
			s.unregisterDone,
			s.tapsDone,
			s.abortDone,
		)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		s.unregister()
		s.abortTaps()
		return nil, abort(ErrClosed, true, s.result, s.unregisterDone, s.tapsDone, s.abortDone)
	}
	r.sessions[channelID] = s
	delete(r.starting, channelID)
	r.mu.Unlock()
	startReservationOwned = false
	startState.finish()
	r.startWG.Done()
	artifactsTransferred = true
	rootOwned = false
	go r.monitor(s, processResult)

	r.logger.Info("recording started",
		zap.Int64("channel_id", channelID),
		zap.String("output", outPath),
	)
	return s, nil
}

func nilTapRouter(router TapRouter) bool {
	if router == nil {
		return true
	}
	value := reflect.ValueOf(router)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (r *Recorder) startCancellationError(cause error) error {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return errors.Join(ErrClosed, cause)
	}
	return cause
}

// Stop ends the recording for the channel: it removes the router taps, asks
// ffmpeg to quit gracefully (the "q" command on stdin), and kills the
// process if it does not exit within the grace period.
func (r *Recorder) Stop(channelID int64) error {
	r.mu.Lock()
	s, ok := r.sessions[channelID]
	starting := r.starting[channelID]
	r.mu.Unlock()
	if ok {
		return r.stopSession(s)
	}
	if starting == nil {
		return ErrNotRecording
	}
	starting.cancel()
	wait := r.killWait
	if wait <= 0 {
		wait = defaultKillWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-starting.done:
		// Startup may have crossed its publication boundary just before this
		// Stop canceled the reservation. The map transition is atomic, so a
		// post-completion recheck either owns that Session or confirms cleanup.
		r.mu.Lock()
		published := r.sessions[channelID]
		r.mu.Unlock()
		if published != nil {
			return r.stopSession(published)
		}
		return starting.result()
	case <-timer.C:
		return errors.Join(
			ErrStopTimeout,
			errors.New("recording startup cleanup remains in progress; channel stays reserved"),
		)
	}
}

func (r *Recorder) stopSession(s *Session) error {
	grace := r.stopGracePeriod
	if grace <= 0 {
		grace = defaultStopGracePeriod
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	// Start the deadline before touching collaborators. requestStop only
	// schedules external router/control work and aborts the owned taps, so a
	// wedged pipe or router cannot keep this goroutine outside the select.
	s.requestStop()
	select {
	case <-s.done:
		r.logger.Info("recording stopped",
			zap.Int64("channel_id", s.channelID),
			zap.String("output", s.filePath),
		)
		return s.stopResult()
	case <-timer.C:
		s.forcedStop.Store(true)
		// Discard any video backlog before terminating ffmpeg. The graceful
		// path gets the full grace window to drain it first.
		s.abortTaps()
		killWait := r.killWait
		if killWait <= 0 {
			killWait = defaultKillWait
		}
		killTimer := time.NewTimer(killWait)
		defer killTimer.Stop()
		if s.cancel != nil {
			s.cancel()
		}
		processExited := false
		select {
		case <-s.processDone:
			processExited = true
			// Wait already returned; the remaining deadline applies only to
			// tracked cleanup collaborators.
		default:
			r.logger.Warn("ffmpeg did not exit in time, killing",
				zap.Int64("channel_id", s.channelID),
			)
			s.startKill()
		}
		if processExited {
			r.logger.Warn("recording cleanup exceeded grace period",
				zap.Int64("channel_id", s.channelID),
			)
		}
		select {
		case <-s.done:
			return s.stopResult()
		case <-killTimer.C:
			// Process ownership is not complete until Wait returns. Keep the
			// channel reserved so a replacement cannot start alongside a wedged
			// or orphaned ffmpeg process; monitor will detach it after reaping.
			s.hardTimedOut.Store(true)
			return errors.Join(
				ErrStopTimeout,
				errors.New("recording cleanup remains in progress; channel stays reserved until process and collaborators are reaped"),
			)
		}
	}
}

// SessionCount returns the number of active recording sessions.
func (r *Recorder) SessionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Close stops all active sessions.
func (r *Recorder) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.closeAll()
	})
	return r.closeErr
}

func (r *Recorder) closeAll() error {
	r.mu.Lock()
	r.closed = true
	starts := make([]pendingStart, 0, len(r.starting))
	for channelID, state := range r.starting {
		starts = append(starts, pendingStart{channelID: channelID, state: state})
	}
	r.mu.Unlock()
	sort.Slice(starts, func(i, j int) bool { return starts[i].channelID < starts[j].channelID })
	for _, start := range starts {
		start.state.cancel()
	}
	startErr := r.waitForStarts(starts)

	r.mu.Lock()
	sessions := make([]*Session, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	r.mu.Unlock()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].channelID < sessions[j].channelID })
	for _, session := range sessions {
		session.requestStop()
	}
	errs := make(chan error, len(sessions))
	var stops sync.WaitGroup
	stops.Add(len(sessions))
	for _, session := range sessions {
		go func() {
			defer stops.Done()
			if err := r.stopSession(session); err != nil {
				errs <- fmt.Errorf("stopping channel %d: %w", session.channelID, err)
			}
		}()
	}
	stops.Wait()
	close(errs)
	joined := []error{startErr}
	for err := range errs {
		joined = append(joined, err)
	}
	return errors.Join(joined...)
}

type pendingStart struct {
	channelID int64
	state     *startReservation
}

func (r *Recorder) waitForStarts(starts []pendingStart) error {
	r.startWaitOnce.Do(func() {
		go func() {
			r.startWG.Wait()
			close(r.startsDone)
		}()
	})
	wait := r.killWait
	if wait <= 0 {
		wait = defaultKillWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-r.startsDone:
		var errs []error
		for _, start := range starts {
			if err := start.state.result(); err != nil {
				errs = append(errs, fmt.Errorf("cleaning up channel %d startup: %w", start.channelID, err))
			}
		}
		return errors.Join(errs...)
	case <-timer.C:
		return ErrStartDrainTimeout
	}
}

func openRecordingRoot(dir string) (*os.Root, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving recording dir: %w", err)
	}
	dir = filepath.Clean(dir)
	if err := validateRecordingRootTarget(dir); err != nil {
		return nil, err
	}
	_, err = ensureRecordingDirectory(dir)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspecting recording dir: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("%w: recording path must be a real directory", ErrInvalidConfig)
	}
	// Mkdir's 0700 request can only be narrowed by umask, never widened. Do not
	// chmod here: the leaf may have been replaced after creation, and recorder
	// startup must never mutate a path it did not conclusively create and hold.
	if err := validateExistingRecordingDirectory(before); err != nil {
		return nil, err
	}
	if err := rejectCurrentWorkingDirectory(dir, before); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening recording root: %w", err)
	}
	if err := verifyOpenedRecordingRoot(root, before); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}

func validateRecordingRootTarget(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: recording directory must resolve to an absolute path", ErrInvalidConfig)
	}
	volumeRoot := filepath.Clean(filepath.VolumeName(dir) + string(filepath.Separator))
	if filepath.Clean(dir) == volumeRoot {
		return fmt.Errorf("%w: recording directory must not be a filesystem or volume root", ErrInvalidConfig)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving current working directory: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("resolving absolute current working directory: %w", err)
	}
	if filepath.Clean(dir) == filepath.Clean(cwd) {
		return fmt.Errorf("%w: recording directory must not be the current working directory", ErrInvalidConfig)
	}
	return nil
}

func ensureRecordingDirectory(dir string) (bool, error) {
	if _, err := os.Lstat(dir); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspecting recording dir before creation: %w", err)
	}
	if runtime.GOOS == "windows" {
		return false, fmt.Errorf(
			"%w: Windows recording directory must already exist with its acknowledged restricted NTFS DACL",
			ErrInvalidConfig,
		)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return false, fmt.Errorf("creating recording dir parent: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("creating recording dir: %w", err)
	}
	// Another actor won the leaf creation race. Treat it as pre-existing and
	// validate it without changing its permissions.
	return false, nil
}

func validateExistingRecordingDirectory(info os.FileInfo) error {
	if info == nil || !info.IsDir() {
		return fmt.Errorf("%w: existing recording path is not a directory", ErrInvalidConfig)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(
			"%w: existing recording directory permissions %o allow group or other access",
			ErrInvalidConfig,
			info.Mode().Perm(),
		)
	}
	if err := validateRecordingDirectoryOwner(info); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return nil
}

func rejectCurrentWorkingDirectory(dir string, info os.FileInfo) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving current working directory: %w", err)
	}
	cwdInfo, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("inspecting current working directory: %w", err)
	}
	if info != nil && os.SameFile(info, cwdInfo) {
		return fmt.Errorf("%w: recording directory must not be the current working directory %q", ErrInvalidConfig, dir)
	}
	return nil
}

func createRecordingPaths(
	root *os.Root,
	dir string,
	channelID int64,
	startedAt time.Time,
	format string,
	sdp string,
) (sdpName, outputName, sdpPath, outPath string, retErr error) {
	if root == nil {
		return "", "", "", "", errors.New("recording root is nil")
	}
	prefix := fmt.Sprintf("channel-%d-%s-", channelID, startedAt.Format("20060102-150405.000000000"))
	file, sdpName, outputName, err := createUniqueRecordingFiles(root, prefix, format)
	if err != nil {
		return "", "", "", "", err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, file.Close(), root.Remove(sdpName))
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", "", "", "", fmt.Errorf("restricting sdp permissions: %w", err)
	}
	if _, err := io.WriteString(file, sdp); err != nil {
		return "", "", "", "", fmt.Errorf("writing sdp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", "", "", "", fmt.Errorf("syncing sdp file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", "", "", "", fmt.Errorf("closing sdp file: %w", err)
	}
	sdpPath = filepath.Join(dir, sdpName)
	outPath = filepath.Join(dir, outputName)
	return sdpName, outputName, sdpPath, outPath, nil
}

func createUniqueRecordingFiles(
	root *os.Root,
	prefix, format string,
) (*os.File, string, string, error) {
	for range 16 {
		var entropy [12]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return nil, "", "", fmt.Errorf("generating recording filename: %w", err)
		}
		base := prefix + hex.EncodeToString(entropy[:])
		sdpName := base + ".sdp"
		outputName := base + "." + format
		if _, err := root.Lstat(outputName); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", "", fmt.Errorf("checking recording output collision: %w", err)
		}
		file, err := root.OpenFile(sdpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", "", fmt.Errorf("creating unique sdp file: %w", err)
		}
		return file, sdpName, outputName, nil
	}
	return nil, "", "", errors.New("creating unique recording paths: collision retry limit exceeded")
}

func verifyRecordingRoot(dir string, root *os.Root) error {
	before, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspecting recording root path: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return fmt.Errorf("%w: recording root path is not a real directory", ErrInvalidConfig)
	}
	return verifyOpenedRecordingRoot(root, before)
}

func verifyOpenedRecordingRoot(root *os.Root, expected os.FileInfo) error {
	if root == nil {
		return errors.New("recording root is nil")
	}
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("opening retained recording root: %w", err)
	}
	opened, statErr := directory.Stat()
	if statErr != nil {
		return errors.Join(fmt.Errorf("inspecting retained recording root: %w", statErr), directory.Close())
	}
	if !opened.IsDir() || !os.SameFile(expected, opened) {
		return errors.Join(
			fmt.Errorf("%w: recording root changed while opening", ErrInvalidConfig),
			directory.Close(),
		)
	}
	if err := validateExistingRecordingDirectory(opened); err != nil {
		return errors.Join(err, directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("closing retained recording root: %w", err)
	}
	return nil
}

// beginStartupCleanup takes ownership of a subprocess that launched but could
// not be published as a recording session. It reports completion only after
// Wait, stdin closure, and (when requested) Kill have all returned. Once Wait
// returns, cleanupArtifacts runs exactly once so a failed startup cannot leave
// a process-writable output behind. Callers may impose a response deadline, but
// must retain the channel reservation until the returned channel yields.
func beginStartupCleanup(
	cmd Command,
	stdin io.WriteCloser,
	cancel context.CancelFunc,
	processDone <-chan struct{},
	kill bool,
	cleanupArtifacts func() error,
) <-chan error {
	cancel()
	result := make(chan error, 1)
	go func() {
		var operations sync.WaitGroup
		errResults := make(chan error, 2)
		if stdin != nil {
			operations.Add(1)
			go func() {
				defer operations.Done()
				if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					errResults <- fmt.Errorf("closing ffmpeg stdin: %w", err)
				}
			}()
		}
		if kill {
			operations.Add(1)
			go func() {
				defer operations.Done()
				if err := ignoreProcessDone(cmd.Kill()); err != nil {
					errResults <- fmt.Errorf("killing ffmpeg: %w", err)
				}
			}()
		}

		<-processDone
		artifactErr := error(nil)
		if cleanupArtifacts != nil {
			artifactErr = cleanupArtifacts()
		}
		operations.Wait()
		close(errResults)
		var errs []error
		for err := range errResults {
			errs = append(errs, err)
		}
		errs = append(errs, artifactErr)
		result <- errors.Join(errs...)
		close(result)
	}()
	return result
}

func cleanupFailedRecordingFiles(root *os.Root, sdpName, outputName string) error {
	if root == nil {
		return errors.New("recording root is nil")
	}
	var cleanupErrs []error
	if err := removeRecordingArtifact(root, sdpName); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("removing startup sdp: %w", err))
	}
	if err := removeRecordingArtifact(root, outputName); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("removing partial recording output: %w", err))
		// If removal fails, still apply the final-output checks and restrictive
		// mode. The cleanup error remains visible, but the stopped partial is not
		// left with broad permissions.
		if secureErr := secureRecordingOutput(root, outputName); secureErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("securing retained partial output: %w", secureErr))
		}
	}
	return errors.Join(cleanupErrs...)
}

func removeRecordingArtifact(root *os.Root, name string) error {
	if name == "" || filepath.Base(name) != name {
		return errors.New("recording artifact name is invalid")
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func combineStartupCleanup(
	processCleanup <-chan error,
	extraResult func() error,
	waits ...<-chan struct{},
) <-chan error {
	result := make(chan error, 1)
	go func() {
		processErr := <-processCleanup
		for _, done := range waits {
			if done != nil {
				<-done
			}
		}
		if extraResult != nil {
			processErr = errors.Join(processErr, extraResult())
		}
		result <- processErr
		close(result)
	}()
	return result
}

func ignoreProcessDone(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (r *Recorder) releaseStart(channelID int64, state *startReservation) {
	r.mu.Lock()
	if current := r.starting[channelID]; current == state {
		delete(r.starting, channelID)
	}
	r.mu.Unlock()
	state.finish()
	r.startWG.Done()
}

func (r *Recorder) monitor(s *Session, processResult *processWait) {
	<-processResult.done
	waitErr := processResult.err
	s.resultMu.Lock()
	s.waitErr = waitErr
	s.resultMu.Unlock()
	r.finalize(s)
	s.signalDone()
	if s.hardTimedOut.Load() {
		fields := []zap.Field{
			zap.Int64("channel_id", s.channelID),
			zap.String("output", s.filePath),
		}
		if resultErr := s.result(); resultErr != nil {
			fields = append(fields, zap.Error(resultErr))
		}
		r.logger.Warn("recording cleanup completed after stop deadline", fields...)
	}

	if !s.stopping.Load() {
		fields := []zap.Field{
			zap.Int64("channel_id", s.channelID),
			zap.String("output", s.filePath),
		}
		if resultErr := s.result(); resultErr != nil {
			fields = append(fields, zap.Error(resultErr))
		}
		r.logger.Warn("ffmpeg exited unexpectedly; recording cleaned up", fields...)
	}
}

func (s *Session) signalDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (r *Recorder) finalize(s *Session) {
	s.finalizeOnce.Do(func() {
		s.unregister()
		s.abortTaps()
		s.closeControl(false)
		if s.cancel != nil {
			s.cancel()
		}
		// A session remains published until every operation launched on its
		// behalf has reconciled. This prevents a replacement from overlapping
		// a late router removal, process kill, pipe close, or tap writer.
		<-s.unregisterDone
		<-s.controlDone
		<-s.tapsDone
		<-s.abortDone
		s.awaitKill()

		var cleanupErrs []error
		if s.root == nil {
			cleanupErrs = append(cleanupErrs, errors.New("recording root handle is missing"))
		} else if err := s.root.Remove(s.sdpName); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("removing sdp file: %w", err))
		}
		if s.root != nil {
			if err := secureRecordingOutput(s.root, s.outputName); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
			if err := s.root.Close(); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("closing recording root: %w", err))
			}
		}

		s.resultMu.Lock()
		s.cleanupErr = errors.Join(s.cleanupErr, errors.Join(cleanupErrs...))
		s.resultMu.Unlock()
		r.detachSession(s)
	})
}

func secureRecordingOutput(root *os.Root, name string) error {
	if root == nil {
		return errors.New("recording root is nil")
	}
	if name == "" || filepath.Base(name) != name {
		return errors.New("recording output name is invalid")
	}
	before, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("recording output is missing after ffmpeg exit")
		}
		return fmt.Errorf("inspecting recording output: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return errors.New("recording output is not a regular file")
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening recording output: %w", err)
	}
	opened, err := file.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("inspecting opened recording output: %w", err), file.Close())
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return errors.Join(errors.New("recording output changed while opening"), file.Close())
	}
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("restricting recording permissions: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("syncing recording output: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing recording output: %w", err)
	}
	return nil
}

func (r *Recorder) detachSession(s *Session) {
	r.mu.Lock()
	if current, ok := r.sessions[s.channelID]; ok && current == s {
		delete(r.sessions, s.channelID)
	}
	r.mu.Unlock()
}

func (s *Session) requestStop() {
	s.stopping.Store(true)
	s.stopOnce.Do(func() {
		s.unregister()
		s.closeTapsGracefully()
		s.closeControl(true)
	})
}

func (s *Session) unregister() {
	s.unregisterOnce.Do(func() {
		// TapRouter is an interface supplied by another subsystem. The owned
		// taps are closed independently, so removal can safely finish in the
		// background without making recorder shutdown depend on that subsystem.
		go func() {
			defer close(s.unregisterDone)
			if s.router != nil {
				s.router.RemoveTap(s.tapID)
			}
		}()
	})
}

func (s *Session) unregisterAfter(registered <-chan struct{}) {
	s.unregisterOnce.Do(func() {
		go func() {
			defer close(s.unregisterDone)
			if registered != nil {
				<-registered
			}
			if s.router != nil {
				s.router.RemoveTap(s.tapID)
			}
		}()
	})
}

func (s *Session) closeControl(graceful bool) {
	s.controlOnce.Do(func() {
		// A pipe Write or Close can block in a faulty process implementation.
		// The process context/kill deadline remains responsible for termination;
		// this best-effort control request must never hold the Stop caller.
		go func() {
			defer close(s.controlDone)
			if s.stdin == nil {
				return
			}
			if graceful {
				// Stop new media and drain the bounded video queue before telling
				// ffmpeg to finalize its container.
				<-s.tapsDone
				if _, err := io.WriteString(s.stdin, "q"); err != nil {
					s.addCleanupError(fmt.Errorf("requesting ffmpeg quit: %w", err))
				}
			}
			if err := s.stdin.Close(); err != nil {
				s.addCleanupError(fmt.Errorf("closing ffmpeg stdin: %w", err))
			}
		}()
	})
}

func (s *Session) closeTapsGracefully() {
	s.closeTapsOnce.Do(func() {
		go func() {
			defer close(s.tapsDone)
			var errs []error
			if s.audioTap != nil {
				if err := s.audioTap.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					errs = append(errs, fmt.Errorf("closing audio tap: %w", err))
				}
			}
			if s.videoTap != nil {
				if err := s.videoTap.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					errs = append(errs, fmt.Errorf("draining video tap: %w", err))
				}
			}
			s.addTapCloseError(errors.Join(errs...))
		}()
	})
}

func (s *Session) abortTaps() {
	s.closeTapsGracefully()
	s.abortTapsOnce.Do(func() {
		go func() {
			defer close(s.abortDone)
			var errs []error
			if s.audioTap != nil {
				if err := s.audioTap.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					errs = append(errs, fmt.Errorf("aborting audio tap: %w", err))
				}
			}
			if s.videoTap != nil {
				if err := s.videoTap.Abort(); err != nil && !errors.Is(err, net.ErrClosed) {
					errs = append(errs, fmt.Errorf("aborting video tap: %w", err))
				}
			}
			s.addTapCloseError(errors.Join(errs...))
		}()
	})
}

func (s *Session) addTapCloseError(err error) {
	if err == nil {
		return
	}
	s.resultMu.Lock()
	s.tapCloseErr = errors.Join(s.tapCloseErr, err)
	s.resultMu.Unlock()
}

func (s *Session) startKill() {
	s.killMu.Lock()
	if s.killFinalized || s.killLaunched {
		s.killMu.Unlock()
		return
	}
	s.killLaunched = true
	s.killMu.Unlock()
	go func() {
		err := ignoreProcessDone(s.cmd.Kill())
		s.resultMu.Lock()
		s.killErr = err
		s.resultMu.Unlock()
		close(s.killDone)
	}()
}

func (s *Session) awaitKill() {
	s.killMu.Lock()
	if !s.killLaunched {
		s.killFinalized = true
		close(s.killDone)
	}
	s.killMu.Unlock()
	<-s.killDone
}

func (s *Session) addCleanupError(err error) {
	if err == nil {
		return
	}
	s.resultMu.Lock()
	s.cleanupErr = errors.Join(s.cleanupErr, err)
	s.resultMu.Unlock()
}

func (s *Session) result() error {
	s.resultMu.RLock()
	defer s.resultMu.RUnlock()
	var waitErr error
	if s.waitErr != nil {
		waitErr = fmt.Errorf("ffmpeg exited: %w", s.waitErr)
	}
	return errors.Join(waitErr, s.killErr, s.tapCloseErr, s.cleanupErr)
}

func (s *Session) stopResult() error {
	if s.forcedStop.Load() {
		return errors.Join(ErrStopTimeout, s.result())
	}
	return s.result()
}

// buildArgs constructs the ffmpeg command line: read the streams described
// by the SDP file, apply the configured audio/video output options, and
// write the output file.
func (r *Recorder) buildArgs(sdpPath, outPath string) []string {
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-protocol_whitelist", "file,udp,rtp",
		"-i", sdpPath,
	}
	args = append(args, r.cfg.AudioArgs...)
	args = append(args, r.cfg.VideoArgs...)
	return append(args, "-n", outPath)
}

// buildSDP generates the SDP file describing the two loopback RTP streams
// (Opus audio, VP8 video) that ffmpeg reads.
func buildSDP(audioPort, videoPort int) string {
	return fmt.Sprintf(`v=0
o=- 0 0 IN IP4 127.0.0.1
s=voicx recording
c=IN IP4 127.0.0.1
t=0 0
m=audio %d RTP/AVP 111
a=rtpmap:111 opus/48000/2
m=video %d RTP/AVP 96
a=rtpmap:96 VP8/90000
`, audioPort, videoPort)
}

// tapID returns the router clientID used for a channel's recording tap.
func tapID(channelID int64, generation uint64) string {
	return fmt.Sprintf("recorder:%d:%d", channelID, generation)
}

// freeUDPPort finds an available loopback UDP port. There is an inherent
// race between releasing the port and ffmpeg binding it; acceptable for the
// loopback recording path.
func freeUDPPort() (int, error) {
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return 0, err
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	return port, l.Close()
}

// Tap implements webrtc.TrackWriter, forwarding RTP packets to the ffmpeg
// process's UDP input.
type Tap struct {
	conn *net.UDPConn
	addr *net.UDPAddr

	closeOnce sync.Once
	closeErr  error
}

// BufferedTap is a bounded asynchronous RTP writer used by the video recorder.
// It owns packet copies because router packets are only valid during WriteRTP.
type BufferedTap struct {
	tap      *Tap
	maxBytes int
	queue    chan []byte
	abort    chan struct{}
	done     chan struct{}

	mu          sync.Mutex
	queued      int
	dropped     uint64
	writeErrors uint64
	closed      bool
	abortOnce   sync.Once
}

// NewBufferedTap creates a UDP tap with a bounded in-memory write ring.
func NewBufferedTap(port, maxBytes int) (*BufferedTap, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("recorder buffer size must be positive")
	}
	tap, err := NewTap(port)
	if err != nil {
		return nil, err
	}
	capacity := max(1, maxBytes/1200)
	b := &BufferedTap{
		tap: tap, maxBytes: maxBytes,
		queue: make(chan []byte, capacity), abort: make(chan struct{}), done: make(chan struct{}),
	}
	go b.writeLoop()
	return b, nil
}

// WriteRTP copies and queues a packet without waiting for the UDP consumer.
func (b *BufferedTap) WriteRTP(pkt *rtp.Packet) error {
	if pkt == nil {
		return ErrInvalidPacket
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return net.ErrClosed
	}
	if b.queued+len(raw) > b.maxBytes {
		b.dropped++
		return nil
	}
	select {
	case b.queue <- raw:
		b.queued += len(raw)
	default:
		b.dropped++
	}
	return nil
}

func (b *BufferedTap) writeLoop() {
	defer close(b.done)
	for {
		// Give abort priority over a ready queue so shutdown discards the
		// backlog instead of probabilistically draining thousands of packets.
		select {
		case <-b.abort:
			b.mu.Lock()
			b.queued = 0
			b.mu.Unlock()
			return
		default:
		}
		select {
		case <-b.abort:
			b.mu.Lock()
			b.queued = 0
			b.mu.Unlock()
			return
		case raw, ok := <-b.queue:
			if !ok {
				return
			}
			_, err := b.tap.conn.WriteToUDP(raw, b.tap.addr)
			b.mu.Lock()
			b.queued -= len(raw)
			if err != nil {
				b.writeErrors++
			}
			b.mu.Unlock()
		}
	}
}

// Dropped returns the number of packets discarded after the ring filled.
func (b *BufferedTap) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// WriteErrors returns the number of queued packets that could not be written
// to the loopback UDP socket.
func (b *BufferedTap) WriteErrors() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writeErrors
}

// QueuedBytes reports the current bounded-ring occupancy.
func (b *BufferedTap) QueuedBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queued
}

// Close flushes queued packets, stops the writer, and releases its socket.
func (b *BufferedTap) Close() error {
	b.beginClose()
	<-b.done
	return b.tap.Close()
}

// Abort discards queued packets and releases the socket. Closing the socket
// first unblocks any in-flight UDP write, so recorder shutdown does not wait
// for the normal flush semantics of Close.
func (b *BufferedTap) Abort() error {
	if b == nil {
		return nil
	}
	b.beginClose()
	b.abortOnce.Do(func() { close(b.abort) })
	err := b.tap.Close()
	<-b.done
	// writeLoop exits immediately on abort, so explicitly drain the now-closed
	// channel to release references to every queued RTP byte slice.
	for range b.queue {
	}
	b.mu.Lock()
	b.queued = 0
	b.mu.Unlock()
	return err
}

func (b *BufferedTap) beginClose() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.queue)
	b.mu.Unlock()
}

// NewTap creates a Tap sending to 127.0.0.1:port.
func NewTap(port int) (*Tap, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("recorder destination port %d is outside 1..65535", port)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, err
	}
	return &Tap{
		conn: conn,
		addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
	}, nil
}

// WriteRTP marshals the packet and sends it to the ffmpeg input.
func (t *Tap) WriteRTP(pkt *rtp.Packet) error {
	if pkt == nil {
		return ErrInvalidPacket
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	_, err = t.conn.WriteToUDP(raw, t.addr)
	return err
}

// Close releases the tap's socket.
func (t *Tap) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.conn != nil {
			t.closeErr = t.conn.Close()
		}
	})
	return t.closeErr
}
