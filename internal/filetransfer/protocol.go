// protocol.go defines the wire format of the file-transfer port: netproto
// framing with a small frame vocabulary. All frames use the standard 4-byte
// length + 2-byte type + payload format (see internal/netproto).
package filetransfer

import (
	"encoding/json"
	"fmt"
	"net"

	"voicx/internal/netproto"
)

// File-transfer frame types (distinct namespace from the control protocol).
const (
	frameInit   uint16 = 1 // client -> server: {token, transfer_id}
	frameChunk  uint16 = 2 // both directions: raw file bytes
	frameDigest uint16 = 3 // upload: client -> server {sha256}; download: server -> client
	frameStatus uint16 = 4 // server -> client: {ok, error}
)

// initMsg authenticates a file-transfer connection.
type initMsg struct {
	Token      string `json:"token"`
	TransferID string `json:"transfer_id"`
	// Offset resumes an interrupted download (259): the server skips this
	// many bytes before streaming. The digest frame still covers the WHOLE
	// file, so a resuming client must fold the bytes it already holds into
	// its own hash — that is what keeps the end-to-end integrity check
	// meaningful across a resume. Uploads ignore it: an upload's partial file
	// is discarded on failure, so there is nothing on the server to resume.
	Offset int64 `json:"offset,omitempty"`
}

// digestMsg carries the SHA-256 (hex) of the transferred file.
type digestMsg struct {
	SHA256 string `json:"sha256"`
}

// statusMsg is the server's final reply on a transfer connection.
type statusMsg struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// chunkSize is the payload size of download chunk frames.
const chunkSize = 32 * 1024

// writeJSON writes a JSON payload frame of the given type.
func writeJSON(conn net.Conn, frameType uint16, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return netproto.WriteFrame(conn, &netproto.Frame{Type: frameType, Payload: payload})
}

// writeStatus sends the final status frame, ignoring write errors (the peer
// may already be gone).
func writeStatus(conn net.Conn, ok bool, errMsg string) {
	_ = writeJSON(conn, frameStatus, statusMsg{OK: ok, Error: errMsg})
}

// decodeJSON unmarshals a frame's payload.
func decodeJSON(f *netproto.Frame, v any) error {
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("decoding frame %d: %w", f.Type, err)
	}
	return nil
}
