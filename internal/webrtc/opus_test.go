// opus_test.go covers per-channel Opus configuration: ChannelAudio semantics
// and the SDP fmtp rewriting, plus the echo channel and the music-channel
// talk-gate bypass in the router.
package webrtc

import (
	"strings"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

// --- ChannelAudio ------------------------------------------------------------

func TestChannelAudioSemantics(t *testing.T) {
	var zero ChannelAudio
	if !zero.IsZero() {
		t.Fatal("zero ChannelAudio is not IsZero")
	}
	if zero.IsMusic() {
		t.Fatal("zero ChannelAudio is music")
	}
	if (ChannelAudio{Bitrate: 128000}).IsMusic() {
		t.Fatal("high-bitrate mono is not music (stereo required)")
	}
	if (ChannelAudio{Bitrate: 64000, Stereo: true}).IsMusic() {
		t.Fatal("stereo below 96k is not music")
	}
	if !(ChannelAudio{Bitrate: 96000, Stereo: true}).IsMusic() {
		t.Fatal("stereo 96k should be music")
	}
	if got := zero.fmtp(); !strings.Contains(got, "maxaveragebitrate=32000") {
		t.Fatalf("default fmtp = %q, want bitrate 32000", got)
	}
}

// --- RewriteOpusFMTP ---------------------------------------------------------

const sampleSDP = "v=0\r\n" +
	"o=- 123 456 IN IP4 0.0.0.0\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111 0\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=fmtp:111 minptime=10;useinbandfec=1\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=fmtp:0 some=thing\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
	"a=rtpmap:96 VP8/90000\r\n" +
	"a=fmtp:96 max-fs=3600\r\n"

func TestRewriteOpusFMTP(t *testing.T) {
	tests := []struct {
		name string
		sdp  string
		cfg  ChannelAudio
		want []string // lines that must be present
		keep []string // lines that must survive untouched
	}{
		{
			name: "rewrite opus fmtp only",
			sdp:  sampleSDP,
			cfg:  ChannelAudio{Bitrate: 64000, FEC: true},
			want: []string{"a=fmtp:111 minptime=10;maxaveragebitrate=64000;useinbandfec=1;usedtx=0;stereo=0;sprop-stereo=0"},
			keep: []string{"a=fmtp:0 some=thing", "a=fmtp:96 max-fs=3600", "a=rtpmap:111 opus/48000/2"},
		},
		{
			name: "music preset",
			sdp:  sampleSDP,
			cfg:  ChannelAudio{Bitrate: 128000, Stereo: true},
			want: []string{"a=fmtp:111 minptime=10;maxaveragebitrate=128000;useinbandfec=0;usedtx=0;stereo=1;sprop-stereo=1"},
		},
		{
			name: "dtx",
			sdp:  sampleSDP,
			cfg:  ChannelAudio{DTX: true},
			want: []string{"a=fmtp:111 minptime=10;maxaveragebitrate=32000;useinbandfec=0;usedtx=1;stereo=0;sprop-stereo=0"},
		},
		{
			name: "missing fmtp inserted after rtpmap",
			sdp: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=rtpmap:111 opus/48000/2\r\n" +
				"a=rtcp-fb:111 nack\r\n",
			cfg:  ChannelAudio{Bitrate: 48000},
			want: []string{"a=rtpmap:111 opus/48000/2\r\na=fmtp:111 minptime=10;maxaveragebitrate=48000;useinbandfec=0;usedtx=0;stereo=0;sprop-stereo=0\r\na=rtcp-fb:111 nack"},
		},
		{
			name: "LF endings preserved",
			sdp: "m=audio 9 UDP/TLS/RTP/SAVPF 111\n" +
				"a=rtpmap:111 opus/48000/2\n" +
				"a=fmtp:111 minptime=10;useinbandfec=1\n",
			cfg:  ChannelAudio{Bitrate: 64000},
			want: []string{"a=fmtp:111 minptime=10;maxaveragebitrate=64000;useinbandfec=0;usedtx=0;stereo=0;sprop-stereo=0\n"},
		},
		{
			name: "no audio section untouched",
			sdp: "m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
				"a=rtpmap:96 VP8/90000\r\n",
			cfg:  ChannelAudio{Bitrate: 64000},
			want: []string{"a=rtpmap:96 VP8/90000"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RewriteOpusFMTP(tc.sdp, tc.cfg)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("rewritten SDP missing %q:\n%s", want, got)
				}
			}
			for _, keep := range tc.keep {
				if !strings.Contains(got, keep) {
					t.Errorf("rewritten SDP lost %q:\n%s", keep, got)
				}
			}
		})
	}
}

