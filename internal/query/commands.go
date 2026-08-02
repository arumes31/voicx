// commands.go implements the ServerQuery command set: parsing, the auth
// gate, and one handler per command.
package query

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// session holds per-connection query state. It is transport-agnostic (224):
// the raw TCP port and the SSH transport differ only in the reader/writer and
// in whether read deadlines are available.
type session struct {
	r *bufio.Reader
	w io.Writer
	// setReadDeadline arms the idle timeout; nil when the transport has no
	// deadline of its own.
	setReadDeadline func(time.Time) error
	remoteIP        string
	authed          bool
	username        string
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
	case "channelinfo":
		return s.cmdChannelinfo(ctx, sess, cmd)
	case "channeledit":
		return s.cmdChanneledit(ctx, sess, cmd)
	case "serverset":
		return s.cmdServerset(ctx, sess, cmd)
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
	case "auditlog":
		return s.cmdAuditlog(ctx, sess, cmd)
	case "serveredit":
		return s.cmdServeredit(ctx, sess, cmd)
	case "serverstop":
		return s.cmdServerstop(ctx, sess, cmd, false)
	case "serverrestart":
		return s.cmdServerstop(ctx, sess, cmd, true)
	case "permoverview":
		return s.cmdPermoverview(ctx, sess, cmd)
	case "channelpermlist":
		return s.cmdChannelpermlist(ctx, sess, cmd)
	case "channeladdperm":
		return s.cmdChanneladdperm(ctx, sess, cmd)
	case "channeldelperm":
		return s.cmdChanneldelperm(ctx, sess, cmd)
	case "servergroupadd":
		return s.cmdServergroupadd(ctx, sess, cmd)
	case "servergroupdel":
		return s.cmdServergroupdel(ctx, sess, cmd)
	case "servergroupaddclient":
		return s.cmdServergroupaddclient(ctx, sess, cmd)
	case "servergroupdelclient":
		return s.cmdServergroupdelclient(ctx, sess, cmd)
	case "servergrouplist":
		return s.cmdServergrouplist(ctx, sess)
	case "servergroupclientlist":
		return s.cmdServergroupclientlist(ctx, sess, cmd)
	case "customset":
		return s.cmdCustomset(ctx, sess, cmd)
	case "customdel":
		return s.cmdCustomdel(ctx, sess, cmd)
	case "custominfo":
		return s.cmdCustominfo(ctx, sess, cmd)
	case "logview":
		return s.cmdLogview(ctx, sess, cmd)
	case "serverrules":
		return s.cmdServerrules(ctx, sess)
	default:
		return s.write(sess, errorLine(errUnknownCommand, "unknown command: "+cmd.name))
	}
}

// write sends lines to the client. It returns false when the write fails.
func (s *Server) write(sess *session, text string) bool {
	_, err := io.WriteString(sess.w, text)
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
	line := fmt.Sprintf("virtualserver_name=%s virtualserver_uptime=%d virtualserver_clientsonline=%d virtualserver_maxclients=%d virtualserver_channels_online=%d",
		escape(info.Name), int64(info.Uptime.Seconds()), info.ClientsOnline, info.MaxClients, info.ChannelsOnline)
	if info.TLSFingerprint != "" {
		line += fmt.Sprintf(" virtualserver_tls_fingerprint=%s", info.TLSFingerprint)
	}
	return s.write(sess, line+"\n"+errorLine(errOK, "ok"))
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

// cmdChannelinfo shows one channel's details: channelinfo cid=<id>
func (s *Server) cmdChannelinfo(ctx context.Context, sess *session, cmd command) bool {
	cid, err := strconv.ParseInt(cmd.args["cid"], 10, 64)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: channelinfo cid=<id>"))
	}
	ch, ok := s.backend.ChannelInfo(ctx, cid)
	if !ok {
		return s.write(sess, errorLine(errInvalidParameter, "channel not found"))
	}
	line := fmt.Sprintf("cid=%d pid=%d channel_name=%s channel_topic=%s channel_type=%d channel_maxclients=%d total_clients=%d opus_bitrate=%d opus_fec=%d opus_dtx=%d opus_stereo=%d slow_mode_seconds=%d\n",
		ch.ChannelID, ch.ParentID, escape(ch.Name), escape(ch.Topic), ch.Type, ch.MaxClients, ch.ClientCount,
		ch.OpusBitrate, boolInt(ch.OpusFEC), boolInt(ch.OpusDTX), boolInt(ch.OpusStereo), ch.SlowModeSeconds)
	return s.write(sess, line+errorLine(errOK, "ok"))
}

