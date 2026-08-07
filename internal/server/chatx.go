// chatx.go implements the wave-5a server-side chat infrastructure: the
// moderation pipeline for channel/global messages (rate limit → slow mode →
// decrypt → length/filters → spam → mentions → store → relay), history,
// edit/delete, pins, reactions, typing indicators, DM receipts, and custom
// emoji. Direct messages are exempt from moderation and storage: they are
// true E2EE (wave 4b) and their history is client-local by design.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
	"voicx/internal/store"
)

// Broadcast event types for the wave-5a chat features.
const (
	eventChatEdited   = "chat_edited"
	eventChatDeleted  = "chat_deleted"
	eventChatPinned   = "chat_pinned"
	eventChatUnpinned = "chat_unpinned"
	eventChatReaction = "chat_reaction"
	eventTyping       = "typing"
	eventDMDelivered  = "dm_delivered"
	eventDMRead       = "dm_read"
	eventEmojiAdded   = "emoji_added"
	eventEmojiRemoved = "emoji_removed"
	eventEmojiRenamed = "emoji_renamed"
	eventAnnouncement = "announcement"
)

// ---------------------------------------------------------------------------
// Rate limiting (115) + anti-spam (116) + slow mode (114)
// ---------------------------------------------------------------------------

// chatRateLimiter is a per-user token bucket for chat messages.
type chatRateLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	buckets map[string]*chatBucket
}

type chatBucket struct {
	tokens int
	reset  time.Time
}

func newChatRateLimiter(max int, window time.Duration) *chatRateLimiter {
	if max <= 0 {
		max = 5
	}
	if window <= 0 {
		window = 3 * time.Second
	}
	return &chatRateLimiter{max: max, window: window, buckets: map[string]*chatBucket{}}
}

// allow consumes one token; false means the user is over the rate limit.
func (l *chatRateLimiter) allow(uid string, now time.Time) bool {
	return l.allowLimit(uid, now, l.max)
}

func (l *chatRateLimiter) allowLimit(uid string, now time.Time, limit int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		limit = l.max
	}
	b, ok := l.buckets[uid]
	if !ok || now.After(b.reset) {
		b = &chatBucket{tokens: limit, reset: now.Add(l.window)}
		l.buckets[uid] = b
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

func (s *TCPServer) chatActionLimit(ctx context.Context, client *Client) int {
	limit := s.chatRate.max
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return limit
	}
	switch power := pc.power(permissions.PermissionKeyClientTalkPower); {
	case power >= 75:
		return limit * 8
	case power >= 50:
		return limit * 4
	case power >= 25:
		return limit * 2
	default:
		return limit
	}
}

// spamEntry is one recently seen message body for spam detection. Only the
// digest is retained: the heuristic compares bodies for exact equality, so a
// hash is a drop-in that keeps no plaintext in server memory (91).
type spamEntry struct {
	sum [32]byte
	at  time.Time
}

// spamTracker rejects the third identical message within 30 seconds (116).
type spamTracker struct {
	mu     sync.Mutex
	recent map[string][]spamEntry
}

func newSpamTracker() *spamTracker {
	return &spamTracker{recent: map[string][]spamEntry{}}
}

// record notes a message digest and reports whether it trips the spam
// heuristic.
func (t *spamTracker) record(uid string, sum [32]byte, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := now.Add(-30 * time.Second)
	keep := t.recent[uid][:0]
	same := 0
	for _, e := range t.recent[uid] {
		if e.at.Before(cutoff) {
			continue
		}
		keep = append(keep, e)
		if e.sum == sum {
			same++
		}
	}
	t.recent[uid] = append(keep, spamEntry{sum: sum, at: now})
	return same >= 2 // this is the 3rd identical message in 30s
}

// recentFor returns a copy of recent entries for a user under lock (for tests).
func (t *spamTracker) recentFor(uid string) []spamEntry {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entries, ok := t.recent[uid]
	if !ok {
		return nil
	}
	cp := make([]spamEntry, len(entries))
	copy(cp, entries)
	return cp
}

// bodyDigest is the spam tracker's key for a moderated body.
func bodyDigest(body string) [32]byte {
	return sha256.Sum256([]byte(body))
}

// attachmentRefRe matches the chat attachment token
// [file:<storage>#<base64 file key>#<display name>] (12).
var attachmentRefRe = regexp.MustCompile(`\[file:([^\]#]*)#([^\]#]*)#([^\]]*)\]`)

// stripAttachmentRefs rewrites attachment tokens to [file:<display name>].
// The per-file key is fresh random base64 on every paste, so without this no
// two image posts are ever equal and the "same message 3x in 30s" heuristic
// (116) stops catching image spam. The word filter still sees the file name.
func stripAttachmentRefs(body string) string {
	return attachmentRefRe.ReplaceAllString(body, "[file:$3]")
}

// slowTracker records each user's last send time per channel (114).
type slowTracker struct {
	mu   sync.Mutex
	last map[string]map[int64]time.Time
}

type typingTracker struct {
	mu          sync.Mutex
	last        map[string]time.Time
	lastCleanup time.Time
}

func newTypingTracker() *typingTracker { return &typingTracker{last: map[string]time.Time{}} }

func (t *typingTracker) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if previous := t.last[key]; !previous.IsZero() && now.Sub(previous) < 2*time.Second {
		return false
	}
	t.last[key] = now
	if len(t.last) > 10_000 && now.Sub(t.lastCleanup) >= 10*time.Second {
		for candidate, seen := range t.last {
			if now.Sub(seen) > 10*time.Second {
				delete(t.last, candidate)
			}
		}
		t.lastCleanup = now
	}
	return true
}

func newSlowTracker() *slowTracker {
	return &slowTracker{last: map[string]map[int64]time.Time{}}
}

// check returns the remaining wait when the user is inside the slow-mode
// window, recording the send otherwise (0 = allowed).
func (t *slowTracker) check(uid string, channelID int64, seconds int, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.last[uid]
	if !ok {
		m = map[int64]time.Time{}
		t.last[uid] = m
	}
	if prev, ok := m[channelID]; ok {
		wait := time.Duration(seconds)*time.Second - now.Sub(prev)
		if wait > 0 {
			return wait
		}
	}
	m[channelID] = now
	return 0
}

// ---------------------------------------------------------------------------
// The moderation pipeline
// ---------------------------------------------------------------------------

