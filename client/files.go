// files.go defines the Wails-bound API for wave-7 files & media: the file
// browser (list/delete/rename/versions/links), progress-reporting transfers
// with cancel, and server/channel icon management. Long transfers run in
// goroutines and report via the "ft_progress" event; cancel closes the
// transfer connection (the server cleans up the partial upload).
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
)

// --- file browser -------------------------------------------------------------

// FileList returns the channel's file listing for one folder (plus quota).
func (a *App) FileList(channelID int64, folder string) (netproto.FileListResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgFileList, netproto.MsgFileListResponse,
		netproto.FileList{ChannelID: channelID, Folder: folder}, 5*time.Second)
	if err != nil {
		return netproto.FileListResponse{}, err
	}
	var resp netproto.FileListResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.FileListResponse{}, err
	}
	return resp, nil
}

// FileDelete deletes a channel file (uploader / b_ft_delete gated).
func (a *App) FileDelete(channelID int64, folder, name string) string {
	if err := a.cmLoad().write(netproto.MsgFileDelete, netproto.FileDelete{
		ChannelID: channelID, Folder: folder, Name: name,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// FileRename renames or moves a channel file.
func (a *App) FileRename(channelID int64, folder, name, newFolder, newName string) string {
	if err := a.cmLoad().write(netproto.MsgFileRename, netproto.FileRename{
		ChannelID: channelID, Folder: folder, Name: name, NewFolder: newFolder, NewName: newName,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// FileVersions lists a file's rotated old versions (264).
func (a *App) FileVersions(channelID int64, folder, name string) (netproto.FileVersionsResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgFileVersions, netproto.MsgFileVersionsResponse,
		netproto.FileVersions{ChannelID: channelID, Folder: folder, Name: name}, 5*time.Second)
	if err != nil {
		return netproto.FileVersionsResponse{}, err
	}
	var resp netproto.FileVersionsResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.FileVersionsResponse{}, err
	}
	return resp, nil
}

// FileLink creates an expiring download link for a file (267).
func (a *App) FileLink(channelID int64, folder, name string) (netproto.FileLinkResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgFileLink, netproto.MsgFileLinkResponse,
		netproto.FileLink{ChannelID: channelID, Folder: folder, Name: name}, 5*time.Second)
	if err != nil {
		return netproto.FileLinkResponse{}, err
	}
	var resp netproto.FileLinkResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.FileLinkResponse{}, err
	}
	return resp, nil
}

// --- icons --------------------------------------------------------------------

// ServerIconSet uploads the server icon (admin only).
func (a *App) ServerIconSet(dataBase64 string) string {
	if err := a.cmLoad().write(netproto.MsgServerIconSet, netproto.ServerIconSet{DataBase64: dataBase64}); err != nil {
		return err.Error()
	}
	return ""
}

// ServerIconGet fetches the server icon (empty = none).
func (a *App) ServerIconGet() (netproto.ServerIconData, error) {
	f, err := a.cmLoad().request(netproto.MsgServerIconGet, netproto.MsgServerIconData,
		netproto.ServerIconGet{}, 5*time.Second)
	if err != nil {
		return netproto.ServerIconData{}, err
	}
	var data netproto.ServerIconData
	if err := decodeJSON(f, &data); err != nil {
		return netproto.ServerIconData{}, err
	}
	return data, nil
}

// ChannelIconSet uploads a channel icon, or copies it from another channel
// (copyFromChannelID != 0, 271 icon library).
func (a *App) ChannelIconSet(channelID int64, dataBase64 string, copyFromChannelID int64) string {
	if err := a.cmLoad().write(netproto.MsgChannelIconSet, netproto.ChannelIconSet{
		ChannelID: channelID, DataBase64: dataBase64, CopyFromChannelID: copyFromChannelID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// --- transfers with progress + cancel -----------------------------------------

// ftProgress is the payload of the "ft_progress" event.
type ftProgress struct {
	ID          string `json:"id"`
	Direction   string `json:"direction"`
	Name        string `json:"name"`
	Transferred int64  `json:"transferred"`
	Total       int64  `json:"total"`
	BytesPerSec int64  `json:"bytes_per_sec"`
	Status      string `json:"status"` // active | done | error | canceled
	Error       string `json:"error,omitempty"`
}

// transfers tracks in-flight transfers for cancel (258).
var transfers = struct {
	sync.Mutex
	conns map[string]net.Conn
}{conns: map[string]net.Conn{}}

// CancelTransfer aborts an in-flight transfer by closing its connection.
// The server removes the partial upload on its side.
func (a *App) CancelTransfer(id string) {
	transfers.Lock()
	conn := transfers.conns[id]
	transfers.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func trackTransfer(id string, conn net.Conn) func() {
	transfers.Lock()
	transfers.conns[id] = conn
	transfers.Unlock()
	return func() {
		transfers.Lock()
		delete(transfers.conns, id)
		transfers.Unlock()
	}
}

// ftEmit reports transfer progress to the frontend.
func (a *App) ftEmit(p ftProgress) {
	a.cmLoad().emit("ft_progress", p)
}

// UploadFileProgress uploads data into a channel folder with progress
// events. It returns immediately ("" or an init error); completion arrives
// as ft_progress status changes.
func (a *App) UploadFileProgress(id string, channelID int64, folder, name, dataBase64 string) string {
	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "invalid file data"
	}
	f, err := a.cmLoad().request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "upload", Name: name, Folder: folder, Size: int64(len(data))},
		10*time.Second)
	if err != nil {
		return err.Error()
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return err.Error()
	}
	ep, err := a.ftTarget(init)
	if err != nil {
		return err.Error()
	}
	// recover is per-goroutine: transfer workers need their own guard (331).
	go guardCrash("ft upload", func() {
		p := ftProgress{ID: id, Direction: "upload", Name: name, Total: int64(len(data)), Status: "active"}
		err := a.ftUploadProgress(id, ep, init.Token, init.TransferID, data, &p)
		if err != nil {
			p.Status = "error"
			if errors.Is(err, errTransferCanceled) {
				p.Status = "canceled"
			}
			p.Error = err.Error()
		} else {
			p.Status = "done"
			p.Transferred = p.Total
		}
		a.ftEmit(p)
	})
	return ""
}

// errTransferCanceled marks a user-aborted transfer.
var errTransferCanceled = errors.New("transfer canceled")

// ftUploadProgress streams data with per-chunk progress callbacks.
func (a *App) ftUploadProgress(id string, ep ftEndpoint, token, transferID string, data []byte, p *ftProgress) error {
	conn, err := ftDial(ep)
	if err != nil {
		return err
	}
	defer conn.Close()
	untrack := trackTransfer(id, conn)
	defer untrack()

	if err := ftWriteJSON(conn, ftInit, map[string]string{"token": token, "transfer_id": transferID}); err != nil {
		return err
	}
	const chunk = 32 * 1024
	start := time.Now()
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		if err := netproto.WriteFrame(conn, &netproto.Frame{Type: ftChunk, Payload: data[off:end]}); err != nil {
			if isClosedConn(err) {
				return errTransferCanceled
			}
			return err
		}
		p.Transferred = int64(end)
		if d := time.Since(start).Seconds(); d > 0 {
			p.BytesPerSec = int64(float64(end) / d)
		}
		a.ftEmit(*p)
	}
	sum := sha256.Sum256(data)
	if err := ftWriteJSON(conn, ftDigest, map[string]string{"sha256": hex.EncodeToString(sum[:])}); err != nil {
		return err
	}
	return ftReadStatus(conn)
}

// isClosedConn reports whether the error came from our own cancel close.
func isClosedConn(err error) bool {
	var nerr *net.OpError
	return errors.As(err, &nerr) && nerr.Err != nil &&
		(errors.Is(nerr.Err, net.ErrClosed) || nerr.Err.Error() == "use of closed network connection")
}

// PickSavePath opens the native save dialog and returns the chosen path
// ("" = cancelled) for downloads.
func (a *App) PickSavePath(defaultName string) string {
	path, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		DefaultFilename: defaultName,
	})
	if err != nil {
		return ""
	}
	return path
}

