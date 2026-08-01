// reneg_test.go covers server-initiated renegotiation (debounce, rate
// limiting, unanswered-offer tolerance) and benchmarks the forwarding core.
package webrtc

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
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
	t.Cleanup(func() { pc.Close() })
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
	mu    sync.Mutex
	times []time.Time
	sdps  []string
}

func (o *offerRecorder) add(sdp string) {
	o.mu.Lock()
	o.times = append(o.times, time.Now())
	o.sdps = append(o.sdps, sdp)
	o.mu.Unlock()
}

func (o *offerRecorder) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.times)
}

func (o *offerRecorder) gap(i, j int) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.times[j].Sub(o.times[i])
}

func (o *offerRecorder) sdp(i int) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sdps[i]
}

// waitForOffers polls until n offers were recorded or the timeout expires.
func waitForOffers(t *testing.T, rec *offerRecorder, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec.count() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d offers, got %d", n, rec.count())
}

// withRenegTiming temporarily shrinks the renegotiation debounce and rate
// limit for the duration of a test.
func withRenegTiming(t *testing.T, debounce, rateLimit time.Duration) {
	t.Helper()
	oldDebounce, oldRateLimit := renegDebounce, renegRateLimit
	renegDebounce, renegRateLimit = debounce, rateLimit
	t.Cleanup(func() { renegDebounce, renegRateLimit = oldDebounce, oldRateLimit })
}

// TestRenegotiationDebounceAndRateLimit verifies a burst of membership
// changes coalesces into a single renegotiation offer, and that a follow-up
// change is delayed to respect the per-peer rate limit.
func TestRenegotiationDebounceAndRateLimit(t *testing.T) {
	const (
		debounce  = 50 * time.Millisecond
		rateLimit = 400 * time.Millisecond
	)
	withRenegTiming(t, debounce, rateLimit)

	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)
	v := NewVoice(e, r, testLogger())
	defer v.ClosePeer("c1") // stops pending renegotiation timers

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
	waitForOffers(t, rec, 1, 3*time.Second)

	// A change right after the first offer is rate-limited: no offer within
	// most of the rate-limit window, then exactly one more.
	r.JoinChannel(1, "e")
	time.Sleep(rateLimit / 2)
	if got := rec.count(); got != 1 {
		t.Fatalf("offers during rate-limit window = %d, want 1 (rate limited)", got)
	}
	waitForOffers(t, rec, 2, 3*time.Second)
	if gap := rec.gap(0, 1); gap < rateLimit*7/8 {
		t.Fatalf("gap between offers = %v, want >= ~%v (rate limit)", gap, rateLimit)
	}

	// Settled: no further offers.
	time.Sleep(2 * debounce)
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
	withRenegTiming(t, debounce, rateLimit)

	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)
	v := NewVoice(e, r, testLogger())
	defer v.ClosePeer("c1") // stops pending renegotiation timers

	clientPC := newClientPC(t)
	rec := &offerRecorder{}
	v.SetOfferSender(func(clientID, offerSDP string) error {
		rec.add(offerSDP)
		return nil // old client: offers are recorded but never answered
	})

	establishVoiceSession(t, v, clientPC, "c1")
	r.JoinChannel(1, "c1")
	r.JoinChannel(1, "b")
	waitForOffers(t, rec, 1, 3*time.Second)

	// The client never answers; the next change is skipped (warning logged)
	// and must not produce an offer or wedge the scheduler.
	r.JoinChannel(1, "c")
	time.Sleep(rateLimit + 4*debounce)
	if got := rec.count(); got != 1 {
		t.Fatalf("offers while previous is unanswered = %d, want 1 (skipped)", got)
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
	waitForOffers(t, rec, 2, 3*time.Second)
}

// TestCreateOfferRequiresStable verifies server-initiated offers are only
// created in the stable signaling state.
func TestCreateOfferRequiresStable(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

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
				r.pubTracks[sub] = map[string]*pubTrack{"pub": {audio: track}}
				r.mu.Unlock()
				r.JoinChannel(1, sub)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if sent := r.ForwardRTP("pub", pkt); sent != n {
					b.Fatalf("ForwardRTP sent = %d, want %d", sent, n)
				}
			}
		})
	}
}
