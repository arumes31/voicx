package webrtc

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// --- fakes ------------------------------------------------------------------

// fakeTrackWriter records written RTP packets.
type fakeTrackWriter struct {
	mu      sync.Mutex
	packets []*rtp.Packet
	err     error
}

func (f *fakeTrackWriter) WriteRTP(p *rtp.Packet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.packets = append(f.packets, p)
	return nil
}

func (f *fakeTrackWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.packets)
}

// fakeTrackReader replays a scripted list of packets, then returns io.EOF.
type fakeTrackReader struct {
	mu      sync.Mutex
	packets []*rtp.Packet
	idx     int
}

func (f *fakeTrackReader) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx >= len(f.packets) {
		return nil, nil, io.EOF
	}
	p := f.packets[f.idx]
	f.idx++
	return p, nil, nil
}

// makeAudioPacket builds an RTP packet; when level >= 0 an ssrc-audio-level
// header extension with that level is attached at extension ID 1.
func makeAudioPacket(t *testing.T, seq uint16, level int) *rtp.Packet {
	t.Helper()
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 960,
			SSRC:           4242,
		},
		Payload: []byte{0xde, 0xad},
	}
	if level >= 0 {
		if err := pkt.SetExtension(1, []byte{byte(level)}); err != nil {
			t.Fatalf("SetExtension: %v", err)
		}
	}
	return pkt
}

// --- membership -------------------------------------------------------------

// TestRouterJoinLeave verifies channel membership tracking, implicit moves,
// and Stats.
func TestRouterJoinLeave(t *testing.T) {
	r := NewRouter(nil)

	r.JoinChannel(1, "a")
	r.JoinChannel(1, "b")
	r.JoinChannel(1, "a") // idempotent

	stats := r.Stats()
	if stats.Clients != 2 || stats.Channels != 1 {
		t.Fatalf("stats = %+v, want 2 clients in 1 channel", stats)
	}

	// Joining another channel moves the client out of the previous one.
	r.JoinChannel(2, "a")
	stats = r.Stats()
	if stats.Channels != 2 {
		t.Fatalf("channels = %d, want 2", stats.Channels)
	}
	for _, cs := range stats.PerChannel {
		if cs.Members != 1 {
			t.Fatalf("channel %d members = %d, want 1", cs.ChannelID, cs.Members)
		}
	}

	r.LeaveChannel(1, "a") // no-op: a already moved
	r.LeaveAll("b")
	stats = r.Stats()
	if stats.Clients != 1 {
		t.Fatalf("clients = %d, want 1 (only a)", stats.Clients)
	}
}

// --- forwarding -------------------------------------------------------------

// TestForwardRTPChannelFanout verifies that audio from a channel member is
// forwarded to every other member with an output track, and never to the
// sender.
func TestForwardRTPChannelFanout(t *testing.T) {
	r := NewRouter(nil)
	wb, wc := &fakeTrackWriter{}, &fakeTrackWriter{}

	r.JoinChannel(1, "a")
	r.JoinChannel(1, "b")
	r.JoinChannel(1, "c")
	r.AddOutput("b", wb)
	r.AddOutput("c", wc)

	pkt := makeAudioPacket(t, 1, -1)
	if sent := r.ForwardRTP("a", pkt); sent != 2 {
		t.Fatalf("ForwardRTP sent = %d, want 2", sent)
	}
	if wb.count() != 1 || wc.count() != 1 {
		t.Fatalf("writes: b=%d c=%d, want 1 each", wb.count(), wc.count())
	}

	// A member without an output track is skipped silently.
	r.JoinChannel(1, "d")
	if sent := r.ForwardRTP("a", pkt); sent != 2 {
		t.Fatalf("ForwardRTP sent = %d, want 2 (d has no output)", sent)
	}
}

// TestForwardRTPNotInChannel verifies that audio from a client that is not in
// any channel goes nowhere.
func TestForwardRTPNotInChannel(t *testing.T) {
	r := NewRouter(nil)
	w := &fakeTrackWriter{}
	r.JoinChannel(1, "b")
	r.AddOutput("b", w)

	if sent := r.ForwardRTP("ghost", makeAudioPacket(t, 1, -1)); sent != 0 {
		t.Fatalf("ForwardRTP sent = %d, want 0", sent)
	}
	if w.count() != 0 {
		t.Fatalf("writes = %d, want 0", w.count())
	}
}

