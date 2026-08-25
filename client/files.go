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
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
)

// --- file browser -------------------------------------------------------------

// FileList returns the channel's file listing for one folder (plus quota).
func (a *App) FileList(channelID int64, folder string) (netproto.FileListResponse, error) {
	cm, err := a.requireCM()
	if err != nil {
		return netproto.FileListResponse{}, err
	}
	f, err := cm.request(netproto.MsgFileList, netproto.MsgFileListResponse,
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
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgFileDelete, netproto.FileDelete{
		ChannelID: channelID, Folder: folder, Name: name,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// FileRename renames or moves a channel file. newChannelID 0 keeps the file
// in its channel; anything else is a cross-channel move (262), which the
// server checks against both channels.
func (a *App) FileRename(channelID int64, folder, name, newFolder, newName string, newChannelID int64) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgFileRename, netproto.FileRename{
		ChannelID: channelID, Folder: folder, Name: name,
		NewFolder: newFolder, NewName: newName, NewChannelID: newChannelID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// FileVersions lists a file's rotated old versions (264).
func (a *App) FileVersions(channelID int64, folder, name string) (netproto.FileVersionsResponse, error) {
	cm, err := a.requireCM()
	if err != nil {
		return netproto.FileVersionsResponse{}, err
	}
	f, err := cm.request(netproto.MsgFileVersions, netproto.MsgFileVersionsResponse,
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
	cm, err := a.requireCM()
	if err != nil {
		return netproto.FileLinkResponse{}, err
	}
	f, err := cm.request(netproto.MsgFileLink, netproto.MsgFileLinkResponse,
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
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgServerIconSet, netproto.ServerIconSet{DataBase64: dataBase64}); err != nil {
		return err.Error()
	}
	return ""
}

// ServerIconGet fetches the server icon (empty = none).
func (a *App) ServerIconGet() (netproto.ServerIconData, error) {
	cm, err := a.requireCM()
	if err != nil {
		return netproto.ServerIconData{}, err
	}
	f, err := cm.request(netproto.MsgServerIconGet, netproto.MsgServerIconData,
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

// ServerBannerSet uploads the server banner (admin only, 270).
func (a *App) ServerBannerSet(dataBase64 string) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgServerBannerSet, netproto.ServerBannerSet{DataBase64: dataBase64}); err != nil {
		return err.Error()
	}
	return ""
}

// ServerBannerGet fetches the server banner (empty = none).
func (a *App) ServerBannerGet() (netproto.ServerBannerData, error) {
	cm, err := a.requireCM()
	if err != nil {
		return netproto.ServerBannerData{}, err
	}
	f, err := cm.request(netproto.MsgServerBannerGet, netproto.MsgServerBannerDat,
		netproto.ServerBannerGet{}, 5*time.Second)
	if err != nil {
		return netproto.ServerBannerData{}, err
	}
	var data netproto.ServerBannerData
	if err := decodeJSON(f, &data); err != nil {
		return netproto.ServerBannerData{}, err
	}
	return data, nil
}

// ChannelIconGet fetches a channel's icon (271; empty = none set).
func (a *App) ChannelIconGet(channelID int64) (netproto.ChannelIconData, error) {
	cm, err := a.requireCM()
	if err != nil {
		return netproto.ChannelIconData{}, err
	}
	f, err := cm.request(netproto.MsgChannelIconGet, netproto.MsgChannelIconData,
		netproto.ChannelIconGet{ChannelID: channelID}, 5*time.Second)
	if err != nil {
		return netproto.ChannelIconData{}, err
	}
	var data netproto.ChannelIconData
	if err := decodeJSON(f, &data); err != nil {
		return netproto.ChannelIconData{}, err
	}
	return data, nil
}

// EmojiDelete removes a custom server emoji (272, b_emoji_manage).
func (a *App) EmojiDelete(name string) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgEmojiDelete, netproto.EmojiDelete{Name: name}); err != nil {
		return err.Error()
	}
	return ""
}

// EmojiRename renames a custom server emoji (272, b_emoji_manage).
func (a *App) EmojiRename(name, newName string) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgEmojiRename, netproto.EmojiRename{Name: name, NewName: newName}); err != nil {
		return err.Error()
	}
	return ""
}

// ChannelIconSet uploads a channel icon, or copies it from another channel
// (copyFromChannelID != 0, 271 icon library).
func (a *App) ChannelIconSet(channelID int64, dataBase64 string, copyFromChannelID int64) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	if err := cm.write(netproto.MsgChannelIconSet, netproto.ChannelIconSet{
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
	// Resumed is how many bytes this attempt inherited from an interrupted
	// one (259); it lets the transfer list say so instead of looking like a
	// download that started implausibly fast.
	Resumed int64 `json:"resumed,omitempty"`
}

// transferRegistry belongs to one connManager. The frontend ID is merely a
// correlation label and is intentionally not a map key: duplicate IDs within
// a tab must all be cancellable, while the same ID in another tab is isolated.
type transferRegistry struct {
	mu      sync.Mutex
	next    uint64
	entries map[uint64]transferEntry
}

type transferEntry struct {
	frontendID string
	conn       net.Conn
}

type transferProgressReporter struct {
	mu    sync.Mutex
	state map[string]transferProgressState
}

type transferProgressState struct {
	at          time.Time
	transferred int64
}

var ftProgressClock = time.Now

// CancelTransfer aborts an in-flight transfer by closing its connection.
// The server removes the partial upload on its side.
func (a *App) CancelTransfer(id string) {
	cm, err := a.requireCM()
	if err != nil {
		return
	}
	cm.cancelTransfers(id)
}

func (m *connManager) trackTransfer(frontendID string, conn net.Conn) func() {
	m.transfers.mu.Lock()
	m.transfers.next++
	token := m.transfers.next
	if m.transfers.entries == nil {
		m.transfers.entries = make(map[uint64]transferEntry)
	}
	m.transfers.entries[token] = transferEntry{frontendID: frontendID, conn: conn}
	m.transfers.mu.Unlock()
	return func() {
		m.transfers.mu.Lock()
		delete(m.transfers.entries, token)
		m.transfers.mu.Unlock()
	}
}

// trackTransferAt records a data-port connection only when the control
// connection captured by ftTarget is still installed. The lock order is
// manager then registry, matching detachLocked; this closes the otherwise
// unavoidable dial/disconnect/register race.
func (m *connManager) trackTransferAt(frontendID string, epoch uint64, conn net.Conn) (func(), bool) {
	m.mu.Lock()
	m.transfers.mu.Lock()
	if !m.acceptingTransfers || m.conn == nil || m.transferEpoch != epoch {
		m.transfers.mu.Unlock()
		m.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		return func() {}, false
	}
	m.transfers.next++
	token := m.transfers.next
	if m.transfers.entries == nil {
		m.transfers.entries = make(map[uint64]transferEntry)
	}
	m.transfers.entries[token] = transferEntry{frontendID: frontendID, conn: conn}
	m.transfers.mu.Unlock()
	m.mu.Unlock()
	return func() {
		m.transfers.mu.Lock()
		delete(m.transfers.entries, token)
		m.transfers.mu.Unlock()
	}, true
}

func (m *connManager) cancelTransfers(frontendID string) {
	m.transfers.mu.Lock()
	conns := make([]net.Conn, 0)
	for token, entry := range m.transfers.entries {
		if entry.frontendID == frontendID {
			delete(m.transfers.entries, token)
			conns = append(conns, entry.conn)
		}
	}
	m.transfers.mu.Unlock()
	closeTransfers(conns)
}

// detachTransfersLocked is the registry half of connection teardown. The
// caller holds m.mu and closes the returned sockets only after releasing it.
func (m *connManager) detachTransfersLocked() []net.Conn {
	m.transfers.mu.Lock()
	conns := make([]net.Conn, 0, len(m.transfers.entries))
	for token, entry := range m.transfers.entries {
		delete(m.transfers.entries, token)
		conns = append(conns, entry.conn)
	}
	m.transfers.mu.Unlock()
	return conns
}

func closeTransfers(conns []net.Conn) {
	for _, conn := range conns {
		if conn != nil {
			_ = conn.Close()
		}
	}
}

// ftEmit reports transfer progress through the manager that initiated it.
func (m *connManager) ftEmit(p ftProgress) {
	// The transfer worker is the sole reporter: throttling is synchronous and
	// deterministic, so it cannot leak a background goroutine or reorder a
	// terminal update behind an older progress event.
	terminal := p.Status != "active"
	key := p.ID + "\x00" + p.Direction + "\x00" + p.Name
	now := ftProgressClock()
	m.progress.mu.Lock()
	if m.progress.state == nil {
		m.progress.state = make(map[string]transferProgressState)
	}
	previous, seen := m.progress.state[key]
	step := int64(0)
	if p.Total > 0 {
		step = max(int64(1), (p.Total+99)/100) // one percent, rounded up
	}
	emit := terminal || !seen || now.Sub(previous.at) >= 100*time.Millisecond ||
		(step > 0 && p.Transferred-previous.transferred >= step)
	if emit {
		if terminal {
			delete(m.progress.state, key)
		} else {
			m.progress.state[key] = transferProgressState{at: now, transferred: p.Transferred}
		}
	}
	m.progress.mu.Unlock()
	if !emit {
		return
	}
	m.emit("ft_progress", p)
}

// UploadFileProgress uploads data into a channel folder with progress
// events. It returns immediately ("" or an init error); completion arrives
// as ft_progress status changes.
func (a *App) UploadFileProgress(id string, channelID int64, folder, name, dataBase64 string) string {
	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "invalid file data"
	}
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	f, err := cm.request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "upload", Name: name, Folder: folder, Size: int64(len(data))},
		10*time.Second)
	if err != nil {
		return err.Error()
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return err.Error()
	}
	ep, err := cm.ftTarget(init)
	if err != nil {
		return err.Error()
	}
	// recover is per-goroutine: transfer workers need their own guard (331).
	go guardCrash("ft upload", func() {
		p := ftProgress{ID: id, Direction: "upload", Name: name, Total: int64(len(data)), Status: "active"}
		err := cm.ftUploadProgress(id, ep, init.Token, init.TransferID, data, &p)
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
		cm.ftEmit(p)
	})
	return ""
}

