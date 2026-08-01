// chat.go implements the wave-5b client chat API: history, edit/delete,
// pins, reactions, typing, receipts, emoji, attachments (file-transfer
// upload/download), search and chat export.
//
// Everything the server stores is ciphertext (91-135). This file is the
// boundary where it stops being ciphertext: history, pins and search decrypt
// here, in Go, and the webview only ever receives plaintext bodies — never a
// sealed body and never a key id.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/netproto"
	"voicx/internal/tlscert"
)

// ---------------------------------------------------------------------------
// Chat history / edit / delete / pins / reactions / typing / receipts
// ---------------------------------------------------------------------------

// ChatHistory fetches a page of channel/global history (beforeID 0 = latest)
// and decrypts it. The page carries the generations it references, so no
// nested key round trip is needed (110).
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
	m := a.cmLoad()
	refused := installPageKeys(m, channelID, resp.Keys, resp.Refused)
	for i := range resp.Messages {
		openChatEntry(m, channelID, &resp.Messages[i], refused)
	}
	resp.Keys = nil // sealed key material has no business in the webview
	return resp, nil
}

// installPageKeys unseals the generations bundled with a history/pins page,
// records the permanently withheld ones, and returns the refusal set for this
// page. The keys are ARCHIVAL: they must never advance the send generation.
func installPageKeys(m *connManager, scope int64, keys []netproto.ChannelKey, refusedIDs []uint32) map[uint32]bool {
	m.installKeys(scope, keys)
	m.scopeKeys.markRefused(scope, refusedIDs)
	refused := make(map[uint32]bool, len(refusedIDs))
	for _, g := range refusedIDs {
		refused[g] = true
	}
	return refused
}

// openChatEntry decrypts one stored entry IN PLACE and always strips BodyEnc
// and KeyID, so no code path can hand the webview ciphertext or a key id.
// EncVerified is the provenance flag the renderer draws its shield from: it is
// set only when this client actually opened the body itself.
func openChatEntry(m *connManager, scope int64, e *netproto.ChatHistoryEntry, refused map[uint32]bool) {
	defer func() { e.BodyEnc, e.KeyID = "", 0 }()
	e.EncVerified = false
	if e.Deleted {
		e.Body = ""
		return
	}
	if e.BodyEnc == "" && e.Body != "" {
		// The downgrade hole closed from this side: no server config makes
		// this client render a plaintext history body (91-135).
		e.Body = refusedPlainText
		return
	}
	key, ok := m.scopeKeys.get(scope, e.KeyID)
	switch {
	case refused[e.KeyID]:
		e.Body = refusedKeyText
	case !ok:
		e.Body = missingKeyText
	default:
		plain, err := openScope(e.BodyEnc, key)
		if err != nil {
			e.Body = missingKeyText
			return
		}
		e.Body, e.EncVerified = plain, true
	}
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

// ChatPins lists a channel's pinned messages, decrypted exactly like history.
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
	m := a.cmLoad()
	refused := installPageKeys(m, channelID, resp.Keys, resp.Refused)
	for i := range resp.Pins {
		if resp.Pins[i].Message != nil {
			openChatEntry(m, channelID, resp.Pins[i].Message, refused)
		}
	}
	resp.Keys = nil
	return resp, nil
}

// ---------------------------------------------------------------------------
// Search (110)
// ---------------------------------------------------------------------------

// chatSearchPage is the store's own page cap. Paging in Go rather than in the
// webview lets a search use it instead of the UI's 50, cutting round trips 4x.
const chatSearchPage = 200

// chatSearchDefaultMax mirrors the server's chat_search_max_messages default.
const chatSearchDefaultMax = 2000

// ChatSearchResult is one client-side history search. Undecryptable counts
// messages under a generation this client could not obtain, so a partial
// search never looks complete.
type ChatSearchResult struct {
	Messages      []netproto.ChatHistoryEntry `json:"messages"`
	Scanned       int                         `json:"scanned"`
	Undecryptable int                         `json:"undecryptable"`
}

