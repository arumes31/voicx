// transfer.go implements the per-connection transfer flow: token validation,
// upload receive with size and checksum enforcement, and download send.
package filetransfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/store"
)

// connTimeout bounds a single transfer connection.
const connTimeout = 10 * time.Minute

// serve handles one file-transfer connection: validate the init frame's
// token, then run the upload or download flow.
func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))

	f, err := netproto.ReadFrame(conn)
	if err != nil {
		return
	}
	if f.Type != frameInit {
		writeStatus(conn, false, "expected init frame")
		return
	}
	var init initMsg
	if err := decodeJSON(f, &init); err != nil {
		writeStatus(conn, false, "malformed init frame")
		return
	}

	tr, err := s.consume(init.Token, init.TransferID)
	if err != nil {
		writeStatus(conn, false, err.Error())
		return
	}

	if tr.Direction == "upload" {
		err = s.receiveUpload(ctx, conn, tr)
	} else {
		err = s.sendDownload(conn, tr)
	}
	if s.OnTransferComplete != nil {
		result := "ok"
		if err != nil {
			result = "error"
		}
		s.OnTransferComplete(tr.Direction, result)
	}
	if err != nil {
		s.logger.Warn("transfer failed",
			zap.String("transfer_id", tr.ID),
			zap.String("direction", tr.Direction),
			zap.Error(err),
		)
		writeStatus(conn, false, err.Error())
		return
	}
	writeStatus(conn, true, "")
}

// receiveUpload reads chunk frames into a .part file, enforces the declared
// size, verifies the client's SHA-256 digest, and atomically moves the file
// into place with a files-table record. On any failure the .part file is
// removed.
func (s *Server) receiveUpload(ctx context.Context, conn net.Conn, tr *transfer) error {
	finalPath := s.filePath(tr.ChannelID, tr.Name)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		return fmt.Errorf("creating channel dir: %w", err)
	}
	tmpPath := finalPath + ".part"

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	// On any error path: close and remove the partial file.
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	h := sha256.New()
	limiter := newRateLimiter(s.cfg.MaxKBps)
	var received int64

	for {
		frame, err := netproto.ReadFrame(conn)
		if err != nil {
			return fail(fmt.Errorf("reading chunk: %w", err))
		}

		switch frame.Type {
		case frameChunk:
			received += int64(len(frame.Payload))
			if received > tr.Size {
				return fail(errors.New("received more bytes than the declared size"))
			}
			if _, err := f.Write(frame.Payload); err != nil {
				return fail(fmt.Errorf("writing file: %w", err))
			}
			_, _ = h.Write(frame.Payload)
			if err := limiter.wait(ctx, len(frame.Payload)); err != nil {
				return fail(err)
			}

		case frameDigest:
			var digest digestMsg
			if err := decodeJSON(frame, &digest); err != nil {
				return fail(errors.New("malformed digest frame"))
			}
			if received != tr.Size {
				return fail(fmt.Errorf("size mismatch: got %d bytes, declared %d", received, tr.Size))
			}
			sum := hex.EncodeToString(h.Sum(nil))
			if digest.SHA256 != sum {
				return fail(errors.New("checksum mismatch"))
			}
			if err := f.Close(); err != nil {
				_ = os.Remove(tmpPath)
				return fmt.Errorf("closing file: %w", err)
			}
			if err := os.Rename(tmpPath, finalPath); err != nil {
				_ = os.Remove(tmpPath)
				return fmt.Errorf("finalizing file: %w", err)
			}
			if err := s.store.AddFile(ctx, store.FileRecord{
				ChannelID: tr.ChannelID,
				Name:      tr.Name,
				Size:      received,
				SHA256:    sum,
				Uploader:  tr.Uploader,
			}); err != nil {
				return fmt.Errorf("recording file: %w", err)
			}
			s.logger.Info("upload complete",
				zap.String("transfer_id", tr.ID),
				zap.Int64("channel_id", tr.ChannelID),
				zap.String("name", tr.Name),
				zap.Int64("size", received),
			)
			return nil

		default:
			return fail(fmt.Errorf("unexpected frame type %d", frame.Type))
		}
	}
}

// sendDownload streams the file in chunk frames followed by the digest
// frame carrying its SHA-256.
func (s *Server) sendDownload(conn net.Conn, tr *transfer) error {
	f, err := os.Open(s.filePath(tr.ChannelID, tr.Name))
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	limiter := newRateLimiter(s.cfg.MaxKBps)
	buf := make([]byte, chunkSize)

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if err := netproto.WriteFrame(conn, &netproto.Frame{Type: frameChunk, Payload: chunk}); err != nil {
				return fmt.Errorf("writing chunk: %w", err)
			}
			_, _ = h.Write(chunk)
			if err := limiter.wait(context.Background(), n); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("reading file: %w", readErr)
		}
	}

	if err := writeJSON(conn, frameDigest, digestMsg{SHA256: hex.EncodeToString(h.Sum(nil))}); err != nil {
		return fmt.Errorf("writing digest: %w", err)
	}
	s.logger.Info("download complete",
		zap.String("transfer_id", tr.ID),
		zap.Int64("channel_id", tr.ChannelID),
		zap.String("name", tr.Name),
	)
	return nil
}
