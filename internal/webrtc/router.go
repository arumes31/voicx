// router.go implements the voicx SFU audio router. It tracks channel
// membership, per-peer audio output tracks, and whisper configurations, and
// fans incoming RTP packets out to the appropriate subscribers.
//
// Design notes:
//   - The SFU never decodes or re-encodes media: packets are forwarded as-is
//     (headers included) to each subscriber's per-publisher output track.
//     Each (subscriber, publisher) pair has dedicated audio/video tracks
//     whose track ID carries the publisher's client ID, so clients can map
//     incoming tracks to publishers via the MSID track ID.
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

// pubTrack is one publisher's media as seen by one subscriber: a dedicated
// audio and video output track (per-publisher SFU model). The track ID
// carries the publisher's client ID so clients can map track -> publisher
// via the MSID track ID.
type pubTrack struct {
	audio       *webrtc.TrackLocalStaticRTP
	audioSender *webrtc.RTPSender
	video       *webrtc.TrackLocalStaticRTP
	videoSender *webrtc.RTPSender
}

// Router tracks channel membership, per-publisher output tracks, simulcast
// sources, and whisper configurations, and forwards RTP packets between
// peers. It is safe for concurrent use.
//
// PER-PUBLISHER MODEL (wave 2a): each subscriber gets one
// TrackLocalStaticRTP per publisher (audio and video), bound 1:1 so the
// client can map track -> publisher via the MSID track ID (the publisher's
// client ID). When membership changes, the server renegotiates with affected
// subscribers (see voice.go renegotiation orchestration).
type Router struct {
	logger *zap.Logger

	mu           sync.RWMutex
	members      map[int64]map[string]bool         // channelID -> member clientIDs
	clientChan   map[string]int64                  // clientID -> current channelID
	pubTracks    map[string]map[string]*pubTrack   // subscriberID -> publisherID -> tracks
	pubPeers     map[string]*PeerConnectionWrapper // subscriberID -> its wrapper
	outputs      map[string]TrackWriter            // tap clientID -> audio output track (recorder taps only)
	videoOutputs map[string]TrackWriter            // tap clientID -> video output track (taps only)
	rtcpWriters  map[string]RTCPWriter             // clientID -> RTCP destination (its peer connection)
	videoSources map[string]map[string]uint32      // publisherID -> RID -> SSRC (RID "" = non-simulcast)
	layerPrefs   map[string]string                 // subscriberID -> preferred RID ("f"/"h"/"q")
	whispers     map[string]*whisperConfig         // clientID -> whisper settings
	// whisperPairs tracks publisher tracks created solely to serve an active
	// whisper (publisherID -> subscriberID), so they can be torn down when
	// the whisper ends without touching channel-justified pairs.
	whisperPairs map[string]map[string]bool

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

	// onRenegotiate, when set, is called after tracks are added or removed
	// for a subscriber so the signaling layer can renegotiate (debounced by
	// the caller).
	onRenegotiate func(subscriberID string)

	// echoChannel, when non-zero, is the loopback test channel: publishers in
	// it get a self pair track and hear their own audio (the only channel
	// with self-fan-out). Set via SetEchoChannel.
	echoChannel int64

	// channelAudio, when set, resolves a channel's Opus audio configuration
	// (bitrate/FEC/DTX/stereo). Music channels (stereo + bitrate >= 96k)
	// bypass the talk-power gate so high-quality publishing is never dropped.
	channelAudio func(channelID int64) ChannelAudio
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
		pubTracks:    make(map[string]map[string]*pubTrack),
		pubPeers:     make(map[string]*PeerConnectionWrapper),
		outputs:      make(map[string]TrackWriter),
		videoOutputs: make(map[string]TrackWriter),
		rtcpWriters:  make(map[string]RTCPWriter),
		videoSources: make(map[string]map[string]uint32),
		layerPrefs:   make(map[string]string),
		whispers:     make(map[string]*whisperConfig),
		whisperPairs: make(map[string]map[string]bool),
	}
}

