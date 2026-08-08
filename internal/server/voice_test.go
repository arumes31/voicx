// voice_test.go exercises the voice-pipeline control handlers (WebRTC
// signaling, whisper, position) over real TCP with a fake VoiceBackend.
package server

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/recorder"
	"voicx/internal/webrtc"
)

func closeVoiceTestResource(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Logf("closing test resource: %v", err)
	}
}

// fakeVoice implements VoiceBackend, recording all calls.
type fakeVoice struct {
	mu sync.Mutex

	answerSDP string
	offerErr  error

	canTalkFn    func(string) bool
	onSpeakingFn func(string, bool)
	canVideoFn   func(string) bool
	qualityErr   error
	offerSender  func(clientID, offerSDP string) error

	onCandidate func(candidate, sdpMid string, mlineIndex uint16)
	offers      []string // offer SDPs by client
	answers     []string
	candidates  []string
	closed      []string
	joins       [][2]any // (clientID, channelID)
	leaves      [][2]any
	qualities   [][2]string
	taps        [][2]any
	removedTaps []string

	whispers       []whisperCall
	whisperTargets map[string][]string // clientID -> targets of its active whisper
}

type whisperCall struct {
	clientID string
	clients  []string
	channels []int64
	active   bool
}

func (f *fakeVoice) SetHandlers(canTalk func(string) bool, onSpeaking func(string, bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canTalkFn = canTalk
	f.onSpeakingFn = onSpeaking
}

func (f *fakeVoice) HandleOffer(_, offerSDP string, onCandidate func(candidate, sdpMid string, mlineIndex uint16)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offers = append(f.offers, offerSDP)
	f.onCandidate = onCandidate
	return f.answerSDP, f.offerErr
}

func (f *fakeVoice) HandleAnswer(_, answerSDP string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, answerSDP)
	return nil
}

func (f *fakeVoice) AddICECandidate(_, candidate, _ string, _ uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candidates = append(f.candidates, candidate)
	return nil
}

func (f *fakeVoice) ClosePeer(clientID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, clientID)
	return nil
}

func (f *fakeVoice) JoinChannel(clientID string, channelID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joins = append(f.joins, [2]any{clientID, channelID})
}

func (f *fakeVoice) LeaveChannel(clientID string, channelID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaves = append(f.leaves, [2]any{clientID, channelID})
}

func (f *fakeVoice) SetWhisper(clientID string, clients []string, channels []int64, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.whispers = append(f.whispers, whisperCall{clientID, clients, channels, active})
	// Mirror the router's routing state so WhisperTargets answers like the real
	// backend does (32/33). Channel targets are not modelled: the fake has no
	// membership.
	if f.whisperTargets == nil {
		f.whisperTargets = make(map[string][]string)
	}
	if active {
		f.whisperTargets[clientID] = clients
	} else {
		delete(f.whisperTargets, clientID)
	}
}

// WhisperTargets satisfies the optional whisperTargeter capability.
func (f *fakeVoice) WhisperTargets(clientID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.whisperTargets[clientID]
}

