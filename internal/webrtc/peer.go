package webrtc

import (
	"fmt"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

// PeerConnectionWrapper wraps a Pion webrtc.PeerConnection with the metadata
// and signaling plumbing required by the voicx server. It tracks the owning
// clientID, exposes offer/answer/ICE/track helpers, and surfaces local ICE
// candidates and the local/remote SDP through buffered channels for the
// signaling layer to consume.
type PeerConnectionWrapper struct {
	pc       *webrtc.PeerConnection
	clientID string
	logger   *zap.Logger

	// localCandidates forwards ICE candidates generated locally. It is never
	// closed (see Close); consumers should also select on Done.
	localCandidates chan webrtc.ICECandidateInit

	// remoteCandidates is a buffered channel for candidates received from the
	// remote peer via signaling. It is consumed by an internal goroutine that
	// applies them to the underlying PeerConnection.
	remoteCandidates chan webrtc.ICECandidateInit

	// localSDP / remoteSDP carry the negotiated session descriptions.
	localSDP  chan webrtc.SessionDescription
	remoteSDP chan webrtc.SessionDescription

	onTrack func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)

	closeOnce sync.Once
	closed    chan struct{}
}

// newPeerConnectionWrapper constructs a wrapper around pc and starts the
// internal goroutine that drains remote ICE candidates. It is called by the
// Engine when a new peer connection is created.
func newPeerConnectionWrapper(pc *webrtc.PeerConnection, clientID string, logger *zap.Logger) *PeerConnectionWrapper {
	w := &PeerConnectionWrapper{
		pc:               pc,
		clientID:         clientID,
		logger:           logger,
		localCandidates:  make(chan webrtc.ICECandidateInit, 64),
		remoteCandidates: make(chan webrtc.ICECandidateInit, 64),
		localSDP:         make(chan webrtc.SessionDescription, 4),
		remoteSDP:        make(chan webrtc.SessionDescription, 4),
		closed:           make(chan struct{}),
	}

	pc.OnICECandidate(w.handleLocalICECandidate)
	pc.OnTrack(w.handleTrack)

	go w.drainRemoteCandidates()
	return w
}

// ClientID returns the identifier of the client that owns this peer connection.
func (w *PeerConnectionWrapper) ClientID() string { return w.clientID }

// LocalCandidates returns a channel that receives locally-gathered ICE
// candidates. The channel is never closed (Pion's gatherer may send
// concurrently with Close); consumers should also select on Done.
func (w *PeerConnectionWrapper) LocalCandidates() <-chan webrtc.ICECandidateInit {
	return w.localCandidates
}

// LocalSDP returns a channel that receives the local session description once
// it has been set (after HandleOffer or a future CreateOffer).
func (w *PeerConnectionWrapper) LocalSDP() <-chan webrtc.SessionDescription {
	return w.localSDP
}

// RemoteSDP returns a channel that receives the remote session description once
// it has been set via HandleOffer or HandleAnswer.
func (w *PeerConnectionWrapper) RemoteSDP() <-chan webrtc.SessionDescription {
	return w.remoteSDP
}

// HandleOffer sets the remote description (an offer SDP), creates an answer,
// sets the local description, and returns the answer SDP string.
func (w *PeerConnectionWrapper) HandleOffer(sdp string) (string, error) {
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
	if err := w.pc.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("setting remote offer: %w", err)
	}
	select {
	case w.remoteSDP <- offer:
	default:
	}

	answer, err := w.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("creating answer: %w", err)
	}
	if err := w.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("setting local answer: %w", err)
	}

	select {
	case w.localSDP <- answer:
	default:
	}

	w.logger.Info("webrtc offer handled",
		zap.String("client_id", w.clientID),
		zap.Int("sdp_len", len(answer.SDP)),
	)
	return answer.SDP, nil
}

// HandleAnswer sets the remote description (an answer SDP) after a local offer
// has been created and sent.
func (w *PeerConnectionWrapper) HandleAnswer(sdp string) error {
	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}
	if err := w.pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("setting remote answer: %w", err)
	}
	select {
	case w.remoteSDP <- answer:
	default:
	}
	w.logger.Info("webrtc answer handled",
		zap.String("client_id", w.clientID),
		zap.Int("sdp_len", len(sdp)),
	)
	return nil
}

