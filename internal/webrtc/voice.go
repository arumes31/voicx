// voice.go ties the Engine and Router together and exposes plain-type
// operations for the TCP control server, so the server package never touches
// Pion types directly.
package webrtc

import (
	"errors"

	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

// ErrNoPeer is returned by Voice operations that reference a client with no
// active peer connection.
var ErrNoPeer = errors.New("webrtc: no peer connection for client")

// Voice is the signaling and routing facade for the voicx voice pipeline.
type Voice struct {
	engine *Engine
	router *Router
	logger *zap.Logger
}

// NewVoice constructs a Voice facade over the given engine and router.
func NewVoice(engine *Engine, router *Router, logger *zap.Logger) *Voice {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Voice{engine: engine, router: router, logger: logger}
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

	answer, err := wrapper.HandleOffer(offerSDP)
	if err != nil {
		v.router.DetachPeer(clientID)
		_ = v.engine.ClosePeerConnection(clientID)
		return "", err
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
	return wrapper.HandleAnswer(answerSDP)
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
