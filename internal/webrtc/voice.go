// voice.go ties the Engine and Router together and exposes plain-type
// operations for the TCP control server, so the server package never touches
// Pion types directly.
package webrtc

import (
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

// ErrNoPeer is returned by Voice operations that reference a client with no
// active peer connection.
var ErrNoPeer = errors.New("webrtc: no peer connection for client")

// renegState tracks per-peer renegotiation scheduling (debounce + rate limit
// + unanswered-offer tolerance).
type renegState struct {
	timer      *time.Timer
	lastSent   time.Time
	unanswered int
}

// Voice is the signaling and routing facade for the voicx voice pipeline.
type Voice struct {
	engine *Engine
	router *Router
	logger *zap.Logger

	renegMu     sync.Mutex
	reneg       map[string]*renegState
	offerSender func(clientID, offerSDP string) error
}

// renegDebounce coalesces bursts of track changes into one offer per peer.
var renegDebounce = 200 * time.Millisecond

// renegRateLimit caps server-initiated renegotiation to one offer per this
// interval per peer (ICE restart storm protection).
var renegRateLimit = 2 * time.Second

// NewVoice constructs a Voice facade over the given engine and router.
func NewVoice(engine *Engine, router *Router, logger *zap.Logger) *Voice {
	if logger == nil {
		logger = zap.NewNop()
	}
	v := &Voice{engine: engine, router: router, logger: logger, reneg: make(map[string]*renegState)}
	router.SetRenegotiateHook(v.scheduleRenegotiate)
	return v
}

// SetHandlers installs the talk-permission gate and the speaking-state
// callback on the router. See Router.SetHandlers.
func (v *Voice) SetHandlers(canTalk func(clientID string) bool, onSpeaking func(clientID string, speaking bool)) {
	v.router.SetHandlers(canTalk, onSpeaking)
}

// SetVideoHandlers installs the video-publish permission gate on the router.
// See Router.SetVideoHandlers.
func (v *Voice) SetVideoHandlers(canVideo func(clientID string) bool) {
	v.router.SetVideoHandlers(canVideo)
}

// SetVideoQuality sets a subscriber's preferred simulcast layer ("high",
// "mid", or "low"). See Router.SetVideoQuality.
func (v *Voice) SetVideoQuality(clientID, quality string) error {
	return v.router.SetVideoQuality(clientID, quality)
}

// SetOfferSender installs the callback used to deliver server-initiated
// renegotiation offers to clients (the TCP control channel in production).
func (v *Voice) SetOfferSender(fn func(clientID, offerSDP string) error) {
	v.renegMu.Lock()
	defer v.renegMu.Unlock()
	v.offerSender = fn
}

// SetEchoChannel sets the loopback test channel (15). See
// Router.SetEchoChannel.
func (v *Voice) SetEchoChannel(channelID int64) {
	v.router.SetEchoChannel(channelID)
}

// SetChannelAudioLookup installs the per-channel Opus configuration resolver
// (21-25): it drives SDP fmtp rewriting (per subscriber channel) and
// music-channel detection (talk-gate bypass). See
// Router.SetChannelAudioLookup.
func (v *Voice) SetChannelAudioLookup(fn func(channelID int64) ChannelAudio) {
	v.router.SetChannelAudioLookup(fn)
}

// channelAudioFor resolves the Opus configuration of the client's current
// channel. It returns the zero ChannelAudio (server default, no munging)
// when no lookup is installed or the client is in no channel.
func (v *Voice) channelAudioFor(clientID string) ChannelAudio {
	v.router.mu.RLock()
	defer v.router.mu.RUnlock()
	if v.router.channelAudio == nil {
		return ChannelAudio{}
	}
	channelID, ok := v.router.clientChan[clientID]
	if !ok {
		return ChannelAudio{}
	}
	return v.router.channelAudio(channelID)
}

// scheduleRenegotiate debounces track-set changes for a peer into a single
// renegotiation offer, capped at one offer per renegRateLimit.
func (v *Voice) scheduleRenegotiate(clientID string) {
	v.renegMu.Lock()
	defer v.renegMu.Unlock()
	st, ok := v.reneg[clientID]
	if !ok {
		st = &renegState{}
		v.reneg[clientID] = st
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	delay := renegDebounce
	if elapsed := time.Since(st.lastSent); !st.lastSent.IsZero() && elapsed < renegRateLimit {
		delay = renegRateLimit - elapsed
	}
	st.timer = time.AfterFunc(delay, func() { v.sendRenegotiation(clientID) })
}

// sendRenegotiation creates and delivers a renegotiation offer for a peer.
// It tolerates unanswered previous offers: the skip is logged and the server
// keeps running. Pion v3.3 cannot roll back a local offer, so a fresh offer
// can only be created once the client answers (HandleAnswer) or re-offers
// (HandleOffer recreates the connection) — until then renegotiation offers
// for that peer are refused by CreateOffer and skipped.
func (v *Voice) sendRenegotiation(clientID string) {
	v.renegMu.Lock()
	st, ok := v.reneg[clientID]
	if !ok {
		v.renegMu.Unlock()
		return
	}
	st.timer = nil
	sender := v.offerSender
	unanswered := st.unanswered
	v.renegMu.Unlock()

	wrapper := v.engine.PeerConnection(clientID)
	if wrapper == nil {
		return
	}

	if unanswered > 0 {
		v.logger.Warn("client did not answer previous renegotiation offer; skipping until it answers or re-offers (old clients get no audio until updated)",
			zap.String("client_id", clientID),
			zap.Int("unanswered", unanswered),
		)
	}

	offer, err := wrapper.CreateOffer()
	if err != nil {
		v.logger.Debug("renegotiation offer skipped",
			zap.String("client_id", clientID),
			zap.Error(err),
		)
		return
	}

	// Per-channel Opus parameters (21-23): rewrite the offer's Opus fmtp for
	// the subscriber's channel before delivery.
	if cfg := v.channelAudioFor(clientID); !cfg.IsZero() {
		offer = RewriteOpusFMTP(offer, cfg)
	}

	v.renegMu.Lock()
	st.lastSent = time.Now()
	st.unanswered++
	v.renegMu.Unlock()

	if sender != nil {
		if err := sender(clientID, offer); err != nil {
			v.logger.Warn("delivering renegotiation offer failed",
				zap.String("client_id", clientID),
				zap.Error(err),
			)
		}
	}
}

// AddTap registers an additional subscriber (e.g. a recorder) in a channel
// with explicit audio/video output tracks.
func (v *Voice) AddTap(channelID int64, tapID string, audio, video TrackWriter) {
	if audio != nil {
		v.router.AddOutput(tapID, audio)
	}
	if video != nil {
		v.router.addVideoOutput(tapID, video)
	}
	v.router.JoinChannel(channelID, tapID)
}

// RemoveTap removes a tap previously registered with AddTap.
func (v *Voice) RemoveTap(tapID string) {
	v.router.DetachPeer(tapID)
}

// HandleOffer (re)creates the peer connection for clientID, attaches it to
// the router, applies the SDP offer, and returns the SDP answer. An existing
// session for the client is torn down first, making re-offers idempotent.
//
// onLocalCandidate, if non-nil, is invoked asynchronously for every locally
// gathered ICE candidate until the peer connection is closed.
func (v *Voice) HandleOffer(clientID, offerSDP string, onLocalCandidate func(candidate, sdpMid string, mlineIndex uint16)) (string, error) {
	_ = v.ClosePeer(clientID)

	wrapper, err := v.engine.NewPeerConnection(clientID)
	if err != nil {
		return "", err
	}
	if err := v.router.AttachPeer(clientID, wrapper); err != nil {
		_ = v.engine.ClosePeerConnection(clientID)
		return "", err
	}

	// Per-publisher model: create tracks for every current channel member
	// BEFORE the initial answer so they are negotiated right away.
	v.router.EnsurePublishers(clientID)

	answer, err := wrapper.HandleOffer(offerSDP)
	if err != nil {
		v.router.DetachPeer(clientID)
		_ = v.engine.ClosePeerConnection(clientID)
		return "", err
	}

	// Per-channel Opus parameters (21-23): rewrite the answer's Opus fmtp
	// for the subscriber's channel.
	if cfg := v.channelAudioFor(clientID); !cfg.IsZero() {
		answer = RewriteOpusFMTP(answer, cfg)
	}

	if onLocalCandidate != nil {
		go func() {
			candidates := wrapper.LocalCandidates()
			for {
				select {
				case c, ok := <-candidates:
					if !ok {
						return
					}
					var mid string
					if c.SDPMid != nil {
						mid = *c.SDPMid
					}
					var idx uint16
					if c.SDPMLineIndex != nil {
						idx = *c.SDPMLineIndex
					}
					onLocalCandidate(c.Candidate, mid, idx)
				case <-wrapper.Done():
					return
				}
			}
		}()
	}

	v.logger.Info("voice session established", zap.String("client_id", clientID))
	return answer, nil
}

// HandleAnswer applies an SDP answer from the client (used when the server
// initiated renegotiation).
func (v *Voice) HandleAnswer(clientID, answerSDP string) error {
	wrapper := v.engine.PeerConnection(clientID)
	if wrapper == nil {
		return ErrNoPeer
	}
	if err := wrapper.HandleAnswer(answerSDP); err != nil {
		return err
	}
	v.renegMu.Lock()
	if st, ok := v.reneg[clientID]; ok {
		st.unanswered = 0
	}
	v.renegMu.Unlock()
	return nil
}

// AddICECandidate applies a trickle ICE candidate received from the client.
func (v *Voice) AddICECandidate(clientID, candidate, sdpMid string, mlineIndex uint16) error {
	wrapper := v.engine.PeerConnection(clientID)
	if wrapper == nil {
		return ErrNoPeer
	}
	init := webrtc.ICECandidateInit{Candidate: candidate}
	if sdpMid != "" {
		init.SDPMid = &sdpMid
	}
	init.SDPMLineIndex = &mlineIndex
	return wrapper.AddICECandidate(init)
}

// ClosePeer tears down the client's voice session: peer connection, router
// output, channel membership, and whisper configuration. It is a no-op when
// no session exists.
func (v *Voice) ClosePeer(clientID string) error {
	v.renegMu.Lock()
	if st, ok := v.reneg[clientID]; ok {
		if st.timer != nil {
			st.timer.Stop()
		}
		delete(v.reneg, clientID)
	}
	v.renegMu.Unlock()
	v.router.DetachPeer(clientID)
	return v.engine.ClosePeerConnection(clientID)
}

// JoinChannel records channel membership for routing.
func (v *Voice) JoinChannel(clientID string, channelID int64) {
	v.router.JoinChannel(channelID, clientID)
}

// LeaveChannel removes channel membership for routing.
func (v *Voice) LeaveChannel(clientID string, channelID int64) {
	v.router.LeaveChannel(channelID, clientID)
}

// SetWhisper replaces the client's whisper configuration.
func (v *Voice) SetWhisper(clientID string, clients []string, channels []int64, active bool) {
	v.router.SetWhisper(clientID, clients, channels, active)
}

// PeerCount returns the number of active peer connections.
func (v *Voice) PeerCount() int {
	return v.engine.PeerCount()
}