// TestRewriteOpusFMTPMultipleAudioSections verifies every audio m-section is
// rewritten independently.
func TestRewriteOpusFMTPMultipleAudioSections(t *testing.T) {
	sdp := "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n" +
		"a=fmtp:111 minptime=10;useinbandfec=1\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 112\r\n" +
		"a=rtpmap:112 opus/48000/2\r\n" +
		"a=fmtp:112 minptime=10;useinbandfec=1\r\n"
	got := RewriteOpusFMTP(sdp, ChannelAudio{Bitrate: 96000})
	for _, pt := range []string{"111", "112"} {
		want := "a=fmtp:" + pt + " minptime=10;maxaveragebitrate=96000;useinbandfec=0;usedtx=0;stereo=0;sprop-stereo=0"
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// --- echo channel ------------------------------------------------------------

// TestEchoChannelSelfHearing verifies a publisher in the echo channel gets a
// self pair track and hears their own audio, while normal channels still
// exclude the sender.
func TestEchoChannelSelfHearing(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)
	r.SetEchoChannel(99)

	attachFakePeer(t, e, r, "a")
	attachFakePeer(t, e, r, "b")
	r.JoinChannel(99, "a")

	if pubTrackFor(r, "a", "a") == nil {
		t.Fatal("no echo self pair created on join")
	}
	pkt := makeAudioPacket(t, 1, -1)
	if sent := r.ForwardRTP("a", pkt); sent != 1 {
		t.Fatalf("echo forward sent = %d, want 1 (self)", sent)
	}

	r.JoinChannel(99, "b")
	if sent := r.ForwardRTP("a", pkt); sent != 2 {
		t.Fatalf("echo forward sent = %d, want 2 (self + b)", sent)
	}

	// Moving to a normal channel tears the self pair down and re-excludes
	// the sender from fan-out.
	r.JoinChannel(1, "a")
	if pubTrackFor(r, "a", "a") != nil {
		t.Fatal("self pair leaked into normal channel")
	}
	if sent := r.ForwardRTP("a", pkt); sent != 0 {
		t.Fatalf("normal channel forward sent = %d, want 0 (sender excluded)", sent)
	}
}

// TestEchoChannelSelfPairThroughSignaling walks item 15 the way a user does:
// the control server puts the client in the echo channel BEFORE it joins voice,
// then the client offers, re-offers, and finally restarts ICE. Every rebuild
// drops the peer connection (and with it the self pair track), so the self pair
// has to be recreated from EnsurePublishers each time or the user stops hearing
// themselves after the first reconnect.
func TestEchoChannelSelfPairThroughSignaling(t *testing.T) {
	e, err := New(testLogger(), nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()
	r := NewRouter(nil)
	r.SetEchoChannel(99)
	v := NewVoice(e, r, testLogger())
	defer v.ClosePeer("a")

	// Joined the echo channel with no peer connection yet: nothing to pair to.
	r.JoinChannel(99, "a")
	if pubTrackFor(r, "a", "a") != nil {
		t.Fatal("self pair created without a peer connection")
	}

	clientPC := newClientPC(t)
	pkt := makeAudioPacket(t, 1, -1)

	assertEcho := func(stage string) {
		t.Helper()
		if ch, ok := r.ChannelOf("a"); !ok || ch != 99 {
			t.Fatalf("%s: channel membership = %d (present %v), want 99", stage, ch, ok)
		}
		if pubTrackFor(r, "a", "a") == nil {
			t.Fatalf("%s: no echo self pair", stage)
		}
		if sent := r.ForwardRTP("a", pkt); sent != 1 {
			t.Fatalf("%s: echo forward sent = %d, want 1 (self)", stage, sent)
		}
	}

	establishVoiceSession(t, v, clientPC, "a")
	assertEcho("fresh join")

	// Plain re-offer (the client renegotiates from the same peer connection).
	establishVoiceSession(t, v, clientPC, "a")
	assertEcho("re-offer")

	// ICE restart (59): same peer connection, iceRestart offer.
	offer, err := clientPC.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("client CreateOffer(iceRestart): %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("client SetLocalDescription: %v", err)
	}
	answer, err := v.HandleOffer("a", offer.SDP, nil)
	if err != nil {
		t.Fatalf("HandleOffer(iceRestart): %v", err)
	}
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}
	assertEcho("ice restart")
}

// --- whisper receive-side signal ---------------------------------------------

// TestWhisperTargetsReportsActiveWhisper verifies the router reports who an
// active whisper reaches (the receive-side signal of 32/33) and reports nothing
// once the whisper is off.
func TestWhisperTargetsReportsActiveWhisper(t *testing.T) {
	r := NewRouter(nil)
	r.JoinChannel(5, "listener1")
	r.JoinChannel(5, "listener2")
	r.JoinChannel(9, "whisperer")

	if got := r.WhisperTargets("whisperer"); got != nil {
		t.Fatalf("targets without a whisper = %v, want nil", got)
	}

	// A direct target plus a whole channel; the whisperer never targets itself.
	r.SetWhisper("whisperer", []string{"direct"}, []int64{5}, true)
	got := map[string]bool{}
	for _, id := range r.WhisperTargets("whisperer") {
		got[id] = true
	}
	want := map[string]bool{"direct": true, "listener1": true, "listener2": true}
	if len(got) != len(want) {
		t.Fatalf("whisper targets = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("whisper targets = %v, missing %q", got, id)
		}
	}

	// An inactive configuration must not report targets: the receiving client
	// would otherwise flash the taskbar for ordinary channel audio.
	r.SetWhisper("whisperer", []string{"direct"}, []int64{5}, false)
	if got := r.WhisperTargets("whisperer"); got != nil {
		t.Fatalf("targets after deactivation = %v, want nil", got)
	}
}

// --- music channel talk-gate bypass ------------------------------------------

// TestReadLoopMusicChannelBypassesTalkGate verifies the talk-power gate does
// not drop audio published into a music channel (stereo + bitrate >= 96k),
// while it still applies in normal channels.
func TestReadLoopMusicChannelBypassesTalkGate(t *testing.T) {
	r := NewRouter(nil)
	r.SetChannelAudioLookup(func(channelID int64) ChannelAudio {
		if channelID == 7 {
			return ChannelAudio{Bitrate: 128000, Stereo: true}
		}
		return ChannelAudio{}
	})

	w := &fakeTrackWriter{}
	r.JoinChannel(7, "musician")
	r.JoinChannel(7, "listener")
	r.AddOutput("listener", w)

	// Talk gate denies everyone.
	r.SetHandlers(func(string) bool { return false }, nil)

	track := &fakeTrackReader{packets: []*rtp.Packet{makeAudioPacket(t, 1, 20)}}
	r.ReadLoop("musician", track, 1)

	if w.count() != 1 {
		t.Fatalf("forwarded = %d, want 1 (music channel bypasses talk gate)", w.count())
	}

	// Control: a normal channel with the same gate drops the audio.
	w2 := &fakeTrackWriter{}
	r.JoinChannel(8, "talker")
	r.JoinChannel(8, "listener2")
	r.AddOutput("listener2", w2)
	track2 := &fakeTrackReader{packets: []*rtp.Packet{makeAudioPacket(t, 1, 20)}}
	r.ReadLoop("talker", track2, 1)
	if w2.count() != 0 {
		t.Fatalf("forwarded = %d, want 0 (gate applies in normal channel)", w2.count())
	}
}