// errTransferCanceled marks a user-aborted transfer.
var errTransferCanceled = errors.New("transfer canceled")

// ftUploadProgress streams data with per-chunk progress callbacks.
func (m *connManager) ftUploadProgress(id string, ep ftEndpoint, token, transferID string, data []byte, p *ftProgress) error {
	conn, err := transferDial(ep)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	defer clearTransferDeadlines(conn)
	untrack, tracked := m.trackTransferAt(id, ep.epoch, conn)
	if !tracked {
		return errTransferCanceled
	}
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
		if err := ftWriteFrame(conn, &netproto.Frame{Type: ftChunk, Payload: data[off:end]}); err != nil {
			if isClosedConn(err) {
				return errTransferCanceled
			}
			return err
		}
		p.Transferred = int64(end)
		if d := time.Since(start).Seconds(); d > 0 {
			p.BytesPerSec = int64(float64(end) / d)
		}
		m.ftEmit(*p)
	}
	sum := sha256.Sum256(data)
	if err := ftWriteJSON(conn, ftDigest, map[string]string{"sha256": hex.EncodeToString(sum[:])}); err != nil {
		return err
	}
	return ftReadStatus(conn)
}

// PickUploadPaths opens the native multi-file picker and returns the chosen
// paths ([] = cancelled). Uploading by path is the streaming route: the
// browser route has to read the whole file into memory and base64 it into a
// single IPC argument, which stops working a few MiB in.
func (a *App) PickUploadPaths() []string {
	paths, err := wailsRuntime.OpenMultipleFilesDialog(a.ctx, wailsRuntime.OpenDialogOptions{
		Title: "Upload files",
	})
	if err != nil || paths == nil {
		return []string{}
	}
	return paths
}

