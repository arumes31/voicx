package webrtc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

// Engine is the core WebRTC engine for the voicx server. It owns a reusable
// Pion webrtc.API (built once from a configured MediaEngine, SettingEngine and
// InterceptorRegistry) and a registry of active peer connections keyed by
// clientID. The Engine is safe for concurrent use.
type Engine struct {
	logger *zap.Logger
	api    *webrtc.API

	// iceServers are the configured ICE servers used when creating new peer
	// connections.
	iceServers []webrtc.ICEServer

	// enableAV1 controls whether the AV1 video codec is registered.
	enableAV1   bool
	certificate webrtc.Certificate
	fingerprint string

	mu     sync.RWMutex
	peers  map[string]*PeerConnectionWrapper
	closed bool
}

// New constructs a WebRTC Engine with sensible defaults: Opus audio, VP8/VP9
// and H.264 video (AV1 optional), and the default Pion interceptors (NACK and
// RTCP feedback). The returned Engine is ready to create peer connections via
// NewPeerConnection.
func New(logger *zap.Logger, iceServers []string, enableAV1 bool) (*Engine, error) {
	if logger == nil {
		return nil, fmt.Errorf("webrtc: logger must not be nil")
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := registerCodecs(mediaEngine, enableAV1); err != nil {
		return nil, fmt.Errorf("webrtc: registering codecs: %w", err)
	}

	settingEngine := webrtc.SettingEngine{}
	// Use lite ICE candidate gathering by default; full ICE is configured via
	// the ICE servers. Keep defaults conservative for a server-side SFU.
	settingEngine.SetSCTPMaxReceiveBufferSize(16 * 1024 * 1024)
	settingEngine.SetInterfaceFilter(usableICEInterface)

	interceptorRegistry := &interceptor.Registry{}
	congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(1_500_000))
	})
	if err != nil {
		return nil, fmt.Errorf("webrtc: creating GCC interceptor: %w", err)
	}
	interceptorRegistry.Add(congestionController)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("webrtc: configuring TWCC egress: %w", err)
	}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("webrtc: registering default interceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settingEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	)

	parsedICE := make([]webrtc.ICEServer, 0, len(iceServers))
	for _, raw := range iceServers {
		if raw == "" {
			continue
		}
		parsedICE = append(parsedICE, webrtc.ICEServer{URLs: []string{raw}})
	}
	if len(parsedICE) == 0 {
		parsedICE = []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
	}
	certificate, err := generateDTLSCertificate(time.Now())
	if err != nil {
		return nil, fmt.Errorf("webrtc: generating DTLS certificate: %w", err)
	}
	fingerprints, err := certificate.GetFingerprints()
	if err != nil {
		return nil, fmt.Errorf("webrtc: fingerprinting DTLS certificate: %w", err)
	}
	if len(fingerprints) == 0 {
		return nil, fmt.Errorf("webrtc: DTLS certificate has no fingerprint")
	}

	e := &Engine{
		logger:      logger,
		api:         api,
		iceServers:  parsedICE,
		enableAV1:   enableAV1,
		certificate: *certificate,
		fingerprint: fingerprints[0].Value,
		peers:       make(map[string]*PeerConnectionWrapper),
	}

	e.logger.Info("webrtc engine ready",
		zap.Int("ice_servers", len(parsedICE)),
		zap.Bool("av1_enabled", enableAV1),
		zap.String("dtls_fingerprint", e.fingerprint),
	)
	return e, nil
}

// generateDTLSCertificate keeps the process-ephemeral identity usable for a
// long-running server. Pion v3.3.6's GenerateCertificate expires after about
// one month; the key here is still replaced on every process start, while the
// X.509 validity covers servers that legitimately remain up for years.
func generateDTLSCertificate(now time.Time) (*webrtc.Certificate, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating serial: %w", err)
	}
	return webrtc.NewCertificate(privateKey, x509.Certificate{
		Issuer:                pkix.Name{CommonName: "voicx ephemeral DTLS"},
		Subject:               pkix.Name{CommonName: "voicx ephemeral DTLS"},
		SerialNumber:          serial,
		Version:               2,
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	})
}