// ChatSearch pages the scope's history backwards, decrypting each page with
// the keys the server bundles alongside it, and returns matching messages
// newest-first. Search runs entirely client-side: the server stores only
// ciphertext and cannot match on content (110).
func (a *App) ChatSearch(channelID int64, query string, maxMessages int) (ChatSearchResult, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if maxMessages <= 0 {
		maxMessages = chatSearchDefaultMax
	}
	var out []netproto.ChatHistoryEntry
	undecryptable, scanned := 0, 0
	before := int64(0)
	for scanned < maxMessages {
		resp, err := a.ChatHistory(channelID, before, chatSearchPage)
		if err != nil {
			if scanned == 0 {
				return ChatSearchResult{}, err // nothing to show; surface it
			}
			break // partial result is better than losing the pages we have
		}
		if len(resp.Messages) == 0 {
			break
		}
		for _, m := range resp.Messages {
			scanned++
			if m.Deleted {
				continue
			}
			if !m.EncVerified {
				undecryptable++
				continue
			}
			if q != "" && strings.Contains(strings.ToLower(m.Body), q) {
				out = append(out, m)
			}
		}
		before = resp.Messages[len(resp.Messages)-1].ID // pages are newest-first
		if len(resp.Messages) < chatSearchPage {
			break
		}
		a.emitPlain("chatsearch:progress", scanned)
	}
	return ChatSearchResult{Messages: out, Scanned: scanned, Undecryptable: undecryptable}, nil
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

// ftEndpoint is a resolved data-port target: where to dial, whether the port
// speaks TLS, and the certificate to pin on it.
type ftEndpoint struct {
	addr        string
	fingerprint string
	tls         bool
}

// ftTarget resolves the data-port address and the certificate to pin on it.
// The data port presents the SAME certificate as the control channel, so the
// pin the client already trusts is the pin here; a server that states a
// different one is redirecting the transfer to a host we never authenticated,
// and the transfer is refused rather than downgraded (91-135). A server that
// reports a plaintext data port is honoured only when its control channel is
// plaintext too, so a TLS server can never talk a client down to a clear port.
func (a *App) ftTarget(init netproto.FileTransferInitResponse) (ftEndpoint, error) {
	addr, err := a.ftAddr(init.Port)
	if err != nil {
		return ftEndpoint{}, err
	}
	_, control, _ := a.cmLoad().securitySnapshot()
	if !init.TLS {
		if control != "" {
			return ftEndpoint{}, errors.New("server offered a plaintext file transfer port — refusing the transfer")
		}
		return ftEndpoint{addr: addr}, nil
	}
	fingerprint := init.TLSFingerprint
	if fingerprint == "" {
		fingerprint = control // server did not state it; same certificate anyway
	}
	if control != "" && !strings.EqualFold(fingerprint, control) {
		return ftEndpoint{}, errors.New("file transfer certificate does not match the server — refusing the transfer")
	}
	return ftEndpoint{addr: addr, fingerprint: fingerprint, tls: true}, nil
}

// ftPutBytes runs the init handshake and streams data to the data port.
func (a *App) ftPutBytes(channelID int64, name string, data []byte) error {
	f, err := a.cmLoad().request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "upload", Name: name, Size: int64(len(data))},
		10*time.Second)
	if err != nil {
		return err
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return err
	}
	ep, err := a.ftTarget(init)
	if err != nil {
		return err
	}
	return ftUpload(ep, init.Token, init.TransferID, data)
}

// ftGetBytes runs the init handshake and reads a file off the data port.
func (a *App) ftGetBytes(channelID int64, name string) ([]byte, error) {
	f, err := a.cmLoad().request(netproto.MsgFileTransferInit, netproto.MsgFileTransferInitResponse,
		netproto.FileTransferInit{ChannelID: channelID, Direction: "download", Name: name},
		10*time.Second)
	if err != nil {
		return nil, err
	}
	var init netproto.FileTransferInitResponse
	if err := decodeJSON(f, &init); err != nil {
		return nil, err
	}
	ep, err := a.ftTarget(init)
	if err != nil {
		return nil, err
	}
	return ftDownload(ep, init.Token, init.TransferID)
}

