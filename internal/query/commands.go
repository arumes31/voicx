// commands.go implements the ServerQuery command set: parsing, the auth
// gate, and one handler per command.
package query

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// session holds per-connection query state.
type session struct {
	conn     net.Conn
	remoteIP string
	authed   bool
	username string
}

// command is a parsed command line: name, key=value args (unescaped), and
// positional arguments (used by login).
type command struct {
	name       string
	args       map[string]string
	positional []string
}

// parseCommand splits a command line into name, key=value pairs, and
// positional tokens. Values with spaces must arrive escaped (\s).
//
// login is the only command with positional arguments, and its unique ID
// argument is base64 (which may END in '='); splitting it as key=value would
// eat the ID, so login's arguments are always parsed positionally.
func parseCommand(line string) command {
	fields := strings.Fields(line)
	cmd := command{args: make(map[string]string)}
	if len(fields) == 0 {
		return cmd
	}
	cmd.name = strings.ToLower(fields[0])

	if cmd.name == "login" {
		for _, f := range fields[1:] {
			cmd.positional = append(cmd.positional, unescape(f))
		}
		return cmd
	}

	for _, f := range fields[1:] {
		if k, v, ok := strings.Cut(f, "="); ok {
			cmd.args[unescape(k)] = unescape(v)
		} else {
			cmd.positional = append(cmd.positional, unescape(f))
		}
	}
	return cmd
}

// noAuthCommands may be used without login.
var noAuthCommands = map[string]bool{
	"login":   true,
	"help":    true,
	"quit":    true,
	"version": true,
}

// execute parses and runs one command line, writing the response. It returns
// false when the connection should close (quit, or a write failure).
func (s *Server) execute(ctx context.Context, sess *session, line string) bool {
	cmd := parseCommand(line)
	if cmd.name == "" {
		return true
	}

	if !sess.authed && !noAuthCommands[cmd.name] {
		return s.write(sess, errorLine(errInsufficientPermissions, "not logged in"))
	}

	switch cmd.name {
	case "login":
		return s.cmdLogin(ctx, sess, cmd)
	case "logout":
		sess.authed = false
		sess.username = ""
		return s.write(sess, errorLine(errOK, "ok"))
	case "quit":
		s.write(sess, errorLine(errOK, "ok"))
		return false
	case "help":
		return s.cmdHelp(sess)
	case "version":
		return s.write(sess, "version="+Version+"\n"+errorLine(errOK, "ok"))
	case "clientlist":
		return s.cmdClientlist(ctx, sess)
	case "channellist":
		return s.cmdChannellist(ctx, sess)
	case "serverinfo":
		return s.cmdServerinfo(ctx, sess)
	case "clientmove":
		return s.cmdClientmove(ctx, sess, cmd)
	case "clientkick":
		return s.cmdClientkick(ctx, sess, cmd)
	case "sendtextmessage":
		return s.cmdSendtextmessage(ctx, sess, cmd)
	case "channelcreate":
		return s.cmdChannelcreate(ctx, sess, cmd)
	case "channeldelete":
		return s.cmdChanneldelete(ctx, sess, cmd)
	case "banclient":
		return s.cmdBanclient(ctx, sess, cmd)
	case "complaintlist":
		return s.cmdComplaintlist(ctx, sess)
	case "complaintdel":
		return s.cmdComplaintdel(ctx, sess, cmd)
	case "complaintdelall":
		return s.cmdComplaintdelall(ctx, sess)
	case "tokenadd":
		return s.cmdTokenadd(ctx, sess, cmd)
	case "tokenlist":
		return s.cmdTokenlist(ctx, sess)
	case "tokendelete":
		return s.cmdTokendelete(ctx, sess, cmd)
	default:
		return s.write(sess, errorLine(errUnknownCommand, "unknown command: "+cmd.name))
	}
}

// write sends lines to the client. It returns false when the write fails.
func (s *Server) write(sess *session, text string) bool {
	_, err := io.WriteString(sess.conn, text)
	return err == nil
}

// cmdLogin authenticates the session. Brute-force protection: after
// MaxLoginFailures failed attempts from one IP, logins from it are refused
// for LockoutDuration.
func (s *Server) cmdLogin(ctx context.Context, sess *session, cmd command) bool {
	if s.lockedOut(sess.remoteIP) {
		return s.write(sess, errorLine(errLoginFailed, "too many failed logins, try again later"))
	}
	if len(cmd.positional) < 2 {
		return s.write(sess, errorLine(errInvalidParameter, "usage: login <unique_id> <password>"))
	}
	uniqueID, password := cmd.positional[0], cmd.positional[1]

	ok, admin, err := s.backend.Authenticate(ctx, uniqueID, password)
	if err != nil {
		s.logger.Warn("query login error", zap.Error(err))
		return s.write(sess, errorLine(errServerError, "internal error"))
	}
	if !ok {
		s.recordLoginFailure(sess.remoteIP)
		return s.write(sess, errorLine(errLoginFailed, "invalid loginname or password"))
	}
	if !admin {
		// Only server admins may use ServerQuery.
		return s.write(sess, errorLine(errInsufficientPermissions, "insufficient permissions"))
	}

	s.clearLoginFailures(sess.remoteIP)
	sess.authed = true
	sess.username = uniqueID
	s.logger.Info("query client logged in",
		zap.String("unique_id", uniqueID),
		zap.String("remote", sess.remoteIP),
	)
	return s.write(sess, errorLine(errOK, "ok"))
}