// routeScopedChat runs the channel/global chat pipeline. msg is already
// shape-validated (enc, key id) by handleChatSend.
func (s *TCPServer) routeScopedChat(ctx context.Context, client *Client, msg netproto.ChatSend, channelID int64) error {
	uid := client.UniqueID

	// (114) slow mode (channel scope; b_chat_slowmode_bypass or admins skip).
	if channelID != 0 && s.cfg != nil {
		if ch, ok := s.deps.State.GetChannel(channelID); ok && ch.SlowModeSeconds > 0 {
			privileged := false
			if pc, err := s.permCheckerFor(ctx, client); err == nil {
				privileged = pc.granted(permissions.PermissionKeyChatSlowmodeBypass)
			}
			if !privileged {
				if wait := s.chatSlow.check(uid, channelID, ch.SlowModeSeconds, time.Now()); wait > 0 {
					s.metricsSink().IncChatMessage("rejected")
					return s.sendError(client, errCodeMalformed,
						fmt.Sprintf("slow mode: wait %ds before your next message in this channel", int(wait.Seconds())+1))
				}
			}
		}
	}

	// Decrypt with the generation the sender named (validated by the caller).
	// The server holds the scope keys so it can moderate; the plaintext lives
	// only for the length of this function.
	plain := msg.Text
	if msg.Enc {
		p, err := s.chatKeys.open(ctx, channelID, msg.KeyID, msg.Text)
		if err != nil {
			return s.sendError(client, errCodeMalformed, "message decryption failed (stale key?)")
		}
		plain = p
	}

	// (640) cap UTF-8 bytes, not runes, so wire/storage cost is predictable.
	if s.cfg != nil && s.cfg.ChatMaxLength > 0 && len([]byte(plain)) > s.cfg.ChatMaxLength {
		s.metricsSink().IncChatMessage("rejected")
		return s.sendError(client, errCodeMalformed, fmt.Sprintf("message too long (max %d bytes)", s.cfg.ChatMaxLength))
	}

	// Attachment tokens carry a fresh random key per upload; the filters and
	// the spam heuristic see the display name instead (12/116).
	moderated := stripAttachmentRefs(plain)

	// (117/118) word + link filters.
	if err := s.moderateBody(ctx, moderated); err != nil {
		s.metricsSink().IncChatMessage("rejected")
		return s.sendError(client, errCodeMalformed, err.Error())
	}

	// (116) anti-spam: identical message x3 in 30s.
	if s.chatSpam != nil && s.chatSpam.record(uid, bodyDigest(moderated), time.Now()) {
		s.metricsSink().IncChatMessage("rejected")
		return s.sendError(client, errCodeMalformed, "possible spam detected — please vary your messages")
	}
	// (105) mention parsing.
	mentions := s.parseMentions(ctx, client, channelID, plain)

	// (91) store the sender's ORIGINAL ciphertext verbatim: handleChatSend has
	// already proved the key id is current, so the bytes that were moderated
	// are exactly the bytes that are stored. A message that arrived through
	// the plaintext escape hatch is sealed here, so neither storage nor the
	// relay is ever plaintext.
	bodyEnc, keyID := msg.Text, msg.KeyID
	if !msg.Enc {
		// The sender is provably in this scope (handleChatSend checked
		// membership, and everyone is in the global scope), so ensuring the
		// scope's first generation here is authorised.
		if _, _, err := s.chatKeys.EnsureScope(ctx, channelID); err != nil {
			return s.sendError(client, errCodeUnavailable, "chat key unavailable")
		}
		id, ct, err := s.chatKeys.seal(ctx, channelID, plain)
		if err != nil {
			return s.sendError(client, errCodeUnavailable, "chat key unavailable")
		}
		bodyEnc, keyID = ct, id
	}

	var messageID int64
	if s.deps.Chat != nil {
		if msg.ReplyToID != 0 {
			parent, err := s.deps.Chat.GetChatMessage(ctx, msg.ReplyToID)
			if err != nil {
				return s.sendError(client, errCodeUnavailable, "reply target lookup failed")
			}
			if parent == nil || parent.DeletedAt != nil || parent.ChannelID != channelID {
				return s.sendError(client, errCodeMalformed, "reply target is not in this chat")
			}
		}
		id, inserted, err := s.deps.Chat.StoreChatMessage(ctx, channelID, uid, client.Username, bodyEnc, keyID, msg.ReplyToID, msg.ClientMsgID)
		if err != nil {
			// Relaying anyway would turn a constraint violation into
			// invisible, indefinite history loss; fail the send instead.
			s.logger.Warn("storing chat message failed", zap.Error(err))
			s.metricsSink().IncChatMessage("rejected")
			return s.sendError(client, errCodeUnavailable, "message not stored — not delivered")
		}
		messageID = id
		if !inserted {
			return nil
		}
	}

	// Relay the ciphertext (clients decrypt with the scope key). The relay is
	// encrypted regardless of chat_allow_plaintext, so the wire format is
	// uniform and no config can fan plaintext out to a scope.
	var channelIDStr string
	if channelID != 0 {
		channelIDStr = strconv.FormatInt(channelID, 10)
	}
	chat := netproto.ChatBroadcast{
		ChannelID:    channelIDStr,
		FromClientID: client.ID,
		FromUniqueID: uid,
		From:         client.Username,
		Text:         bodyEnc,
		Enc:          true,
		KeyID:        keyID,
		ID:           messageID,
		ReplyToID:    msg.ReplyToID,
		Version:      1,
		Mentions:     mentions,
		ClientMsgID:  msg.ClientMsgID,
	}
	payload, err := eventEnvelope(eventChat, chat)
	if err != nil {
		return err
	}
	if channelID == 0 {
		// BroadcastEvent wraps the data in the event envelope itself.
		raw, err := json.Marshal(chat)
		if err != nil {
			return err
		}
		s.deps.Broadcast.BroadcastEvent(eventChat, raw)
		s.metricsSink().IncChatMessage("global")
	} else {
		s.broadcastChannelScoped(ctx, channelID, payload)
		s.metricsSink().IncChatMessage("channel")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Runtime moderation lists (117/118)
// ---------------------------------------------------------------------------

// chatFiltersKey is the server_settings row holding the runtime moderation
// lists. One JSON document rather than three rows makes "an operator has taken
// the lists over" a single observable fact: while the row is absent the
// config.yaml values apply, and the first write stores all three, so a
// half-migrated state where one list is runtime and two are config cannot
// exist (117/118). It is NOT sealed — moderation configuration is not message
// content, so sealedSetting deliberately excludes it.
const chatFiltersKey = "chat_filters"

// permissionKeyChatFilterManage gates reading and writing the moderation
// lists. Admins bypass every check, so the lists are manageable out of the box
// and delegable once the key is granted (117/118).
const permissionKeyChatFilterManage = permissions.PermissionKey("b_chat_filter_manage")

// maxFilterListBytes caps one stored list. moderateBody re-scans every list on
// every send and every edit, so an unbounded list is a self-inflicted DoS.
const maxFilterListBytes = 4096

// chatFilters holds the three comma-separated moderation lists. The JSON tags
// are both the stored document's shape and netproto.ChatFilterResponse's;
// keeping them identical stops the row and the wire form from drifting.
type chatFilters struct {
	WordFilter    string `json:"word_filter"`
	LinkBlacklist string `json:"link_blacklist"`
	LinkWhitelist string `json:"link_whitelist"`
}

// chatFilterCache memoises the lists: moderateBody runs on every moderated
// message, and reloading the setting each time would put a database read on
// the send path. Writers call invalidateFilters.
type chatFilterCache struct {
	mu         sync.Mutex
	loaded     bool
	fromConfig bool
	filters    chatFilters

	// writeMu serialises the read-modify-write in handleChatFilterSet. A set
	// carries only the lists it changes, so two concurrent operators editing
	// different lists would otherwise each store their own view and one edit
	// would vanish.
	writeMu sync.Mutex
}

// configFilters is the boot-time fallback, in force until an operator stores
// a runtime document.
func (s *TCPServer) configFilters() chatFilters {
	if s.cfg == nil {
		return chatFilters{}
	}
	return chatFilters{
		WordFilter:    s.cfg.ChatWordFilter,
		LinkBlacklist: s.cfg.ChatLinkBlacklist,
		LinkWhitelist: s.cfg.ChatLinkWhitelist,
	}
}

// effectiveFilters returns the lists in force and whether they are still the
// config defaults. A store failure falls back to config WITHOUT caching, so a
// transient error cannot pin moderation to the wrong lists for the lifetime of
// the process.
func (s *TCPServer) effectiveFilters(ctx context.Context) (chatFilters, bool) {
	c := s.chatFilters
	if c == nil || s.deps == nil || s.deps.Chat == nil {
		return s.configFilters(), true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.filters, c.fromConfig
	}
	raw, _, err := s.deps.Chat.GetServerSetting(ctx, chatFiltersKey)
	if err != nil {
		s.logger.Warn("reading chat filter settings failed", zap.Error(err))
		return s.configFilters(), true
	}
	f, fromConfig := s.configFilters(), true
	if raw != "" {
		var stored chatFilters
		if err := json.Unmarshal([]byte(raw), &stored); err != nil {
			// A hand-edited row must not silently disable moderation.
			s.logger.Warn("stored chat filter document is malformed; using config defaults", zap.Error(err))
		} else {
			f, fromConfig = stored, false
		}
	}
	c.filters, c.fromConfig, c.loaded = f, fromConfig, true
	return f, fromConfig
}

// invalidateFilters drops the memoised lists so the next moderated message
// reloads them.
func (s *TCPServer) invalidateFilters() {
	if s.chatFilters == nil {
		return
	}
	s.chatFilters.mu.Lock()
	s.chatFilters.loaded = false
	s.chatFilters.mu.Unlock()
}

// splitList parses a comma-separated list into trimmed, lowercased, non-empty
// entries (matching is case-insensitive on both sides).
func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, e := range strings.Split(raw, ",") {
		if e = strings.TrimSpace(strings.ToLower(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// normalizeList re-joins a list with the entries trimmed and the empties
// dropped, so what is stored is what is matched. Case is preserved: the
// operator sees back what they typed and splitList lowercases at match time.
func normalizeList(raw string) string {
	var out []string
	for _, e := range strings.Split(raw, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return strings.Join(out, ",")
}

// linkURLRe matches the http(s) URLs the client renders as clickable links
// (markdown.js linkifies exactly this shape). Matching real URLs instead of
// raw substrings is the whole point of 118: substring matching rejected prose
// that merely NAMED a blocked domain, and accepted
// "http://evil.tld/?q=good.example" because a whitelist entry appeared
// somewhere in the body.
var linkURLRe = regexp.MustCompile(`(?i)https?://[^\s<>"'\])}]+`)

// linkHosts returns the lowercased hostname of every link in body, in order.
// An unparseable URL yields "" — it still reads as a link to a human, so it
// must not slip past a whitelist.
func linkHosts(body string) []string {
	matches := linkURLRe.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, raw := range matches {
		u, err := url.Parse(raw)
		if err != nil {
			out = append(out, "")
			continue
		}
		// Hostname drops the port and any user:pass@ prefix, so
		// "http://good.example@evil.tld/" is correctly read as evil.tld.
		out = append(out, strings.Trim(strings.ToLower(u.Hostname()), "."))
	}
	return out
}

// hostInList reports whether host equals a list entry or is a subdomain of
// one: "evil.tld" matches "a.b.evil.tld" but never "notevil.tld".
func hostInList(host string, list []string) bool {
	if host == "" {
		return false
	}
	for _, e := range list {
		if e = strings.Trim(e, "."); e == "" {
			continue
		}
		if host == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

// moderateBody applies the word filter (117) and the link filters (118) that
// are currently in force.
func (s *TCPServer) moderateBody(ctx context.Context, body string) error {
	f, _ := s.effectiveFilters(ctx)

	lower := strings.ToLower(body)
	for _, w := range splitList(f.WordFilter) {
		if strings.Contains(lower, w) {
			return errors.New("message rejected by the server word filter")
		}
	}

	black, white := splitList(f.LinkBlacklist), splitList(f.LinkWhitelist)
	if len(black) == 0 && len(white) == 0 {
		return nil
	}
	hosts := linkHosts(body)
	for _, h := range hosts {
		if hostInList(h, black) {
			return errors.New("message contains a blocked link")
		}
	}
	// A non-empty whitelist means ONLY those hosts may be linked, so EVERY
	// link has to match — one matching link must not license the rest (118).
	if len(white) > 0 {
		for _, h := range hosts {
			if !hostInList(h, white) {
				return errors.New("message contains a link outside the allowed domains")
			}
		}
	}
	return nil
}

// handleChatFilterGet returns the moderation lists in force. Reads are gated
// like writes: the word list tells an attacker exactly what to evade.
func (s *TCPServer) handleChatFilterGet(ctx context.Context, client *Client, _ *netproto.Frame) error {
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissionKeyChatFilterManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissionKeyChatFilterManage))
	}
	f, fromConfig := s.effectiveFilters(ctx)
	return s.writeMessage(client, netproto.MsgChatFilterResponse, netproto.ChatFilterResponse{
		WordFilter:    f.WordFilter,
		LinkBlacklist: f.LinkBlacklist,
		LinkWhitelist: f.LinkWhitelist,
		FromConfig:    fromConfig,
	})
}

// handleChatFilterSet replaces the runtime moderation lists (117/118) and
// replies with the new effective state.
func (s *TCPServer) handleChatFilterSet(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatFilterSet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_filter_set: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissionKeyChatFilterManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissionKeyChatFilterManage))
	}

	if s.chatFilters != nil {
		s.chatFilters.writeMu.Lock()
		defer s.chatFilters.writeMu.Unlock()
	}

	// Every set writes the FULL triple, so the first one snapshots whatever
	// config.yaml still supplied and the lists can never be half-stored.
	next, _ := s.effectiveFilters(ctx)
	for _, upd := range []struct {
		in  *string
		out *string
	}{
		{msg.WordFilter, &next.WordFilter},
		{msg.LinkBlacklist, &next.LinkBlacklist},
		{msg.LinkWhitelist, &next.LinkWhitelist},
	} {
		if upd.in == nil {
			continue
		}
		if len(*upd.in) > maxFilterListBytes {
			return s.sendError(client, errCodeMalformed,
				fmt.Sprintf("filter list too long (max %d bytes)", maxFilterListBytes))
		}
		*upd.out = normalizeList(*upd.in)
	}

	raw, err := json.Marshal(next)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "encoding chat filters failed")
	}
	if err := s.deps.Chat.SetServerSetting(ctx, chatFiltersKey, string(raw), 0); err != nil {
		s.logger.Warn("storing chat filters failed", zap.Error(err))
		return s.sendError(client, errCodeUnavailable, "storing chat filters failed")
	}
	s.invalidateFilters()
	s.audit(ctx, client.UniqueID, "chat_filter_set", chatFiltersKey,
		fmt.Sprintf("words=%d blacklist=%d whitelist=%d",
			len(splitList(next.WordFilter)), len(splitList(next.LinkBlacklist)), len(splitList(next.LinkWhitelist))))

	return s.writeMessage(client, netproto.MsgChatFilterResponse, netproto.ChatFilterResponse{
		WordFilter:    next.WordFilter,
		LinkBlacklist: next.LinkBlacklist,
		LinkWhitelist: next.LinkWhitelist,
	})
}

