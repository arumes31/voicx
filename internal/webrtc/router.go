// router.go implements the voicx SFU audio router. It tracks channel
// membership, per-peer audio output tracks, and whisper configurations, and
// fans incoming RTP packets out to the appropriate subscribers.
//
// Design notes:
//   - The SFU never decodes or re-encodes media: packets are forwarded as-is
//     (headers included) to each subscriber's output track. Each peer has a
//     single audio output track; multiple senders are multiplexed onto it
//     with their original SSRCs, which is valid RTP.
//   - The forwarding core works against small interfaces (TrackReader /
//     TrackWriter) so it is testable without real peer connections, DTLS, or
//     ICE. *webrtc.TrackRemote satisfies TrackReader and
//     *webrtc.TrackLocalStaticRTP satisfies TrackWriter.
//   - Voice activity is detected from the ssrc-audio-level RTP header
//     extension (see vad) so no Opus decoding is needed.
package webrtc

import (
	"fmt"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

// TrackReader is the subset of *webrtc.TrackRemote the router reads from.
type TrackReader interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
}

// TrackWriter is the subset of *webrtc.TrackLocalStaticRTP the router writes
// to.
type TrackWriter interface {
	WriteRTP(pkt *rtp.Packet) error
}

// VideoTrackReader adds the metadata the video path needs (simulcast layer
// via RID, SSRC for PLI routing) to TrackReader. *webrtc.TrackRemote
// satisfies it.
type VideoTrackReader interface {
	TrackReader
	RID() string
	SSRC() webrtc.SSRC
}

// RTCPWriter sends RTCP packets to a peer connection (e.g. PLI keyframe
// requests). *PeerConnectionWrapper satisfies it.
type RTCPWriter interface {
	WriteRTCP(pkts []rtcp.Packet) error
}

// whisperConfig holds a client's whisper settings: while active, the client's
// outgoing audio is routed to the listed clients and the members of the
// listed channels instead of the client's own channel.
type whisperConfig struct {
	active   bool
	clients  map[string]bool
	channels map[int64]bool
}

// Router tracks channel membership, audio/video output tracks, simulcast
// sources, and whisper configurations, and forwards RTP packets between
// peers. It is safe for concurrent use.
type Router struct {
	logger *zap.Logger

	mu           sync.RWMutex
	members      map[int64]map[string]bool    // channelID -> member clientIDs
	clientChan   map[string]int64             // clientID -> current channelID
	outputs      map[string]TrackWriter       // clientID -> audio output track
	videoOutputs map[string]TrackWriter       // clientID -> video output track
	rtcpWriters  map[string]RTCPWriter        // clientID -> RTCP destination (its peer connection)
	videoSources map[string]map[string]uint32 // publisherID -> RID -> SSRC (RID "" = non-simulcast)
	layerPrefs   map[string]string            // subscriberID -> preferred RID ("f"/"h"/"q")
	whispers     map[string]*whisperConfig    // clientID -> whisper settings

	// canTalk gates outgoing audio per client (talk power); nil allows all.
	// canVideo gates outgoing video per client (video publish permission);
	// nil allows all. onSpeaking reports VAD speaking-state transitions; nil
	// discards them. All are set via SetHandlers/SetVideoHandlers.
	canTalk    func(clientID string) bool
	canVideo   func(clientID string) bool
	onSpeaking func(clientID string, speaking bool)

	// onForward, when set, is called after each ForwardRTP/ForwardVideo with
	// the media type and the number of packets written (metrics).
	onForward func(media string, n int)
}

// NewRouter constructs a Router.
func NewRouter(logger *zap.Logger) *Router {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Router{
		logger:       logger,
		members:      make(map[int64]map[string]bool),
		clientChan:   make(map[string]int64),
		outputs:      make(map[string]TrackWriter),
		videoOutputs: make(map[string]TrackWriter),
		rtcpWriters:  make(map[string]RTCPWriter),
		videoSources: make(map[string]map[string]uint32),
		layerPrefs:   make(map[string]string),
		whispers:     make(map[string]*whisperConfig),
	}
}

