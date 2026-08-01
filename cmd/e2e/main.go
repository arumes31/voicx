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
	"crypto/tls"
	"encoding/base64"
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
	"sync"
	"time"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"

	"voicx/internal/netproto"
	"voicx/internal/tlscert"
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
	tlsInsecure   bool
}

// file-transfer frame types (mirror internal/filetransfer, unexported).
const (
	ftInit   uint16 = 1
	ftChunk  uint16 = 2
	ftDigest uint16 = 3
	ftStatus uint16 = 4
)

const readTimeout = 5 * time.Second

// e2eTLSInsecure mirrors -tls-insecure: dial the control channel with TLS
// but skip certificate verification (self-signed server certs).
var e2eTLSInsecure bool

// loggedFP prints the server fingerprint only on the first TLS dial.
var loggedFP sync.Once

// dialTCP dials the control channel, honoring -tls-insecure. With TLS the
// presented fingerprint is logged once so runs are auditable.
func dialTCP(addr string) (net.Conn, error) {
	if !e2eTLSInsecure {
		return net.DialTimeout("tcp", addr, readTimeout)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: readTimeout}, "tcp", addr,
		&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // e2e flag
	if err != nil {
		return nil, err
	}
	loggedFP.Do(func() {
		if pc := conn.ConnectionState().PeerCertificates; len(pc) > 0 {
			fmt.Printf("e2e: server TLS fingerprint: %s\n", tlscert.FingerprintDER(pc[0].Raw))
		}
	})
	return conn, nil
}

// client is one control-channel connection.
type client struct {
	conn     net.Conn
	uid      string
	clientID string
	nickname string

	// E2EE chat (wave 4b): own X25519 pair + per-scope chat keys captured
	// from MsgChannelKey frames.
	e2ePub      [32]byte
	e2ePriv     [32]byte
	scopeKeys   map[int64]map[uint32][32]byte
	scopeLatest map[int64]uint32
}

// eventEnvelope mirrors the broadcaster's {"type","data"} shape.
type eventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// clientsByConn registers live clients so the frame readers can capture
// MsgChannelKey frames opportunistically.
var clientsByConn sync.Map

// registerClient stores the connection association and publishes the
// client's X25519 public key (the server answers with sealed chat keys).
func registerClient(c *client) error {
	c.scopeKeys = map[int64]map[uint32][32]byte{}
	c.scopeLatest = map[int64]uint32{}
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	c.e2ePub, c.e2ePriv = *pub, *priv
	clientsByConn.Store(c.conn, c)
	return writeMsg(c.conn, netproto.MsgKeyPublish, netproto.KeyPublish{
		PublicKey: base64.StdEncoding.EncodeToString(pub[:]),
	})
}

// captureChannelKey unseals and stores a scope key frame for the connection's
// client. Called by the frame readers for every MsgChannelKey frame.
func captureChannelKey(conn net.Conn, f *netproto.Frame) {
	v, ok := clientsByConn.Load(conn)
	if !ok {
		return
	}
	c := v.(*client)
	var ck netproto.ChannelKey
	if err := netproto.Decode(f, &ck); err != nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(ck.SealedKey)
	if err != nil {
		return
	}
	key, ok := box.OpenAnonymous(nil, raw, &c.e2ePub, &c.e2ePriv)
	if !ok || len(key) != 32 {
		return
	}
	var k [32]byte
	copy(k[:], key)
	if c.scopeKeys[ck.ChannelID] == nil {
		c.scopeKeys[ck.ChannelID] = map[uint32][32]byte{}
	}
	c.scopeKeys[ck.ChannelID][ck.KeyID] = k
	c.scopeLatest[ck.ChannelID] = ck.KeyID
}

// awaitScopeKey reads frames until the client holds a key for the scope.
func awaitScopeKey(conn net.Conn, c *client, scope int64) error {
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		if _, ok := c.scopeLatest[scope]; ok {
			return nil
		}
		if _, err := readOfType(conn, netproto.MsgChannelKey, deadline.Sub(time.Now())); err != nil {
			return fmt.Errorf("no chat key for scope %d: %w", scope, err)
		}
	}
	return fmt.Errorf("no chat key for scope %d", scope)
}