// mentionRe matches @word mentions (letters, digits, _, -).
var mentionRe = regexp.MustCompile(`@([A-Za-z0-9_\-]+)`)

// parseMentions resolves @nickname mentions to unique IDs of online users
// (105). @channel mentions all current channel members; @here/@everyone
// require b_chat_mention_all (unset = denied).
func (s *TCPServer) parseMentions(ctx context.Context, client *Client, channelID int64, body string) []string {
	if s.deps.State == nil {
		return nil
	}
	out := map[string]bool{}

	// (105) @channel, @here and @everyone all mass-notify, so all three sit
	// behind the same permission — gating only two of them let any user reach
	// every member by picking the ungated spelling.
	mentionAll := false
	if mentionRe.MatchString(body) {
		lower := strings.ToLower(body)
		if strings.Contains(lower, "@channel") || strings.Contains(lower, "@here") || strings.Contains(lower, "@everyone") {
			if pc, err := s.permCheckerFor(ctx, client); err == nil && pc.granted(permissions.PermissionKeyChatMentionAll) {
				mentionAll = true
			}
		}
	}

	if mentionAll {
		if channelID != 0 {
			for _, m := range s.deps.State.ChannelMembers(channelID) {
				out[m.UniqueID] = true
			}
		} else {
			for _, c := range s.deps.State.ListClients() {
				out[c.UniqueID] = true
			}
		}
	} else {
		lower := strings.ToLower(body)
		for _, c := range s.deps.State.ListClients() {
			if c.Nickname == "" {
				continue
			}
			if strings.Contains(lower, "@"+strings.ToLower(c.Nickname)) {
				out[c.UniqueID] = true
			}
		}
	}

	delete(out, client.UniqueID)
	if len(out) == 0 {
		return nil
	}
	mentions := make([]string, 0, len(out))
	for uid := range out {
		mentions = append(mentions, uid)
	}
	return mentions
}

