// chat.go implements the wave-5b client chat API: history, edit/delete,
// pins, reactions, typing, receipts, emoji, attachments (file-transfer
// upload/download), and chat export.
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
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
)

// ---------------------------------------------------------------------------
// Chat history / edit / delete / pins / reactions / typing / receipts
// ---------------------------------------------------------------------------

// ChatHistory fetches a page of channel/global history (beforeID 0 = latest).
func (a *App) ChatHistory(channelID, beforeID int64, limit int) (netproto.ChatHistoryResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgChatHistory, netproto.MsgChatHistoryResponse,
		netproto.ChatHistory{ChannelID: channelID, BeforeID: beforeID, Limit: limit}, 10*time.Second)
	if err != nil {
		return netproto.ChatHistoryResponse{}, err
	}
	var resp netproto.ChatHistoryResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.ChatHistoryResponse{}, err
	}
	return resp, nil
}

// ChatEditMessage edits one of the caller's own messages. The new body is
// sealed with the channel's current scope key (the server decrypts it for
// storage and re-seals for the broadcast).
func (a *App) ChatEditMessage(channelID, messageID int64, newText string) string {
	keyID, key, ok := a.cmLoad().scopeKeys.current(channelID)
	if !ok {
		return "no chat key for this channel yet"
	}
	blob, err := sealScope(newText, key)
	if err != nil {
		return err.Error()
	}
	if err := a.cmLoad().write(netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: messageID, NewText: blob, Enc: true, KeyID: keyID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// ChatDeleteMessage deletes (tombstones) a message.
func (a *App) ChatDeleteMessage(messageID int64) string {
	if err := a.cmLoad().write(netproto.MsgChatDelete, netproto.ChatDelete{MessageID: messageID}); err != nil {
		return err.Error()
	}
	return ""
}

// ChatPinMessage pins or unpins a message.
func (a *App) ChatPinMessage(channelID, messageID int64, pinned bool) string {
	if err := a.cmLoad().write(netproto.MsgChatPin, netproto.ChatPin{
		ChannelID: channelID, MessageID: messageID, Pinned: pinned,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// ChatPins lists a channel's pinned messages.
func (a *App) ChatPins(channelID int64) (netproto.ChatPinsResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgChatPins, netproto.MsgChatPinsResponse,
		netproto.ChatPins{ChannelID: channelID}, 10*time.Second)
	if err != nil {
		return netproto.ChatPinsResponse{}, err
	}
	var resp netproto.ChatPinsResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.ChatPinsResponse{}, err
	}
	return resp, nil
}

// ChatReact toggles a reaction on a message.
func (a *App) ChatReact(messageID int64, emoji string) string {
	if err := a.cmLoad().write(netproto.MsgChatReact, netproto.ChatReact{
		MessageID: messageID, Emoji: emoji,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// SendTyping relays a typing indicator (channel scope or DM).
func (a *App) SendTyping(channelID int64, toUniqueID string) string {
	if err := a.cmLoad().write(netproto.MsgTyping, netproto.Typing{
		ChannelID: channelID, ToUniqueID: toUniqueID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// SendChatDelivered acks a received DM (delivery receipt to the sender).
func (a *App) SendChatDelivered(toUniqueID, clientMsgID string) string {
	if err := a.cmLoad().write(netproto.MsgChatDelivered, netproto.ChatDelivered{
		ToUniqueID: toUniqueID, ClientMsgID: clientMsgID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// SendChatRead acks a read DM (read receipt to the sender).
func (a *App) SendChatRead(toUniqueID, clientMsgID string) string {
	if err := a.cmLoad().write(netproto.MsgChatRead, netproto.ChatRead{
		ToUniqueID: toUniqueID, ClientMsgID: clientMsgID,
	}); err != nil {
		return err.Error()
	}
	return ""
}

// ---------------------------------------------------------------------------
// Custom emoji
// ---------------------------------------------------------------------------

// EmojiList lists the server's custom emojis.
func (a *App) EmojiList() (netproto.EmojiListResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgEmojiList, netproto.MsgEmojiListResponse,
		netproto.EmojiList{}, 10*time.Second)
	if err != nil {
		return netproto.EmojiListResponse{}, err
	}
	var resp netproto.EmojiListResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.EmojiListResponse{}, err
	}
	return resp, nil
}

// EmojiGet fetches one custom emoji image (cached by the frontend).
func (a *App) EmojiGet(name string) (netproto.EmojiData, error) {
	f, err := a.cmLoad().request(netproto.MsgEmojiGet, netproto.MsgEmojiData,
		netproto.EmojiGet{Name: name}, 10*time.Second)
	if err != nil {
		return netproto.EmojiData{}, err
	}
	var resp netproto.EmojiData
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.EmojiData{}, err
	}
	if resp.DataBase64 == "" {
		return netproto.EmojiData{}, errors.New("emoji not found")
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Attachments: file-transfer upload/download (98/99/100)
// ---------------------------------------------------------------------------

// ftFrame types of the file-transfer port protocol (internal/filetransfer).
const (
	ftInit   uint16 = 1
	ftChunk  uint16 = 2
	ftDigest uint16 = 3
	ftStatus uint16 = 4
)

// ftAddr returns the file-transfer address for the current connection
// (control-channel host + the port from the init response).
func (a *App) ftAddr(port int) (string, error) {
	a.cmLoad().mu.Lock()
	conn := a.cmLoad().conn
	a.cmLoad().mu.Unlock()
	if conn == nil {
		return "", errors.New("not connected")
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, fmt.Sprint(port)), nil
}

// UploadFile uploads data as a file into a channel and returns "" or the
// error. Chat messages reference it as [file:<name>].
func (a *App) UploadFile(channelID int64, name, dataBase64 string) string {
	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "invalid file data"
	}
	f, err := a.cmLoad().request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "upload", Name: name, Size: int64(len(data))},
		10*time.Second)
	if err != nil {
		return err.Error()
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return err.Error()
	}
	addr, err := a.ftAddr(init.Port)
	if err != nil {
		return err.Error()
	}
	if err := ftUpload(addr, init.Token, init.TransferID, data); err != nil {
		return err.Error()
	}
	return ""
}

// DownloadFile downloads a channel file, returned base64-encoded.
func (a *App) DownloadFile(channelID int64, name string) (string, error) {
	f, err := a.cmLoad().request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "download", Name: name},
		10*time.Second)
	if err != nil {
		return "", err
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return "", err
	}
	addr, err := a.ftAddr(init.Port)
	if err != nil {
		return "", err
	}
	data, err := ftDownload(addr, init.Token, init.TransferID)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ftDial dials the file-transfer port (plaintext this wave; it's a separate
// port from the TLS control channel).
func ftDial(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 15*time.Second)
}

// ftUpload streams data to the file-transfer port with digest verification.
func ftUpload(addr, token, transferID string, data []byte) error {
	conn, err := ftDial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := ftWriteJSON(conn, ftInit, map[string]string{"token": token, "transfer_id": transferID}); err != nil {
		return err
	}
	const chunk = 32 * 1024
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		if err := netproto.WriteFrame(conn, &netproto.Frame{Type: ftChunk, Payload: data[off:end]}); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(data)
	if err := ftWriteJSON(conn, ftDigest, map[string]string{"sha256": hex.EncodeToString(sum[:])}); err != nil {
		return err
	}
	return ftReadStatus(conn)
}

// ftDownload reads a file from the file-transfer port with digest
// verification.
func ftDownload(addr, token, transferID string) ([]byte, error) {
	conn, err := ftDial(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := ftWriteJSON(conn, ftInit, map[string]string{"token": token, "transfer_id": transferID}); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	h := sha256.New()
	var got []byte
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case ftChunk:
			got = append(got, f.Payload...)
			h.Write(f.Payload)
		case ftDigest:
			var d struct {
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal(f.Payload, &d); err != nil {
				return nil, err
			}
			if d.SHA256 != hex.EncodeToString(h.Sum(nil)) {
				return nil, errors.New("file digest mismatch")
			}
			if err := ftReadStatus(conn); err != nil {
				return nil, err
			}
			return got, nil
		default:
			return nil, fmt.Errorf("unexpected frame type %d", f.Type)
		}
	}
}

// ftWriteJSON writes a JSON payload frame on the file-transfer port.
func ftWriteJSON(conn net.Conn, frameType uint16, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return netproto.WriteFrame(conn, &netproto.Frame{Type: frameType, Payload: payload})
}

// ftReadStatus reads the server's final status frame.
func ftReadStatus(conn net.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	f, err := netproto.ReadFrame(conn)
	if err != nil {
		return err
	}
	if f.Type != ftStatus {
		return fmt.Errorf("expected status frame, got %d", f.Type)
	}
	var st struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(f.Payload, &st); err != nil {
		return err
	}
	if !st.OK {
		return errors.New("transfer failed: " + st.Error)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Export (125)
// ---------------------------------------------------------------------------

// ExportChat saves text to a user-chosen file via the native save dialog.
// Returns "" on success or when the user cancels, or the error.
func (a *App) ExportChat(defaultName, contents string) string {
	path, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		DefaultFilename: defaultName,
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "Text / HTML", Pattern: "*.txt;*.html"},
		},
	})
	if err != nil || path == "" {
		return "" // cancelled
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		return err.Error()
	}
	return ""
}