// NewPeerConnection creates a new webrtc.PeerConnection using the engine's
// reusable API and the configured ICE servers, wraps it in a
// PeerConnectionWrapper, registers it under clientID and returns the wrapper.
func (e *Engine) NewPeerConnection(clientID string) (*PeerConnectionWrapper, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil, fmt.Errorf("webrtc: engine is closed")
	}
	if clientID == "" {
		return nil, fmt.Errorf("webrtc: clientID must not be empty")
	}
	if _, exists := e.peers[clientID]; exists {
		return nil, fmt.Errorf("webrtc: peer connection already exists for client %q", clientID)
	}

	pc, err := e.api.NewPeerConnection(webrtc.Configuration{
		ICEServers:   e.iceServers,
		Certificates: []webrtc.Certificate{e.certificate},
	})
	if err != nil {
		return nil, fmt.Errorf("webrtc: creating peer connection for %q: %w", clientID, err)
	}

	wrapper := newPeerConnectionWrapper(pc, clientID, e.logger)
	e.peers[clientID] = wrapper
	e.logger.Info("webrtc peer connection created", zap.String("client_id", clientID))
	return wrapper, nil
}

// DTLSFingerprint returns the ephemeral ECDSA P-256 identity shared by peer
// connections during this process run. A fresh identity is generated on the
// next server start, limiting media-key lifetime without changing the stable
// control-channel TLS certificate pinned by clients.
func (e *Engine) DTLSFingerprint() string { return e.fingerprint }

// usableICEInterface removes host-only bridge adapters while keeping physical,
// loopback, VPN, and unknown interfaces eligible. A VPN may be the only route
// to a private deployment, so it is intentionally not filtered by name.
func usableICEInterface(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"docker", "veth", "virbr", "vboxnet", "vmnet", "br-"} {
		if strings.HasPrefix(n, prefix) {
			return false
		}
	}
	return true
}

// PeerConnection returns the registered wrapper for clientID, or nil if none.
func (e *Engine) PeerConnection(clientID string) *PeerConnectionWrapper {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.peers[clientID]
}

// ClosePeerConnection closes and removes the peer connection registered under
// clientID. It is a no-op (returns nil) if no peer connection exists.
func (e *Engine) ClosePeerConnection(clientID string) error {
	e.mu.Lock()
	wrapper, ok := e.peers[clientID]
	if ok {
		delete(e.peers, clientID)
	}
	e.mu.Unlock()

	if !ok {
		return nil
	}
	err := wrapper.Close()
	e.logger.Info("webrtc peer connection closed", zap.String("client_id", clientID), zap.Error(err))
	return err
}

// Close shuts down all registered peer connections and marks the engine as
// closed. It is safe to call multiple times.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	peers := e.peers
	e.peers = make(map[string]*PeerConnectionWrapper)
	e.mu.Unlock()

	var firstErr error
	for id, wrapper := range peers {
		if cerr := wrapper.Close(); cerr != nil && firstErr == nil {
			firstErr = cerr
		}
		e.logger.Info("webrtc peer connection closed on engine shutdown",
			zap.String("client_id", id), zap.Error(firstErr))
	}
	e.logger.Info("webrtc engine closed", zap.Int("peers_closed", len(peers)))
	return firstErr
}

// PeerCount returns the number of currently registered peer connections.
func (e *Engine) PeerCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.peers)
}

// registerCodecs registers the audio and video codecs supported by the voicx
// SFU on the given MediaEngine. AV1 is only registered when enableAV1 is true.
func registerCodecs(m *webrtc.MediaEngine, enableAV1 bool) error {
	// Audio: Opus (preferred) and G.722 as a fallback.
	opus := webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: opus,
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("opus: %w", err)
	}

	// Audio level header extension: lets the server detect speech (VAD)
	// without decoding Opus.
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{
		URI: sdp.AudioLevelURI,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("audio-level header extension: %w", err)
	}

	// Video: VP8, VP9, H.264 (and optionally AV1).
	vp8 := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		PayloadType: 96,
	}
	if err := m.RegisterCodec(vp8, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("vp8: %w", err)
	}

	vp9 := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP9,
			ClockRate: 90000,
		},
		PayloadType: 98,
	}
	if err := m.RegisterCodec(vp9, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("vp9: %w", err)
	}

	h264 := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		PayloadType: 102,
	}
	if err := m.RegisterCodec(h264, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("h264: %w", err)
	}

	if enableAV1 {
		av1 := webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeAV1,
				ClockRate: 90000,
			},
			PayloadType: 45,
		}
		if err := m.RegisterCodec(av1, webrtc.RTPCodecTypeVideo); err != nil {
			return fmt.Errorf("av1: %w", err)
		}
	}

	return nil
}