// ---------------------------------------------------------------------------
// History (103)
// ---------------------------------------------------------------------------

// maxKeysPerResponse caps the sealed generations piggybacked on one history
// or pins page. Overflow sets Truncated so the client re-requests the rest;
// it is NEVER reported as Refused, which is a permanent state (91).
const maxKeysPerResponse = 64

// publishedKey returns the caller's X25519 public key, or "" when it never
// published one. A client with no key cannot open anything, so history and
// pins refuse it outright rather than shipping rows it provably cannot read.
func (s *TCPServer) publishedKey(client *Client) string {
	if s.deps == nil || s.deps.State == nil {
		return ""
	}
	sc, ok := s.deps.State.GetClient(client.ID)
	if !ok {
		return ""
	}
	return sc.E2EPublicKey
}

// scopeKeyBundle seals every generation a page references for the caller.
// Generations are sorted so the bundle is deterministic, and the cap is
// reported as truncation rather than refusal.
func (s *TCPServer) scopeKeyBundle(ctx context.Context, scope int64, memberPub string, gens map[uint32]bool) (keys []netproto.ChannelKey, refused []uint32, truncated bool) {
	ordered := make([]uint32, 0, len(gens))
	for gen := range gens {
		ordered = append(ordered, gen)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] > ordered[j] })
	for _, gen := range ordered {
		if len(keys) >= maxKeysPerResponse {
			truncated = true
			break
		}
		ck, err := s.chatKeys.sealFor(ctx, scope, gen, memberPub)
		if err != nil {
			refused = append(refused, gen)
			continue
		}
		keys = append(keys, *ck)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].KeyID < keys[j].KeyID })
	sort.Slice(refused, func(i, j int) bool { return refused[i] < refused[j] })
	return keys, refused, truncated
}

