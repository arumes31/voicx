// Package netproto implements the wire protocol framing used by the voicx
// control channel. It provides a length-prefixed binary framing layer and a
// minimal message type registry / dispatch helper.
//
// Wire format (all integers big-endian):
//
//	+-------------------+----------------+-------------------+
//	| length (4 bytes)  | type (2 bytes) | payload (N bytes) |
//	+-------------------+----------------+-------------------+
//
// `length` is the total number of bytes following the 4-byte length prefix,
// i.e. 2 (type) + len(payload). Frames larger than MaxFrameSize are rejected
// to prevent abuse.
package netproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"voicx/internal/safecast"
)

// MaxFrameSize is the largest frame (type + payload) the server will accept.
// 1 MiB is plenty for control messages while keeping malicious peers from
// exhausting memory.
const MaxFrameSize = 1 << 20 // 1 MiB

// MaxPayloadSize is the largest payload allowed, derived from MaxFrameSize
// minus the 2-byte type field.
const MaxPayloadSize = MaxFrameSize - 2

// Frame is a single unit of communication on the control channel.
type Frame struct {
	Type    uint16
	Payload []byte
}

// ErrFrameTooLarge is returned when a peer announces a frame larger than
// MaxFrameSize.
var ErrFrameTooLarge = errors.New("netproto: frame too large")

// ReadFrame reads a single frame from r. It blocks until a complete frame is
// available, the connection is closed, or an error occurs.
func ReadFrame(r io.Reader) (*Frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("netproto: reading length prefix: %w", err)
	}

	length := binary.BigEndian.Uint32(header[:])
	if length < 2 {
		return nil, fmt.Errorf("netproto: invalid frame length %d (must be >= 2)", length)
	}
	if length > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("netproto: reading frame body: %w", err)
	}

	return &Frame{
		Type:    binary.BigEndian.Uint16(buf[:2]),
		Payload: buf[2:],
	}, nil
}

// WriteFrame writes a single frame to w.
func WriteFrame(w io.Writer, f *Frame) error {
	if f == nil {
		return errors.New("netproto: nil frame")
	}
	if len(f.Payload) > MaxPayloadSize {
		return ErrFrameTooLarge
	}

	length, err := safecast.IntToUint32(2 + len(f.Payload))
	if err != nil {
		return ErrFrameTooLarge
	}
	var header [6]byte
	binary.BigEndian.PutUint32(header[:4], length)
	binary.BigEndian.PutUint16(header[4:6], f.Type)

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("netproto: writing header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("netproto: writing payload: %w", err)
		}
	}
	return nil
}