func (s *Server) lockedOut(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.lockouts[ip]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(s.lockouts, ip)
		return false
	}
	return true
}

func (s *Server) recordLoginFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginFails[ip]++
	if s.loginFails[ip] >= s.MaxLoginFailures {
		s.lockouts[ip] = time.Now().Add(s.LockoutDuration)
		delete(s.loginFails, ip)
		s.logger.Warn("query login lockout", zap.String("ip", ip))
	}
}

func (s *Server) clearLoginFailures(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.loginFails, ip)
	delete(s.lockouts, ip)
}

// cmdClientlist lists online clients.
func (s *Server) cmdClientlist(ctx context.Context, sess *session) bool {
	var b strings.Builder
	for _, c := range s.backend.ListClients(ctx) {
		fmt.Fprintf(&b, "clid=%s client_unique_identifier=%s client_nickname=%s cid=%d\n",
			escape(c.ClientID), escape(c.UniqueID), escape(c.Nickname), c.ChannelID)
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdChannellist lists channels.
func (s *Server) cmdChannellist(ctx context.Context, sess *session) bool {
	var b strings.Builder
	for _, ch := range s.backend.ListChannels(ctx) {
		fmt.Fprintf(&b, "cid=%d pid=%d channel_name=%s channel_type=%d total_clients=%d\n",
			ch.ChannelID, ch.ParentID, escape(ch.Name), ch.Type, ch.ClientCount)
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdServerinfo reports server-level information.
func (s *Server) cmdServerinfo(ctx context.Context, sess *session) bool {
	info := s.backend.ServerInfo(ctx)
	line := fmt.Sprintf("virtualserver_name=%s virtualserver_uptime=%d virtualserver_clientsonline=%d virtualserver_maxclients=%d virtualserver_channels_online=%d\n",
		escape(info.Name), int64(info.Uptime.Seconds()), info.ClientsOnline, info.MaxClients, info.ChannelsOnline)
	return s.write(sess, line+errorLine(errOK, "ok"))
}

// cmdClientmove moves a client: clientmove clid=<id> cid=<channel_id>
func (s *Server) cmdClientmove(ctx context.Context, sess *session, cmd command) bool {
	clid := cmd.args["clid"]
	cid, err := strconv.ParseInt(cmd.args["cid"], 10, 64)
	if clid == "" || err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: clientmove clid=<id> cid=<channel_id>"))
	}
	if err := s.backend.MoveClient(ctx, clid, cid); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdClientkick kicks a client: clientkick clid=<id> reasonid=<4|5> [reasonmsg=<text>]
// reasonid 4 = from channel, 5 = from server.
func (s *Server) cmdClientkick(ctx context.Context, sess *session, cmd command) bool {
	clid := cmd.args["clid"]
	reasonID, err := strconv.Atoi(cmd.args["reasonid"])
	if clid == "" || (err != nil || (reasonID != 4 && reasonID != 5)) {
		return s.write(sess, errorLine(errInvalidParameter, "usage: clientkick clid=<id> reasonid=<4|5> [reasonmsg=<text>]"))
	}
	if err := s.backend.KickClient(ctx, clid, reasonID == 5, cmd.args["reasonmsg"]); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdSendtextmessage injects a chat message:
// sendtextmessage targetmode=<1|2|3> target=<id> msg=<text>
func (s *Server) cmdSendtextmessage(ctx context.Context, sess *session, cmd command) bool {
	targetMode, err := strconv.Atoi(cmd.args["targetmode"])
	msg := cmd.args["msg"]
	if err != nil || msg == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: sendtextmessage targetmode=<1|2|3> target=<id> msg=<text>"))
	}
	if err := s.backend.SendText(ctx, targetMode, cmd.args["target"], msg); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdChannelcreate creates a channel:
// channelcreate channel_name=<name> [channel_topic=<t>]
// [channel_flag_permanent=1 | channel_flag_semi_permanent=1]
// Default type is temporary. It returns cid=<id>.
func (s *Server) cmdChannelcreate(ctx context.Context, sess *session, cmd command) bool {
	name := cmd.args["channel_name"]
	if name == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: channelcreate channel_name=<name> [channel_topic=<t>] [channel_flag_permanent=1|channel_flag_semi_permanent=1]"))
	}
	channelType := 0
	if cmd.args["channel_flag_semi_permanent"] == "1" {
		channelType = 1
	}
	if cmd.args["channel_flag_permanent"] == "1" {
		channelType = 2
	}
	id, err := s.backend.CreateChannel(ctx, name, cmd.args["channel_topic"], channelType)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, fmt.Sprintf("cid=%d\n", id)+errorLine(errOK, "ok"))
}

// cmdChanneldelete deletes a channel: channeldelete cid=<id> [force=1]
// force is accepted for TS3 compatibility; deletion always cascades.
func (s *Server) cmdChanneldelete(ctx context.Context, sess *session, cmd command) bool {
	cid, err := strconv.ParseInt(cmd.args["cid"], 10, 64)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: channeldelete cid=<id> [force=1]"))
	}
	if err := s.backend.DeleteChannel(ctx, cid); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdBanclient bans and kicks a client:
// banclient clid=<id> [time=<seconds>] [banreason=<text>]
func (s *Server) cmdBanclient(ctx context.Context, sess *session, cmd command) bool {
	clid := cmd.args["clid"]
	if clid == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: banclient clid=<id> [time=<seconds>] [banreason=<text>]"))
	}
	var seconds int64
	if t := cmd.args["time"]; t != "" {
		var err error
		seconds, err = strconv.ParseInt(t, 10, 64)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid time parameter"))
		}
	}
	if err := s.backend.BanClient(ctx, clid, seconds, cmd.args["banreason"]); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdHelp lists the available commands.
func (s *Server) cmdHelp(sess *session) bool {
	return s.write(sess, helpText+errorLine(errOK, "ok"))
}

// cmdComplaintlist lists all complaints.
func (s *Server) cmdComplaintlist(ctx context.Context, sess *session) bool {
	complaints, err := s.backend.ListComplaints(ctx)
	if err != nil {
		return s.write(sess, errorLine(errServerError, "listing complaints failed"))
	}
	var b strings.Builder
	for _, c := range complaints {
		fmt.Fprintf(&b, "id=%d reporter=%s target=%s reason=%s created_at=%d\n",
			c.ID, escape(c.Reporter), escape(c.Target), escape(c.Reason), c.CreatedAt.Unix())
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdComplaintdel deletes one complaint: complaintdel id=<id>
func (s *Server) cmdComplaintdel(ctx context.Context, sess *session, cmd command) bool {
	id, err := strconv.ParseInt(cmd.args["id"], 10, 64)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: complaintdel id=<id>"))
	}
	if err := s.backend.DeleteComplaint(ctx, id); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdComplaintdelall deletes all complaints.
func (s *Server) cmdComplaintdelall(ctx context.Context, sess *session) bool {
	if err := s.backend.DeleteAllComplaints(ctx); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdTokenadd creates a privilege token:
// tokenadd [tokentype=0] tokenid1=<group_id>
// tokenid1=0 (or omitted) creates an admin-grant token. It returns token=<key>.
func (s *Server) cmdTokenadd(ctx context.Context, sess *session, cmd command) bool {
	tokenType := 0
	if v := cmd.args["tokentype"]; v != "" {
		var err error
		tokenType, err = strconv.Atoi(v)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid tokentype"))
		}
	}
	groupID, err := strconv.ParseInt(cmd.args["tokenid1"], 10, 64)
	if cmd.args["tokenid1"] != "" && err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "invalid tokenid1 (group id)"))
	}
	key, err := s.backend.TokenAdd(ctx, tokenType, groupID)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, "token="+escape(key)+"\n"+errorLine(errOK, "ok"))
}