// fetchPub resolves a user's X25519 public key from the server directory.
func fetchPub(conn net.Conn, uid string) ([32]byte, error) {
	var out [32]byte
	if err := writeMsg(conn, netproto.MsgKeyRequest, netproto.KeyRequest{UniqueID: uid}); err != nil {
		return out, err
	}
	f, err := readOfType(conn, netproto.MsgKeyResponse, readTimeout)
	if err != nil {
		return out, err
	}
	var resp netproto.KeyResponse
	if err := netproto.Decode(f, &resp); err != nil {
		return out, err
	}
	if resp.PublicKey == "" {
		return out, fmt.Errorf("no public key published for %s", uid)
	}
	raw, err := base64.StdEncoding.DecodeString(resp.PublicKey)
	if err != nil || len(raw) != 32 {
		return out, fmt.Errorf("invalid public key for %s", uid)
	}
	copy(out[:], raw)
	return out, nil
}

// e2eSealScope encrypts a channel/global message with the scope key.
func e2eSealScope(text string, key [32]byte) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(append(nonce[:], secretbox.Seal(nil, []byte(text), &nonce, &key)...)), nil
}

// e2eOpenScope decrypts a channel/global message with the scope key.
func e2eOpenScope(blobB64 string, key [32]byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil || len(raw) < 24 {
		return "", errors.New("invalid scope ciphertext")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := secretbox.Open(nil, raw[24:], &nonce, &key)
	if !ok {
		return "", errors.New("scope open failed")
	}
	return string(plain), nil
}

// e2eSealDM encrypts a direct message for the recipient's public key.
func e2eSealDM(text string, recipientPub, senderPriv [32]byte) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(append(nonce[:], box.Seal(nil, []byte(text), &nonce, &recipientPub, &senderPriv)...)), nil
}

