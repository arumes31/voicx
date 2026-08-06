// router_video_test.go covers the video SFU: fan-out, simulcast layer
// selection and fallback, PLI/keyframe relay, and the video-publish gate.
package webrtc

import (
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// fakeVideoTrack is a VideoTrackReader with a fixed RID/SSRC and a scripted
// packet list.
type fakeVideoTrack struct {
	fakeTrackReader
	rid  string
	ssrc webrtc.SSRC
}

func (f *fakeVideoTrack) RID() string       { return f.rid }
func (f *fakeVideoTrack) SSRC() webrtc.SSRC { return f.ssrc }

// fakeRTCPWriter records WriteRTCP calls.
type fakeRTCPWriter struct {
	pkts [][]rtcp.Packet
}

func (f *fakeRTCPWriter) WriteRTCP(pkts []rtcp.Packet) error {
	f.pkts = append(f.pkts, pkts)
	return nil
}

func (f *fakeRTCPWriter) pliCount() int {
	n := 0
	for _, batch := range f.pkts {
		for _, p := range batch {
			if _, ok := p.(*rtcp.PictureLossIndication); ok {
				n++
			}
		}
	}
	return n
}

// registerVideoSource registers a publisher's layer SSRC for one video slot
// directly in the router (mirroring what ReadVideoLoop does on track start).
func registerVideoSource(r *Router, publisherID, slot, rid string, ssrc uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bySlot, ok := r.videoSources[publisherID]
	if !ok {
		bySlot = make(map[string]map[string]uint32)
		r.videoSources[publisherID] = bySlot
	}
	src, ok := bySlot[slot]
	if !ok {
		src = make(map[string]uint32)
		bySlot[slot] = src
	}
	src[rid] = ssrc
}

// TestForwardVideoFanout verifies video packets fan out to channel members
// on their per-publisher video tracks, sender excluded.
func TestForwardVideoFanout(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)

	attachFakePeer(t, e, r, "b")
	attachFakePeer(t, e, r, "c")
	r.JoinChannel(1, "b")
	r.JoinChannel(1, "c")
	r.JoinChannel(1, "pub") // creates (b,pub) and (c,pub) video tracks
	registerVideoSource(r, "pub", SlotCam, "", 9001)

	pkt := makeAudioPacket(t, 1, -1) // packet contents don't matter here
	if sent := r.ForwardVideo("pub", SlotCam, "", pkt); sent != 2 {
		t.Fatalf("ForwardVideo sent = %d, want 2", sent)
	}
}

// TestSimulcastLayerSelection verifies each subscriber receives only the
// layer matching its preference (default mid), with fallback when the
// preferred layer is not published.
func TestSimulcastLayerSelection(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)

	for _, id := range []string{"hi", "mid", "lo", "def"} {
		attachFakePeer(t, e, r, id)
		r.JoinChannel(1, id)
	}
	r.JoinChannel(1, "pub")

	if err := r.SetVideoQuality("hi", "high"); err != nil {
		t.Fatalf("SetVideoQuality: %v", err)
	}
	if err := r.SetVideoQuality("mid", "mid"); err != nil {
		t.Fatalf("SetVideoQuality: %v", err)
	}
	if err := r.SetVideoQuality("lo", "low"); err != nil {
		t.Fatalf("SetVideoQuality: %v", err)
	}

	// Publisher sends all three layers.
	registerVideoSource(r, "pub", SlotCam, "f", 9001)
	registerVideoSource(r, "pub", SlotCam, "h", 9002)
	registerVideoSource(r, "pub", SlotCam, "q", 9003)

	pkt := makeAudioPacket(t, 1, -1)
	if sent := r.ForwardVideo("pub", SlotCam, "f", pkt); sent != 1 {
		t.Errorf("layer f sent = %d, want 1 (high subscriber only)", sent)
	}
	if sent := r.ForwardVideo("pub", SlotCam, "h", pkt); sent != 2 {
		t.Errorf("layer h sent = %d, want 2 (mid + default-mid subscribers)", sent)
	}
	if sent := r.ForwardVideo("pub", SlotCam, "q", pkt); sent != 1 {
		t.Errorf("layer q sent = %d, want 1 (low subscriber only)", sent)
	}
}

// TestSimulcastFallback verifies the fallback order when the preferred layer
// is missing: mid prefers low over high; low climbs to mid — both end at the
// only published layer f.
func TestSimulcastFallback(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)

	attachFakePeer(t, e, r, "mid")
	attachFakePeer(t, e, r, "lo")
	r.JoinChannel(1, "mid")
	r.JoinChannel(1, "lo")
	r.JoinChannel(1, "pub")

	if err := r.SetVideoQuality("mid", "mid"); err != nil {
		t.Fatalf("SetVideoQuality: %v", err)
	}
	if err := r.SetVideoQuality("lo", "low"); err != nil {
		t.Fatalf("SetVideoQuality: %v", err)
	}

	// Publisher sends only the high layer: both subscribers fall back to it
	// (mid chain h->q->f, low chain q->h->f, both end at f).
	registerVideoSource(r, "pub", SlotCam, "f", 9001)

	pkt := makeAudioPacket(t, 1, -1)
	if sent := r.ForwardVideo("pub", SlotCam, "f", pkt); sent != 2 {
		t.Errorf("layer f sent = %d, want 2 (both fall back to f)", sent)
	}
	// An unpublished layer is forwarded to nobody.
	if sent := r.ForwardVideo("pub", SlotCam, "h", pkt); sent != 0 {
		t.Errorf("layer h sent = %d, want 0 (not published)", sent)
	}
}