// SetHandlers installs the talk-permission gate and the speaking-state
// callback. Either may be nil: a nil gate allows all clients, a nil callback
// discards speaking events.
func (r *Router) SetHandlers(canTalk func(clientID string) bool, onSpeaking func(clientID string, speaking bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canTalk = canTalk
	r.onSpeaking = onSpeaking
}

// SetForwardObserver installs a callback invoked after each forward with the
// media type ("audio"/"video") and the number of packets written. Nil
// disables it.
func (r *Router) SetForwardObserver(fn func(media string, n int)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onForward = fn
}

// SetVideoHandlers installs the video-publish permission gate. A nil gate
// allows all clients.
func (r *Router) SetVideoHandlers(canVideo func(clientID string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canVideo = canVideo
}

// JoinChannel records that clientID has joined channelID. It is idempotent
// and moves the client out of any previous channel (a client occupies exactly
// one channel at a time in voicx).
func (r *Router) JoinChannel(channelID int64, clientID string) {
	r.mu.Lock()

	if prev, ok := r.clientChan[clientID]; ok && prev != channelID {
		r.leaveChannelLocked(prev, clientID)
	}

	set, ok := r.members[channelID]
	if !ok {
		set = make(map[string]bool)
		r.members[channelID] = set
	}
	set[clientID] = true
	r.clientChan[clientID] = channelID

	// A new video subscriber joining mid-stream needs a keyframe before it
	// can decode; collect the publishers in the channel to PLI (the RTCP
	// writes happen after unlock).
	var keyframeTargets []string
	for publisherID := range r.videoSources {
		if publisherID != clientID && set[publisherID] {
			keyframeTargets = append(keyframeTargets, publisherID)
		}
	}
	pref := r.layerPrefLocked(clientID)
	members := len(set)
	r.mu.Unlock()

	for _, publisherID := range keyframeTargets {
		r.RequestKeyframe(publisherID, pref)
	}

	r.logger.Debug("router: client joined channel",
		zap.Int64("channel_id", channelID),
		zap.String("client_id", clientID),
		zap.Int("members", members),
	)
}

// LeaveChannel removes clientID from channelID. It is a no-op if the client
// is not a member.
func (r *Router) LeaveChannel(channelID int64, clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaveChannelLocked(channelID, clientID)
}

// leaveChannelLocked removes clientID from channelID. Callers must hold mu.
func (r *Router) leaveChannelLocked(channelID int64, clientID string) {
	if set, ok := r.members[channelID]; ok {
		delete(set, clientID)
		if len(set) == 0 {
			delete(r.members, channelID)
		}
	}
	if r.clientChan[clientID] == channelID {
		delete(r.clientChan, clientID)
	}
	r.logger.Debug("router: client left channel",
		zap.Int64("channel_id", channelID),
		zap.String("client_id", clientID),
	)
}

// LeaveAll removes clientID from every channel. Intended for disconnects.
func (r *Router) LeaveAll(clientID string) {
	r.mu.Lock()
	channelID, ok := r.clientChan[clientID]
	r.mu.Unlock()
	if ok {
		r.LeaveChannel(channelID, clientID)
	}
}

// SetWhisper replaces the whisper configuration for clientID.
func (r *Router) SetWhisper(clientID string, clients []string, channels []int64, active bool) {
	cfg := &whisperConfig{
		active:   active,
		clients:  make(map[string]bool, len(clients)),
		channels: make(map[int64]bool, len(channels)),
	}
	for _, c := range clients {
		cfg.clients[c] = true
	}
	for _, ch := range channels {
		cfg.channels[ch] = true
	}

	r.mu.Lock()
	r.whispers[clientID] = cfg
	r.mu.Unlock()

	r.logger.Debug("router: whisper set",
		zap.String("client_id", clientID),
		zap.Bool("active", active),
		zap.Int("clients", len(clients)),
		zap.Int("channels", len(channels)),
	)
}

// AddOutput registers the audio output track for clientID. Forwarded audio
// addressed to the client is written to this track.
func (r *Router) AddOutput(clientID string, w TrackWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outputs[clientID] = w
}

// RemoveOutput removes the audio output track for clientID, if any.
func (r *Router) RemoveOutput(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.outputs, clientID)
}

// AttachPeer prepares a peer connection for routing: it adds one audio and
// one video output track (which must happen before the SDP answer is created
// so the tracks are negotiated) and hooks OnTrack to start read loops for
// incoming tracks. The video output track advertises VP8; packets from
// publishers using other codecs are forwarded with their original payload
// types (multi-codec passthrough with per-codec output tracks and
// renegotiation is future work).
func (r *Router) AttachPeer(clientID string, pc *PeerConnectionWrapper) error {
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		"audio", "voicx",
	)
	if err != nil {
		return fmt.Errorf("creating audio output track: %w", err)
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		return fmt.Errorf("adding audio output track: %w", err)
	}
	r.AddOutput(clientID, audioTrack)

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		"video", "voicx",
	)
	if err != nil {
		return fmt.Errorf("creating video output track: %w", err)
	}
	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("adding video output track: %w", err)
	}
	r.addVideoOutput(clientID, videoTrack)

	r.mu.Lock()
	r.rtcpWriters[clientID] = pc
	r.mu.Unlock()

	// Relay subscriber RTCP (PLI/FIR keyframe requests) back to publishers.
	go r.rtcpRelayLoop(clientID, videoSender)

	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		switch remote.Kind() {
		case webrtc.RTPCodecTypeAudio:
			go r.ReadLoop(clientID, remote, audioLevelExtID(receiver))
		case webrtc.RTPCodecTypeVideo:
			go r.ReadVideoLoop(clientID, remote)
		default:
			r.logger.Debug("router: ignoring unknown track kind",
				zap.String("client_id", clientID),
				zap.String("kind", remote.Kind().String()),
			)
		}
	})

	r.logger.Debug("router: peer attached", zap.String("client_id", clientID))
	return nil
}

