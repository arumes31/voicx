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
	"strconv"
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
	defer func() {
		if err := conn.Close(); err != nil {
			s.logger.Debug("closing file transfer connection", zap.Error(err))
		}
	}()
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
		err = s.sendDownload(conn, tr, init.Offset)
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
	finalPath := s.filePath(tr.ChannelID, tr.Folder, tr.Name)
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
	limiter := s.limiterFor(time.Now())
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
			if err := s.finalizeUpload(ctx, tr, tmpPath, finalPath, received, sum); err != nil {
				return err
			}
			s.logger.Info("upload complete",
				zap.String("transfer_id", tr.ID),
				zap.Int64("channel_id", tr.ChannelID),
				zap.String("folder", tr.Folder),
				zap.String("name", tr.Name),
				zap.Int64("size", received),
			)
			return nil

		default:
			return fail(fmt.Errorf("unexpected frame type %d", frame.Type))
		}
	}
}

// maxFileVersions is how many old versions are kept on overwrite (264).
const maxFileVersions = 3

// finalizeUpload places a verified upload: it rotates the replaced file into
// <name>.v1..v3 (264), hard-links to an identical existing blob when one
// exists (275 dedup), and records the row. A ".vcx" upload is flagged
// (91-135) so the file browser can refuse to hand out a blob it has no key
// for, and its name must be the truncated digest of the ciphertext: that is
// what makes the name unforgeable, so an upload cannot displace the blob an
// older message still points at.
func (s *Server) finalizeUpload(ctx context.Context, tr *transfer, tmpPath, finalPath string, size int64, sum string) error {
	encrypted := isEncryptedAttachment(tr.Name)
	if encrypted && tr.Name != sum[:encryptedNameLen]+encryptedSuffix {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("encrypted attachment name is not its content digest")
	}

	// Identical re-upload of the current file: keep the blob, refresh the row.
	if cur, err := s.store.GetFile(ctx, tr.ChannelID, tr.Folder, tr.Name); err == nil && cur.SHA256 == sum {
		_ = os.Remove(tmpPath)
		return s.store.AddFile(ctx, store.FileRecord{
			ChannelID: tr.ChannelID, Folder: tr.Folder, Name: tr.Name,
			Size: size, SHA256: sum, Uploader: tr.Uploader, Encrypted: encrypted,
		})
	}

	// Content-derived .vcx names never collide, so rotation can only burn four
	// store lookups on a guaranteed miss for them (91-135).
	if !encrypted {
		s.rotateVersions(ctx, tr)
	}

	// Dedup (275): point at an identical existing blob instead of storing a
	// second copy.
	linked := false
	if ex, err := s.store.FindFileBySHA(ctx, tr.ChannelID, sum, tr.Folder, tr.Name); err == nil && ex != nil {
		if err := os.Link(s.filePath(tr.ChannelID, ex.Folder, ex.Name), finalPath); err == nil {
			linked = true
			_ = os.Remove(tmpPath)
		}
	}
	if !linked {
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("finalizing file: %w", err)
		}
	}
	return s.store.AddFile(ctx, store.FileRecord{
		ChannelID: tr.ChannelID, Folder: tr.Folder, Name: tr.Name,
		Size: size, SHA256: sum, Uploader: tr.Uploader, Encrypted: encrypted,
	})
}

// rotateVersions shifts <name>.v1..v2 up one slot (dropping the oldest) and
// renames the current file to <name>.v1 (264). Failures are logged, not
// fatal: losing a version must not break the upload.
func (s *Server) rotateVersions(ctx context.Context, tr *transfer) {
	rename := func(folder, from, to string) {
		if _, err := s.store.GetFile(ctx, tr.ChannelID, folder, from); err != nil {
			return // nothing to rotate
		}
		if err := s.store.RenameFile(ctx, tr.ChannelID, folder, from, folder, to); err != nil {
			s.logger.Warn("version rotate (db) failed", zap.String("from", from), zap.Error(err))
			return
		}
		oldPath := s.filePath(tr.ChannelID, folder, from)
		newPath := s.filePath(tr.ChannelID, folder, to)
		if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("version rotate (disk) failed", zap.String("from", from), zap.Error(err))
		}
	}
	// Oldest version falls off the end.
	oldest := tr.Name + ".v" + strconv.Itoa(maxFileVersions)
	if _, err := s.store.GetFile(ctx, tr.ChannelID, tr.Folder, oldest); err == nil {
		_ = s.store.DeleteFile(ctx, tr.ChannelID, tr.Folder, oldest)
		_ = os.Remove(s.filePath(tr.ChannelID, tr.Folder, oldest))
	}
	for v := maxFileVersions - 1; v >= 1; v-- {
		rename(tr.Folder, tr.Name+".v"+strconv.Itoa(v), tr.Name+".v"+strconv.Itoa(v+1))
	}
	rename(tr.Folder, tr.Name, tr.Name+".v1")
}

// sendDownload streams the file in chunk frames followed by the digest
// frame carrying its SHA-256. offset > 0 resumes an interrupted download
// (259): the prefix the client already holds is folded into the hash but not
// re-sent, so the digest still covers the whole file and a resumed download
// is verified exactly as strictly as a fresh one.
func (s *Server) sendDownload(conn net.Conn, tr *transfer, offset int64) (retErr error) {
	f, err := os.Open(s.filePath(tr.ChannelID, tr.Folder, tr.Name))
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing file: %w", err))
		}
	}()

	h := sha256.New()
	if offset > 0 {
		if offset > tr.Size {
			return fmt.Errorf("resume offset %d is past the end of %s (%d bytes)", offset, tr.Name, tr.Size)
		}
		if _, err := io.CopyN(h, f, offset); err != nil {
			return fmt.Errorf("hashing resumed prefix: %w", err)
		}
	}
	limiter := s.limiterFor(time.Now())
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
		zap.Int64("resume_offset", offset),
	)
	return nil
}
