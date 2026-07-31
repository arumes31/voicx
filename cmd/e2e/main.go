// e2e is a live-server end-to-end checklist runner for voicx. It connects to
// a running server and exercises the health, UDP, auth, channel, chat,
// permission, file-transfer, and ServerQuery paths, printing PASS/FAIL per
// check and exiting non-zero unless everything passes.
//
// Usage:
//
//	e2e -addr 127.0.0.1:10011 -alice-uid <uid> -alice-pass <pw> \
//	    -bob-uid <uid> -bob-pass <pw> -admin-uid <uid> -admin-pass <pw>
//
// The *-uid flags take the unique IDs printed by cmd/adduser. All endpoints
// default to localhost with the standard ports.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"voicx/internal/netproto"
)

// options holds the e2e parameters.
type options struct {
	addr          string
	queryAddr     string
	healthURL     string
	udpAddr       string
	fileAddr      string
	aliceUID      string
	alicePass     string
	aliceNickname string
	bobUID        string
	bobPass       string
	adminUID      string
	adminPass     string
	serverPass    string
	filePayload   int64
}

// file-transfer frame types (mirror internal/filetransfer, unexported).
const (
	ftInit   uint16 = 1
	ftChunk  uint16 = 2
	ftDigest uint16 = 3
	ftStatus uint16 = 4
)

const readTimeout = 5 * time.Second

// client is one control-channel connection.
type client struct {
	conn     net.Conn
	uid      string
	clientID string
	nickname string
}

// eventEnvelope mirrors the broadcaster's {"type","data"} shape.
type eventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func main() {
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:10011", "control channel address")
	flag.StringVar(&o.queryAddr, "query-addr", "127.0.0.1:10012", "ServerQuery address")
	flag.StringVar(&o.healthURL, "health-url", "http://127.0.0.1:9090", "health endpoint base URL")
	flag.StringVar(&o.udpAddr, "udp-addr", "127.0.0.1:9987", "UDP media address")
	flag.StringVar(&o.fileAddr, "file-addr", "127.0.0.1:30033", "file-transfer address (fallback; the port from the init response wins)")
	flag.StringVar(&o.aliceUID, "alice-uid", "", "alice's unique ID")
	flag.StringVar(&o.alicePass, "alice-pass", "", "alice's password")
	flag.StringVar(&o.aliceNickname, "alice-nick", "alice", "alice's account nickname (nickname-login check)")
	flag.StringVar(&o.bobUID, "bob-uid", "", "bob's unique ID")
	flag.StringVar(&o.bobPass, "bob-pass", "", "bob's password")
	flag.StringVar(&o.adminUID, "admin-uid", "", "admin's unique ID (ServerQuery)")
	flag.StringVar(&o.adminPass, "admin-pass", "", "admin's password")
	flag.StringVar(&o.serverPass, "server-password", "", "global server password (if set)")
	flag.Int64Var(&o.filePayload, "file-payload", 64*1024, "upload test payload size in bytes")
	flag.Parse()

	for _, req := range []struct{ name, val string }{
		{"alice-uid", o.aliceUID}, {"alice-pass", o.alicePass},
		{"bob-uid", o.bobUID}, {"bob-pass", o.bobPass},
		{"admin-uid", o.adminUID}, {"admin-pass", o.adminPass},
	} {
		if req.val == "" {
			fmt.Fprintf(os.Stderr, "e2e: -%s is required\n", req.name)
			os.Exit(2)
		}
	}

	os.Exit(runChecks(o))
}