// SetRenegotiateHook installs the callback invoked when a subscriber's track
// set changes (publisher added/removed), so the signaling layer can
// renegotiate with that subscriber (debounced by the caller).
func (r *Router) SetRenegotiateHook(fn func(subscriberID string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRenegotiate = fn
}

// SetEchoChannel sets the loopback test channel (15): publishers in this
// channel get a self pair track and receive their own audio back. 0 disables
// the echo channel.
func (r *Router) SetEchoChannel(channelID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.echoChannel = channelID
}

// SetChannelAudioLookup installs the resolver for per-channel Opus audio
// configuration. Nil disables music-channel detection (the talk-power gate
// then applies everywhere).
func (r *Router) SetChannelAudioLookup(fn func(channelID int64) ChannelAudio) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channelAudio = fn
}

// ChannelOf returns the channel a client currently occupies (0 = none).
func (r *Router) ChannelOf(clientID string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.clientChan[clientID]
	return id, ok
}

// isMusicChannelLocked reports whether the client's current channel is a
// music channel (stereo + bitrate >= 96k). Callers must hold at least a read
// lock.
func (r *Router) isMusicChannelLocked(clientID string) bool {
	if r.channelAudio == nil {
		return false
	}
	channelID, ok := r.clientChan[clientID]
	if !ok {
		return false
	}
	return r.channelAudio(channelID).IsMusic()
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
// one channel at a time in voicx). It also creates publisher tracks between
// the client and every other channel member (per-publisher model) and
// schedules renegotiation for peers that have a connection.
func (r *Router) JoinChannel(channelID int64, clientID string) {
	r.mu.Lock()

	if prev, ok := r.clientChan[clientID]; ok && prev != channelID {
		r.leaveChannelLocked(prev, clientID)
		r.removePairsLocked(prev, clientID)
	}

	set, ok := r.members[channelID]
	if !ok {
		set = make(map[string]bool)
		r.members[channelID] = set
	}
	set[clientID] = true
	r.clientChan[clientID] = channelID

	// Create publisher tracks between the new member and every existing
	// member that has a peer connection.
	renegotiate := make(map[string]bool)
	for other := range set {
		if other == clientID {
			continue
		}
		if r.addPublisherLocked(clientID, other) {
			renegotiate[clientID] = true
		}
		if r.addPublisherLocked(other, clientID) {
			renegotiate[other] = true
		}
	}

	// Echo channel (15): the new member also gets a self pair so they hear
	// their own audio routed back.
	if channelID == r.echoChannel && r.echoChannel != 0 {
		if r.addPublisherLocked(clientID, clientID) {
			renegotiate[clientID] = true
		}
	}

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
	hook := r.onRenegotiate
	r.mu.Unlock()

	for _, publisherID := range keyframeTargets {
		r.RequestKeyframe(publisherID, pref)
	}
	if hook != nil {
		for sub := range renegotiate {
			hook(sub)
		}
	}

	r.logger.Debug("router: client joined channel",
		zap.Int64("channel_id", channelID),
		zap.String("client_id", clientID),
		zap.Int("members", members),
	)
}

// LeaveChannel removes clientID from channelID. It is a no-op if the client
// is not a member. Publisher tracks between the client and the channel's
// members are torn down.
func (r *Router) LeaveChannel(channelID int64, clientID string) {
	r.mu.Lock()
	r.leaveChannelLocked(channelID, clientID)
	renegotiate := r.removePairsLocked(channelID, clientID)
	hook := r.onRenegotiate
	r.mu.Unlock()

	if hook != nil {
		for sub := range renegotiate {
			hook(sub)
		}
	}
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

// removePairsLocked tears down every publisher track between clientID and
// the members of channelID (in both directions), plus any echo self pair.
// It returns the set of subscribers that need renegotiation. Callers must
// hold mu.
func (r *Router) removePairsLocked(channelID int64, clientID string) map[string]bool {
	out := make(map[string]bool)
	for other := range r.members[channelID] {
		if other == clientID {
			continue
		}
		if r.removePublisherLocked(clientID, other) {
			out[clientID] = true
		}
		if r.removePublisherLocked(other, clientID) {
			out[other] = true
		}
	}
	// Echo self pair (if any): the departing client never needs renegotiation.
	r.removePublisherLocked(clientID, clientID)
	// The departing client never needs renegotiation; its tracks die with it.
	delete(out, clientID)
	return out
}

// addPublisherLocked creates the per-publisher tracks for (subscriber,
// publisher) if the subscriber has a peer connection and the pair does not
// exist yet. Self pairs (subscriber == publisher) are only created by the
// echo-channel path; all other callers exclude them. It returns true when
// tracks were added. Callers must hold mu.
func (r *Router) addPublisherLocked(subscriberID, publisherID string) bool {
	pc, ok := r.pubPeers[subscriberID]
	if !ok {
		return false // no connection yet; EnsurePublishers syncs on attach
	}
	tracks, ok := r.pubTracks[subscriberID]
	if !ok {
		tracks = make(map[string]*pubTrack)
		r.pubTracks[subscriberID] = tracks
	}
	if _, exists := tracks[publisherID]; exists {
		return false
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		publisherID, "voicx",
	)
	if err != nil {
		r.logger.Warn("router: create audio track failed",
			zap.String("subscriber_id", subscriberID), zap.String("publisher_id", publisherID), zap.Error(err))
		return false
	}
	audioSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		r.logger.Warn("router: add audio track failed",
			zap.String("subscriber_id", subscriberID), zap.String("publisher_id", publisherID), zap.Error(err))
		return false
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		publisherID, "voicx",
	)
	if err != nil {
		r.logger.Warn("router: create video track failed",
			zap.String("subscriber_id", subscriberID), zap.String("publisher_id", publisherID), zap.Error(err))
		return false
	}
	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		r.logger.Warn("router: add video track failed",
			zap.String("subscriber_id", subscriberID), zap.String("publisher_id", publisherID), zap.Error(err))
		return false
	}

	tracks[publisherID] = &pubTrack{
		audio: audioTrack, audioSender: audioSender,
		video: videoTrack, videoSender: videoSender,
	}
	r.logger.Debug("router: publisher track added",
		zap.String("subscriber_id", subscriberID),
		zap.String("publisher_id", publisherID),
	)
	return true
}

