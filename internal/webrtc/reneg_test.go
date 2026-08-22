// reneg_test.go covers server-initiated renegotiation (debounce, rate
// limiting, unanswered-offer tolerance) and benchmarks the forwarding core.
package webrtc

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// newClientPC builds a client-side peer connection with the same codec set as
// the engine, so negotiation with a server-side peer connection succeeds.
func newClientPC(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	clientME := &webrtc.MediaEngine{}
	if err := registerCodecs(clientME, false); err != nil {
		t.Fatalf("registerCodecs: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(clientME)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("AddTransceiverFromKind: %v", err)
	}
	return pc
}

// establishVoiceSession runs the initial offer/answer exchange between a
// client-side peer connection and the voice facade for clientID.
func establishVoiceSession(t *testing.T, v *Voice, clientPC *webrtc.PeerConnection, clientID string) {
	t.Helper()
	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("client CreateOffer: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("client SetLocalDescription: %v", err)
	}
	answer, err := v.HandleOffer(clientID, offer.SDP, nil)
	if err != nil {
		t.Fatalf("HandleOffer: %v", err)
	}
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}
}

// offerRecorder collects renegotiation offers delivered via SetOfferSender.
type offerRecorder struct {
	mu   sync.Mutex
	sdps []string
}

func (o *offerRecorder) add(sdp string) {
	o.mu.Lock()
	o.sdps = append(o.sdps, sdp)
	o.mu.Unlock()
}

func (o *offerRecorder) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sdps)
}

func (o *offerRecorder) sdp(i int) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sdps[i]
}

type manualRenegClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualRenegTimer
}

type manualRenegTimer struct {
	clock    *manualRenegClock
	due      time.Time
	callback func()
	stopped  bool
	fired    bool
}

func newManualRenegClock(now time.Time) *manualRenegClock {
	return &manualRenegClock{now: now, timers: []*manualRenegTimer{}}
}

func (c *manualRenegClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualRenegClock) AfterFunc(delay time.Duration, callback func()) renegTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &manualRenegTimer{clock: c, due: c.now.Add(delay), callback: callback}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *manualRenegClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	callbacks := []func(){}
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired && !timer.due.After(c.now) {
			timer.fired = true
			callbacks = append(callbacks, timer.callback)
		}
	}
	c.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
}

func (c *manualRenegClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired {
			count++
		}
	}
	return count
}

func (t *manualRenegTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func useManualRenegClock(v *Voice, clock *manualRenegClock, debounce, rateLimit time.Duration) {
	v.renegClock = clock
	v.renegDebounce = debounce
	v.renegRateLimit = rateLimit
}

// TestRenegotiationDebounceAndRateLimit verifies a burst of membership
// changes coalesces into a single renegotiation offer, and that a follow-up
// change is delayed to respect the per-peer rate limit.
func TestRenegotiationDebounceAndRateLimit(t *testing.T) {
	const (
		debounce  = 50 * time.Millisecond
		rateLimit = 400 * time.Millisecond
	)
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()
	r := NewRouter(nil)
	v := NewVoice(e, r, testLogger())
	clock := newManualRenegClock(time.Unix(0, 0))
	useManualRenegClock(v, clock, debounce, rateLimit)
	defer func() { _ = v.ClosePeer("c1") }() // stops pending renegotiation timers

	clientPC := newClientPC(t)
	rec := &offerRecorder{}
	v.SetOfferSender(func(clientID, offerSDP string) error {
		rec.add(offerSDP)
		// Answer immediately so the signaling state returns to stable and the
		// next offer can be created.
		if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer, SDP: offerSDP,
		}); err != nil {
			t.Errorf("client SetRemoteDescription(offer): %v", err)
			return nil
		}
		answer, err := clientPC.CreateAnswer(nil)
		if err != nil {
			t.Errorf("client CreateAnswer: %v", err)
			return nil
		}
		if err := clientPC.SetLocalDescription(answer); err != nil {
			t.Errorf("client SetLocalDescription: %v", err)
			return nil
		}
		return v.HandleAnswer(clientID, answer.SDP)
	})

	establishVoiceSession(t, v, clientPC, "c1")
	r.JoinChannel(1, "c1")

	// Burst of membership changes: must coalesce into exactly one offer.
	r.JoinChannel(1, "b")
	r.JoinChannel(1, "c")
	r.JoinChannel(1, "d")
	if got := clock.Pending(); got != 1 {
		t.Fatalf("pending debounce callbacks = %d, want 1", got)
	}
	clock.Advance(debounce - time.Nanosecond)
	if got := rec.count(); got != 0 {
		t.Fatalf("offers before debounce expires = %d, want 0", got)
	}
	clock.Advance(time.Nanosecond)
	if got := rec.count(); got != 1 {
		t.Fatalf("offers after debounce = %d, want 1", got)
	}

	// A change right after the first offer is rate-limited: no offer within
	// most of the rate-limit window, then exactly one more.
	r.JoinChannel(1, "e")
	clock.Advance(rateLimit - time.Nanosecond)
	if got := rec.count(); got != 1 {
		t.Fatalf("offers during rate-limit window = %d, want 1 (rate limited)", got)
	}
	clock.Advance(time.Nanosecond)
	if got := rec.count(); got != 2 {
		t.Fatalf("offers at the rate-limit boundary = %d, want 2", got)
	}

	// Settled: no further offers.
	clock.Advance(2 * debounce)
	if got := rec.count(); got != 2 {
		t.Fatalf("total offers = %d, want 2", got)
	}
}