// DetachPeer removes all router state for clientID: output tracks, channel
// membership, whisper configuration, RTCP writer, video sources, and layer
// preference.
func (r *Router) DetachPeer(clientID string) {
	r.mu.Lock()
	delete(r.outputs, clientID)
	delete(r.videoOutputs, clientID)
	delete(r.rtcpWriters, clientID)
	delete(r.whispers, clientID)
	delete(r.layerPrefs, clientID)
	delete(r.videoSources, clientID)
	channelID, ok := r.clientChan[clientID]
	r.mu.Unlock()
	if ok {
		r.LeaveChannel(channelID, clientID)
	}
}

// ForwardRTP fans pkt out to the appropriate audio subscribers of senderID:
// the whisper targets when the sender is whispering, otherwise the other
// members of the sender's current channel. The sender never receives its own
// audio. It returns the number of successful writes.
func (r *Router) ForwardRTP(senderID string, pkt *rtp.Packet) int {
	r.mu.RLock()
	targets := r.targetsLocked(senderID, r.outputs)
	r.mu.RUnlock()

	sent := 0
	for clientID, w := range targets {
		if err := w.WriteRTP(pkt); err != nil {
			r.logger.Debug("router: dropping output with write error",
				zap.String("client_id", clientID),
				zap.Error(err),
			)
			r.RemoveOutput(clientID)
			continue
		}
		sent++
	}
	if sent > 0 {
		r.notifyForward("audio", sent)
	}
	return sent
}

// notifyForward invokes the forward observer, if any.
func (r *Router) notifyForward(media string, n int) {
	r.mu.RLock()
	obs := r.onForward
	r.mu.RUnlock()
	if obs != nil {
		obs(media, n)
	}
}