// DownloadFileProgress downloads a channel file to destPath with progress
// events.
func (a *App) DownloadFileProgress(id string, channelID int64, folder, name, destPath string) string {
	f, err := a.cmLoad().request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "download", Folder: folder, Name: name},
		10*time.Second)
	if err != nil {
		return err.Error()
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return err.Error()
	}
	ep, err := a.ftTarget(init)
	if err != nil {
		return err.Error()
	}
	go guardCrash("ft download", func() {
		p := ftProgress{ID: id, Direction: "download", Name: name, Status: "active"}
		err := a.ftDownloadProgress(id, ep, init.Token, init.TransferID, destPath, &p)
		if err != nil {
			p.Status = "error"
			if errors.Is(err, errTransferCanceled) {
				p.Status = "canceled"
			}
			p.Error = err.Error()
		} else {
			p.Status = "done"
			p.Transferred = p.Total
		}
		a.ftEmit(p)
	})
	return ""
}

// ftDownloadProgress streams a download into destPath with progress.
func (a *App) ftDownloadProgress(id string, ep ftEndpoint, token, transferID, destPath string, p *ftProgress) error {
	conn, err := ftDial(ep)
	if err != nil {
		return err
	}
	defer conn.Close()
	untrack := trackTransfer(id, conn)
	defer untrack()

	if err := ftWriteJSON(conn, ftInit, map[string]string{"token": token, "transfer_id": transferID}); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = out.Close()
		return err
	}

	h := sha256.New()
	start := time.Now()
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			_ = out.Close()
			if isClosedConn(err) {
				return errTransferCanceled
			}
			return err
		}
		switch f.Type {
		case ftChunk:
			if _, err := out.Write(f.Payload); err != nil {
				return fail(err)
			}
			h.Write(f.Payload)
			p.Transferred += int64(len(f.Payload))
			if p.Total == 0 {
				p.Total = -1 // unknown until the digest frame
			}
			if d := time.Since(start).Seconds(); d > 0 {
				p.BytesPerSec = int64(float64(p.Transferred) / d)
			}
			a.ftEmit(*p)
		case ftDigest:
			_ = out.Close()
			var d struct {
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal(f.Payload, &d); err != nil {
				return err
			}
			if d.SHA256 != hex.EncodeToString(h.Sum(nil)) {
				return errors.New("file digest mismatch")
			}
			return ftReadStatus(conn)
		default:
			return fail(fmt.Errorf("unexpected frame type %d", f.Type))
		}
	}
}

// VerifyFile re-downloads a file and compares its SHA-256 against the
// expected value from the listing (280). It returns true when they match.
func (a *App) VerifyFile(channelID int64, folder, name, expectedSHA string) (bool, error) {
	f, err := a.cmLoad().request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "download", Folder: folder, Name: name},
		10*time.Second)
	if err != nil {
		return false, err
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return false, err
	}
	ep, err := a.ftTarget(init)
	if err != nil {
		return false, err
	}
	data, err := ftDownload(ep, init.Token, init.TransferID)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == expectedSHA, nil
}