// TestWhisperRouting verifies that an active whisper configuration reroutes
// audio to the whisper targets (clients and channel members) instead of the
// sender's channel, and that deactivating restores normal routing.
func TestWhisperRouting(t *testing.T) {
	r := NewRouter(nil)
	wb, wc, wd := &fakeTrackWriter{}, &fakeTrackWriter{}, &fakeTrackWriter{}

	r.JoinChannel(1, "a")
	r.JoinChannel(1, "b")
	r.JoinChannel(1, "c")
	r.JoinChannel(2, "d")
	r.AddOutput("b", wb)
	r.AddOutput("c", wc)
	r.AddOutput("d", wd)

	// a whispers to client c and channel 2 (d), bypassing channel-mate b.
	r.SetWhisper("a", []string{"c"}, []int64{2}, true)
	pkt := makeAudioPacket(t, 1, -1)
	if sent := r.ForwardRTP("a", pkt); sent != 2 {
		t.Fatalf("whisper forward sent = %d, want 2", sent)
	}
	if wb.count() != 0 {
		t.Fatalf("b received %d packets while whispering, want 0", wb.count())
	}
	if wc.count() != 1 || wd.count() != 1 {
		t.Fatalf("whisper targets: c=%d d=%d, want 1 each", wc.count(), wd.count())
	}

	// Deactivating the whisper restores normal channel fanout.
	r.SetWhisper("a", nil, nil, false)
	if sent := r.ForwardRTP("a", pkt); sent != 2 {
		t.Fatalf("channel forward sent = %d, want 2", sent)
	}
	if wb.count() != 1 || wc.count() != 2 {
		t.Fatalf("channel fanout: b=%d c=%d, want 1 and 2", wb.count(), wc.count())
	}
}

// TestWhisperDedup verifies a target listed both as a client and via a
// channel receives the packet only once.
func TestWhisperDedup(t *testing.T) {
	r := NewRouter(nil)
	wc := &fakeTrackWriter{}

	r.JoinChannel(1, "a")
	r.JoinChannel(2, "c")
	r.AddOutput("c", wc)

	r.SetWhisper("a", []string{"c"}, []int64{2}, true)
	if sent := r.ForwardRTP("a", makeAudioPacket(t, 1, -1)); sent != 1 {
		t.Fatalf("whisper forward sent = %d, want 1 (dedup)", sent)
	}
	if wc.count() != 1 {
		t.Fatalf("c received %d packets, want 1", wc.count())
	}
}

// --- VAD --------------------------------------------------------------------

// TestVADStateMachine feeds synthetic audio-level readings and verifies
// threshold and hangover behavior.
func TestVADStateMachine(t *testing.T) {
	v := &vad{}
	start := time.Now()

	// Silence: no transition.
	if speaking, changed := v.Update(100, start); speaking || changed {
		t.Fatal("silent reading caused a transition")
	}

	// Voice: transition to speaking.
	if speaking, changed := v.Update(20, start); !speaking || !changed {
		t.Fatal("voice reading did not trigger speaking")
	}

	// Continued voice: no new transition.
	if speaking, changed := v.Update(30, start.Add(50*time.Millisecond)); !speaking || changed {
		t.Fatal("continued voice caused an unexpected transition")
	}

	// Brief pause within the hangover: still speaking, no transition.
	if speaking, changed := v.Update(100, start.Add(100*time.Millisecond)); !speaking || changed {
		t.Fatal("hangover not honored: speaking dropped too early")
	}

	// Pause beyond the hangover (last voice was at t=50ms): transition to
	// silent.
	if speaking, changed := v.Update(100, start.Add(400*time.Millisecond)); speaking || !changed {
		t.Fatal("hangover expired but speaking did not drop")
	}
}

// TestReadLoopSpeakingAndForwarding verifies the read loop: VAD transitions
// fire the speaking callback (including the final "off" when the track ends)
// and packets are forwarded to channel members.
func TestReadLoopSpeakingAndForwarding(t *testing.T) {
	r := NewRouter(nil)
	w := &fakeTrackWriter{}

	r.JoinChannel(1, "talker")
	r.JoinChannel(1, "listener")
	r.AddOutput("listener", w)

	var mu sync.Mutex
	var events []bool
	r.SetHandlers(nil, func(_ string, speaking bool) {
		mu.Lock()
		events = append(events, speaking)
		mu.Unlock()
	})

	track := &fakeTrackReader{packets: []*rtp.Packet{
		makeAudioPacket(t, 1, 20), // voice
		makeAudioPacket(t, 2, 25), // voice
	}}
	r.ReadLoop("talker", track, 1)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || !events[0] || events[1] {
		t.Fatalf("speaking events = %v, want [true false]", events)
	}
	if w.count() != 2 {
		t.Fatalf("forwarded = %d, want 2", w.count())
	}
}