// targetsLocked computes the set of output tracks (from the given outputs
// map) that should receive senderID's media right now. Callers must hold at
// least a read lock.
func (r *Router) targetsLocked(senderID string, outputs map[string]TrackWriter) map[string]TrackWriter {
	out := make(map[string]TrackWriter)

	if cfg, ok := r.whispers[senderID]; ok && cfg.active {
		for clientID := range cfg.clients {
			if w, ok := outputs[clientID]; ok {
				out[clientID] = w
			}
		}
		for channelID := range cfg.channels {
			for clientID := range r.members[channelID] {
				if w, ok := outputs[clientID]; ok {
					out[clientID] = w
				}
			}
		}
		delete(out, senderID)
		return out
	}

	channelID, ok := r.clientChan[senderID]
	if !ok {
		return out
	}
	for clientID := range r.members[channelID] {
		if clientID == senderID {
			continue
		}
		if w, ok := outputs[clientID]; ok {
			out[clientID] = w
		}
	}
	return out
}

// ReadLoop reads RTP packets from an incoming audio track until the track
// ends or errors, feeding the VAD and forwarding each packet. extID is the
// negotiated ssrc-audio-level header extension ID, or 0 when the extension
// was not negotiated (VAD disabled).
func (r *Router) ReadLoop(clientID string, track TrackReader, extID uint8) {
	v := &vad{}
	muted := false

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			break
		}

		if extID != 0 {
			if level, ok := audioLevel(pkt, extID); ok {
				speaking, changed := v.Update(level, time.Now())
				if changed {
					if speaking {
						// Re-evaluate the talk gate on every speech start.
						muted = !r.allowTalk(clientID)
					}
					if !muted {
						r.speak(clientID, speaking)
					}
				}
			}
		}

		if muted {
			continue
		}
		r.ForwardRTP(clientID, pkt)
	}

	// Track ended: make sure the client does not stay marked as speaking.
	if v.speaking && !muted {
		r.speak(clientID, false)
	}
}

// allowTalk evaluates the talk gate for clientID, defaulting to allowed.
func (r *Router) allowTalk(clientID string) bool {
	r.mu.RLock()
	gate := r.canTalk
	r.mu.RUnlock()
	if gate == nil {
		return true
	}
	return gate(clientID)
}

// speak reports a speaking-state transition, if a callback is registered.
func (r *Router) speak(clientID string, speaking bool) {
	r.mu.RLock()
	cb := r.onSpeaking
	r.mu.RUnlock()
	if cb != nil {
		cb(clientID, speaking)
	}
}

// audioLevelExtID extracts the negotiated ssrc-audio-level header extension
// ID from the receiver parameters, or 0 when the extension was not
// negotiated.
func audioLevelExtID(receiver *webrtc.RTPReceiver) uint8 {
	for _, ext := range receiver.GetParameters().HeaderExtensions {
		if ext.URI == sdp.AudioLevelURI {
			return uint8(ext.ID)
		}
	}
	return 0
}

// audioLevel extracts the audio level (0 = loudest, 127 = silence) from the
// ssrc-audio-level header extension of pkt.
func audioLevel(pkt *rtp.Packet, extID uint8) (byte, bool) {
	ext := pkt.GetExtension(extID)
	if len(ext) == 0 {
		return 0, false
	}
	return ext[0] & 0x7F, true
}

// ChannelStats describes the member count for a single channel.
type ChannelStats struct {
	ChannelID int64 `json:"channel_id"`
	Members   int   `json:"members"`
}

// RouterStats is an aggregate snapshot of the router state.
type RouterStats struct {
	Channels   int            `json:"channels"`
	Clients    int            `json:"clients"`
	PerChannel []ChannelStats `json:"per_channel"`
}

// Stats returns a snapshot of the router state: number of channels with
// members, number of clients in a channel, and per-channel member counts.
func (r *Router) Stats() RouterStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := RouterStats{
		Channels:   len(r.members),
		Clients:    len(r.clientChan),
		PerChannel: make([]ChannelStats, 0, len(r.members)),
	}
	for channelID, set := range r.members {
		stats.PerChannel = append(stats.PerChannel, ChannelStats{
			ChannelID: channelID,
			Members:   len(set),
		})
	}
	return stats
}

// ---------------------------------------------------------------------------
// Voice activity detection
// ---------------------------------------------------------------------------

const (
	// vadSpeakThreshold is the audio level (0 = loudest, 127 = silence) at or
	// below which a reading counts as voice activity.
	vadSpeakThreshold = 55
	// vadHangover is how long the speaking state is held after the last
	// voice-active reading, to avoid flapping during natural pauses.
	vadHangover = 300 * time.Millisecond
)