// handleChatHistory returns a paged history for a scope the caller belongs
// to (guests included; membership is checked at query time). Bodies are
// ciphertext; the generations that page references ride along in Keys, so a
// scroll-back costs no extra round trips (91).
func (s *TCPServer) handleChatHistory(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatHistory
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_history: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	if !s.scopeReadable(ctx, client, msg.ChannelID) {
		return s.sendError(client, errCodePermissionDenied, "not a member of this channel")
	}
	// Anonymous users may still join and inspect public channel membership,
	// but one request cannot bulk-export more than a normal page of history.
	if client.UserID == 0 && (msg.Limit <= 0 || msg.Limit > 50) {
		msg.Limit = 50
	}
	memberPub := s.publishedKey(client)
	if memberPub == "" {
		return s.sendError(client, errCodePermissionDenied, "publish an encryption key before reading history")
	}

	msgs, err := s.deps.Chat.ChatHistory(ctx, msg.ChannelID, msg.BeforeID, msg.Limit)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "history query failed")
	}
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	reactions, err := s.deps.Chat.ReactionsFor(ctx, ids)
	if err != nil {
		reactions = nil // best-effort
	}

	resp := netproto.ChatHistoryResponse{ChannelID: msg.ChannelID, Messages: []netproto.ChatHistoryEntry{}}
	gens := map[uint32]bool{}
	for _, m := range msgs {
		e := chatHistoryEntry(m, reactions[m.ID])
		if e.KeyID != 0 {
			gens[e.KeyID] = true
		}
		resp.Messages = append(resp.Messages, e)
	}
	resp.Keys, resp.Refused, resp.Truncated = s.scopeKeyBundle(ctx, msg.ChannelID, memberPub, gens)
	return s.writeMessage(client, netproto.MsgChatHistoryResponse, resp)
}

// chatHistoryEntry converts a stored message to the wire shape.
func chatHistoryEntry(m store.ChatMessage, reactions map[string]int) netproto.ChatHistoryEntry {
	e := netproto.ChatHistoryEntry{
		ID:           m.ID,
		FromUniqueID: m.FromUniqueID,
		FromNickname: m.FromNickname,
		ReplyToID:    m.ReplyToID,
		Version:      m.Version,
		BodyEnc:      m.BodyEnc,
		KeyID:        m.KeyID,
		SentAt:       m.SentAt.Unix(),
		Reactions:    reactions,
	}
	if m.EditedAt != nil {
		e.EditedAt = m.EditedAt.Unix()
	}
	if m.DeletedAt != nil {
		e.Deleted = true
		e.BodyEnc = ""
		e.KeyID = 0
	}
	return e
}

// ---------------------------------------------------------------------------
// Edit (101) + delete (102)
// ---------------------------------------------------------------------------

// handleChatEdit edits the caller's own message: validate the generation,
// decrypt for moderation, store the caller's ciphertext VERBATIM, and
// broadcast those same bytes. Storing the sender's own bytes means the bytes
// that were moderated are exactly the bytes that end up at rest (91).
func (s *TCPServer) handleChatEdit(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatEdit
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_edit: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	if !msg.Enc && (s.cfg == nil || !s.cfg.ChatAllowPlaintext) {
		return s.sendError(client, errCodePermissionDenied, "plaintext chat is disabled on this server — update your client (chat encryption is mandatory)")
	}
	stored, err := s.deps.Chat.GetChatMessage(ctx, msg.MessageID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "message lookup failed")
	}
	if stored == nil || stored.DeletedAt != nil {
		return s.sendError(client, errCodeNotFound, "message not found")
	}
	if stored.FromUniqueID != client.UniqueID {
		return s.sendError(client, errCodePermissionDenied, "you can only edit your own messages")
	}
	if s.chatKeys == nil {
		return s.sendError(client, errCodeUnavailable, "chat key manager unavailable")
	}

	plain := msg.NewText
	if msg.Enc {
		// Mirror handleChatSend: an edit under a rotated generation would
		// otherwise fail deep in the pipeline with a confusing error.
		currentID, _, err := s.chatKeys.current(ctx, stored.ChannelID)
		if err != nil {
			return s.sendError(client, errCodeUnavailable, "no chat key for this channel yet — rejoin the channel")
		}
		if msg.KeyID != currentID {
			return s.sendError(client, errCodeMalformed, "stale chat key for channel (key rotated; wait for re-key)")
		}
		p, err := s.chatKeys.open(ctx, stored.ChannelID, msg.KeyID, msg.NewText)
		if err != nil {
			return s.sendError(client, errCodeMalformed, "message decryption failed (stale key?)")
		}
		plain = p
	}
	// (119) same cap as the send path. The nil guard leads: this function
	// already treats s.cfg as possibly nil above, so testing ChatMaxLength
	// first would panic on exactly the path that guard exists for.
	if s.cfg != nil && s.cfg.ChatMaxLength > 0 && len([]byte(plain)) > s.cfg.ChatMaxLength {
		return s.sendError(client, errCodeMalformed, fmt.Sprintf("message too long (max %d bytes)", s.cfg.ChatMaxLength))
	}
	if err := s.moderateBody(ctx, stripAttachmentRefs(plain)); err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}

	// The plaintext escape hatch never reaches storage or the relay: the
	// server seals before both, exactly as it does on the send path.
	bodyEnc, keyID := msg.NewText, msg.KeyID
	if !msg.Enc {
		id, ct, err := s.chatKeys.seal(ctx, stored.ChannelID, plain)
		if err != nil {
			return s.sendError(client, errCodeUnavailable, "chat key unavailable")
		}
		bodyEnc, keyID = ct, id
	}

	version, err := s.deps.Chat.EditChatMessage(ctx, msg.MessageID, bodyEnc, keyID, msg.ExpectedVersion)
	if errors.Is(err, store.ErrChatEditConflict) {
		return s.sendError(client, errCodeConflict, "message changed on another client; reload before editing")
	}
	if err != nil {
		return s.sendError(client, errCodeNotFound, "edit failed: "+err.Error())
	}

	// Uniform wire format: the edit event carries ciphertext like any other
	// chat frame, whatever the sender did.
	s.broadcastScope(ctx, stored.ChannelID, eventChatEdited, map[string]any{
		"message_id": msg.MessageID,
		"channel_id": stored.ChannelID,
		"body":       bodyEnc,
		"enc":        true,
		"key_id":     keyID,
		"edited_by":  client.UniqueID,
		"version":    version,
	})
	return nil
}

// handleChatDelete tombstones a message: own messages, or any with
// b_chat_delete_any.
func (s *TCPServer) handleChatDelete(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatDelete
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_delete: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	stored, err := s.deps.Chat.GetChatMessage(ctx, msg.MessageID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "message lookup failed")
	}
	if stored == nil || stored.DeletedAt != nil {
		return s.sendError(client, errCodeNotFound, "message not found")
	}
	if stored.FromUniqueID != client.UniqueID {
		pc, err := s.permCheckerFor(ctx, client)
		if err != nil {
			return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
		}
		if !pc.granted(permissions.PermissionKeyChatDeleteAny) {
			return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyChatDeleteAny))
		}
	}
	if err := s.deps.Chat.DeleteChatMessage(ctx, msg.MessageID); err != nil {
		return s.sendError(client, errCodeNotFound, "delete failed: "+err.Error())
	}
	s.broadcastScope(ctx, stored.ChannelID, eventChatDeleted, map[string]any{
		"message_id": msg.MessageID,
		"channel_id": stored.ChannelID,
		"deleted_by": client.UniqueID,
	})
	return nil
}