// TestReadLoopTalkGateMuted verifies that a client denied talk power has its
// audio dropped and never enters the speaking state.
func TestReadLoopTalkGateMuted(t *testing.T) {
	r := NewRouter(nil)
	w := &fakeTrackWriter{}

	r.JoinChannel(1, "muted-talker")
	r.JoinChannel(1, "listener")
	r.AddOutput("listener", w)

	spoke := false
	r.SetHandlers(func(string) bool { return false }, func(string, bool) { spoke = true })

	track := &fakeTrackReader{packets: []*rtp.Packet{
		makeAudioPacket(t, 1, 20), // voice, but muted
	}}
	r.ReadLoop("muted-talker", track, 1)

	if spoke {
		t.Fatal("muted client entered speaking state")
	}
	if w.count() != 0 {
		t.Fatalf("forwarded = %d, want 0 (muted)", w.count())
	}
}

// TestAudioLevel verifies header-extension extraction.
func TestAudioLevel(t *testing.T) {
	pkt := makeAudioPacket(t, 1, 33)
	level, ok := audioLevel(pkt, 1)
	if !ok || level != 33 {
		t.Fatalf("audioLevel = %d, %v; want 33, true", level, ok)
	}
	if _, ok := audioLevel(pkt, 7); ok {
		t.Fatal("audioLevel found extension at wrong ID")
	}
}

// --- peer attach ------------------------------------------------------------

// TestAttachPeer verifies that attaching a peer connection registers an audio
// output track and that DetachPeer removes all router state.
func TestAttachPeer(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	pc, err := e.NewPeerConnection("c1")
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}

	r := NewRouter(nil)
	if err := r.AttachPeer("c1", pc); err != nil {
		t.Fatalf("AttachPeer: %v", err)
	}

	r.mu.RLock()
	_, hasOutput := r.outputs["c1"]
	r.mu.RUnlock()
	if !hasOutput {
		t.Fatal("no output track registered after AttachPeer")
	}

	r.JoinChannel(1, "c1")
	r.SetWhisper("c1", []string{"x"}, nil, true)
	r.DetachPeer("c1")

	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.outputs) != 0 || len(r.whispers) != 0 || len(r.clientChan) != 0 {
		t.Fatal("DetachPeer left router state behind")
	}
}

// --- voice facade -----------------------------------------------------------

// TestVoiceHandleOffer runs the signaling path against a real SDP offer from
// a client-side Pion peer connection (no ICE/DTLS needed for negotiation).
func TestVoiceHandleOffer(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := NewRouter(nil)
	v := NewVoice(e, r, testLogger())
	defer e.Close()

	// Build a genuine offer with a client-side peer connection (using the
	// same codec set as the engine so negotiation can succeed).
	clientME := &webrtc.MediaEngine{}
	if err := registerCodecs(clientME, false); err != nil {
		t.Fatalf("registerCodecs: %v", err)
	}
	clientPC, err := webrtc.NewAPI(webrtc.WithMediaEngine(clientME)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	defer clientPC.Close()
	if _, err := clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("AddTransceiverFromKind: %v", err)
	}
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("client SetLocalDescription: %v", err)
	}

	answer, err := v.HandleOffer("c1", offer.SDP, nil)
	if err != nil {
		t.Fatalf("HandleOffer: %v", err)
	}
	if answer == "" {
		t.Fatal("empty answer SDP")
	}
	if got := v.PeerCount(); got != 1 {
		t.Fatalf("PeerCount = %d, want 1", got)
	}

	// The client can complete negotiation with the answer.
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	}); err != nil {
		t.Fatalf("client SetRemoteDescription(answer): %v", err)
	}

	// A re-offer is idempotent: the old session is replaced.
	if _, err := v.HandleOffer("c1", offer.SDP, nil); err != nil {
		t.Fatalf("re-offer HandleOffer: %v", err)
	}
	if got := v.PeerCount(); got != 1 {
		t.Fatalf("PeerCount after re-offer = %d, want 1", got)
	}

	if err := v.ClosePeer("c1"); err != nil {
		t.Fatalf("ClosePeer: %v", err)
	}
	if got := v.PeerCount(); got != 0 {
		t.Fatalf("PeerCount after ClosePeer = %d, want 0", got)
	}
}

// TestVoiceNoPeer verifies operations against a missing peer connection fail
// cleanly.
func TestVoiceNoPeer(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	v := NewVoice(e, NewRouter(nil), testLogger())

	if err := v.HandleAnswer("ghost", "sdp"); err != ErrNoPeer {
		t.Errorf("HandleAnswer = %v, want ErrNoPeer", err)
	}
	if err := v.AddICECandidate("ghost", "cand", "", 0); err != ErrNoPeer {
		t.Errorf("AddICECandidate = %v, want ErrNoPeer", err)
	}
	if err := v.ClosePeer("ghost"); err != nil {
		t.Errorf("ClosePeer(ghost) = %v, want nil", err)
	}
}
