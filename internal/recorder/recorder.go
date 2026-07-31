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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

// ErrNotRecording is returned by Stop when no session exists for the channel.
var ErrNotRecording = errors.New("recorder: channel is not being recorded")

// stopGracePeriod is how long Stop waits for ffmpeg to finalize the output
// after receiving the quit command before killing the process.
const stopGracePeriod = 5 * time.Second

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
}

// withDefaults fills unset fields with their defaults.
func (c Config) withDefaults() Config {
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
	return c
}

// Command abstracts an OS process so tests can fake exec.
type Command interface {
	// StdinPipe returns the process's standard input (used for the graceful
	// "q" quit command).
	StdinPipe() (io.WriteCloser, error)
	Start() error
	Wait() error
	Kill() error
}

// ExecFunc starts a process. It matches the shape of exec.CommandContext.
type ExecFunc func(ctx context.Context, name string, args ...string) Command

// cmdWrapper adapts *exec.Cmd to Command.
type cmdWrapper struct {
	cmd *exec.Cmd
}

func (w *cmdWrapper) StdinPipe() (io.WriteCloser, error) { return w.cmd.StdinPipe() }
func (w *cmdWrapper) Start() error                       { return w.cmd.Start() }
func (w *cmdWrapper) Wait() error                        { return w.cmd.Wait() }
func (w *cmdWrapper) Kill() error {
	if w.cmd.Process == nil {
		return nil
	}
	return w.cmd.Process.Kill()
}

// defaultExec starts a real OS process.
func defaultExec(ctx context.Context, name string, args ...string) Command {
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
	ChannelID int64
	FilePath  string
	StartedAt time.Time

	// AudioTap and VideoTap are the router-facing track writers feeding the
	// ffmpeg process.
	AudioTap *Tap
	VideoTap *Tap

	router TapRouter
	tapID  string
	cmd    Command
	stdin  io.WriteCloser
	done   chan error
}

// Recorder manages ffmpeg recording sessions, one per channel.
type Recorder struct {
	cfg    Config
	logger *zap.Logger

	// Exec starts the ffmpeg process. It defaults to os/exec and is exported
	// so tests can inject a fake.
	Exec ExecFunc

	mu       sync.Mutex
	sessions map[int64]*Session
}

// New constructs a Recorder. cfg fields left at their zero value get
// defaults (see Config.withDefaults).
func New(cfg Config, logger *zap.Logger) *Recorder {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Recorder{
		cfg:      cfg.withDefaults(),
		logger:   logger,
		Exec:     defaultExec,
		sessions: make(map[int64]*Session),
	}
}

// Start begins recording the given channel: it allocates loopback UDP ports,
// writes an SDP file describing the streams, starts ffmpeg, and registers
// taps in the router so a copy of the channel's audio and video reaches the
// process.
func (r *Recorder) Start(ctx context.Context, channelID int64, router TapRouter) (*Session, error) {
	if !r.cfg.Enabled {
		return nil, ErrDisabled
	}

	r.mu.Lock()
	if _, ok := r.sessions[channelID]; ok {
		r.mu.Unlock()
		return nil, ErrAlreadyRecording
	}
	r.mu.Unlock()

	if err := os.MkdirAll(r.cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating recording dir: %w", err)
	}

	audioPort, err := freeUDPPort()
	if err != nil {
		return nil, fmt.Errorf("allocating audio port: %w", err)
	}
	videoPort, err := freeUDPPort()
	if err != nil {
		return nil, fmt.Errorf("allocating video port: %w", err)
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	base := fmt.Sprintf("channel-%d-%s", channelID, stamp)
	sdpPath := filepath.Join(r.cfg.Dir, base+".sdp")
	outPath := filepath.Join(r.cfg.Dir, base+"."+r.cfg.Format)

	if err := os.WriteFile(sdpPath, []byte(buildSDP(audioPort, videoPort)), 0o600); err != nil {
		return nil, fmt.Errorf("writing sdp file: %w", err)
	}

	args := r.buildArgs(sdpPath, outPath)
	cmd := r.Exec(ctx, r.cfg.FFmpegPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}

	audioTap, err := NewTap(audioPort)
	if err != nil {
		_ = cmd.Kill()
		return nil, fmt.Errorf("audio tap: %w", err)
	}
	videoTap, err := NewTap(videoPort)
	if err != nil {
		_ = audioTap.Close()
		_ = cmd.Kill()
		return nil, fmt.Errorf("video tap: %w", err)
	}

	s := &Session{
		ChannelID: channelID,
		FilePath:  outPath,
		StartedAt: time.Now(),
		AudioTap:  audioTap,
		VideoTap:  videoTap,
		router:    router,
		tapID:     tapID(channelID),
		cmd:       cmd,
		stdin:     stdin,
		done:      make(chan error, 1),
	}
	go func() { s.done <- cmd.Wait() }()

	router.AddTap(channelID, s.tapID, audioTap, videoTap)

	r.mu.Lock()
	r.sessions[channelID] = s
	r.mu.Unlock()

	r.logger.Info("recording started",
		zap.Int64("channel_id", channelID),
		zap.String("output", outPath),
	)
	return s, nil
}

// Stop ends the recording for the channel: it removes the router taps, asks
// ffmpeg to quit gracefully (the "q" command on stdin), and kills the
// process if it does not exit within the grace period.
func (r *Recorder) Stop(channelID int64) error {
	r.mu.Lock()
	s, ok := r.sessions[channelID]
	if ok {
		delete(r.sessions, channelID)
	}
	r.mu.Unlock()
	if !ok {
		return ErrNotRecording
	}

	s.router.RemoveTap(s.tapID)
	_ = s.AudioTap.Close()
	_ = s.VideoTap.Close()

	if s.stdin != nil {
		_, _ = io.WriteString(s.stdin, "q")
	}

	select {
	case err := <-s.done:
		r.logger.Info("recording stopped",
			zap.Int64("channel_id", channelID),
			zap.String("output", s.FilePath),
			zap.Error(err),
		)
	case <-time.After(stopGracePeriod):
		r.logger.Warn("ffmpeg did not exit in time, killing",
			zap.Int64("channel_id", channelID),
		)
		_ = s.cmd.Kill()
		<-s.done
	}
	return nil
}

// SessionCount returns the number of active recording sessions.
func (r *Recorder) SessionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Close stops all active sessions.
func (r *Recorder) Close() {
	r.mu.Lock()
	ids := make([]int64, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		_ = r.Stop(id)
	}
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
	return append(args, "-y", outPath)
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
func tapID(channelID int64) string {
	return fmt.Sprintf("recorder:%d", channelID)
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
}

// NewTap creates a Tap sending to 127.0.0.1:port.
func NewTap(port int) (*Tap, error) {
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
	raw, err := pkt.Marshal()
	if err != nil {
		return err
	}
	_, err = t.conn.WriteToUDP(raw, t.addr)
	return err
}

// Close releases the tap's socket.
func (t *Tap) Close() error {
	return t.conn.Close()
}