// TestRenegotiationUnansweredTolerance verifies the server tolerates a client
// that never answers a renegotiation offer: the follow-up change is logged
// and skipped without wedging (Pion v3.3 cannot roll back a local offer), and
// once the client answers late, renegotiation resumes.
func TestRenegotiationUnansweredTolerance(t *testing.T) {
	const (
		debounce  = 50 * time.Millisecond
		rateLimit = 300 * time.Millisecond
	)
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()
	r := NewRouter(nil)
	v := NewVoice(e, r, testLogger())
	clock := newManualRenegClock(time.Unix(0, 0))
	useManualRenegClock(v, clock, debounce, rateLimit)
	defer func() { _ = v.ClosePeer("c1") }() // stops pending renegotiation timers

	clientPC := newClientPC(t)
	rec := &offerRecorder{}
	v.SetOfferSender(func(clientID, offerSDP string) error {
		rec.add(offerSDP)
		return nil // old client: offers are recorded but never answered
	})

	establishVoiceSession(t, v, clientPC, "c1")
	r.JoinChannel(1, "c1")
	r.JoinChannel(1, "b")
	clock.Advance(debounce)
	if got := rec.count(); got != 1 {
		t.Fatalf("initial offers = %d, want 1", got)
	}

	// The client never answers; the next change is skipped (warning logged)
	// and must not produce an offer or wedge the scheduler.
	r.JoinChannel(1, "c")
	clock.Advance(rateLimit)
	if got := rec.count(); got != 1 {
		t.Fatalf("offers while previous is unanswered = %d, want 1 (skipped)", got)
	}
	if got := clock.Pending(); got != 0 {
		t.Fatalf("pending callbacks after unanswered skip = %d, want 0", got)
	}

	// The client answers the first offer late: renegotiation resumes.
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: rec.sdp(0),
	}); err != nil {
		t.Fatalf("client SetRemoteDescription(offer): %v", err)
	}
	answer, err := clientPC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("client CreateAnswer: %v", err)
	}
	if err := clientPC.SetLocalDescription(answer); err != nil {
		t.Fatalf("client SetLocalDescription: %v", err)
	}
	if err := v.HandleAnswer("c1", answer.SDP); err != nil {
		t.Fatalf("HandleAnswer: %v", err)
	}

	r.JoinChannel(1, "d")
	clock.Advance(debounce)
	if got := rec.count(); got != 2 {
		t.Fatalf("offers after late answer recovery = %d, want 2", got)
	}
}

// TestCreateOfferRequiresStable verifies server-initiated offers are only
// created in the stable signaling state.
func TestCreateOfferRequiresStable(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close() }()

	wrapper, err := e.NewPeerConnection("c1")
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}

	if _, err := wrapper.CreateOffer(); err != nil {
		t.Fatalf("CreateOffer in stable state: %v", err)
	}
	// The first offer is now unanswered (have-local-offer): a second one must
	// be refused.
	if _, err := wrapper.CreateOffer(); err == nil {
		t.Fatal("CreateOffer with unanswered offer: expected error, got nil")
	}
}

// BenchmarkForwardRTP measures the fan-out core: computing the subscriber
// set, resolving per-publisher tracks, and dispatching one RTP packet to
// every channel member. Writes go to real (unbound) TrackLocalStaticRTP
// tracks, so per-subscriber marshaling is excluded; the numbers represent
// pure router dispatch overhead per packet.
func BenchmarkForwardRTP(b *testing.B) {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: 1,
			Timestamp:      960,
			SSRC:           4242,
		},
		Payload: []byte{0xde, 0xad},
	}

	for _, n := range []int{10, 100} {
		b.Run(fmt.Sprintf("subscribers=%d", n), func(b *testing.B) {
			r := NewRouter(nil)
			r.JoinChannel(1, "pub")
			for i := 0; i < n; i++ {
				sub := fmt.Sprintf("sub%d", i)
				track, err := webrtc.NewTrackLocalStaticRTP(
					webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
					"pub", "voicx",
				)
				if err != nil {
					b.Fatalf("NewTrackLocalStaticRTP: %v", err)
				}
				r.mu.Lock()
				r.pubTracks[sub] = map[string]*pubTrack{"pub": {
					audio: map[string]*pubSlot{SlotMic: {track: track}},
					video: map[string]*pubSlot{},
				}}
				r.mu.Unlock()
				r.JoinChannel(1, sub)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if sent := r.ForwardRTP("pub", SlotMic, pkt); sent != n {
					b.Fatalf("ForwardRTP sent = %d, want %d", sent, n)
				}
			}
		})
	}
}