// SetVideoHandlers records the video gate callback.
func (f *fakeVoice) SetVideoHandlers(canVideo func(string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canVideoFn = canVideo
}

// SetOfferSender records the renegotiation offer delivery callback.
func (f *fakeVoice) SetOfferSender(fn func(clientID, offerSDP string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offerSender = fn
}

// SetVideoQuality records the requested quality.
func (f *fakeVoice) SetVideoQuality(clientID, quality string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qualities = append(f.qualities, [2]string{clientID, quality})
	return f.qualityErr
}

// AddTap records tap registrations.
func (f *fakeVoice) AddTap(channelID int64, tapID string, _, _ webrtc.TrackWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taps = append(f.taps, [2]any{channelID, tapID})
}

// RemoveTap records tap removals.
func (f *fakeVoice) RemoveTap(tapID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedTaps = append(f.removedTaps, tapID)
}

// PeerCount returns 0 (no peers in the fake).
func (f *fakeVoice) PeerCount() int { return 0 }

func (f *fakeVoice) lastWhisper() (whisperCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.whispers) == 0 {
		return whisperCall{}, false
	}
	return f.whispers[len(f.whispers)-1], true
}

// --- tests ------------------------------------------------------------------

// TestWebRTCOfferAnswer verifies the signaling round-trip: the client's SDP
// offer is answered, and a server-side ICE candidate is pushed back as an
// ICECandidate message.
func TestWebRTCOfferAnswer(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	env.voice.answerSDP = "server-answer-sdp"

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, conn)

	// The server must have installed its voice callbacks at construction.
	env.voice.mu.Lock()
	if env.voice.canTalkFn == nil || env.voice.onSpeakingFn == nil {
		t.Fatal("voice handlers not installed by server.New")
	}
	env.voice.mu.Unlock()

	send(t, conn, netproto.MsgWebRTCOffer, netproto.WebRTCOffer{SDP: "client-offer-sdp"})
	f := readOfType(t, conn, netproto.MsgWebRTCAnswer)
	var answer netproto.WebRTCAnswer
	if err := netproto.Decode(f, &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer.SDP != "server-answer-sdp" {
		t.Fatalf("answer SDP = %q, want %q", answer.SDP, "server-answer-sdp")
	}

	// Simulate a gathered server-side candidate: it must arrive as an
	// ICECandidate message.
	env.voice.mu.Lock()
	cb := env.voice.onCandidate
	env.voice.mu.Unlock()
	if cb == nil {
		t.Fatal("no candidate callback captured by fake voice")
	}
	cb("candidate:1 udp 127.0.0.1 12334 typ host", "0", 0)

	f = readOfType(t, conn, netproto.MsgICECandidate)
	var ice netproto.ICECandidate
	if err := netproto.Decode(f, &ice); err != nil {
		t.Fatalf("decode ice candidate: %v", err)
	}
	if ice.Candidate != "candidate:1 udp 127.0.0.1 12334 typ host" {
		t.Fatalf("candidate = %q", ice.Candidate)
	}
}

// TestICECandidateFromClient verifies client-side trickle candidates reach
// the voice backend.
func TestICECandidateFromClient(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, conn)

	send(t, conn, netproto.MsgICECandidate, netproto.ICECandidate{Candidate: "cand-xyz", SDPMid: "0"})
	waitFor(t, "candidate forwarded", func() bool {
		env.voice.mu.Lock()
		defer env.voice.mu.Unlock()
		return len(env.voice.candidates) == 1 && env.voice.candidates[0] == "cand-xyz"
	})
}

// TestWhisperSet verifies whisper configuration reaches the voice backend
// with unique IDs resolved to online client IDs.
func TestWhisperSet(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, adminID := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, adminConn)
	userConn, _ := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, userConn)

	send(t, userConn, netproto.MsgWhisperSet, netproto.WhisperSet{
		UniqueIDs:  []string{"admin-uid", "offline-uid"},
		ChannelIDs: []int64{7},
		Active:     true,
	})

	waitFor(t, "whisper recorded", func() bool {
		_, ok := env.voice.lastWhisper()
		return ok
	})
	call, _ := env.voice.lastWhisper()
	if !call.active || len(call.channels) != 1 || call.channels[0] != 7 {
		t.Fatalf("whisper call = %+v", call)
	}
	// Only the online target (admin) is resolved; the offline one is dropped.
	if len(call.clients) != 1 || call.clients[0] != adminID {
		t.Fatalf("whisper clients = %v, want [%s]", call.clients, adminID)
	}
}

// TestWhisperSetDenied verifies a negated i_client_whisper_power denies
// whisper configuration.
func TestWhisperSetDenied(t *testing.T) {
	perms := tieredWith(&permissions.Permission{
		Key:    permissions.PermissionKeyClientWhisperPower,
		Type:   permissions.PermissionTypeInteger,
		Value:  0,
		Negate: true,
	})
	env := startTestEnv(t, &perms)
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, conn)

	send(t, conn, netproto.MsgWhisperSet, netproto.WhisperSet{Active: true})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
	if _, ok := env.voice.lastWhisper(); ok {
		t.Fatal("whisper recorded despite denied permission")
	}
}