// broadcastScope broadcasts an event to a channel's members and subscribers
// (channelID 0 = everyone). Note the two broadcast paths differ:
// broadcastChannelScoped takes a pre-wrapped envelope, BroadcastEvent wraps
// the data itself.
func (s *TCPServer) broadcastScope(ctx context.Context, channelID int64, eventType string, data any) {
	if channelID == 0 {
		raw, err := json.Marshal(data)
		if err != nil {
			return
		}
		s.deps.Broadcast.BroadcastEvent(eventType, raw)
		return
	}
	payload, err := eventEnvelope(eventType, data)
	if err != nil {
		return
	}
	s.broadcastChannelScoped(ctx, channelID, payload)
}

// ---------------------------------------------------------------------------
// Pins (109)
// ---------------------------------------------------------------------------

// handleChatPin pins or unpins a message. Gated by b_channel_modify (the
// channel-edit permission — pin curation is a channel-management action).
func (s *TCPServer) handleChatPin(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatPin
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_pin: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyChannelModify) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyChannelModify))
	}

	if msg.Pinned {
		stored, err := s.deps.Chat.GetChatMessage(ctx, msg.MessageID)
		if err != nil || stored == nil || stored.DeletedAt != nil {
			return s.sendError(client, errCodeNotFound, "message not found")
		}
		if stored.ChannelID != msg.ChannelID {
			return s.sendError(client, errCodeMalformed, "message is not in this channel")
		}
		if err := s.deps.Chat.PinChatMessage(ctx, msg.ChannelID, msg.MessageID, client.UniqueID); err != nil {
			return s.sendError(client, errCodeUnavailable, "pin failed")
		}
	} else {
		if err := s.deps.Chat.UnpinChatMessage(ctx, msg.ChannelID, msg.MessageID); err != nil {
			return s.sendError(client, errCodeUnavailable, "unpin failed")
		}
	}

	event := eventChatPinned
	if !msg.Pinned {
		event = eventChatUnpinned
	}
	s.broadcastScope(ctx, msg.ChannelID, event, map[string]any{
		"message_id": msg.MessageID,
		"channel_id": msg.ChannelID,
		"by":         client.UniqueID,
	})
	return nil
}

// handleChatPins lists a channel's pins. It is gated exactly like history:
// without the membership check any authenticated client could enumerate the
// message ids, authors, timestamps and bodies of a channel it never joined,
// and key-gating alone would close the body leak while leaving the metadata
// enumeration open (91).
func (s *TCPServer) handleChatPins(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatPins
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_pins: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	if !s.scopeReadable(ctx, client, msg.ChannelID) {
		return s.sendError(client, errCodePermissionDenied, "not a member of this channel")
	}
	memberPub := s.publishedKey(client)
	if memberPub == "" {
		return s.sendError(client, errCodePermissionDenied, "publish an encryption key before reading history")
	}
	pins, err := s.deps.Chat.ChatPins(ctx, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "pins query failed")
	}
	resp := netproto.ChatPinsResponse{ChannelID: msg.ChannelID, Pins: []netproto.ChatPinEntry{}}
	gens := map[uint32]bool{}
	for _, p := range pins {
		entry := netproto.ChatPinEntry{MessageID: p.MessageID, PinnedBy: p.PinnedBy, PinnedAt: p.PinnedAt.Unix()}
		if p.Message != nil {
			m := chatHistoryEntry(*p.Message, nil)
			if m.KeyID != 0 {
				gens[m.KeyID] = true
			}
			entry.Message = &m
		}
		resp.Pins = append(resp.Pins, entry)
	}
	resp.Keys, resp.Refused, resp.Truncated = s.scopeKeyBundle(ctx, msg.ChannelID, memberPub, gens)
	return s.writeMessage(client, netproto.MsgChatPinsResponse, resp)
}

// ---------------------------------------------------------------------------
// Reactions (97)
// ---------------------------------------------------------------------------

// handleChatReact toggles a reaction and broadcasts the full updated
// reaction map for the message.
func (s *TCPServer) handleChatReact(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatReact
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_react: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	if msg.Emoji == "" || utf8.RuneCountInString(msg.Emoji) > 32 {
		return s.sendError(client, errCodeMalformed, "invalid emoji")
	}
	stored, err := s.deps.Chat.GetChatMessage(ctx, msg.MessageID)
	if err != nil || stored == nil || stored.DeletedAt != nil {
		return s.sendError(client, errCodeNotFound, "message not found")
	}
	counts, added, err := s.deps.Chat.ToggleReaction(ctx, msg.MessageID, client.UniqueID, msg.Emoji)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "reaction failed")
	}
	s.broadcastScope(ctx, stored.ChannelID, eventChatReaction, map[string]any{
		"message_id": msg.MessageID,
		"channel_id": stored.ChannelID,
		"reactions":  counts,
		"by":         client.UniqueID,
		"added":      added,
	})
	return nil
}

// ---------------------------------------------------------------------------
// Typing (120) + DM receipts (124)
// ---------------------------------------------------------------------------

// typingEvent is the payload of the "typing" event (120). It is the whole
// contract clients render against, so it is a struct rather than an ad-hoc
// map: the JSON tags cannot then drift from what the frontend reads.
// ChannelID is 0 for global and DM indicators.
type typingEvent struct {
	ClientID  string `json:"client_id"`
	UniqueID  string `json:"unique_id"`
	Nickname  string `json:"nickname"`
	ChannelID int64  `json:"channel_id"`
}