// removePublisherLocked tears down the (subscriber, publisher) tracks. It
// returns true when tracks were removed. Callers must hold mu.
func (r *Router) removePublisherLocked(subscriberID, publisherID string) bool {
	tracks, ok := r.pubTracks[subscriberID]
	if !ok {
		return false
	}
	t, exists := tracks[publisherID]
	if !exists {
		return false
	}
	if pc, ok := r.pubPeers[subscriberID]; ok {
		if t.audioSender != nil {
			_ = pc.pc.RemoveTrack(t.audioSender)
		}
		if t.videoSender != nil {
			_ = pc.pc.RemoveTrack(t.videoSender)
		}
	}
	delete(tracks, publisherID)
	if len(tracks) == 0 {
		delete(r.pubTracks, subscriberID)
	}
	r.logger.Debug("router: publisher track removed",
		zap.String("subscriber_id", subscriberID),
		zap.String("publisher_id", publisherID),
	)
	return true
}

// EnsurePublishers creates all missing publisher tracks between clientID and
// the other members of its channel. It is called when a peer connection
// attaches (the initial SDP answer then already contains all tracks).
func (r *Router) EnsurePublishers(clientID string) {
	r.mu.Lock()
	channelID, ok := r.clientChan[clientID]
	if !ok {
		r.mu.Unlock()
		return
	}
	changed := false
	for other := range r.members[channelID] {
		if other == clientID {
			continue
		}
		if r.addPublisherLocked(clientID, other) {
			changed = true
		}
		if r.addPublisherLocked(other, clientID) {
			changed = true
		}
	}
	hook := r.onRenegotiate
	r.mu.Unlock()

	if changed && hook != nil {
		hook(clientID)
	}
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

// SetWhisper replaces the whisper configuration for clientID. Activating a
// whisper creates publisher tracks towards targets that have no track for
// this publisher yet (typically targets outside the client's channel) and
// schedules renegotiation with them; deactivating or replacing the
// configuration tears down the whisper-created tracks again, unless channel
// membership also justifies them.
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
	old := r.whispers[clientID]
	r.whispers[clientID] = cfg

	renegotiate := make(map[string]bool)
	if cfg.active {
		for sub := range r.whisperTargetsLocked(clientID, cfg) {
			if r.addPublisherLocked(sub, clientID) {
				if r.whisperPairs[clientID] == nil {
					r.whisperPairs[clientID] = make(map[string]bool)
				}
				r.whisperPairs[clientID][sub] = true
				renegotiate[sub] = true
			}
		}
	}
	if old != nil && old.active {
		var keep map[string]bool
		if cfg.active {
			keep = r.whisperTargetsLocked(clientID, cfg)
		}
		for sub := range r.whisperTargetsLocked(clientID, old) {
			if keep[sub] || !r.whisperPairs[clientID][sub] {
				continue
			}
			delete(r.whisperPairs[clientID], sub)
			// Keep pairs that channel membership also justifies.
			if chSub, ok1 := r.clientChan[sub]; ok1 {
				if chPub, ok2 := r.clientChan[clientID]; ok2 && chSub == chPub {
					continue
				}
			}
			if r.removePublisherLocked(sub, clientID) {
				renegotiate[sub] = true
			}
		}
		if len(r.whisperPairs[clientID]) == 0 {
			delete(r.whisperPairs, clientID)
		}
	}
	hook := r.onRenegotiate
	r.mu.Unlock()

	if hook != nil {
		for sub := range renegotiate {
			hook(sub)
		}
	}

	r.logger.Debug("router: whisper set",
		zap.String("client_id", clientID),
		zap.Bool("active", active),
		zap.Int("clients", len(clients)),
		zap.Int("channels", len(channels)),
	)
}