// UploadPathProgress uploads a local file into a channel folder, streaming it
// off disk in chunks instead of buffering and base64-encoding it (259). It
// returns immediately ("" or an init error); completion arrives as ft_progress
// status changes.
func (a *App) UploadPathProgress(id string, channelID int64, folder, path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "cannot read " + filepath.Base(path) + ": " + err.Error()
	}
	if st.IsDir() {
		return filepath.Base(path) + " is a folder"
	}
	name := filepath.Base(path)
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	f, err := cm.request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "upload", Name: name, Folder: folder, Size: st.Size()},
		10*time.Second)
	if err != nil {
		return err.Error()
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return err.Error()
	}
	ep, err := cm.ftTarget(init)
	if err != nil {
		return err.Error()
	}
	go guardCrash("ft upload path", func() {
		p := ftProgress{ID: id, Direction: "upload", Name: name, Total: st.Size(), Status: "active"}
		err := cm.ftUploadFile(id, ep, init.Token, init.TransferID, path, &p)
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
		cm.ftEmit(p)
	})
	return ""
}

// ftUploadFile streams a file off disk to the data port, hashing as it goes so
// nothing larger than one chunk is ever held in memory.
func (m *connManager) ftUploadFile(id string, ep ftEndpoint, token, transferID, path string, p *ftProgress) error {
	// #nosec G304 -- path is an explicit native-picker selection and is
	// intentionally opened for upload.
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	conn, err := transferDial(ep)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	defer clearTransferDeadlines(conn)
	untrack, tracked := m.trackTransferAt(id, ep.epoch, conn)
	if !tracked {
		return errTransferCanceled
	}
	defer untrack()

	if err := ftWriteJSON(conn, ftInit, map[string]string{"token": token, "transfer_id": transferID}); err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 32*1024)
	start := time.Now()
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if err := ftWriteFrame(conn, &netproto.Frame{Type: ftChunk, Payload: buf[:n]}); err != nil {
				if isClosedConn(err) {
					return errTransferCanceled
				}
				return err
			}
			h.Write(buf[:n])
			p.Transferred += int64(n)
			if d := time.Since(start).Seconds(); d > 0 {
				p.BytesPerSec = int64(float64(p.Transferred) / d)
			}
			m.ftEmit(*p)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := ftWriteJSON(conn, ftDigest, map[string]string{"sha256": hex.EncodeToString(h.Sum(nil))}); err != nil {
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

// DownloadPath returns where a download should land without asking: the
// configured download folder joined with name, or "" when the user has not
// set one, in which case the caller falls back to PickSavePath. This is what
// gives the Downloads settings page's folder an effect.
func (a *App) DownloadPath(name string) string {
	a.settingsMu.Lock()
	dir := a.settings.DownloadFolder
	a.settingsMu.Unlock()
	if dir == "" {
		return ""
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(dir, filepath.Base(name))
}

// partSuffix marks the incomplete download sitting next to its destination.
// It survives a failed or cancelled transfer on purpose: that leftover IS the
// resume state (259).
const partSuffix = ".vcxpart"

// DownloadFileProgress downloads a channel file to destPath with progress
// events. Calling it again for the same destination resumes from whatever the
// interrupted attempt already wrote (259).
func (a *App) DownloadFileProgress(id string, channelID int64, folder, name, destPath string, total int64) string {
	cm, err := a.requireCM()
	if err != nil {
		return err.Error()
	}
	f, err := cm.request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "download", Folder: folder, Name: name},
		10*time.Second)
	if err != nil {
		return err.Error()
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return err.Error()
	}
	ep, err := cm.ftTarget(init)
	if err != nil {
		return err.Error()
	}
	go guardCrash("ft download", func() {
		// total comes from the listing row: the wire never carries the size on
		// the data port, and without it a resumed transfer has no denominator.
		p := ftProgress{ID: id, Direction: "download", Name: name, Total: total, Status: "active"}
		err := cm.ftDownloadProgress(id, ep, init.Token, init.TransferID, destPath, &p)
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
		cm.ftEmit(p)
	})
	return ""
}