// handleTyping relays a typing indicator. It is never stored — there is no
// body to seal, so it sits outside the ciphertext-at-rest path entirely (91).
// Clients throttle to ~3s between sends.
//
// A relay the sender is not entitled to is DROPPED rather than answered with
// an error: an indicator is fire-and-forget, and a client that left a channel
// mid-keystroke should not get an error frame for it.
func (s *TCPServer) handleTyping(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.Typing
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed typing: "+err.Error())
	}
	if s.deps == nil || s.deps.Broadcast == nil {
		return s.sendError(client, errCodeUnavailable, "broadcast backend unavailable")
	}
	typingScope := "global"
	if msg.ToUniqueID != "" {
		typingScope = "dm:" + msg.ToUniqueID
	} else if msg.ChannelID != 0 {
		typingScope = fmt.Sprintf("channel:%d", msg.ChannelID)
	}
	if s.typingRate != nil && !s.typingRate.allow(client.UniqueID+"\x00"+typingScope, time.Now()) {
		return nil
	}
	data := typingEvent{
		ClientID:  client.ID,
		UniqueID:  client.UniqueID,
		Nickname:  client.Username,
		ChannelID: msg.ChannelID,
	}
	switch {
	case msg.ToUniqueID != "":
		payload, err := eventEnvelope(eventTyping, data)
		if err != nil {
			return err
		}
		if tc, ok := s.clientByUniqueID(msg.ToUniqueID); ok {
			_ = s.deps.Broadcast.BroadcastToClient(tc.ID, payload)
		}
	case msg.ChannelID != 0:
		// Same gate as history: without it any client could fake "X is
		// typing" into a channel it never joined, which is the metadata half
		// of the leak scopeReadable closes for bodies.
		if !s.scopeReadable(ctx, client, msg.ChannelID) {
			return nil
		}
		payload, err := eventEnvelope(eventTyping, data)
		if err != nil {
			return err
		}
		s.broadcastChannelScoped(ctx, msg.ChannelID, payload)
	default:
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		s.deps.Broadcast.BroadcastEvent(eventTyping, raw)
	}
	return nil
}

// handleChatDelivered relays a DM delivery ack to the original sender (124).
func (s *TCPServer) handleChatDelivered(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatDelivered
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_delivered: "+err.Error())
	}
	return s.relayReceipt(client, msg.ToUniqueID, eventDMDelivered, msg.ClientMsgID)
}

// handleChatRead relays a DM read receipt to the original sender (124).
func (s *TCPServer) handleChatRead(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatRead
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_read: "+err.Error())
	}
	return s.relayReceipt(client, msg.ToUniqueID, eventDMRead, msg.ClientMsgID)
}

// relayReceipt forwards a receipt event to the (online) original DM sender.
func (s *TCPServer) relayReceipt(client *Client, toUniqueID, eventType, clientMsgID string) error {
	if s.deps == nil || s.deps.Broadcast == nil {
		return s.sendError(client, errCodeUnavailable, "broadcast backend unavailable")
	}
	if clientMsgID == "" {
		return s.sendError(client, errCodeMalformed, "client_msg_id is required")
	}
	payload, err := eventEnvelope(eventType, map[string]any{
		"from_unique_id": client.UniqueID,
		"client_msg_id":  clientMsgID,
	})
	if err != nil {
		return err
	}
	if tc, ok := s.clientByUniqueID(toUniqueID); ok {
		_ = s.deps.Broadcast.BroadcastToClient(tc.ID, payload)
	}
	return nil // offline sender: receipt is dropped (documented)
}

// ---------------------------------------------------------------------------
// Custom emoji (96)
// ---------------------------------------------------------------------------

// emojiNameRe validates emoji names.
var emojiNameRe = regexp.MustCompile(`^[a-z0-9_\-]{1,32}$`)

// handleEmojiUpload stores a custom emoji image and announces it. Gated by
// b_emoji_manage.
func (s *TCPServer) handleEmojiUpload(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.EmojiUpload
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed emoji_upload: "+err.Error())
	}
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyEmojiManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyEmojiManage))
	}
	if !emojiNameRe.MatchString(msg.Name) {
		return s.sendError(client, errCodeMalformed, "invalid emoji name (1-32 of a-z 0-9 _ -)")
	}
	raw, ext, err := decodeImage(msg.DataBase64)
	if err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}
	fileName, err := s.assets().writeImage("emojis", msg.Name, ext, raw)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "emoji write failed")
	}
	s.broadcastEvent(eventEmojiAdded, map[string]any{"name": msg.Name, "file_name": fileName, "by": client.UniqueID})
	return nil
}

// emojiManageAllowed applies the same gate as upload (272).
func (s *TCPServer) emojiManageAllowed(ctx context.Context, client *Client) error {
	pc, err := s.permCheckerFor(ctx, client)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "permission backend unavailable")
	}
	if !pc.granted(permissions.PermissionKeyEmojiManage) {
		return s.sendError(client, errCodePermissionDenied, "insufficient permission: "+string(permissions.PermissionKeyEmojiManage))
	}
	return nil
}

// handleEmojiDelete removes a custom emoji and announces it (272).
func (s *TCPServer) handleEmojiDelete(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.EmojiDelete
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed emoji_delete: "+err.Error())
	}
	if err := s.emojiManageAllowed(ctx, client); err != nil {
		return err
	}
	if !emojiNameRe.MatchString(msg.Name) {
		return s.sendError(client, errCodeMalformed, "invalid emoji name")
	}
	_, err := s.assets().removeImage("emojis", msg.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return s.sendError(client, errCodeNotFound, "emoji not found")
	}
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "emoji delete failed")
	}
	s.audit(ctx, client.UniqueID, "emoji_delete", msg.Name, "")
	s.broadcastEvent(eventEmojiRemoved, map[string]any{"name": msg.Name, "by": client.UniqueID})
	return nil
}

// handleEmojiRename renames a custom emoji (272). Messages already sent keep
// the old shortcode as literal text — a rename must not rewrite history — so
// this only affects the picker and future messages.
func (s *TCPServer) handleEmojiRename(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.EmojiRename
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed emoji_rename: "+err.Error())
	}
	if err := s.emojiManageAllowed(ctx, client); err != nil {
		return err
	}
	if !emojiNameRe.MatchString(msg.Name) || !emojiNameRe.MatchString(msg.NewName) {
		return s.sendError(client, errCodeMalformed, "invalid emoji name (1-32 of a-z 0-9 _ -)")
	}
	if msg.Name == msg.NewName {
		return s.sendError(client, errCodeMalformed, "new name is the same")
	}
	fileName, err := s.assets().renameImage("emojis", msg.Name, msg.NewName)
	if errors.Is(err, fs.ErrNotExist) {
		return s.sendError(client, errCodeNotFound, "emoji not found")
	}
	if errors.Is(err, fs.ErrExist) {
		return s.sendError(client, errCodeMalformed, "an emoji with that name already exists")
	}
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "emoji rename failed")
	}
	s.audit(ctx, client.UniqueID, "emoji_rename", msg.Name, "to "+msg.NewName)
	s.broadcastEvent(eventEmojiRenamed, map[string]any{
		"name": msg.Name, "new_name": msg.NewName, "file_name": fileName, "by": client.UniqueID,
	})
	return nil
}