// UploadFile uploads data as a file into a channel and returns "" or the
// error. It is the PLAIN path, used by the file browser; chat attachments go
// through UploadChatAttachment instead.
func (a *App) UploadFile(channelID int64, name, dataBase64 string) string {
	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "invalid file data"
	}
	if err := a.ftPutBytes(channelID, name, data); err != nil {
		return err.Error()
	}
	return ""
}

// DownloadFile downloads a channel file, returned base64-encoded.
func (a *App) DownloadFile(channelID int64, name string) (string, error) {
	data, err := a.ftGetBytes(channelID, name)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ---------------------------------------------------------------------------
// Encrypted chat attachments (91-135)
// ---------------------------------------------------------------------------

// attachmentStorageName derives the upload name from the CIPHERTEXT. Content
// addressing is what makes per-file keys viable: files has UNIQUE (channel_id,
// name) and same-name uploads rotate through .v1..v3, so with a random key per
// file a second "image.png" would leave every older message holding a key that
// fails Poly1305 against the new blob. Random keys make two uploads of the
// same bytes distinct, so a chat attachment never collides and never rotates.
func attachmentStorageName(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])[:32] + ".vcx"
}

// safeDisplayName strips the two characters that would break the token
// grammar. The name is only ever shown, never used to address storage.
func safeDisplayName(name string) string {
	return strings.NewReplacer("]", "_", "#", "_").Replace(name)
}

// parseFileRef splits a [file:<capture>] body token. It is total: zero or one
// separators means a legacy plain reference, which keeps pre-encryption
// messages rendering. chat-ui.js mirrors this exactly.
func parseFileRef(capture string) (storage, keyB64, name string) {
	i := strings.Index(capture, "#")
	if i < 0 {
		return capture, "", capture // legacy [file:photo.png]
	}
	j := strings.Index(capture[i+1:], "#")
	if j < 0 {
		return capture, "", capture // malformed -> treat as plain
	}
	j += i + 1
	return capture[:i], capture[i+1 : j], capture[j+1:]
}

// UploadChatAttachment seals data with a fresh random 32-byte key, uploads
// the ciphertext under a content-derived name, and returns the body token
// "[file:<storage>#<keyB64>#<display>]" for embedding in the chat message.
// The key only ever exists inside the (encrypted) message body, so the file
// gets exactly the protection the message text gets.
func (a *App) UploadChatAttachment(channelID int64, name, dataBase64 string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "", errors.New("invalid file data")
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", err
	}
	blob, err := sealFile(data, key)
	if err != nil {
		return "", err
	}
	storage := attachmentStorageName(blob)
	if err := a.ftPutBytes(channelID, storage, blob); err != nil {
		return "", err
	}
	return "[file:" + storage + "#" + base64.StdEncoding.EncodeToString(key[:]) +
		"#" + safeDisplayName(name) + "]", nil
}

// DownloadChatAttachment fetches <storage> and unseals it with keyB64,
// returning base64 plaintext. An empty keyB64 downloads a plain file, which is
// what a legacy [file:photo.png] reference resolves to.
func (a *App) DownloadChatAttachment(channelID int64, storage, keyB64 string) (string, error) {
	// A caller that hands over the raw capture instead of its parts is parsed
	// here too, so a token can never be mistaken for a file name.
	if keyB64 == "" && strings.Contains(storage, "#") {
		storage, keyB64, _ = parseFileRef(storage)
	}
	blob, err := a.ftGetBytes(channelID, storage)
	if err != nil {
		return "", err
	}
	if keyB64 == "" {
		return base64.StdEncoding.EncodeToString(blob), nil
	}
	raw, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(raw) != 32 {
		return "", errors.New("invalid attachment key")
	}
	var key [32]byte
	copy(key[:], raw)
	plain, err := openFile(blob, key)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(plain), nil
}

