// Package netproto defines the wire formats used by the voicx server.
// This file declares the UDP media/signaling message types and a small
// header parser used by the UDP listener (internal/server/udp.go).
//
// UDP packets use a minimal 1-byte header: the first byte is the message
// type (one of the UDPMsg* constants below) and the remaining bytes are
// the type-specific payload. This keeps per-packet overhead tiny, which
// matters for high-frequency media/speaking traffic.
package netproto

import "errors"

// UDP message types. The values are deliberately small, stable integers so
// they can be serialized directly on the wire and compared in switch
// statements. Add new types at the end to preserve compatibility.
const (
	// UDPMsgPing is a connectivity probe. The server replies with UDPMsgPong.
	UDPMsgPing byte = 0x01
	// UDPMsgPong is the reply to UDPMsgPing.
	UDPMsgPong byte = 0x02
	// UDPMsgMedia carries a raw media payload (e.g. Opus/RTP). Not yet wired
	// to a real codec; the server logs and drops these packets for now.
	UDPMsgMedia byte = 0x03
	// UDPMsgSignal carries WebRTC signaling fragments. Real Pion wiring lands
	// in a later phase; currently logged and dropped.
	UDPMsgSignal byte = 0x04
	// UDPMsgSpeaking indicates a client's speaking state changes. Logged and
	// dropped until the media pipeline is implemented.
	UDPMsgSpeaking byte = 0x05
)

// udpMsgName maps a UDP message type to a human-readable name used in logs.
var udpMsgName = map[byte]string{
	UDPMsgPing:     "ping",
	UDPMsgPong:     "pong",
	UDPMsgMedia:    "media",
	UDPMsgSignal:   "signal",
	UDPMsgSpeaking: "speaking",
}

// UDPMsgName returns a human-readable name for a UDP message type, or
// "unknown" if the type is not recognized.
func UDPMsgName(t byte) string {
	if name, ok := udpMsgName[t]; ok {
		return name
	}
	return "unknown"
}

// ErrUDPTooShort is returned by ParseUDPHeader when a packet is shorter than
// the 1-byte header required to determine its type.
var ErrUDPTooShort = errors.New("udp: packet too short for header")

// ParseUDPHeader validates the leading message-type byte of a UDP packet and
// returns the message type together with the remaining payload. It does not
// copy the payload; callers must not retain the slice beyond the lifetime of
// the underlying buffer (the UDP server returns buffers to a pool after
// dispatch, so handlers must copy any data they need to keep).
func ParseUDPHeader(b []byte) (msgType byte, payload []byte, err error) {
	if len(b) == 0 {
		return 0, nil, ErrUDPTooShort
	}
	return b[0], b[1:], nil
}