// TestWhisperSpeakingSignalsTarget verifies the receive-side whisper signal
// (32/33): while a whisper is active the broadcast speaking event is marked as
// a whisper, and the targets — and only the targets — additionally get a
// whisper event naming the whisperer's unique ID (what the reply hotkey
// whispers back to).
func TestWhisperSpeakingSignalsTarget(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, adminID := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, adminConn)
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, userConn)

	send(t, userConn, netproto.MsgWhisperSet, netproto.WhisperSet{
		UniqueIDs: []string{"admin-uid"},
		Active:    true,
	})
	waitFor(t, "whisper recorded", func() bool {
		call, ok := env.voice.lastWhisper()
		return ok && call.active && len(call.clients) == 1 && call.clients[0] == adminID
	})

	// The router reports the speaking transition of a whispering publisher.
	env.voice.mu.Lock()
	onSpeaking := env.voice.onSpeakingFn
	env.voice.mu.Unlock()
	if onSpeaking == nil {
		t.Fatal("speaking callback not installed by server.New")
	}
	onSpeaking(userID, true)

	data := readEventOfType(t, adminConn, eventSpeakingChanged)
	var se speakingEvent
	if err := json.Unmarshal(data, &se); err != nil {
		t.Fatalf("unmarshal speaking event: %v", err)
	}
	if se.ClientID != userID || !se.Speaking || !se.Whisper {
		t.Fatalf("speaking event = %+v, want client %s speaking as a whisper", se, userID)
	}

	data = readEventOfType(t, adminConn, eventWhisper)
	var we whisperEvent
	if err := json.Unmarshal(data, &we); err != nil {
		t.Fatalf("unmarshal whisper event: %v", err)
	}
	if we.FromClientID != userID || we.FromUniqueID != "user-uid" || !we.Speaking {
		t.Fatalf("whisper event = %+v, want %s / user-uid speaking", we, userID)
	}

	// Turning the whisper off makes the next transition an ordinary one.
	send(t, userConn, netproto.MsgWhisperSet, netproto.WhisperSet{Active: false})
	waitFor(t, "whisper cleared", func() bool {
		call, ok := env.voice.lastWhisper()
		return ok && !call.active
	})
	onSpeaking(userID, false)

	data = readEventOfType(t, adminConn, eventSpeakingChanged)
	se = speakingEvent{}
	if err := json.Unmarshal(data, &se); err != nil {
		t.Fatalf("unmarshal speaking event: %v", err)
	}
	if se.Whisper {
		t.Fatalf("speaking event = %+v, want whisper cleared", se)
	}
}

// TestPositionUpdate verifies a position update is relayed to the other
// members of the sender's channel as a position event.
func TestPositionUpdate(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, adminConn)
	userConn, userID := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, userConn)

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Arena", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, adminConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	waitFor(t, "both in channel", func() bool {
		return len(env.state.ChannelMembers(1)) == 2
	})

	send(t, userConn, netproto.MsgPositionUpdate, netproto.PositionUpdate{X: 1.5, Y: -2, Z: 3.25})

	data := readEventOfType(t, adminConn, eventPosition)
	var pe positionEvent
	if err := json.Unmarshal(data, &pe); err != nil {
		t.Fatalf("unmarshal position event: %v", err)
	}
	if pe.ClientID != userID || pe.X != 1.5 || pe.Y != -2 || pe.Z != 3.25 {
		t.Fatalf("position event = %+v, want client %s at (1.5, -2, 3.25)", pe, userID)
	}
}

// TestVoiceMembershipSync verifies channel joins/moves are mirrored into the
// voice router and disconnects tear the voice session down.
func TestVoiceMembershipSync(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, adminConn)
	userConn, userID := dialAuthed(t, env.addr, "user-uid")

	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Lobby", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, userConn, netproto.MsgJoinChannel, netproto.JoinChannel{ChannelID: 1})
	waitFor(t, "voice join recorded", func() bool {
		env.voice.mu.Lock()
		defer env.voice.mu.Unlock()
		for _, j := range env.voice.joins {
			if j[0] == userID && j[1] == int64(1) {
				return true
			}
		}
		return false
	})

	_ = userConn.Close()
	waitFor(t, "voice peer closed", func() bool {
		env.voice.mu.Lock()
		defer env.voice.mu.Unlock()
		for _, id := range env.voice.closed {
			if id == userID {
				return true
			}
		}
		return false
	})
}

// --- recording / video quality ----------------------------------------------

// fakeRecorder implements RecordingBackend, recording calls.
type fakeRecorder struct {
	mu           sync.Mutex
	started      []int64
	stopped      []int64
	startErr     error
	sawRouter    bool
	startEntered chan struct{}
	startRelease <-chan struct{}
	stopEntered  chan struct{}
	stopRelease  <-chan struct{}
	stopActive   int
	stopMax      int
}

func (f *fakeRecorder) Start(_ context.Context, channelID int64, router recorder.TapRouter) (*recorder.Session, error) {
	f.mu.Lock()
	f.started = append(f.started, channelID)
	if router != nil {
		f.sawRouter = true
	}
	startErr := f.startErr
	entered := f.startEntered
	release := f.startRelease
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if startErr != nil {
		return nil, startErr
	}
	return &recorder.Session{ChannelID: channelID, FilePath: "out.webm"}, nil
}

func (f *fakeRecorder) Stop(channelID int64) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, channelID)
	f.stopActive++
	if f.stopActive > f.stopMax {
		f.stopMax = f.stopActive
	}
	entered := f.stopEntered
	release := f.stopRelease
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.stopActive--
		f.mu.Unlock()
	}()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	return nil
}

func (f *fakeRecorder) startedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

// TestVideoQuality verifies the quality message reaches the voice backend.
func TestVideoQuality(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	conn, userID := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, conn)

	send(t, conn, netproto.MsgVideoQuality, netproto.VideoQuality{Quality: "high"})
	waitFor(t, "quality recorded", func() bool {
		env.voice.mu.Lock()
		defer env.voice.mu.Unlock()
		for _, q := range env.voice.qualities {
			if q[0] == userID && q[1] == "high" {
				return true
			}
		}
		return false
	})
}