// cmdChanneledit edits a channel:
// channeledit cid=<id> [channel_topic=<t>] [channel_maxclients=<n>]
// [opus_bitrate=<bps>] [opus_fec=0|1] [opus_dtx=0|1] [opus_stereo=0|1]
// [channel_needed_join_power=<n>] [channel_order=<n>] [cpid=<id>]
// [channel_inherit_permissions=0|1]
func (s *Server) cmdChanneledit(ctx context.Context, sess *session, cmd command) bool {
	cid, err := strconv.ParseInt(cmd.args["cid"], 10, 64)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: "+channeleditUsage))
	}
	var params ChannelEditParams
	if v, ok := cmd.args["channel_topic"]; ok {
		params.Topic = &v
	}
	if v, ok := cmd.args["channel_maxclients"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid channel_maxclients"))
		}
		params.MaxClients = &n
	}
	if v, ok := cmd.args["opus_bitrate"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid opus_bitrate"))
		}
		params.OpusBitrate = &n
	}
	if v, ok := cmd.args["slow_mode_seconds"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid slow_mode_seconds"))
		}
		params.SlowModeSeconds = &n
	}
	if v, ok := cmd.args["channel_needed_join_power"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid channel_needed_join_power"))
		}
		params.NeededJoinPower = &n
	}
	if v, ok := cmd.args["channel_order"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid channel_order"))
		}
		params.OrderIndex = &n
	}
	// cpid is the TS3 name for the parent; 0 moves the channel to the root (168).
	if v, ok := cmd.args["cpid"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid cpid"))
		}
		params.ParentID = &n
	}
	for _, b := range []struct {
		key string
		dst **bool
	}{
		{"opus_fec", &params.OpusFEC},
		{"opus_dtx", &params.OpusDTX},
		{"opus_stereo", &params.OpusStereo},
		{"channel_inherit_permissions", &params.InheritPermissions},
	} {
		if v, ok := cmd.args[b.key]; ok {
			val, err := parseBoolArg(v)
			if err != nil {
				return s.write(sess, errorLine(errInvalidParameter, "invalid "+b.key+" (want 0 or 1)"))
			}
			*b.dst = &val
		}
	}
	if err := s.backend.EditChannel(ctx, cid, params); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// boolInt renders a bool as 0/1 for TS3-style output.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// parseBoolArg parses a TS3-style boolean argument ("0"/"1").
func parseBoolArg(v string) (bool, error) {
	switch v {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, strconv.ErrSyntax
	}
}

// chatFiltersDoc mirrors the moderation document the chat server stores under
// "chat_filters" (117/118). It exists here only to reject a malformed value:
// the server falls back to the config.yaml lists when the stored document does
// not parse, so a typo would silently stop applying the operator's filters.
type chatFiltersDoc struct {
	WordFilter    string `json:"word_filter"`
	LinkBlacklist string `json:"link_blacklist"`
	LinkWhitelist string `json:"link_whitelist"`
}

// chatFiltersUsage is the error text for a bad chat_filters document.
const chatFiltersUsage = `chat_filters value must be a JSON document with the three comma-separated lists, e.g. {"word_filter":"badword","link_blacklist":"evil.tld","link_whitelist":""}`

// validateChatFilters parses value as a chat_filters document. Unknown fields
// are rejected: a misspelled key would otherwise decode into an empty document
// and clear all three lists.
func validateChatFilters(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "null" {
		return fmt.Errorf("%s: invalid json document", chatFiltersUsage)
	}
	dec := json.NewDecoder(strings.NewReader(value))
	dec.DisallowUnknownFields()
	var doc chatFiltersDoc
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("%s: %w", chatFiltersUsage, err)
	}
	if dec.More() {
		return fmt.Errorf("%s: trailing data after json document", chatFiltersUsage)
	}
	return nil
}