// runChecks executes the checklist and returns the process exit code.
func runChecks(o options) int {
	cx := &checkCtx{opts: o}
	defer cx.close()

	checks := []check{
		{"healthz", checkHealthz},
		{"readyz", checkReadyz},
		{"metrics", checkMetrics},
		{"udp-ping-pong", checkUDP},
		{"auth", checkAuth},
		{"auth-nickname", checkAuthNickname},
		{"channel-create-denied-for-user", checkCreateDenied},
		{"channel-create-via-query", checkCreateViaQuery},
		{"channel-join", checkJoin},
		{"auth-anonymous", checkAnonymousAuth},
		{"anonymous-create-denied", checkAnonymousCreateDenied},
		{"chat-channel", checkChatChannel},
		{"chat-direct", checkChatDirect},
		{"chat-global", checkChatGlobal},
		{"permission-delete-denied", checkDeleteDenied},
		{"file-upload-download-list", checkFiles},
		{"query", checkQuery},
	}
	if o.serverPass != "" {
		checks = append(checks, check{"auth-anonymous-server-password", checkAnonymousServerPassword})
	}

	passed := 0
	for _, c := range checks {
		if err := c.run(cx); err != nil {
			fmt.Printf("FAIL %s: %v\n", c.name, err)
		} else {
			fmt.Printf("PASS %s\n", c.name)
			passed++
		}
	}
	fmt.Printf("%d/%d checks passed\n", passed, len(checks))
	if passed != len(checks) {
		return 1
	}
	return 0
}

// checkCtx carries shared state between checks.
type checkCtx struct {
	opts      options
	alice     *client
	bob       *client
	guest     *client
	channelID int64
}

func (c *checkCtx) close() {
	if c.alice != nil {
		_ = c.alice.conn.Close()
	}
	if c.bob != nil {
		_ = c.bob.conn.Close()
	}
	if c.guest != nil {
		_ = c.guest.conn.Close()
	}
}

type check struct {
	name string
	run  func(*checkCtx) error
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func httpGet(url string) (int, string, error) {
	client := &http.Client{Timeout: readTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(body), err
}

func checkHealthz(c *checkCtx) error {
	code, _, err := httpGet(c.opts.healthURL + "/healthz")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("healthz status = %d, want 200", code)
	}
	return nil
}

func checkReadyz(c *checkCtx) error {
	code, _, err := httpGet(c.opts.healthURL + "/readyz")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("readyz status = %d, want 200", code)
	}
	return nil
}

func checkMetrics(c *checkCtx) error {
	code, body, err := httpGet(c.opts.healthURL + "/metrics")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("metrics status = %d, want 200", code)
	}
	if !strings.Contains(body, "voicx_clients_connected") {
		return errors.New("metrics body missing voicx_clients_connected")
	}
	return nil
}

// ---------------------------------------------------------------------------
// UDP
// ---------------------------------------------------------------------------

func checkUDP(c *checkCtx) error {
	raddr, err := net.ResolveUDPAddr("udp", c.opts.udpAddr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{netproto.UDPMsgPing}); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return err
	}
	if n < 1 || buf[0] != netproto.UDPMsgPong {
		return fmt.Errorf("reply = %x, want pong", buf[:n])
	}
	return nil
}

// ---------------------------------------------------------------------------
// Auth & protocol helpers
// ---------------------------------------------------------------------------

func dialAuth(addr, uid, password, serverPassword string) (*client, error) {
	conn, err := net.DialTimeout("tcp", addr, readTimeout)
	if err != nil {
		return nil, err
	}
	if err := writeMsg(conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Username:       uid,
		Password:       password,
		ServerPassword: serverPassword,
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	f, err := readOfType(conn, netproto.MsgAuthResponse, readTimeout)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, errors.New("auth rejected: " + resp.Reason)
	}
	if _, err := readOfType(conn, netproto.MsgSnapshot, readTimeout); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("reading snapshot: %w", err)
	}
	return &client{conn: conn, uid: uid, clientID: resp.ClientID, nickname: resp.Nickname}, nil
}

func writeMsg(conn net.Conn, mt netproto.MessageType, msg any) error {
	f, err := netproto.Encode(mt, msg)
	if err != nil {
		return err
	}
	return netproto.WriteFrame(conn, f)
}

func readOfType(conn net.Conn, mt netproto.MessageType, timeout time.Duration) (*netproto.Frame, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		// Answer server-initiated keepalive pings while waiting.
		if netproto.MessageType(f.Type) == netproto.MsgPing {
			_ = writeMsg(conn, netproto.MsgPong, netproto.Pong{})
			continue
		}
		if netproto.MessageType(f.Type) == mt {
			return f, nil
		}
	}
}