// TestNonSimulcastAcceptsAll verifies a publisher without RIDs has every
// packet forwarded regardless of subscriber preferences.
func TestNonSimulcastAcceptsAll(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)

	attachFakePeer(t, e, r, "lo")
	r.JoinChannel(1, "lo")
	r.JoinChannel(1, "pub")
	if err := r.SetVideoQuality("lo", "low"); err != nil {
		t.Fatalf("SetVideoQuality: %v", err)
	}

	registerVideoSource(r, "pub", SlotCam, "", 9001) // no simulcast
	pkt := makeAudioPacket(t, 1, -1)
	if sent := r.ForwardVideo("pub", SlotCam, "", pkt); sent != 1 {
		t.Fatalf("ForwardVideo sent = %d, want 1", sent)
	}
}

// TestVideoQualityInvalid verifies invalid quality names are rejected.
func TestVideoQualityInvalid(t *testing.T) {
	r := NewRouter(nil)
	if err := r.SetVideoQuality("a", "ultra"); err == nil {
		t.Fatal("SetVideoQuality(ultra) expected error, got nil")
	}
}

// TestKeyframeRequestOnJoin verifies a subscriber joining a channel with an
// active video publisher triggers a PLI to the publisher.
func TestKeyframeRequestOnJoin(t *testing.T) {
	r := NewRouter(nil)
	pub := &fakeRTCPWriter{}

	r.JoinChannel(1, "pub")
	r.mu.Lock()
	r.rtcpWriters["pub"] = pub
	r.mu.Unlock()
	registerVideoSource(r, "pub", SlotCam, "h", 9002)

	// Publisher itself joining must not PLI; a new subscriber joining must.
	r.JoinChannel(1, "viewer")
	if got := pub.pliCount(); got != 1 {
		t.Fatalf("PLIs after join = %d, want 1", got)
	}

	// The PLI targets the subscriber's preferred layer SSRC (default mid=h).
	pli := pub.pkts[0][0].(*rtcp.PictureLossIndication)
	if pli.MediaSSRC != 9002 {
		t.Fatalf("PLI MediaSSRC = %d, want 9002", pli.MediaSSRC)
	}
}

// TestKeyframeRequestOnQualityChange verifies SetVideoQuality triggers a PLI
// for the newly selected layer.
func TestKeyframeRequestOnQualityChange(t *testing.T) {
	r := NewRouter(nil)
	pub := &fakeRTCPWriter{}

	r.JoinChannel(1, "pub")
	r.JoinChannel(1, "viewer")
	r.mu.Lock()
	r.rtcpWriters["pub"] = pub
	r.mu.Unlock()
	registerVideoSource(r, "pub", SlotCam, "f", 9001)
	registerVideoSource(r, "pub", SlotCam, "q", 9003)

	before := pub.pliCount()
	if err := r.SetVideoQuality("viewer", "low"); err != nil {
		t.Fatalf("SetVideoQuality: %v", err)
	}
	if got := pub.pliCount(); got != before+1 {
		t.Fatalf("PLIs = %d, want %d", got, before+1)
	}
	pli := pub.pkts[len(pub.pkts)-1][0].(*rtcp.PictureLossIndication)
	if pli.MediaSSRC != 9003 {
		t.Fatalf("PLI MediaSSRC = %d, want 9003 (low layer)", pli.MediaSSRC)
	}
}

// TestRelayKeyframeRequest verifies a subscriber's PLI is relayed to the
// publisher owning the referenced SSRC.
func TestRelayKeyframeRequest(t *testing.T) {
	r := NewRouter(nil)
	pub := &fakeRTCPWriter{}

	r.mu.Lock()
	r.rtcpWriters["pub"] = pub
	r.mu.Unlock()
	registerVideoSource(r, "pub", SlotCam, "f", 9001)

	r.relayKeyframeRequest(9001)
	if got := pub.pliCount(); got != 1 {
		t.Fatalf("relayed PLIs = %d, want 1", got)
	}
	pli := pub.pkts[0][0].(*rtcp.PictureLossIndication)
	if pli.MediaSSRC != 9001 {
		t.Fatalf("relayed PLI MediaSSRC = %d, want 9001", pli.MediaSSRC)
	}

	// Unknown SSRC: no relay.
	r.relayKeyframeRequest(424242)
	if got := pub.pliCount(); got != 1 {
		t.Fatalf("relayed PLIs after unknown SSRC = %d, want 1", got)
	}
}

// TestVideoPublishGate verifies the video-publish permission gate drops
// tracks from denied clients.
func TestVideoPublishGate(t *testing.T) {
	r := NewRouter(nil)
	r.SetVideoHandlers(func(clientID string) bool { return clientID == "allowed" })

	r.JoinChannel(1, "denied")
	r.JoinChannel(1, "viewer")
	w := &fakeTrackWriter{}
	r.addVideoOutput("viewer", w)

	track := &fakeVideoTrack{
		fakeTrackReader: fakeTrackReader{packets: []*rtp.Packet{makeAudioPacket(t, 1, -1)}},
		ssrc:            9001,
	}
	r.ReadVideoLoop("denied", SlotCam, track)

	if w.count() != 0 {
		t.Fatalf("viewer received %d packets from denied publisher, want 0", w.count())
	}

	// A denied publisher must not even register a video source.
	r.mu.RLock()
	_, registered := r.videoSources["denied"]
	r.mu.RUnlock()
	if registered {
		t.Fatal("denied publisher registered a video source")
	}
}