// cmdServerset stores a server setting (wave 5a: motd, announcement; 117/118:
// chat_filters; 215: server_rules):
// serverset key=<motd|announcement|server_rules|chat_filters> value=<text>
// Setting "announcement" also broadcasts it to all online clients; setting
// chat_filters drops the server's memoised lists, so it takes effect on the
// next message rather than at the next restart.
func (s *Server) cmdServerset(ctx context.Context, sess *session, cmd command) bool {
	key := cmd.args["key"]
	value := cmd.args["value"]
	switch key {
	case "motd", "announcement":
	// (215) rules are operator content shown on first join; an empty value
	// clears them and stops the dialog being asked.
	case "server_rules":
	case "chat_filters":
		if err := validateChatFilters(value); err != nil {
			return s.write(sess, errorLine(errInvalidParameter, err.Error()))
		}
	default:
		return s.write(sess, errorLine(errInvalidParameter, "usage: serverset key=<motd|announcement|server_rules|chat_filters> value=<text>"))
	}
	if err := s.backend.ServerSet(ctx, key, value); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
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

// cmdAuditlog lists the newest audit entries: auditlog [limit=<n>]
func (s *Server) cmdAuditlog(ctx context.Context, sess *session, cmd command) bool {
	limit := 0
	if v := cmd.args["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return s.write(sess, errorLine(errInvalidParameter, "invalid limit"))
		}
		limit = n
	}
	entries, err := s.backend.AuditLog(ctx, limit)
	if err != nil {
		return s.write(sess, errorLine(errServerError, "listing audit log failed"))
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "id=%d actor=%s action=%s target=%s detail=%s created_at=%d\n",
			e.ID, escape(e.Actor), escape(e.Action), escape(e.Target), escape(e.Detail), e.CreatedAt.Unix())
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// channeleditUsage is shared by the help text and the parameter errors so the
// two cannot drift as fields are added.
const channeleditUsage = "channeledit cid=<id> [channel_topic=<t>] [channel_maxclients=<n>] [opus_bitrate=<bps>] [opus_fec=0|1] [opus_dtx=0|1] [opus_stereo=0|1] [slow_mode_seconds=<n>] [channel_needed_join_power=<n>] [channel_order=<n>] [cpid=<id>] [channel_inherit_permissions=0|1]"

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
channelinfo cid=<id>
` + channeleditUsage + `
serverset key=<motd|announcement|server_rules|chat_filters> value=<text>
serverrules
banclient clid=<id> [time=<seconds>] [banreason=<text>]
complaintlist
complaintdel id=<id>
complaintdelall
tokenadd [tokentype=0] tokenid1=<group_id>
tokenlist
tokendelete token=<key>
auditlog [limit=<n>]
serveredit [virtualserver_name=<n>] [virtualserver_welcomemessage=<m>] [virtualserver_maxclients=<n>]
serverstop confirm=1
serverrestart confirm=1
permoverview unique_id=<id> [cid=<channel_id>]
channelpermlist cid=<id>
channeladdperm cid=<id> permid=<key> permvalue=<v> [permgrant=<g>] [permskip=0|1] [permnegate=0|1]
channeldelperm cid=<id> permid=<key>
servergroupadd name=<n> [sortid=<i>]
servergroupdel sgid=<id> [force=1]
servergroupaddclient sgid=<id> cldbid=<unique_id> [duration=<seconds>]
servergroupdelclient sgid=<id> cldbid=<unique_id>
servergrouplist
servergroupclientlist sgid=<id>
customset cldbid=<unique_id> ident=<key> value=<v>
customdel cldbid=<unique_id> ident=<key>
custominfo cldbid=<unique_id>
logview [lines=<n>] [filter=<substr>] [follow=<seconds>]
`

// ---------------------------------------------------------------------------
// Wave 10a: server management commands (217-223)
// ---------------------------------------------------------------------------

// cmdServeredit updates server name / welcome message / max-clients override
// (217): serveredit [virtualserver_name=<n>] [virtualserver_welcomemessage=<m>]
// [virtualserver_maxclients=<n>].
//
// A field counts as edited when the KEY is present, not when its value is
// non-empty: passing an empty value is the only way to drop an override and
// fall back to the config.yaml default.
func (s *Server) cmdServeredit(ctx context.Context, sess *session, cmd command) bool {
	var params ServerEditParams
	if v, ok := cmd.args["virtualserver_name"]; ok {
		params.Name = &v
	}
	if v, ok := cmd.args["virtualserver_welcomemessage"]; ok {
		params.Welcome = &v
	}
	if v, ok := cmd.args["virtualserver_maxclients"]; ok {
		n := 0
		if v != "" {
			var err error
			n, err = strconv.Atoi(v)
			if err != nil || n < 0 {
				return s.write(sess, errorLine(errInvalidParameter, "invalid virtualserver_maxclients"))
			}
		}
		params.MaxClients = &n
	}
	if params.Name == nil && params.Welcome == nil && params.MaxClients == nil {
		return s.write(sess, errorLine(errInvalidParameter,
			"usage: serveredit [virtualserver_name=<n>] [virtualserver_welcomemessage=<m>] [virtualserver_maxclients=<n>]"))
	}
	if err := s.backend.ServerEdit(ctx, params); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdServerstop handles serverstop and serverrestart (218); both require
// confirm=1. serverrestart is a graceful stop that a supervisor restarts.
func (s *Server) cmdServerstop(ctx context.Context, sess *session, cmd command, restart bool) bool {
	if cmd.args["confirm"] != "1" {
		verb := "serverstop"
		if restart {
			verb = "serverrestart"
		}
		return s.write(sess, errorLine(errInvalidParameter, verb+" requires confirm=1"))
	}
	ok := s.write(sess, errorLine(errOK, "ok"))
	if err := s.backend.Shutdown(ctx, restart); err != nil {
		s.logger.Warn("shutdown via query failed", zap.Error(err))
	}
	return ok
}

// cmdPermoverview lists a user's resolved permissions (219):
// permoverview <unique_id> [cid=<channel_id>]
func (s *Server) cmdPermoverview(ctx context.Context, sess *session, cmd command) bool {
	uid := cmd.args["unique_id"]
	if uid == "" && len(cmd.positional) > 0 {
		uid = cmd.positional[0]
	}
	if uid == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: permoverview unique_id=<id> [cid=<channel_id>]"))
	}
	var channelID int64
	if v := cmd.args["cid"]; v != "" {
		var err error
		channelID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid cid"))
		}
	}
	lines, err := s.backend.PermOverview(ctx, uid, channelID)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	var b strings.Builder
	for _, p := range lines {
		fmt.Fprintf(&b, "permid=%s permvalue=%d permgrant=%d tier=%s\n",
			escape(p.Key), p.Value, p.Grant, escape(p.Tier))
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdChannelpermlist lists a channel's tier permissions (220).
func (s *Server) cmdChannelpermlist(ctx context.Context, sess *session, cmd command) bool {
	cid, err := strconv.ParseInt(cmd.args["cid"], 10, 64)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: channelpermlist cid=<id>"))
	}
	perms, err := s.backend.ChannelPermList(ctx, cid)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	var b strings.Builder
	for _, p := range perms {
		fmt.Fprintf(&b, "cid=%d permid=%s permvalue=%d permgrant=%d permskip=%d permnegate=%d\n",
			cid, escape(p.Key), p.Value, p.Grant, boolInt(p.Skip), boolInt(p.Negate))
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdChanneladdperm writes a channel-tier permission (220):
// channeladdperm cid=<id> permid=<key> permvalue=<v> [permgrant=<g>]
// [permskip=0|1] [permnegate=0|1]
func (s *Server) cmdChanneladdperm(ctx context.Context, sess *session, cmd command) bool {
	cid, err := strconv.ParseInt(cmd.args["cid"], 10, 64)
	if err != nil || cmd.args["permid"] == "" || cmd.args["permvalue"] == "" {
		return s.write(sess, errorLine(errInvalidParameter,
			"usage: channeladdperm cid=<id> permid=<key> permvalue=<v> [permgrant=<g>] [permskip=0|1] [permnegate=0|1]"))
	}
	if !knownPermKey(cmd.args["permid"]) {
		return s.write(sess, errorLine(errInvalidParameter, "unknown permid: "+cmd.args["permid"]))
	}
	value, err := strconv.Atoi(cmd.args["permvalue"])
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "invalid permvalue"))
	}
	grant := 0
	if v := cmd.args["permgrant"]; v != "" {
		if grant, err = strconv.Atoi(v); err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid permgrant"))
		}
	}
	skip := cmd.args["permskip"] == "1"
	negate := cmd.args["permnegate"] == "1"
	if err := s.backend.ChannelAddPerm(ctx, sess.username, cid, cmd.args["permid"], value, grant, skip, negate); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdChanneldelperm removes a channel-tier permission (220).
func (s *Server) cmdChanneldelperm(ctx context.Context, sess *session, cmd command) bool {
	cid, err := strconv.ParseInt(cmd.args["cid"], 10, 64)
	if err != nil || cmd.args["permid"] == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: channeldelperm cid=<id> permid=<key>"))
	}
	if err := s.backend.ChannelDelPerm(ctx, sess.username, cid, cmd.args["permid"]); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdServergroupadd creates a server group (221).
func (s *Server) cmdServergroupadd(ctx context.Context, sess *session, cmd command) bool {
	name := cmd.args["name"]
	if name == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: servergroupadd name=<n> [sortid=<i>]"))
	}
	sortID := 0
	if v := cmd.args["sortid"]; v != "" {
		var err error
		if sortID, err = strconv.Atoi(v); err != nil {
			return s.write(sess, errorLine(errInvalidParameter, "invalid sortid"))
		}
	}
	id, err := s.backend.ServerGroupAdd(ctx, sess.username, name, sortID)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, fmt.Sprintf("sgid=%d\n", id)+errorLine(errOK, "ok"))
}

// cmdServergroupdel deletes a server group (221; force=1 with members).
func (s *Server) cmdServergroupdel(ctx context.Context, sess *session, cmd command) bool {
	sgid, err := strconv.ParseInt(cmd.args["sgid"], 10, 64)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: servergroupdel sgid=<id> [force=1]"))
	}
	if err := s.backend.ServerGroupDel(ctx, sess.username, sgid, cmd.args["force"] == "1"); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdServergroupaddclient assigns a user to a server group (221).
func (s *Server) cmdServergroupaddclient(ctx context.Context, sess *session, cmd command) bool {
	sgid, err := strconv.ParseInt(cmd.args["sgid"], 10, 64)
	uid := cmd.args["cldbid"]
	if err != nil || uid == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: servergroupaddclient sgid=<id> cldbid=<unique_id> [duration=<seconds>]"))
	}
	var duration int64
	if v := cmd.args["duration"]; v != "" {
		if duration, err = strconv.ParseInt(v, 10, 64); err != nil || duration < 0 {
			return s.write(sess, errorLine(errInvalidParameter, "invalid duration"))
		}
	}
	if err := s.backend.ServerGroupAddClient(ctx, sess.username, sgid, uid, duration); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdServergroupdelclient removes a user from a server group (221).
func (s *Server) cmdServergroupdelclient(ctx context.Context, sess *session, cmd command) bool {
	sgid, err := strconv.ParseInt(cmd.args["sgid"], 10, 64)
	uid := cmd.args["cldbid"]
	if err != nil || uid == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: servergroupdelclient sgid=<id> cldbid=<unique_id>"))
	}
	if err := s.backend.ServerGroupDelClient(ctx, sess.username, sgid, uid); err != nil {
		return s.write(sess, errorLine(errInvalidParameter, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdServergrouplist lists server groups (221).
func (s *Server) cmdServergrouplist(ctx context.Context, sess *session) bool {
	groups, err := s.backend.ServerGroupList(ctx)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	var b strings.Builder
	for _, g := range groups {
		fmt.Fprintf(&b, "sgid=%d name=%s sortid=%d membercount=%d\n", g.ID, escape(g.Name), g.SortID, g.MemberCount)
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdServergroupclientlist lists a server group's members (221).
func (s *Server) cmdServergroupclientlist(ctx context.Context, sess *session, cmd command) bool {
	sgid, err := strconv.ParseInt(cmd.args["sgid"], 10, 64)
	if err != nil {
		return s.write(sess, errorLine(errInvalidParameter, "usage: servergroupclientlist sgid=<id>"))
	}
	members, err := s.backend.ServerGroupClientList(ctx, sgid)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	var b strings.Builder
	for _, m := range members {
		fmt.Fprintf(&b, "cldbid=%s nickname=%s expires_at=%d\n", escape(m.UniqueID), escape(m.Nickname), m.ExpiresAt)
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdCustomset sets a custom client property (222).
func (s *Server) cmdCustomset(ctx context.Context, sess *session, cmd command) bool {
	uid, key, value := cmd.args["cldbid"], cmd.args["ident"], cmd.args["value"]
	if uid == "" || key == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: customset cldbid=<unique_id> ident=<key> value=<v>"))
	}
	if err := s.backend.CustomSet(ctx, uid, key, value); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdCustomdel removes a custom client property (222).
func (s *Server) cmdCustomdel(ctx context.Context, sess *session, cmd command) bool {
	uid, key := cmd.args["cldbid"], cmd.args["ident"]
	if uid == "" || key == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: customdel cldbid=<unique_id> ident=<key>"))
	}
	if err := s.backend.CustomDel(ctx, uid, key); err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// cmdCustominfo lists a client's custom properties (222).
func (s *Server) cmdCustominfo(ctx context.Context, sess *session, cmd command) bool {
	uid := cmd.args["cldbid"]
	if uid == "" {
		return s.write(sess, errorLine(errInvalidParameter, "usage: custominfo cldbid=<unique_id>"))
	}
	props, err := s.backend.CustomInfo(ctx, uid)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	var b strings.Builder
	for _, p := range props {
		fmt.Fprintf(&b, "ident=%s value=%s\n", escape(p.Key), escape(p.Value))
	}
	b.WriteString(errorLine(errOK, "ok"))
	return s.write(sess, b.String())
}

// cmdServerrules shows the rules new users must accept, the hash acceptances
// are keyed by, and how many users accepted the current wording (215). The
// text is written with `serverset key=server_rules value=<text>`.
func (s *Server) cmdServerrules(ctx context.Context, sess *session) bool {
	text, hash, accepted, err := s.backend.ServerRules(ctx)
	if err != nil {
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	line := fmt.Sprintf("rules=%s rules_hash=%s accepted_clients=%d\n",
		escape(text), escape(hash), accepted)
	return s.write(sess, line+errorLine(errOK, "ok"))
}

// maxFollowSeconds caps `logview follow` so a forgotten bot session cannot
// hold a query connection open forever.
const maxFollowSeconds = 3600

// cmdLogview returns recent server log lines (223):
// logview [lines=<n>] [filter=<substr>] [follow=<seconds>]
// follow keeps streaming lines as they are emitted for up to <seconds>, then
// closes the response normally; the connection stays usable afterwards.
func (s *Server) cmdLogview(ctx context.Context, sess *session, cmd command) bool {
	lines := 50
	if v := cmd.args["lines"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return s.write(sess, errorLine(errInvalidParameter, "invalid lines"))
		}
		lines = n
	}
	follow := 0
	if v := cmd.args["follow"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > maxFollowSeconds {
			return s.write(sess, errorLine(errInvalidParameter,
				fmt.Sprintf("invalid follow (want 1..%d seconds)", maxFollowSeconds)))
		}
		follow = n
	}
	filter := cmd.args["filter"]

	// Subscribe BEFORE reading the tail so nothing emitted between the two is
	// lost; the cost is that one line can repeat at the seam. The
	// subscription is released before the response is closed, so an idle
	// connection leaves no follower behind.
	var stream <-chan string
	cancel := func() {}
	if follow > 0 {
		stream, cancel = s.backend.LogFollow()
	}

	entries, err := s.backend.LogView(ctx, lines, filter)
	if err != nil {
		cancel()
		return s.write(sess, errorLine(errServerError, err.Error()))
	}
	var b strings.Builder
	for _, l := range entries {
		b.WriteString("line=" + escape(l) + "\n")
	}
	if !s.write(sess, b.String()) {
		cancel()
		return false
	}
	streamed := true
	if follow > 0 {
		streamed = s.streamLog(ctx, sess, stream, filter, time.Duration(follow)*time.Second)
	}
	cancel()
	if !streamed {
		return false
	}
	return s.write(sess, errorLine(errOK, "ok"))
}

// streamLog writes live log lines until the follow window closes or the server
// shuts down. It returns false when a write failed.
func (s *Server) streamLog(ctx context.Context, sess *session, stream <-chan string, filter string, window time.Duration) bool {
	deadline := time.NewTimer(window)
	defer deadline.Stop()
	lower := strings.ToLower(filter)
	for {
		select {
		case <-ctx.Done():
			return true
		case <-s.stopCh:
			return true
		case <-deadline.C:
			return true
		case line := <-stream:
			if lower != "" && !strings.Contains(strings.ToLower(line), lower) {
				continue
			}
			if !s.write(sess, "line="+escape(line)+"\n") {
				return false
			}
		}
	}
}