// readEvent reads MsgEvent frames until the wanted envelope type arrives. A
// server error frame aborts the wait immediately (surfaces the real cause
// instead of a bare timeout).
func readEvent(conn net.Conn, want string, timeout time.Duration) (*eventEnvelope, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		// Answer server-initiated keepalive pings while waiting.
		if netproto.MessageType(f.Type) == netproto.MsgPing {
			_ = writeMsg(conn, netproto.MsgPong, netproto.Pong{})
			continue
		}
		if netproto.MessageType(f.Type) == netproto.MsgError {
			var e netproto.Error
			if err := netproto.Decode(f, &e); err == nil {
				return nil, fmt.Errorf("server error %d: %s (while waiting for %q)", e.Code, e.Message, want)
			}
			continue
		}
		if netproto.MessageType(f.Type) != netproto.MsgEvent {
			continue
		}
		var env eventEnvelope
		if err := json.Unmarshal(f.Payload, &env); err != nil {
			return nil, err
		}
		if env.Type == want {
			return &env, nil
		}
	}
	return nil, fmt.Errorf("no %q event within %s", want, timeout)
}

func checkAuth(c *checkCtx) error {
	alice, err := dialAuth(c.opts.addr, c.opts.aliceUID, c.opts.alicePass, c.opts.serverPass)
	if err != nil {
		return fmt.Errorf("alice: %w", err)
	}
	c.alice = alice

	bob, err := dialAuth(c.opts.addr, c.opts.bobUID, c.opts.bobPass, c.opts.serverPass)
	if err != nil {
		return fmt.Errorf("bob: %w", err)
	}
	c.bob = bob

	// Wrong password must be rejected.
	conn, err := net.DialTimeout("tcp", c.opts.addr, readTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeMsg(conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Username: c.opts.aliceUID, Password: "definitely-wrong", ServerPassword: c.opts.serverPass,
	}); err != nil {
		return err
	}
	f, err := readOfType(conn, netproto.MsgAuthResponse, readTimeout)
	if err != nil {
		return err
	}
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		return err
	}
	if resp.OK {
		return errors.New("wrong password accepted")
	}
	return nil
}

// checkAuthNickname verifies password auth by nickname: authenticating with
// the account nickname instead of the unique ID succeeds and returns the
// canonical unique ID.
func checkAuthNickname(c *checkCtx) error {
	nick := c.opts.aliceNickname
	if nick == "" {
		nick = "alice" // cmd/adduser default nickname in our docs
	}
	conn, err := net.DialTimeout("tcp", c.opts.addr, readTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeMsg(conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Username:       nick,
		Password:       c.opts.alicePass,
		ServerPassword: c.opts.serverPass,
	}); err != nil {
		return err
	}
	f, err := readOfType(conn, netproto.MsgAuthResponse, readTimeout)
	if err != nil {
		return err
	}
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("nickname login rejected: %s", resp.Reason)
	}
	if resp.UniqueID != c.opts.aliceUID {
		return fmt.Errorf("unique id = %q, want canonical %q", resp.UniqueID, c.opts.aliceUID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

// checkCreateDenied verifies a default (non-admin, unprivileged) user cannot
// create a permanent channel — creation goes through ServerQuery instead.
func checkCreateDenied(c *checkCtx) error {
	if err := writeMsg(c.alice.conn, netproto.MsgCreateChannel,
		netproto.CreateChannel{Name: "e2e-denied", Type: 2}); err != nil {
		return err
	}
	f, err := readOfType(c.alice.conn, netproto.MsgError, readTimeout)
	if err != nil {
		return err
	}
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		return err
	}
	if e.Code != 4 {
		return fmt.Errorf("error code = %d, want 4 (permission denied)", e.Code)
	}
	return nil
}

// querySession is a ServerQuery connection.
type querySession struct {
	conn net.Conn
	r    *lineReader
}

// lineReader wraps a conn for line-based reads with a deadline.
type lineReader struct {
	conn net.Conn
	buf  []byte
}

func (l *lineReader) readLine(timeout time.Duration) (string, error) {
	_ = l.conn.SetReadDeadline(time.Now().Add(timeout))
	defer l.conn.SetReadDeadline(time.Time{})
	one := make([]byte, 1)
	for {
		n, err := l.conn.Read(one)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		if one[0] == '\n' {
			line := string(l.buf)
			l.buf = l.buf[:0]
			return strings.TrimRight(line, "\r"), nil
		}
		l.buf = append(l.buf, one[0])
	}
}

func dialQuery(addr, uid, password string) (*querySession, error) {
	conn, err := net.DialTimeout("tcp", addr, readTimeout)
	if err != nil {
		return nil, err
	}
	q := &querySession{conn: conn, r: &lineReader{conn: conn}}
	// Banner: two lines.
	if _, err := q.r.readLine(readTimeout); err != nil {
		return nil, err
	}
	if _, err := q.r.readLine(readTimeout); err != nil {
		return nil, err
	}
	lines, err := q.cmd("login " + uid + " " + password)
	if err != nil {
		return nil, err
	}
	if last := lines[len(lines)-1]; last != "error id=0 msg=ok" {
		return nil, fmt.Errorf("query login failed: %s", last)
	}
	return q, nil
}

// cmd writes a command and reads lines until the terminating error line.
func (q *querySession) cmd(command string) ([]string, error) {
	if _, err := q.conn.Write([]byte(command + "\n")); err != nil {
		return nil, err
	}
	var lines []string
	for {
		line, err := q.r.readLine(readTimeout)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, "error id=") {
			return lines, nil
		}
	}
}