// e2eOpenDM decrypts a direct message with the sender's public key.
func e2eOpenDM(blobB64 string, senderPub, recipientPriv [32]byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil || len(raw) < 24 {
		return "", errors.New("invalid DM ciphertext")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := box.Open(nil, raw[24:], &nonce, &senderPub, &recipientPriv)
	if !ok {
		return "", errors.New("DM open failed")
	}
	return string(plain), nil
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
	flag.BoolVar(&o.tlsInsecure, "tls-insecure", false, "dial the control channel with TLS but skip certificate verification (self-signed certs), logging the fingerprint")
	flag.Parse()
	e2eTLSInsecure = o.tlsInsecure

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
		{"chat-history", checkChatHistory},
		{"chat-edit-delete", checkChatEditDelete},
		{"chat-slowmode", checkChatSlowMode},
		{"permission-delete-denied", checkDeleteDenied},
		{"file-upload-download-list", checkFiles},
		{"query", checkQuery},
		{"query-wave10a", checkQueryWave10a},
		{"group-management", checkGroupManagement},
		{"guest-default-group", checkGuestDefaultGroup},
		{"perm-set-trace", checkPermSetTrace},
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
	conn, err := dialTCP(addr)
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
	c := &client{conn: conn, uid: uid, clientID: resp.ClientID, nickname: resp.Nickname}
	if err := registerClient(c); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("e2e key publish: %w", err)
	}
	return c, nil
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
		if netproto.MessageType(f.Type) == netproto.MsgChannelKey {
			captureChannelKey(conn, f)
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
		if netproto.MessageType(f.Type) == netproto.MsgChannelKey {
			captureChannelKey(conn, f)
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
	conn, err := dialTCP(c.opts.addr)
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
	conn, err := dialTCP(c.opts.addr)
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
	if err := writeEncChannelChat(c.alice, c.channelID, text); err != nil {
		return fmt.Errorf("alice send: %w", err)
	}
	if err := readEncChannelChat(c.bob, c.channelID, text); err != nil {
		return fmt.Errorf("bob: %w", err)
	}
	return nil
}

// writeEncChannelChat seals a channel message with the sender's scope key
// and sends it (wave 4b: plaintext chat is rejected by the server).
func writeEncChannelChat(cl *client, channelID int64, text string) error {
	if err := awaitScopeKey(cl.conn, cl, channelID); err != nil {
		return err
	}
	id := cl.scopeLatest[channelID]
	key := cl.scopeKeys[channelID][id]
	blob, err := e2eSealScope(text, key)
	if err != nil {
		return err
	}
	return writeMsg(cl.conn, netproto.MsgChatSend, netproto.ChatSend{
		ChannelID: strconv.FormatInt(channelID, 10), Text: blob, Enc: true, KeyID: id,
	})
}

// readEncChannelChat reads chat events until one decrypts to wantText. It
// asserts the wire payload was ciphertext (the server never saw plaintext)
// and marked Enc.
func readEncChannelChat(cl *client, channelID int64, wantText string) error {
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		env, err := readEvent(cl.conn, "chat", deadline.Sub(time.Now()))
		if err != nil {
			return err
		}
		var chat netproto.ChatBroadcast
		if err := json.Unmarshal(env.Data, &chat); err != nil {
			return err
		}
		if !chat.Enc {
			return fmt.Errorf("server accepted/relayed PLAINTEXT chat: %s", env.Data)
		}
		if chat.Text == wantText {
			return fmt.Errorf("server relayed the message in plaintext")
		}
		key, ok := cl.scopeKeys[channelID][chat.KeyID]
		if !ok {
			continue // key for an older generation; keep waiting
		}
		plain, err := e2eOpenScope(chat.Text, key)
		if err == nil && plain == wantText {
			return nil
		}
	}
	return fmt.Errorf("encrypted chat %q not received/decryptable", wantText)
}

func checkChatDirect(c *checkCtx) error {
	text := "direct-" + randHex(4)
	// Bob seals the DM for alice's published X25519 key (true E2EE).
	alicePub, err := fetchPub(c.bob.conn, c.alice.uid)
	if err != nil {
		return fmt.Errorf("fetch alice pubkey: %w", err)
	}
	blob, err := e2eSealDM(text, alicePub, c.bob.e2ePriv)
	if err != nil {
		return err
	}
	if err := writeMsg(c.bob.conn, netproto.MsgChatSend, netproto.ChatSend{
		ToUniqueID: c.alice.uid, Text: blob, Enc: true,
	}); err != nil {
		return err
	}

	// Alice opens it with bob's public key; the wire form must be ciphertext
	// marked E2E.
	bobPub, err := fetchPub(c.alice.conn, c.bob.uid)
	if err != nil {
		return fmt.Errorf("fetch bob pubkey: %w", err)
	}
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		env, err := readEvent(c.alice.conn, "chat", deadline.Sub(time.Now()))
		if err != nil {
			return err
		}
		var chat netproto.ChatBroadcast
		if err := json.Unmarshal(env.Data, &chat); err != nil {
			return err
		}
		if !chat.Enc || !chat.E2E {
			return fmt.Errorf("DM not marked E2E (enc=%v e2e=%v)", chat.Enc, chat.E2E)
		}
		if chat.Text == text {
			return fmt.Errorf("server relayed the DM in plaintext")
		}
		plain, err := e2eOpenDM(chat.Text, bobPub, c.alice.e2ePriv)
		if err == nil && plain == text {
			return nil
		}
	}
	return fmt.Errorf("encrypted DM %q not received/decryptable", text)
}

func checkChatGlobal(c *checkCtx) error {
	text := "global-" + randHex(4)
	// Global scope is key scope 0.
	if err := awaitScopeKey(c.alice.conn, c.alice, 0); err != nil {
		return fmt.Errorf("alice global key: %w", err)
	}
	id := c.alice.scopeLatest[0]
	key := c.alice.scopeKeys[0][id]
	blob, err := e2eSealScope(text, key)
	if err != nil {
		return err
	}
	if err := writeMsg(c.alice.conn, netproto.MsgChatSend, netproto.ChatSend{
		Text: blob, Enc: true, KeyID: id,
	}); err != nil {
		return err
	}
	if err := readEncChannelChat(c.bob, 0, text); err != nil {
		return fmt.Errorf("bob: %w", err)
	}
	if err := readEncChannelChat(c.alice, 0, text); err != nil {
		return fmt.Errorf("alice (echo): %w", err)
	}
	return nil
}