// handleEmojiList lists the uploaded custom emojis.
func (s *TCPServer) handleEmojiList(_ context.Context, client *Client, f *netproto.Frame) error {
	images, err := s.assets().listImages("emojis")
	if err != nil {
		// No emoji directory yet is an empty list, not an error.
		return s.writeMessage(client, netproto.MsgEmojiListResponse, netproto.EmojiListResponse{Emojis: []netproto.EmojiEntry{}})
	}
	out := netproto.EmojiListResponse{Emojis: []netproto.EmojiEntry{}}
	for _, image := range images {
		if !emojiNameRe.MatchString(image.base) {
			continue
		}
		out.Emojis = append(out.Emojis, netproto.EmojiEntry{
			Name:     image.base,
			FileName: image.fileName,
		})
	}
	return s.writeMessage(client, netproto.MsgEmojiListResponse, out)
}

// handleEmojiGet serves one emoji image over the control channel (the files
// live on the server; clients cache them by name).
func (s *TCPServer) handleEmojiGet(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.EmojiGet
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed emoji_get: "+err.Error())
	}
	if !emojiNameRe.MatchString(msg.Name) {
		return s.sendError(client, errCodeMalformed, "invalid emoji name")
	}
	raw, image, err := s.assets().readImage("emojis", msg.Name)
	if err == nil {
		return s.writeMessage(client, netproto.MsgEmojiData, netproto.EmojiData{
			Name:        msg.Name,
			DataBase64:  base64.StdEncoding.EncodeToString(raw),
			ContentType: image.contentType,
		})
	}
	return s.sendError(client, errCodeNotFound, "emoji not found")
}

// ---------------------------------------------------------------------------
// Server settings: MOTD + announcement (132/133)
// ---------------------------------------------------------------------------

// sealedSetting reports whether a server setting is operator-authored
// broadcast text and therefore stored under the global generation (132/133).
// Everything else (server_name, max_clients_override) is machine
// configuration, not message content, and stays plain.
func sealedSetting(key string) bool { return key == "motd" || key == "announcement" }

// serverSetting returns an UNSEALED server setting ("" when unset or no
// store). Callers must not use it for sealed keys — see serverSettingPlain.
func (s *TCPServer) serverSetting(ctx context.Context, key string) string {
	if s.deps == nil || s.deps.Chat == nil {
		return ""
	}
	v, _, err := s.deps.Chat.GetServerSetting(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

// serverSettingSealed returns a sealed setting as (ciphertext, generation),
// re-sealing it under the current global generation first when the stored
// generation is retired or the value predates 012. Without the re-seal a
// global rotation would lock everyone out of the MOTD forever.
func (s *TCPServer) serverSettingSealed(ctx context.Context, key string) (string, uint32, error) {
	if s.deps == nil || s.deps.Chat == nil {
		return "", 0, nil
	}
	v, gen, err := s.deps.Chat.GetServerSetting(ctx, key)
	if err != nil || v == "" {
		return "", 0, err
	}
	if s.chatKeys == nil || !s.chatKeys.configured() {
		return "", 0, errChatKeysUnconfigured
	}
	currentID, _, err := s.chatKeys.current(ctx, globalChatScope)
	if err != nil {
		return "", 0, err
	}
	if gen == currentID {
		return v, gen, nil
	}
	plain := v
	if gen != 0 {
		p, err := s.chatKeys.open(ctx, globalChatScope, gen, v)
		if err != nil {
			return "", 0, fmt.Errorf("opening %s under generation %d: %w", key, gen, err)
		}
		plain = p
	}
	id, ct, err := s.chatKeys.seal(ctx, globalChatScope, plain)
	if err != nil {
		return "", 0, err
	}
	if err := s.deps.Chat.SetServerSetting(ctx, key, ct, id); err != nil {
		// The re-seal still stands for this caller; the write-back is an
		// optimisation, not a correctness requirement.
		s.logger.Warn("persisting re-sealed server setting failed", zap.String("key", key), zap.Error(err))
	}
	return ct, id, nil
}

// serverSettingPlain returns a sealed setting's plaintext, for the one
// surface that must serve it in the clear (the public server-info reply).
func (s *TCPServer) serverSettingPlain(ctx context.Context, key string) string {
	if s.deps == nil || s.deps.Chat == nil {
		return ""
	}
	v, gen, err := s.deps.Chat.GetServerSetting(ctx, key)
	if err != nil || v == "" || gen == 0 {
		if err != nil {
			return ""
		}
		return v
	}
	if s.chatKeys == nil {
		return ""
	}
	plain, err := s.chatKeys.open(ctx, globalChatScope, gen, v)
	if err != nil {
		return ""
	}
	return plain
}

// SetServerSettingAndAnnounce stores a server setting. The operator-authored
// broadcast texts (motd, announcement) are SEALED under the current global
// generation before they touch the database, so a dump never yields them;
// "announcement" is additionally broadcast to all online clients.
func (s *TCPServer) SetServerSettingAndAnnounce(ctx context.Context, key, value string) error {
	if s.deps == nil || s.deps.Chat == nil {
		return errors.New("chat store unavailable")
	}
	keyID := uint32(0)
	if sealedSetting(key) && value != "" {
		if s.chatKeys == nil || !s.chatKeys.configured() {
			return errChatKeysUnconfigured
		}
		// Global is a fixed, known scope; the boot-time mint normally beat us
		// here, and ensuring it is the documented authorised third site.
		if _, _, err := s.chatKeys.EnsureScope(ctx, globalChatScope); err != nil {
			s.logger.Warn("ensuring global chat key failed", zap.String("key", key), zap.Error(err))
			return fmt.Errorf("ensuring global chat key for %s: %w", key, err)
		}
		id, ct, err := s.chatKeys.seal(ctx, globalChatScope, value)
		if err != nil {
			s.logger.Warn("sealing server setting failed", zap.String("key", key), zap.Error(err))
			return fmt.Errorf("sealing %s: %w", key, err)
		}
		value, keyID = ct, id
	}
	if err := s.deps.Chat.SetServerSetting(ctx, key, value, keyID); err != nil {
		return err
	}
	if key == chatFiltersKey {
		// This path is reachable from ServerQuery, which does not go through
		// handleChatFilterSet, so the memoised lists have to be dropped here
		// too or the write applies only after a restart (117/118).
		s.invalidateFilters()
	}
	if key == "announcement" && value != "" {
		s.broadcastEvent(eventAnnouncement, map[string]any{"text": value, "enc": keyID > 0, "key_id": keyID})
	}
	return nil
}