func checkCreateViaQuery(c *checkCtx) error {
	q, err := dialQuery(c.opts.queryAddr, c.opts.adminUID, c.opts.adminPass)
	if err != nil {
		return err
	}
	defer q.conn.Close()

	lines, err := q.cmd(`channelcreate channel_name=e2e\schannel channel_flag_permanent=1`)
	if err != nil {
		return err
	}
	var cidLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "cid=") {
			cidLine = l
		}
	}
	if cidLine == "" {
		return fmt.Errorf("no cid in response: %v", lines)
	}
	cid, err := strconv.ParseInt(strings.TrimPrefix(cidLine, "cid="), 10, 64)
	if err != nil {
		return err
	}
	c.channelID = cid

	// Bob reconnects to get a fresh snapshot containing the new channel.
	_ = c.bob.conn.Close()
	bob, err := dialAuth(c.opts.addr, c.opts.bobUID, c.opts.bobPass, c.opts.serverPass)
	if err != nil {
		return fmt.Errorf("bob reconnect: %w", err)
	}
	c.bob = bob
	return nil
}

func checkJoin(c *checkCtx) error {
	if err := writeMsg(c.alice.conn, netproto.MsgJoinChannel,
		netproto.JoinChannel{ChannelID: c.channelID}); err != nil {
		return fmt.Errorf("alice join: %w", err)
	}
	if err := writeMsg(c.bob.conn, netproto.MsgJoinChannel,
		netproto.JoinChannel{ChannelID: c.channelID}); err != nil {
		return fmt.Errorf("bob join: %w", err)
	}
	// user_moved events go to EVERYONE, so a type-only match can observe
	// alice's move while bob's own join is still being processed. Wait for
	// each client's OWN move to prove membership before continuing.
	if err := waitForMove(c.alice.conn, c.alice.clientID); err != nil {
		return fmt.Errorf("alice membership: %w", err)
	}
	if err := waitForMove(c.bob.conn, c.bob.clientID); err != nil {
		return fmt.Errorf("bob membership: %w", err)
	}
	return nil
}

// waitForMove reads user_moved events until one names the given client ID.
func waitForMove(conn net.Conn, clientID string) error {
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		env, err := readEvent(conn, "user_moved", deadline.Sub(time.Now()))
		if err != nil {
			return err
		}
		var ue struct {
			ClientID string `json:"client_id"`
		}
		if err := json.Unmarshal(env.Data, &ue); err != nil {
			return err
		}
		if ue.ClientID == clientID {
			return nil
		}
	}
	return fmt.Errorf("no user_moved for %s", clientID)
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