// checkChatHistory (5a/103): an encrypted channel message is retrievable as
// decrypted history by a channel member.
func checkChatHistory(c *checkCtx) error {
	text := "hist-" + randHex(4)
	if err := writeEncChannelChat(c.alice, c.channelID, text); err != nil {
		return fmt.Errorf("alice send: %w", err)
	}
	if err := readEncChannelChat(c.bob, c.channelID, text); err != nil {
		return fmt.Errorf("bob receive: %w", err)
	}

	if err := writeMsg(c.bob.conn, netproto.MsgChatHistory, netproto.ChatHistory{
		ChannelID: c.channelID, Limit: 10,
	}); err != nil {
		return err
	}
	f, err := readOfType(c.bob.conn, netproto.MsgChatHistoryResponse, readTimeout)
	if err != nil {
		return err
	}
	var resp netproto.ChatHistoryResponse
	if err := netproto.Decode(f, &resp); err != nil {
		return err
	}
	for _, m := range resp.Messages {
		if m.Body == text && m.FromUniqueID == c.alice.uid && !m.Deleted {
			return nil
		}
	}
	return fmt.Errorf("message %q not found decrypted in history (%d entries)", text, len(resp.Messages))
}

// checkChatEditDelete (5a/101+102): own-message edit and delete with events
// and a history tombstone.
func checkChatEditDelete(c *checkCtx) error {
	text := "editme-" + randHex(4)
	if err := writeEncChannelChat(c.alice, c.channelID, text); err != nil {
		return fmt.Errorf("alice send: %w", err)
	}

	// Bob receives it to learn the server-side id.
	env, err := readEvent(c.bob.conn, "chat", readTimeout)
	if err != nil {
		return fmt.Errorf("bob receive: %w", err)
	}
	var chat netproto.ChatBroadcast
	if err := json.Unmarshal(env.Data, &chat); err != nil {
		return err
	}
	if chat.ID == 0 {
		return fmt.Errorf("chat message has no server id")
	}

	// Alice edits (encrypted); bob must see chat_edited with the re-sealed
	// body, decryptable to the new text.
	newText := "edited-" + randHex(4)
	keyID := c.alice.scopeLatest[c.channelID]
	key := c.alice.scopeKeys[c.channelID][keyID]
	blob, err := e2eSealScope(newText, key)
	if err != nil {
		return err
	}
	if err := writeMsg(c.alice.conn, netproto.MsgChatEdit, netproto.ChatEdit{
		MessageID: chat.ID, NewText: blob, Enc: true, KeyID: keyID,
	}); err != nil {
		return err
	}
	editEnv, err := readEvent(c.bob.conn, "chat_edited", readTimeout)
	if err != nil {
		return fmt.Errorf("bob chat_edited: %w", err)
	}
	var edit struct {
		MessageID int64  `json:"message_id"`
		Body      string `json:"body"`
		Enc       bool   `json:"enc"`
		KeyID     uint32 `json:"key_id"`
	}
	if err := json.Unmarshal(editEnv.Data, &edit); err != nil {
		return err
	}
	if edit.MessageID != chat.ID || !edit.Enc {
		return fmt.Errorf("chat_edited = %+v", edit)
	}
	plain, err := e2eOpenScope(edit.Body, key)
	if err != nil || plain != newText {
		return fmt.Errorf("edited body decrypt = %q, %v; want %q", plain, err, newText)
	}

	// Alice deletes; bob must see chat_deleted and history must tombstone.
	if err := writeMsg(c.alice.conn, netproto.MsgChatDelete, netproto.ChatDelete{MessageID: chat.ID}); err != nil {
		return err
	}
	if _, err := readEvent(c.bob.conn, "chat_deleted", readTimeout); err != nil {
		return fmt.Errorf("bob chat_deleted: %w", err)
	}

	if err := writeMsg(c.bob.conn, netproto.MsgChatHistory, netproto.ChatHistory{
		ChannelID: c.channelID, Limit: 10,
	}); err != nil {
		return err
	}
	f, err := readOfType(c.bob.conn, netproto.MsgChatHistoryResponse, readTimeout)
	if err != nil {
		return err
	}
	var resp netproto.ChatHistoryResponse
	if err := netproto.Decode(f, &resp); err != nil {
		return err
	}
	for _, m := range resp.Messages {
		if m.ID == chat.ID {
			if !m.Deleted || m.Body != "" {
				return fmt.Errorf("history entry after delete = %+v, want tombstone", m)
			}
			return nil
		}
	}
	return fmt.Errorf("deleted message %d not in history", chat.ID)
}