// cmdTokenlist lists all privilege tokens.
func (s *Server) cmdTokenlist(ctx context.Context, sess *session) bool {
	tokens, err := s.backend.TokenList(ctx)
	if err != nil {
		return s.write(sess, errorLine(errServerError, "listing tokens failed"))
	}
	var b strings.Builder
	for _, t := range tokens {
		fmt.Fprintf(&b, "token=%s token_type=%d group_id=%d uses=%d max_uses=%d\n",
			escape(t.Key), t.Type, t.GroupID, t.Uses, t.MaxUses)
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdTokendelete deletes a token: tokendelete token=<key>
func (s *Server) cmdTokendelete(ctx context.Context, sess *session, cmd command) bool {
	key := cmd.args["token"]
	if key == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: tokendelete token=<key>"))
	}
	if err := s.backend.TokenDelete(ctx, key); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// helpText documents the command set.
const helpText = `available commands:
login <unique_id> <password>
logout
quit
help
version
clientlist
channellist
serverinfo
clientmove clid=<id> cid=<channel_id>
clientkick clid=<id> reasonid=<4|5> [reasonmsg=<text>]
sendtextmessage targetmode=<1|2|3> target=<id> msg=<text>
channelcreate channel_name=<name> [channel_topic=<t>] [channel_flag_permanent=1|channel_flag_semi_permanent=1]
channeldelete cid=<id> [force=1]
banclient clid=<id> [time=<seconds>] [banreason=<text>]
complaintlist
complaintdel id=<id>
complaintdelall
tokenadd [tokentype=0] tokenid1=<group_id>
tokenlist
tokendelete token=<key>
`