// vad is a per-track voice activity state machine fed with ssrc-audio-level
// readings.
type vad struct {
	speaking  bool
	lastVoice time.Time
}

// Update feeds one audio-level reading at time now and returns the speaking
// state after this reading and whether it changed.
func (v *vad) Update(level byte, now time.Time) (speaking, changed bool) {
	if level <= vadSpeakThreshold {
		v.lastVoice = now
		if !v.speaking {
			v.speaking = true
			return true, true
		}
		return true, false
	}
	if v.speaking && now.Sub(v.lastVoice) >= vadHangover {
		v.speaking = false
		return false, true
	}
	return v.speaking, false
}

// ---------------------------------------------------------------------------
// Video forwarding (SFU)
// ---------------------------------------------------------------------------

// addVideoOutput registers the video output track for clientID.
func (r *Router) addVideoOutput(clientID string, w TrackWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.videoOutputs[clientID] = w
}

// removeVideoOutput removes the video output track for clientID, if any.
func (r *Router) removeVideoOutput(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.videoOutputs, clientID)
}

// ReadVideoLoop reads RTP packets from an incoming video track until the
// track ends or errors and forwards each packet. Video from clients without
// publish permission is dropped (the gate is evaluated once at track start).
// The track's RID (empty for non-simulcast) is registered so subscribers can
// select layers and PLIs can be routed back to this publisher.
func (r *Router) ReadVideoLoop(clientID string, track VideoTrackReader) {
	if !r.allowVideo(clientID) {
		r.logger.Info("router: video publish denied",
			zap.String("client_id", clientID),
		)
		return
	}

	rid := track.RID()
	ssrc := uint32(track.SSRC())
	r.mu.Lock()
	src, ok := r.videoSources[clientID]
	if !ok {
		src = make(map[string]uint32)
		r.videoSources[clientID] = src
	}
	src[rid] = ssrc
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(src, rid)
		if len(src) == 0 {
			delete(r.videoSources, clientID)
		}
		r.mu.Unlock()
	}()

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		r.ForwardVideo(clientID, rid, pkt)
	}
}

// ForwardVideo fans a video packet out to the appropriate subscribers
// (channel members or whisper targets, sender excluded), filtered by each
// subscriber's simulcast layer preference. It returns the number of
// successful writes.
func (r *Router) ForwardVideo(senderID, rid string, pkt *rtp.Packet) int {
	r.mu.RLock()
	targets := r.targetsLocked(senderID, r.videoOutputs)
	accepted := make(map[string]TrackWriter, len(targets))
	for clientID, w := range targets {
		if r.acceptLayerLocked(clientID, senderID, rid) {
			accepted[clientID] = w
		}
	}
	r.mu.RUnlock()

	sent := 0
	for clientID, w := range accepted {
		if err := w.WriteRTP(pkt); err != nil {
			r.logger.Debug("router: dropping video output with write error",
				zap.String("client_id", clientID),
				zap.Error(err),
			)
			r.removeVideoOutput(clientID)
			continue
		}
		sent++
	}
	if sent > 0 {
		r.notifyForward("video", sent)
	}
	return sent
}

// qualityToRID maps the client-facing quality names to simulcast RIDs.
var qualityToRID = map[string]string{
	"high": "f",
	"mid":  "h",
	"low":  "q",
}

// SetVideoQuality records the subscriber's preferred simulcast layer and
// requests a keyframe for the new layer from every publisher in the
// subscriber's channel (switching layers mid-stream requires a keyframe of
// the new layer). Quality must be "high", "mid", or "low".
func (r *Router) SetVideoQuality(clientID, quality string) error {
	rid, ok := qualityToRID[quality]
	if !ok {
		return fmt.Errorf("invalid video quality %q (want high, mid, or low)", quality)
	}

	r.mu.Lock()
	r.layerPrefs[clientID] = rid
	channelID, inChannel := r.clientChan[clientID]
	var publishers []string
	if inChannel {
		for publisherID := range r.videoSources {
			if publisherID != clientID && r.members[channelID][publisherID] {
				publishers = append(publishers, publisherID)
			}
		}
	}
	r.mu.Unlock()

	for _, publisherID := range publishers {
		r.RequestKeyframe(publisherID, rid)
	}
	return nil
}