// AddICECandidate enqueues a remote ICE candidate for application to the
// underlying PeerConnection. Candidates are applied asynchronously by an
// internal goroutine to avoid blocking the signaling path.
func (w *PeerConnectionWrapper) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	select {
	case w.remoteCandidates <- candidate:
		return nil
	case <-w.closed:
		return fmt.Errorf("peer connection closed")
	default:
		return fmt.Errorf("remote candidate queue full")
	}
}

// AddTrack adds a local track for sending to the remote peer and returns the
// resulting RTPSender.
func (w *PeerConnectionWrapper) AddTrack(track webrtc.TrackLocal) (*webrtc.RTPSender, error) {
	sender, err := w.pc.AddTrack(track)
	if err != nil {
		return nil, fmt.Errorf("adding track: %w", err)
	}
	w.logger.Info("webrtc track added",
		zap.String("client_id", w.clientID),
		zap.String("track_id", track.ID()),
	)
	return sender, nil
}

// WriteRTCP sends RTCP packets to the remote peer (e.g. PLI keyframe
// requests). It satisfies the router's RTCPWriter interface.
func (w *PeerConnectionWrapper) WriteRTCP(pkts []rtcp.Packet) error {
	return w.pc.WriteRTCP(pkts)
}

// OnTrack registers the handler invoked when a remote track arrives.
func (w *PeerConnectionWrapper) OnTrack(fn func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)) {
	w.onTrack = fn
}

// Close closes the underlying PeerConnection and releases all resources. It is
// safe to call multiple times.
//
// Note: localCandidates is deliberately NOT closed here. Pion's ICE gatherer
// may invoke the candidate callback concurrently with Close, and sending on a
// closed channel would race/panic. Consumers should select on Done() instead;
// sends from the gatherer fall through the <-w.closed branch once closed.
func (w *PeerConnectionWrapper) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.closed)
		close(w.localSDP)
		close(w.remoteSDP)
		err = w.pc.Close()
		w.logger.Info("webrtc peer connection closed",
			zap.String("client_id", w.clientID), zap.Error(err))
	})
	return err
}

// Done returns a channel that is closed when the wrapper is closed. Consumers
// of LocalCandidates should select on it to terminate, since LocalCandidates
// itself is never closed (see Close).
func (w *PeerConnectionWrapper) Done() <-chan struct{} {
	return w.closed
}

// handleLocalICECandidate is the OnICECandidate callback. It forwards each
// candidate to localCandidates; a nil candidate signals end-of-gathering.
// Sends fall through once the wrapper is closed.
func (w *PeerConnectionWrapper) handleLocalICECandidate(c *webrtc.ICECandidate) {
	if c == nil {
		w.logger.Debug("webrtc ice gathering complete",
			zap.String("client_id", w.clientID),
		)
		return
	}
	init := c.ToJSON()
	select {
	case w.localCandidates <- init:
	case <-w.closed:
	}
}

// handleTrack is the OnTrack callback. It logs the event and forwards to the
// user-registered handler if one is set.
func (w *PeerConnectionWrapper) handleTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	w.logger.Info("webrtc remote track received",
		zap.String("client_id", w.clientID),
		zap.String("track_id", track.ID()),
		zap.String("kind", track.Kind().String()),
		zap.Uint8("payload_type", uint8(track.PayloadType())),
	)
	if w.onTrack != nil {
		w.onTrack(track, receiver)
	}
}

// drainRemoteCandidates applies queued remote ICE candidates to the underlying
// PeerConnection. It exits when the wrapper is closed, preventing goroutine
// leaks.
func (w *PeerConnectionWrapper) drainRemoteCandidates() {
	for {
		select {
		case <-w.closed:
			return
		case c, ok := <-w.remoteCandidates:
			if !ok {
				return
			}
			if err := w.pc.AddICECandidate(c); err != nil {
				w.logger.Warn("webrtc failed to add remote ice candidate",
					zap.String("client_id", w.clientID),
					zap.String("candidate", c.Candidate),
					zap.Error(err),
				)
			}
		}
	}
}