// checkChatSlowMode (5a/114): a channel with slow mode rejects a quick
// second message with a slow-mode error. Slow mode is set via ServerQuery
// (channel edit requires b_channel_modify, which the e2e users don't hold).
func checkChatSlowMode(c *checkCtx) error {
	q, err := dialQuery(c.opts.queryAddr, c.opts.adminUID, c.opts.adminPass)
	if err != nil {
		return err
	}
	defer q.conn.Close()

	setSlow := func(seconds int) error {
		if _, err := q.cmd(fmt.Sprintf("channeledit cid=%d slow_mode_seconds=%d", c.channelID, seconds)); err != nil {
			return err
		}
		// Drain the channel_updated event on bob so it can't pollute reads.
		if _, err := readEvent(c.bob.conn, "channel_updated", readTimeout); err != nil {
			return fmt.Errorf("bob channel_updated: %w", err)
		}
		return nil
	}

	if err := setSlow(30); err != nil {
		return fmt.Errorf("enable slow mode: %w", err)
	}
	if err := writeEncChannelChat(c.bob, c.channelID, "slow-first"); err != nil {
		return fmt.Errorf("first send: %w", err)
	}
	if err := writeEncChannelChat(c.bob, c.channelID, "slow-second"); err != nil {
		return fmt.Errorf("second send: %w", err)
	}
	f, err := readOfType(c.bob.conn, netproto.MsgError, readTimeout)
	if err != nil {
		return fmt.Errorf("waiting for slow mode error: %w", err)
	}
	var e netproto.Error
	if err := netproto.Decode(f, &e); err != nil {
		return err
	}
	if !strings.Contains(e.Message, "slow mode") {
		return fmt.Errorf("error = %q, want slow mode rejection", e.Message)
	}
	return setSlow(0)
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
	conn, err := dialTCP(addr)
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
	g := &client{conn: conn, uid: resp.UniqueID, clientID: resp.ClientID, nickname: resp.Nickname}
	if err := registerClient(g); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("e2e key publish: %w", err)
	}
	return g, nil
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
	if err := writeEncChannelChat(c.guest, c.channelID, text); err != nil {
		return fmt.Errorf("guest send: %w", err)
	}
	if err := readEncChannelChat(c.bob, c.channelID, text); err != nil {
		return fmt.Errorf("bob reading guest chat: %w", err)
	}

	text = "to-guest-" + randHex(4)
	if err := writeEncChannelChat(c.bob, c.channelID, text); err != nil {
		return fmt.Errorf("bob send: %w", err)
	}
	if err := readEncChannelChat(c.guest, c.channelID, text); err != nil {
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
	conn, err := dialTCP(c.opts.addr)
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

// ---------------------------------------------------------------------------
// Permission/group management (wave 6a)
// ---------------------------------------------------------------------------

// groupList asks the server for the group list of a type.
func groupList(conn net.Conn, groupType string) (*netproto.GroupListResponse, error) {
	if err := writeMsg(conn, netproto.MsgGroupList, netproto.GroupList{Type: groupType}); err != nil {
		return nil, err
	}
	f, err := readOfType(conn, netproto.MsgGroupListResponse, readTimeout)
	if err != nil {
		return nil, err
	}
	var resp netproto.GroupListResponse
	if err := netproto.Decode(f, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// findGroup returns the list entry with the given name, or nil.
func findGroup(list *netproto.GroupListResponse, name string) *netproto.GroupEntry {
	for i := range list.Groups {
		if list.Groups[i].Name == name {
			return &list.Groups[i]
		}
	}
	return nil
}

// checkGroupManagement verifies the default groups are seeded and that an
// admin can create a group, assign alice (she gets the event), unassign her,
// and delete the group.
func checkGroupManagement(c *checkCtx) error {
	admin, err := dialAuth(c.opts.addr, c.opts.adminUID, c.opts.adminPass, c.opts.serverPass)
	if err != nil {
		return fmt.Errorf("admin: %w", err)
	}
	defer admin.conn.Close()

	// Default groups (143/144): Guest and Member are seeded at startup;
	// alice auto-joined Member on her first login.
	list, err := groupList(admin.conn, "server")
	if err != nil {
		return err
	}
	if findGroup(list, "Guest") == nil {
		return errors.New("default Guest group missing")
	}
	member := findGroup(list, "Member")
	if member == nil {
		return errors.New("default Member group missing")
	}
	if member.MemberCount < 1 {
		return fmt.Errorf("Member group has %d members, want >= 1 (alice auto-join)", member.MemberCount)
	}

	// Create a group (the response is the refreshed list).
	name := "e2e-mods-" + randHex(3)
	if err := writeMsg(admin.conn, netproto.MsgGroupCreate,
		netproto.GroupCreate{Type: "server", Name: name, SortID: 10}); err != nil {
		return err
	}
	f, err := readOfType(admin.conn, netproto.MsgGroupListResponse, readTimeout)
	if err != nil {
		return err
	}
	var created netproto.GroupListResponse
	if err := netproto.Decode(f, &created); err != nil {
		return err
	}
	g := findGroup(&created, name)
	if g == nil {
		return fmt.Errorf("created group %q not in list", name)
	}
	groupID := g.ID

	// Assign alice; she receives group_assigned and the member count rises.
	if err := writeMsg(admin.conn, netproto.MsgGroupAssign,
		netproto.GroupAssign{Type: "server", GroupID: groupID, UniqueID: c.opts.aliceUID}); err != nil {
		return err
	}
	if _, err := readEvent(c.alice.conn, "group_assigned", readTimeout); err != nil {
		return fmt.Errorf("alice group_assigned: %w", err)
	}
	list, err = groupList(admin.conn, "server")
	if err != nil {
		return err
	}
	if g := findGroup(list, name); g == nil || g.MemberCount != 1 {
		return fmt.Errorf("group after assign = %+v, want 1 member", g)
	}

	// Unassign and delete again (cleanup, and covers both code paths).
	if err := writeMsg(admin.conn, netproto.MsgGroupUnassign,
		netproto.GroupUnassign{Type: "server", GroupID: groupID, UniqueID: c.opts.aliceUID}); err != nil {
		return err
	}
	if _, err := readEvent(c.alice.conn, "group_unassigned", readTimeout); err != nil {
		return fmt.Errorf("alice group_unassigned: %w", err)
	}
	if err := writeMsg(admin.conn, netproto.MsgGroupDelete,
		netproto.GroupDelete{Type: "server", GroupID: groupID}); err != nil {
		return err
	}
	list, err = groupList(admin.conn, "server")
	if err != nil {
		return err
	}
	if findGroup(list, name) != nil {
		return fmt.Errorf("group %q still listed after delete", name)
	}
	return nil
}

// checkGuestDefaultGroup verifies guests virtually hold the Guest group's
// permissions: a permission set on the Guest group shows up in an anonymous
// client's own resolved permission set.
func checkGuestDefaultGroup(c *checkCtx) error {
	admin, err := dialAuth(c.opts.addr, c.opts.adminUID, c.opts.adminPass, c.opts.serverPass)
	if err != nil {
		return fmt.Errorf("admin: %w", err)
	}
	defer admin.conn.Close()

	list, err := groupList(admin.conn, "server")
	if err != nil {
		return err
	}
	guest := findGroup(list, "Guest")
	if guest == nil {
		return errors.New("default Guest group missing")
	}

	// Marker permission on the Guest group; removed again at the end.
	if err := writeMsg(admin.conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "server_group", GroupID: guest.ID, Key: "i_client_talk_power", Value: 42,
	}); err != nil {
		return err
	}
	defer writeMsg(admin.conn, netproto.MsgPermUnset, netproto.PermUnset{
		Tier: "server_group", GroupID: guest.ID, Key: "i_client_talk_power",
	})

	g, err := dialGuest(c.opts.addr, "e2e-guest-group", c.opts.serverPass)
	if err != nil {
		return err
	}
	defer g.conn.Close()

	// The PermSet travels on the admin connection and is processed
	// asynchronously to the guest connection: poll the guest's resolved
	// permissions until the marker appears.
	deadline := time.Now().Add(readTimeout)
	for {
		if err := writeMsg(g.conn, netproto.MsgPermissionsQuery, netproto.PermissionsQuery{}); err != nil {
			return err
		}
		f, err := readOfType(g.conn, netproto.MsgPermissionsResponse, readTimeout)
		if err != nil {
			return err
		}
		var resp netproto.PermissionsResponse
		if err := netproto.Decode(f, &resp); err != nil {
			return err
		}
		for _, e := range resp.Entries {
			if e.Key == "i_client_talk_power" && e.Value == 42 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guest permissions lack the Guest group's marker (entries=%+v)", resp.Entries)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// checkPermSetTrace verifies the permission write path and the trace: a
// client-tier set shows up as the winning tier and disappears after unset.
func checkPermSetTrace(c *checkCtx) error {
	admin, err := dialAuth(c.opts.addr, c.opts.adminUID, c.opts.adminPass, c.opts.serverPass)
	if err != nil {
		return fmt.Errorf("admin: %w", err)
	}
	defer admin.conn.Close()

	trace := func() (*netproto.PermTraceResponse, error) {
		if err := writeMsg(admin.conn, netproto.MsgPermTrace, netproto.PermTrace{
			UniqueID: c.opts.aliceUID, Key: "i_client_talk_power",
		}); err != nil {
			return nil, err
		}
		f, err := readOfType(admin.conn, netproto.MsgPermTraceResponse, readTimeout)
		if err != nil {
			return nil, err
		}
		var resp netproto.PermTraceResponse
		if err := netproto.Decode(f, &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	}

	if err := writeMsg(admin.conn, netproto.MsgPermSet, netproto.PermSet{
		Tier: "client", UniqueID: c.opts.aliceUID, Key: "i_client_talk_power", Value: 77,
	}); err != nil {
		return err
	}
	defer writeMsg(admin.conn, netproto.MsgPermUnset, netproto.PermUnset{
		Tier: "client", UniqueID: c.opts.aliceUID, Key: "i_client_talk_power",
	})

	resp, err := trace()
	if err != nil {
		return err
	}
	if resp.Effective != 77 || resp.EffectiveTier != "client_specific" {
		return fmt.Errorf("trace after set = %d/%q, want 77/client_specific", resp.Effective, resp.EffectiveTier)
	}

	if err := writeMsg(admin.conn, netproto.MsgPermUnset, netproto.PermUnset{
		Tier: "client", UniqueID: c.opts.aliceUID, Key: "i_client_talk_power",
	}); err != nil {
		return err
	}
	resp, err = trace()
	if err != nil {
		return err
	}
	if resp.EffectiveTier != "" {
		return fmt.Errorf("trace after unset = %d/%q, want unset", resp.Effective, resp.EffectiveTier)
	}
	return nil
}

// checkQueryWave10a exercises the wave-10a ServerQuery commands against the
// live server: serveredit (name reflected in serverinfo), server groups,
// channel permissions, and permoverview.
func checkQueryWave10a(c *checkCtx) error {
	q, err := dialQuery(c.opts.queryAddr, c.opts.adminUID, c.opts.adminPass)
	if err != nil {
		return err
	}
	defer q.conn.Close()

	// Read the current server name for later restore.
	lines, err := q.cmd("serverinfo")
	if err != nil {
		return err
	}
	var origName string
	for _, l := range lines {
		if strings.HasPrefix(l, "virtualserver_name=") {
			origName = strings.TrimPrefix(strings.Split(l, " ")[0], "virtualserver_name=")
		}
	}

	// (217) serveredit changes the name; serverinfo reflects it.
	if _, err := q.cmd(`serveredit virtualserver_name=WaveTen`); err != nil {
		return fmt.Errorf("serveredit: %w", err)
	}
	lines, err = q.cmd("serverinfo")
	if err != nil {
		return err
	}
	if !strings.Contains(lines[0], "virtualserver_name=WaveTen") {
		return fmt.Errorf("serverinfo after edit = %v", lines)
	}
	if origName != "" {
		defer q.cmd(`serveredit virtualserver_name=` + origName)
	}

	// (221) server group cycle.
	lines, err = q.cmd(`servergroupadd name=w10test`)
	if err != nil || len(lines) == 0 || !strings.HasPrefix(lines[0], "sgid=") {
		return fmt.Errorf("servergroupadd = %v, %v", lines, err)
	}
	sgid := strings.TrimPrefix(lines[0], "sgid=")
	if _, err := q.cmd("servergroupaddclient sgid=" + sgid + " cldbid=" + c.opts.aliceUID); err != nil {
		return fmt.Errorf("servergroupaddclient: %w", err)
	}
	lines, err = q.cmd("servergroupclientlist sgid=" + sgid)
	if err != nil || len(lines) == 0 || !strings.Contains(lines[0], c.opts.aliceUID) {
		return fmt.Errorf("servergroupclientlist = %v, %v", lines, err)
	}
	defer func() {
		q.cmd("servergroupdelclient sgid=" + sgid + " cldbid=" + c.opts.aliceUID)
		q.cmd("servergroupdel sgid=" + sgid + " force=1")
	}()

	// (220) channel-tier permission, then (219) permoverview shows it.
	if _, err := q.cmd("channeladdperm cid=1 permid=i_client_needed_talk_power permvalue=77"); err != nil {
		return fmt.Errorf("channeladdperm: %w", err)
	}
	defer q.cmd("channeldelperm cid=1 permid=i_client_needed_talk_power")
	lines, err = q.cmd("permoverview unique_id=" + c.opts.aliceUID + " cid=1")
	if err != nil {
		return fmt.Errorf("permoverview: %w", err)
	}
	found := false
	for _, l := range lines {
		if strings.Contains(l, "permid=i_client_needed_talk_power") && strings.Contains(l, "permvalue=77") {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("permoverview missing channel-tier entry: %v", lines)
	}

	// (222) custom property cycle.
	if _, err := q.cmd("customset cldbid=" + c.opts.aliceUID + " ident=role value=tester"); err != nil {
		return fmt.Errorf("customset: %w", err)
	}
	defer q.cmd("customdel cldbid=" + c.opts.aliceUID + " ident=role")
	lines, err = q.cmd("custominfo cldbid=" + c.opts.aliceUID)
	if err != nil || len(lines) == 0 || !strings.Contains(lines[0], "ident=role") {
		return fmt.Errorf("custominfo = %v, %v", lines, err)
	}

	// (223) logview returns lines.
	lines, err = q.cmd("logview lines=5")
	if err != nil || len(lines) < 2 {
		return fmt.Errorf("logview = %v, %v", lines, err)
	}
	if !strings.HasPrefix(lines[0], "line=") {
		return fmt.Errorf("logview row = %q", lines[0])
	}
	return nil
}