// layerPrefLocked returns the subscriber's preferred RID, defaulting to "h"
// (mid). Callers must hold at least a read lock.
func (r *Router) layerPrefLocked(clientID string) string {
	if pref, ok := r.layerPrefs[clientID]; ok {
		return pref
	}
	return "h"
}

// layerFallback returns the RIDs to try in order when the preferred layer is
// not published: high falls down through mid to low, mid prefers low over
// high (less bandwidth), low climbs through mid to high.
func layerFallback(pref string) []string {
	switch pref {
	case "f":
		return []string{"f", "h", "q"}
	case "q":
		return []string{"q", "h", "f"}
	default:
		return []string{"h", "q", "f"}
	}
}

// acceptLayerLocked reports whether a packet from publisher carrying the
// given RID should be forwarded to subscriber. Callers must hold at least a
// read lock.
func (r *Router) acceptLayerLocked(subscriber, publisher, rid string) bool {
	src := r.videoSources[publisher]
	// Unknown publisher or non-simulcast (no RIDs published): accept all.
	if len(src) == 0 {
		return true
	}
	if _, ok := src[""]; ok {
		return true
	}
	for _, candidate := range layerFallback(r.layerPrefLocked(subscriber)) {
		if _, ok := src[candidate]; ok {
			return rid == candidate
		}
	}
	return true
}

// RequestKeyframe sends a Picture Loss Indication to the publisher's peer
// connection for the given layer's SSRC (or any known SSRC of the publisher
// when the layer is unknown). It is a no-op when the publisher is unknown or
// has no RTCP writer.
func (r *Router) RequestKeyframe(publisherID, rid string) {
	r.mu.RLock()
	w := r.rtcpWriters[publisherID]
	var ssrc uint32
	if src, ok := r.videoSources[publisherID]; ok {
		if s, ok := src[rid]; ok {
			ssrc = s
		} else {
			for _, s := range src {
				ssrc = s
				break
			}
		}
	}
	r.mu.RUnlock()

	if w == nil {
		return
	}
	if err := w.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: ssrc},
	}); err != nil {
		r.logger.Debug("router: PLI write failed",
			zap.String("publisher_id", publisherID),
			zap.Error(err),
		)
	}
}

// rtcpRelayLoop reads RTCP from a subscriber's video sender and relays
// keyframe requests (PLI/FIR) to the publisher that owns the referenced SSRC.
// It exits when the sender errors (peer connection closed).
func (r *Router) rtcpRelayLoop(clientID string, sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			switch p := pkt.(type) {
			case *rtcp.PictureLossIndication:
				r.relayKeyframeRequest(p.MediaSSRC)
			case *rtcp.FullIntraRequest:
				r.relayKeyframeRequest(p.MediaSSRC)
			}
		}
	}
}

// relayKeyframeRequest forwards a keyframe request for mediaSSRC to the
// publisher that owns it.
func (r *Router) relayKeyframeRequest(mediaSSRC uint32) {
	r.mu.RLock()
	var owner string
	for publisherID, src := range r.videoSources {
		for _, ssrc := range src {
			if ssrc == mediaSSRC {
				owner = publisherID
				break
			}
		}
	}
	w := r.rtcpWriters[owner]
	r.mu.RUnlock()

	if owner == "" || w == nil {
		return
	}
	if err := w.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: mediaSSRC},
	}); err != nil {
		r.logger.Debug("router: PLI relay failed",
			zap.String("publisher_id", owner),
			zap.Error(err),
		)
	}
}

// allowVideo evaluates the video-publish gate for clientID, defaulting to
// allowed.
func (r *Router) allowVideo(clientID string) bool {
	r.mu.RLock()
	gate := r.canVideo
	r.mu.RUnlock()
	if gate == nil {
		return true
	}
	return gate(clientID)
}