// TestRecordingControl verifies an admin can start and stop a recording and
// that the recorder receives the voice facade as its tap router.
func TestRecordingControl(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, adminConn)
	send(t, adminConn, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Recorded", Type: 2})
	readOfType(t, adminConn, netproto.MsgChannelList)

	send(t, adminConn, netproto.MsgRecordingControl, netproto.RecordingControl{ChannelID: 1, Action: "start"})
	waitFor(t, "recording started", func() bool {
		return env.recorder.startedCount() == 1
	})

	env.recorder.mu.Lock()
	if !env.recorder.sawRouter {
		t.Error("recorder did not receive the tap router")
	}
	env.recorder.mu.Unlock()

	send(t, adminConn, netproto.MsgRecordingControl, netproto.RecordingControl{ChannelID: 1, Action: "stop"})
	waitFor(t, "recording stopped", func() bool {
		env.recorder.mu.Lock()
		defer env.recorder.mu.Unlock()
		return len(env.recorder.stopped) == 1 && env.recorder.stopped[0] == 1
	})
}

func TestRecordingStartRejectsUnknownChannel(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	adminConn, _ := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, adminConn)

	send(t, adminConn, netproto.MsgRecordingControl, netproto.RecordingControl{ChannelID: 999, Action: "start"})
	f := readOfType(t, adminConn, netproto.MsgError)
	var protocolErr netproto.Error
	if err := netproto.Decode(f, &protocolErr); err != nil {
		t.Fatal(err)
	}
	if protocolErr.Code != errCodeNotFound {
		t.Fatalf("error code = %d, want %d", protocolErr.Code, errCodeNotFound)
	}
	if got := env.recorder.startedCount(); got != 0 {
		t.Fatalf("recordings started = %d, want 0", got)
	}
}

func TestRecordingStartSerializesWithChannelDeletion(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()
	starter, _ := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, starter)
	send(t, starter, netproto.MsgCreateChannel, netproto.CreateChannel{Name: "Recorded", Type: 2})
	readOfType(t, starter, netproto.MsgChannelList)
	deleter, _ := dialAuthed(t, env.addr, "admin-uid")
	defer closeVoiceTestResource(t, deleter)

	startEntered := make(chan struct{}, 1)
	allowStart := make(chan struct{})
	env.recorder.mu.Lock()
	env.recorder.startEntered = startEntered
	env.recorder.startRelease = allowStart
	env.recorder.mu.Unlock()
	env.channels.deleteAttempt = make(chan struct{}, 1)

	send(t, starter, netproto.MsgRecordingControl, netproto.RecordingControl{ChannelID: 1, Action: "start"})
	<-startEntered
	send(t, deleter, netproto.MsgDeleteChannel, netproto.DeleteChannel{ChannelID: 1})
	<-env.channels.deleteAttempt
	if _, ok := env.state.GetChannel(1); !ok {
		t.Fatal("deletion crossed an in-flight recording start")
	}
	close(allowStart)
	readEventOfType(t, deleter, eventChannelDeleted)
	waitFor(t, "recording stopped after serialized deletion", func() bool {
		env.recorder.mu.Lock()
		defer env.recorder.mu.Unlock()
		return len(env.recorder.started) == 1 && len(env.recorder.stopped) == 1 && env.recorder.stopped[0] == 1
	})
}

// TestRecordingControlDenied verifies a non-admin without the recording
// permission cannot start a recording.
func TestRecordingControlDenied(t *testing.T) {
	env := startTestEnv(t, nil) // no permissions granted
	defer env.stop()

	conn, _ := dialAuthed(t, env.addr, "user-uid")
	defer closeVoiceTestResource(t, conn)

	send(t, conn, netproto.MsgRecordingControl, netproto.RecordingControl{ChannelID: 1, Action: "start"})
	f := readOfType(t, conn, netproto.MsgError)
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != errCodePermissionDenied {
		t.Fatalf("error code = %d, want %d", e.Code, errCodePermissionDenied)
	}
	if got := env.recorder.startedCount(); got != 0 {
		t.Fatalf("recordings started = %d, want 0", got)
	}
}

// TestVideoGateInstalled verifies the server installs the video-publish gate.
func TestVideoGateInstalled(t *testing.T) {
	env := startTestEnv(t, nil)
	defer env.stop()

	env.voice.mu.Lock()
	defer env.voice.mu.Unlock()
	if env.voice.canVideoFn == nil {
		t.Fatal("video gate not installed by server.New")
	}
}