// ftDial dials the file-transfer port. The data port is TLS with the same
// certificate as the control channel, pinned here: file bytes must not cross
// the network in the clear just because they use a different port (91-135).
// ftTarget only clears ep.tls for an all-plaintext dev server.
func ftDial(ep ftEndpoint) (net.Conn, error) {
	if !ep.tls {
		return net.DialTimeout("tcp", ep.addr, 15*time.Second)
	}
	return tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", ep.addr, &tls.Config{
		InsecureSkipVerify:    true, // pinned below — same TOFU model as client/tofu.go
		MinVersion:            tls.VersionTLS12,
		VerifyPeerCertificate: pinFingerprint(ep.fingerprint),
	})
}

// pinFingerprint builds the certificate check for the data port. An empty pin
// means the control channel itself is plaintext (dev server), so there is
// nothing trustworthy to compare against and the connection is unverified.
func pinFingerprint(want string) func([][]byte, [][]*x509.Certificate) error {
	if want == "" {
		return nil
	}
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("file transfer server presented no certificate")
		}
		got := tlscert.FingerprintDER(rawCerts[0])
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("file transfer certificate mismatch (%s, expected %s)", got, want)
		}
		return nil
	}
}

// ftUpload streams data to the file-transfer port with digest verification.
func ftUpload(ep ftEndpoint, token, transferID string, data []byte) error {
	conn, err := ftDial(ep)
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
func ftDownload(ep ftEndpoint, token, transferID string) ([]byte, error) {
	conn, err := ftDial(ep)
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

// Encrypted export container: magic || salt || nonce || secretbox. The key is
// derived from the passphrase with argon2id, so the file is worth no more than
// the passphrase — but a plaintext export of an encrypted chat is worth far
// less than that (125).
var exportMagic = []byte("VOICXCHAT1")

const (
	exportSaltLen = 16
	// argon2id parameters: one pass over 64 MiB, 4 lanes. Interactive cost on
	// a desktop, far past a GPU-friendly KDF.
	exportArgonTime    = 1
	exportArgonMemory  = 64 * 1024
	exportArgonThreads = 4
)

// sealExport builds the encrypted export container.
func sealExport(contents, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("a passphrase is required for an encrypted export")
	}
	salt := make([]byte, exportSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	var key [32]byte
	copy(key[:], argon2.IDKey([]byte(passphrase), salt, exportArgonTime, exportArgonMemory, exportArgonThreads, 32))

	out := make([]byte, 0, len(exportMagic)+exportSaltLen+24+len(contents)+secretbox.Overhead)
	out = append(out, exportMagic...)
	out = append(out, salt...)
	out = append(out, nonce[:]...)
	return secretbox.Seal(out, []byte(contents), &nonce, &key), nil
}

// openExport reverses sealExport (the import half and the round-trip test).
func openExport(blob []byte, passphrase string) (string, error) {
	head := len(exportMagic) + exportSaltLen + 24
	if len(blob) < head || !bytes.Equal(blob[:len(exportMagic)], exportMagic) {
		return "", errors.New("not a voicx encrypted chat export")
	}
	salt := blob[len(exportMagic) : len(exportMagic)+exportSaltLen]
	var nonce [24]byte
	copy(nonce[:], blob[len(exportMagic)+exportSaltLen:head])
	var key [32]byte
	copy(key[:], argon2.IDKey([]byte(passphrase), salt, exportArgonTime, exportArgonMemory, exportArgonThreads, 32))

	plain, ok := secretbox.Open(nil, blob[head:], &nonce, &key)
	if !ok {
		return "", errors.New("wrong passphrase or corrupt export")
	}
	return string(plain), nil
}

// ExportChatEncrypted saves the transcript passphrase-encrypted as
// .voicxchat, so an export of an encrypted chat does not become the plaintext
// copy the whole design exists to prevent. Returns "" on success or cancel.
func (a *App) ExportChatEncrypted(defaultName, contents, passphrase string) (string, error) {
	blob, err := sealExport(contents, passphrase)
	if err != nil {
		return "", err
	}
	path, err := wailsRuntime.SaveFileDialog(a.ctx, wailsRuntime.SaveDialogOptions{
		DefaultFilename: defaultName,
		Filters: []wailsRuntime.FileFilter{
			{DisplayName: "Encrypted chat export", Pattern: "*.voicxchat"},
		},
	})
	if err != nil || path == "" {
		return "", nil // cancelled
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