// resumeState opens the partial file for destPath and returns it positioned
// at the end, the byte count already on disk, and a hasher primed with those
// bytes (259). The server's digest covers the whole file, so the prefix we
// keep has to go through the same hash or a resumed transfer could never
// verify.
func resumeState(destPath string) (*os.File, int64, hash.Hash, error) {
	partPath := destPath + partSuffix
	h := sha256.New()
	// #nosec G304 -- destPath is a user-selected or configured download target;
	// its sibling partial file is intentional.
	f, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, 0, nil, err
	}
	have, err := io.Copy(h, f)
	if err != nil {
		_ = f.Close()
		return nil, 0, nil, err
	}
	return f, have, h, nil
}

// ftDownloadProgress streams a download into destPath with progress. It
// writes to destPath+partSuffix and only renames on a verified digest, so an
// interrupted attempt leaves a resumable remnant instead of a truncated file
// that looks complete.
func (m *connManager) ftDownloadProgress(id string, ep ftEndpoint, token, transferID, destPath string, p *ftProgress) error {
	partPath := destPath + partSuffix
	out, have, h, err := resumeState(destPath)
	if err != nil {
		return err
	}
	p.Transferred = have
	p.Resumed = have

	conn, err := transferDial(ep)
	if err != nil {
		_ = out.Close()
		return err
	}
	defer func() { _ = conn.Close() }()
	defer clearTransferDeadlines(conn)
	untrack, tracked := m.trackTransferAt(id, ep.epoch, conn)
	if !tracked {
		_ = out.Close()
		return errTransferCanceled
	}
	defer untrack()

	start := time.Now()
	_, err = ftDownloadStream(
		conn,
		map[string]any{"token": token, "transfer_id": transferID, "offset": have},
		out,
		0,
		have,
		h,
		func(transferred int64) error {
			p.Transferred = transferred
			if p.Total == 0 {
				p.Total = -1 // unknown until the digest frame
			}
			if d := time.Since(start).Seconds(); d > 0 {
				p.BytesPerSec = int64(float64(transferred-have) / d)
			}
			m.ftEmit(*p)
			return nil
		},
	)
	closeErr := out.Close()
	if err != nil {
		if errors.Is(err, errFileDigestMismatch) {
			// A stale remnant from a different version of the file would
			// poison every retry, so drop it and let the next attempt start
			// clean.
			_ = os.Remove(partPath)
		}
		if isClosedConn(err) {
			return errTransferCanceled
		}
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return finalizeDownloadedPart(partPath, destPath)
}

// finalizeDownloadedPart replaces the selected destination only after the
// partial file has been closed and its digest verified. Windows cannot rename
// over an existing file, so remove it explicitly and surface every failure
// except an already-absent destination.
func finalizeDownloadedPart(partPath, destPath string) error {
	if err := os.Remove(destPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace existing download: %w", err)
	}
	if err := os.Rename(partPath, destPath); err != nil {
		return fmt.Errorf("finalize download: %w", err)
	}
	return nil
}

// VerifyFile re-downloads a file and compares its SHA-256 against the
// expected value from the listing (280). It returns true when they match.
func (a *App) VerifyFile(channelID int64, folder, name, expectedSHA string) (bool, error) {
	cm, err := a.requireCM()
	if err != nil {
		return false, err
	}
	f, err := cm.request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "download", Folder: folder, Name: name},
		10*time.Second)
	if err != nil {
		return false, err
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return false, err
	}
	ep, err := cm.ftTarget(init)
	if err != nil {
		return false, err
	}
	conn, err := transferDial(ep)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	untrack, ok := cm.trackTransferAt(init.TransferID, ep.epoch, conn)
	if !ok {
		return false, errTransferCanceled
	}
	defer untrack()
	h := sha256.New()
	if _, err := ftDownloadTo(conn, init.Token, init.TransferID, io.MultiWriter(io.Discard, h), 0); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == expectedSHA, nil
}