func readChat(conn net.Conn, wantText string) error {
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		env, err := readEvent(conn, "chat", deadline.Sub(time.Now()))
		if err != nil {
			return err
		}
		var chat netproto.ChatBroadcast
		if err := json.Unmarshal(env.Data, &chat); err != nil {
			return err
		}
		if chat.Text == wantText {
			return nil
		}
	}
	return fmt.Errorf("chat %q not received", wantText)
}

func checkChatChannel(c *checkCtx) error {
	text := "channel-" + randHex(4)
	if err := writeMsg(c.alice.conn, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: strconv.FormatInt(c.channelID, 10), Text: text,
	}); err != nil {
		return err
	}
	if err := readChat(c.bob.conn, text); err != nil {
		return fmt.Errorf("bob: %w", err)
	}
	return nil
}

func checkChatDirect(c *checkCtx) error {
	text := "direct-" + randHex(4)
	if err := writeMsg(c.bob.conn, netproto.MsgChatSend, netproto.ChatSend{
		ToUniqueID: c.alice.uid, Text: text,
	}); err != nil {
		return err
	}
	if err := readChat(c.alice.conn, text); err != nil {
		return fmt.Errorf("alice: %w", err)
	}
	return nil
}

func checkChatGlobal(c *checkCtx) error {
	text := "global-" + randHex(4)
	if err := writeMsg(c.alice.conn, netproto.MsgChatSend, netproto.ChatSend{Text: text}); err != nil {
		return err
	}
	if err := readChat(c.bob.conn, text); err != nil {
		return fmt.Errorf("bob: %w", err)
	}
	if err := readChat(c.alice.conn, text); err != nil {
		return fmt.Errorf("alice (echo): %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

func checkDeleteDenied(c *checkCtx) error {
	if err := writeMsg(c.bob.conn, netproto.MsgDeleteChannel,
		netproto.DeleteChannel{ChannelID: c.channelID}); err != nil {
		return err
	}
	f, err := readOfType(c.bob.conn, netproto.MsgError, readTimeout)
	if err != nil {
		return err
	}
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		return err
	}
	if e.Code != 4 {
		return fmt.Errorf("error code = %d, want 4 (permission denied)", e.Code)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

func (c *checkCtx) fileTransferAddr(port int) string {
	if port != 0 {
		host, _, err := net.SplitHostPort(c.opts.addr)
		if err == nil {
			return net.JoinHostPort(host, strconv.Itoa(port))
		}
	}
	return c.opts.fileAddr
}

func checkFiles(c *checkCtx) error {
	// Upload.
	payload := make([]byte, c.opts.filePayload)
	if _, err := rand.Read(payload); err != nil {
		return err
	}
	name := "e2e-" + randHex(4) + ".bin"

	if err := writeMsg(c.alice.conn, netproto.MsgFileTransferInit, netproto.FileTransferInit{
		ChannelID: c.channelID, Direction: "upload", Name: name, Size: int64(len(payload)),
	}); err != nil {
		return err
	}
	f, err := readOfType(c.alice.conn, netproto.MsgFileTransferInitResponse, readTimeout)
	if err != nil {
		return err
	}
	var initResp netproto.FileTransferInitResponse
	if err := netproto.Decode(f, &initResp); err != nil {
		return err
	}

	if err := uploadFile(c.fileTransferAddr(initResp.Port), initResp, payload); err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	// Download.
	if err := writeMsg(c.bob.conn, netproto.MsgFileTransferInit, netproto.FileTransferInit{
		ChannelID: c.channelID, Direction: "download", Name: name,
	}); err != nil {
		return err
	}
	f, err = readOfType(c.bob.conn, netproto.MsgFileTransferInitResponse, readTimeout)
	if err != nil {
		return err
	}
	var dlResp netproto.FileTransferInitResponse
	if err := netproto.Decode(f, &dlResp); err != nil {
		return err
	}
	got, err := downloadFile(c.fileTransferAddr(dlResp.Port), dlResp)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if !equalBytes(got, payload) {
		return fmt.Errorf("download content mismatch (%d vs %d bytes)", len(got), len(payload))
	}

	// List.
	if err := writeMsg(c.alice.conn, netproto.MsgFileList, netproto.FileList{ChannelID: c.channelID}); err != nil {
		return err
	}
	f, err = readOfType(c.alice.conn, netproto.MsgFileListResponse, readTimeout)
	if err != nil {
		return err
	}
	var list netproto.FileListResponse
	if err := netproto.Decode(f, &list); err != nil {
		return err
	}
	for _, e := range list.Entries {
		if e.Name == name {
			return nil
		}
	}
	return fmt.Errorf("file %q not in listing", name)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeFTJSON(conn net.Conn, frameType uint16, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return netproto.WriteFrame(conn, &netproto.Frame{Type: frameType, Payload: payload})
}

func readStatusFrame(conn net.Conn) (bool, string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	f, err := netproto.ReadFrame(conn)
	if err != nil {
		return false, "", err
	}
	if f.Type != ftStatus {
		return false, "", fmt.Errorf("frame type = %d, want status", f.Type)
	}
	var st struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(f.Payload, &st); err != nil {
		return false, "", err
	}
	return st.OK, st.Error, nil
}

func uploadFile(addr string, init netproto.FileTransferInitResponse, payload []byte) error {
	conn, err := net.DialTimeout("tcp", addr, readTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := writeFTJSON(conn, ftInit, map[string]string{
		"token": init.Token, "transfer_id": init.TransferID,
	}); err != nil {
		return err
	}
	const chunk = 32 * 1024
	for off := 0; off < len(payload); off += chunk {
		end := off + chunk
		if end > len(payload) {
			end = len(payload)
		}
		if err := netproto.WriteFrame(conn, &netproto.Frame{Type: ftChunk, Payload: payload[off:end]}); err != nil {
			return err
		}
	}
	if err := writeFTJSON(conn, ftDigest, map[string]string{"sha256": sha256Hex(payload)}); err != nil {
		return err
	}
	ok, errMsg, err := readStatusFrame(conn)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("status: " + errMsg)
	}
	return nil
}

func downloadFile(addr string, init netproto.FileTransferInitResponse) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", addr, readTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := writeFTJSON(conn, ftInit, map[string]string{
		"token": init.Token, "transfer_id": init.TransferID,
	}); err != nil {
		return nil, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	var got []byte
	for {
		f, err := netproto.ReadFrame(conn)
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case ftChunk:
			got = append(got, f.Payload...)
		case ftDigest:
			var d struct {
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal(f.Payload, &d); err != nil {
				return nil, err
			}
			if d.SHA256 != sha256Hex(got) {
				return nil, errors.New("digest mismatch")
			}
			ok, errMsg, err := readStatusFrame(conn)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errors.New("status: " + errMsg)
			}
			return got, nil
		default:
			return nil, fmt.Errorf("unexpected frame type %d", f.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// ServerQuery
// ---------------------------------------------------------------------------

func checkQuery(c *checkCtx) error {
	q, err := dialQuery(c.opts.queryAddr, c.opts.adminUID, c.opts.adminPass)
	if err != nil {
		return err
	}
	defer q.conn.Close()

	lines, err := q.cmd("clientlist")
	if err != nil {
		return err
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, c.opts.aliceUID) {
		return fmt.Errorf("clientlist missing alice (%s)", c.opts.aliceUID)
	}
	if !strings.Contains(joined, c.opts.bobUID) {
		return fmt.Errorf("clientlist missing bob (%s)", c.opts.bobUID)
	}

	lines, err = q.cmd("serverinfo")
	if err != nil {
		return err
	}
	if last := lines[len(lines)-1]; last != "error id=0 msg=ok" {
		return fmt.Errorf("serverinfo failed: %s", last)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Anonymous (guest) login
// ---------------------------------------------------------------------------

// dialGuest connects and authenticates as an anonymous guest.
func dialGuest(addr, nickname, serverPassword string) (*client, error) {
	conn, err := net.DialTimeout("tcp", addr, readTimeout)
	if err != nil {
		return nil, err
	}
	if err := writeMsg(conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Anonymous:      true,
		Nickname:       nickname,
		ServerPassword: serverPassword,
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	f, err := readOfType(conn, netproto.MsgAuthResponse, readTimeout)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, errors.New("guest auth rejected: " + resp.Reason)
	}
	if _, err := readOfType(conn, netproto.MsgSnapshot, readTimeout); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("reading snapshot: %w", err)
	}
	return &client{conn: conn, uid: resp.UniqueID, clientID: resp.ClientID, nickname: resp.Nickname}, nil
}

// checkAnonymousAuth verifies a guest can log in with just a nickname, is
// visible to other clients, can join a channel, and can chat in it.
func checkAnonymousAuth(c *checkCtx) error {
	guest, err := dialGuest(c.opts.addr, "e2e-guest", c.opts.serverPass)
	if err != nil {
		return err
	}
	c.guest = guest
	if !strings.HasPrefix(guest.uid, "guest:") {
		return fmt.Errorf("guest unique id = %q, want guest: prefix", guest.uid)
	}
	if guest.nickname != "e2e-guest" {
		return fmt.Errorf("guest nickname = %q, want e2e-guest", guest.nickname)
	}

	// Bob must see the guest's user_joined with the nickname.
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		env, err := readEvent(c.bob.conn, "user_joined", deadline.Sub(time.Now()))
		if err != nil {
			return fmt.Errorf("bob user_joined: %w", err)
		}
		var ue struct {
			ClientID string `json:"client_id"`
			Nickname string `json:"nickname"`
		}
		if err := json.Unmarshal(env.Data, &ue); err != nil {
			return err
		}
		if ue.Nickname == "e2e-guest" {
			break
		}
	}

	// Guest joins the channel and both directions of channel chat work.
	if err := writeMsg(c.guest.conn, netproto.MsgJoinChannel,
		netproto.JoinChannel{ChannelID: c.channelID}); err != nil {
		return fmt.Errorf("guest join: %w", err)
	}

	text := "guest-chat-" + randHex(4)
	if err := writeMsg(c.guest.conn, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: strconv.FormatInt(c.channelID, 10), Text: text,
	}); err != nil {
		return err
	}
	if err := readChat(c.bob.conn, text); err != nil {
		return fmt.Errorf("bob reading guest chat: %w", err)
	}

	text = "to-guest-" + randHex(4)
	if err := writeMsg(c.bob.conn, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: strconv.FormatInt(c.channelID, 10), Text: text,
	}); err != nil {
		return err
	}
	if err := readChat(c.guest.conn, text); err != nil {
		return fmt.Errorf("guest reading bob chat: %w", err)
	}
	return nil
}

// checkAnonymousCreateDenied verifies guests cannot create channels.
func checkAnonymousCreateDenied(c *checkCtx) error {
	if err := writeMsg(c.guest.conn, netproto.MsgCreateChannel,
		netproto.CreateChannel{Name: "guest-denied", Type: 0}); err != nil {
		return err
	}
	f, err := readOfType(c.guest.conn, netproto.MsgError, readTimeout)
	if err != nil {
		return err
	}
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		return err
	}
	if e.Code != 4 {
		return fmt.Errorf("error code = %d, want 4 (permission denied)", e.Code)
	}
	return nil
}

// checkAnonymousServerPassword verifies guests must supply the server
// password on a protected server (only run when -server-password is set).
func checkAnonymousServerPassword(c *checkCtx) error {
	conn, err := net.DialTimeout("tcp", c.opts.addr, readTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeMsg(conn, netproto.MsgAuthenticate, netproto.Authenticate{
		Anonymous: true, Nickname: "e2e-guest-nopass",
	}); err != nil {
		return err
	}
	f, err := readOfType(conn, netproto.MsgAuthResponse, readTimeout)
	if err != nil {
		return err
	}
	var resp netproto.AuthResponse
	if err := netproto.Decode(f, &resp); err != nil {
		return err
	}
	if resp.OK {
		return errors.New("guest auth without server password accepted on a protected server")
	}
	return nil
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