// whisperTargetsLocked computes the subscriber IDs a whisper configuration
// targets (listed clients plus members of the listed channels), excluding
// the whisperer itself. Callers must hold at least a read lock.
func (r *Router) whisperTargetsLocked(clientID string, cfg *whisperConfig) map[string]bool {
	out := make(map[string]bool)
	for c := range cfg.clients {
		out[c] = true
	}
	for ch := range cfg.channels {
		for c := range r.members[ch] {
			out[c] = true
		}
	}
	delete(out, clientID)
	return out
}

// AddOutput registers the audio output track for clientID (recorder taps
// only; per-publisher audio uses pubTracks).
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

// AttachPeer prepares a peer connection for routing: it registers the
// wrapper for per-publisher track management and hooks OnTrack to start read
// loops for incoming tracks. No output tracks are created here — they are
// created per publisher via EnsurePublishers before the SDP answer, and via
// membership changes afterwards (each triggering renegotiation).
func (r *Router) AttachPeer(clientID string, pc *PeerConnectionWrapper) error {
	r.mu.Lock()
	r.pubPeers[clientID] = pc
	r.rtcpWriters[clientID] = pc
	r.mu.Unlock()

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

// DetachPeer removes all router state for clientID: its wrapper, publisher
// tracks in both directions, tap outputs, RTCP writer, video sources, layer
// preference, whisper configuration, and channel membership.
func (r *Router) DetachPeer(clientID string) {
	r.mu.Lock()
	delete(r.pubPeers, clientID)
	delete(r.outputs, clientID)
	delete(r.videoOutputs, clientID)
	delete(r.rtcpWriters, clientID)
	delete(r.whispers, clientID)
	delete(r.whisperPairs, clientID)
	for pub, subs := range r.whisperPairs {
		delete(subs, clientID)
		if len(subs) == 0 {
			delete(r.whisperPairs, pub)
		}
	}
	delete(r.layerPrefs, clientID)
	delete(r.videoSources, clientID)
	// Remove pairs where clientID is the subscriber.
	delete(r.pubTracks, clientID)
	// Remove pairs where clientID is the publisher.
	renegotiate := make(map[string]bool)
	for sub, tracks := range r.pubTracks {
		if _, ok := tracks[clientID]; ok {
			renegotiate[sub] = true
		}
	}
	channelID, ok := r.clientChan[clientID]
	hook := r.onRenegotiate
	r.mu.Unlock()

	for sub := range renegotiate {
		r.mu.Lock()
		r.removePublisherLocked(sub, clientID)
		r.mu.Unlock()
	}
	if hook != nil {
		for sub := range renegotiate {
			hook(sub)
		}
	}
	if ok {
		r.LeaveChannel(channelID, clientID)
	}
}

// ForwardRTP fans pkt out to the appropriate subscribers of senderID: the
// whisper targets when the sender is whispering, otherwise the other members
// of the sender's current channel. Each subscriber receives the packet on
// its dedicated per-publisher track for senderID; taps (e.g. recorders)
// receive it on their single output track. The sender never receives its own
// audio. It returns the number of successful writes.
func (r *Router) ForwardRTP(senderID string, pkt *rtp.Packet) int {
	r.mu.RLock()
	subs := r.targetSubscribersLocked(senderID)
	writers := make([]TrackWriter, 0, len(subs)+1)
	for _, sub := range subs {
		if tracks, ok := r.pubTracks[sub]; ok {
			if t, ok := tracks[senderID]; ok && t.audio != nil {
				writers = append(writers, t.audio)
			}
		}
	}
	// Taps receive everything from the channel.
	if w, ok := r.outputs[r.senderTapID(senderID)]; ok {
		writers = append(writers, w)
	}
	r.mu.RUnlock()

	sent := 0
	for _, w := range writers {
		if err := w.WriteRTP(pkt); err != nil {
			r.logger.Debug("router: dropping output with write error",
				zap.String("sender_id", senderID),
				zap.Error(err),
			)
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

// senderTapID returns the tap clientID that records a given channel, or ""
// when none is registered. Callers must hold at least a read lock.
func (r *Router) senderTapID(senderID string) string {
	channelID, ok := r.clientChan[senderID]
	if !ok {
		return ""
	}
	for tapID := range r.outputs {
		if r.clientChan[tapID] == channelID {
			return tapID
		}
	}
	return ""
}

// targetSubscribersLocked computes the subscriber IDs that should receive
// senderID's media right now (whisper targets or channel members, sender
// excluded). Callers must hold at least a read lock.
func (r *Router) targetSubscribersLocked(senderID string) []string {
	out := make([]string, 0, 8)

	if cfg, ok := r.whispers[senderID]; ok && cfg.active {
		seen := make(map[string]bool)
		for clientID := range cfg.clients {
			seen[clientID] = true
		}
		for channelID := range cfg.channels {
			for clientID := range r.members[channelID] {
				seen[clientID] = true
			}
		}
		delete(seen, senderID)
		for clientID := range seen {
			out = append(out, clientID)
		}
		return out
	}

	channelID, ok := r.clientChan[senderID]
	if !ok {
		return out
	}
	for clientID := range r.members[channelID] {
		if clientID == senderID && channelID != r.echoChannel {
			continue
		}
		out = append(out, clientID)
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
						// Music channels (25) bypass the gate: high-quality
						// publishing is never dropped, regardless of talk power.
						muted = !r.allowTalk(clientID)
						if muted {
							r.mu.RLock()
							music := r.isMusicChannelLocked(clientID)
							r.mu.RUnlock()
							if music {
								muted = false
							}
						}
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

// addVideoOutput registers the video output track for clientID (recorder
// taps only; per-publisher video uses pubTracks).
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

// senderVideoTapID returns the tap clientID recording a given channel, or ""
// when none is registered. Callers must hold at least a read lock.
func (r *Router) senderVideoTapID(senderID string) string {
	channelID, ok := r.clientChan[senderID]
	if !ok {
		return ""
	}
	for tapID := range r.videoOutputs {
		if r.clientChan[tapID] == channelID {
			return tapID
		}
	}
	return ""
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
// (channel members or whisper targets, sender excluded) on their
// per-publisher tracks for senderID, filtered by each subscriber's simulcast
// layer preference. Taps receive it on their single output track. It returns
// the number of successful writes.
func (r *Router) ForwardVideo(senderID, rid string, pkt *rtp.Packet) int {
	r.mu.RLock()
	subs := r.targetSubscribersLocked(senderID)
	accepted := make([]TrackWriter, 0, len(subs)+1)
	for _, sub := range subs {
		if !r.acceptLayerLocked(sub, senderID, rid) {
			continue
		}
		if tracks, ok := r.pubTracks[sub]; ok {
			if t, ok := tracks[senderID]; ok && t.video != nil {
				accepted = append(accepted, t.video)
			}
		}
	}
	if w, ok := r.videoOutputs[r.senderVideoTapID(senderID)]; ok {
		accepted = append(accepted, w)
	}
	r.mu.RUnlock()

	sent := 0
	for _, w := range accepted {
		if err := w.WriteRTP(pkt); err != nil {
			r.logger.Debug("router: dropping video output with write error",
				zap.String("sender_id", senderID),
				zap.Error(err),
			)
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
