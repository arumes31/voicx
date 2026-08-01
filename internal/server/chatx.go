// chatx.go implements the wave-5a server-side chat infrastructure: the
// moderation pipeline for channel/global messages (rate limit → slow mode →
// decrypt → length/filters → spam → mentions → store → relay), history,
// edit/delete, pins, reactions, typing indicators, DM receipts, and custom
// emoji. Direct messages are exempt from moderation and storage: they are
// true E2EE (wave 4b) and their history is client-local by design.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[uid]
	if !ok || now.After(b.reset) {
		b = &chatBucket{tokens: l.max, reset: now.Add(l.window)}
		l.buckets[uid] = b
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// spamEntry is one recently seen message body for spam detection.
type spamEntry struct {
	body string
	at   time.Time
}

// spamTracker rejects the third identical message within 30 seconds (116).
type spamTracker struct {
	mu     sync.Mutex
	recent map[string][]spamEntry
}

func newSpamTracker() *spamTracker {
	return &spamTracker{recent: map[string][]spamEntry{}}
}

// record notes a message and reports whether it trips the spam heuristic.
func (t *spamTracker) record(uid, body string, now time.Time) bool {
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
		if e.body == body {
			same++
		}
	}
	t.recent[uid] = append(keep, spamEntry{body: body, at: now})
	return same >= 2 // this is the 3rd identical message in 30s
}

// slowTracker records each user's last send time per channel (114).
type slowTracker struct {
	mu   sync.Mutex
	last map[string]map[int64]time.Time
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

	// (115) per-user rate limit (all scopes).
	if s.chatRate != nil && !s.chatRate.allow(uid, time.Now()) {
		s.metricsSink().IncChatMessage("rejected")
		return s.sendError(client, errCodeMalformed, "chat rate limit exceeded — slow down")
	}

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

	// Decrypt with the scope's current key (the key id was validated by the
	// caller). The server holds the scope keys so it can moderate and store.
	plain := msg.Text
	if msg.Enc {
		if s.chatKeys == nil {
			return s.sendError(client, errCodeUnavailable, "chat key manager unavailable")
		}
		p, err := s.chatKeys.open(channelID, msg.Text)
		if err != nil {
			return s.sendError(client, errCodeMalformed, "message decryption failed (stale key?)")
		}
		plain = p
	}

	// (119) max length (plaintext characters post-decrypt).
	if s.cfg != nil && s.cfg.ChatMaxLength > 0 && utf8.RuneCountInString(plain) > s.cfg.ChatMaxLength {
		s.metricsSink().IncChatMessage("rejected")
		return s.sendError(client, errCodeMalformed, fmt.Sprintf("message too long (max %d characters)", s.cfg.ChatMaxLength))
	}

	// (117/118) word + link filters.
	if err := s.moderateBody(plain); err != nil {
		s.metricsSink().IncChatMessage("rejected")
		return s.sendError(client, errCodeMalformed, err.Error())
	}

	// (116) anti-spam: identical message x3 in 30s.
	if s.chatSpam != nil && s.chatSpam.record(uid, plain, time.Now()) {
		s.metricsSink().IncChatMessage("rejected")
		return s.sendError(client, errCodeMalformed, "possible spam detected — please vary your messages")
	}
	// (105) mention parsing.
	mentions := s.parseMentions(ctx, client, channelID, plain)

	// (103) store decrypted (history/search/moderation need readable bodies).
	var messageID int64
	if s.deps.Chat != nil {
		id, err := s.deps.Chat.StoreChatMessage(ctx, channelID, uid, client.Username, plain)
		if err != nil {
			s.logger.Warn("storing chat message failed")
			// Storage failure is not fatal for relay.
		} else {
			messageID = id
		}
	}

	// Relay the ORIGINAL ciphertext (clients decrypt with the scope key);
	// history reads the decrypted body from the store.
	chat := netproto.ChatBroadcast{
		ChannelID:    msg.ChannelID,
		FromClientID: client.ID,
		FromUniqueID: uid,
		From:         client.Username,
		Text:         msg.Text,
		Enc:          msg.Enc,
		KeyID:        msg.KeyID,
		ID:           messageID,
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
		s.deps.Broadcast.BroadcastToChannel(channelID, payload)
		s.metricsSink().IncChatMessage("channel")
	}
	return nil
}

// moderateBody applies the word filter (117) and link filters (118).
func (s *TCPServer) moderateBody(body string) error {
	if s.cfg == nil {
		return nil
	}
	lower := strings.ToLower(body)
	if s.cfg.ChatWordFilter != "" {
		for _, w := range strings.Split(s.cfg.ChatWordFilter, ",") {
			w = strings.TrimSpace(strings.ToLower(w))
			if w != "" && strings.Contains(lower, w) {
				return errors.New("message rejected by the server word filter")
			}
		}
	}
	if s.cfg.ChatLinkBlacklist != "" {
		for _, d := range strings.Split(s.cfg.ChatLinkBlacklist, ",") {
			d = strings.TrimSpace(strings.ToLower(d))
			if d != "" && strings.Contains(lower, d) {
				return errors.New("message contains a blocked link")
			}
		}
	}
	if s.cfg.ChatLinkWhitelist != "" && strings.Contains(lower, "http") {
		allowed := false
		for _, d := range strings.Split(s.cfg.ChatLinkWhitelist, ",") {
			d = strings.TrimSpace(strings.ToLower(d))
			if d != "" && strings.Contains(lower, d) {
				allowed = true
				break
			}
		}
		if !allowed {
			return errors.New("message contains a link outside the allowed domains")
		}
	}
	return nil
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

	mentionAll := false
	if mentionRe.MatchString(body) {
		lower := strings.ToLower(body)
		if strings.Contains(lower, "@channel") || strings.Contains(lower, "@here") || strings.Contains(lower, "@everyone") {
			if strings.Contains(lower, "@channel") {
				mentionAll = true
			} else if pc, err := s.permCheckerFor(ctx, client); err == nil && pc.granted(permissions.PermissionKeyChatMentionAll) {
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

// handleChatHistory returns a paged, decrypted history for a scope the
// caller belongs to (guests included; membership is checked at query time).
func (s *TCPServer) handleChatHistory(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatHistory
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_history: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	if msg.ChannelID != 0 {
		sc, ok := s.deps.State.GetClient(client.ID)
		if !ok || sc.ChannelID != msg.ChannelID {
			return s.sendError(client, errCodePermissionDenied, "not a member of this channel")
		}
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
	for _, m := range msgs {
		resp.Messages = append(resp.Messages, chatHistoryEntry(m, reactions[m.ID]))
	}
	return s.writeMessage(client, netproto.MsgChatHistoryResponse, resp)
}

// chatHistoryEntry converts a stored message to the wire shape.
func chatHistoryEntry(m store.ChatMessage, reactions map[string]int) netproto.ChatHistoryEntry {
	e := netproto.ChatHistoryEntry{
		ID:           m.ID,
		FromUniqueID: m.FromUniqueID,
		FromNickname: m.FromNickname,
		Body:         m.Body,
		SentAt:       m.SentAt.Unix(),
		Reactions:    reactions,
	}
	if m.EditedAt != nil {
		e.EditedAt = m.EditedAt.Unix()
	}
	if m.DeletedAt != nil {
		e.Deleted = true
		e.Body = ""
	}
	return e
}

// ---------------------------------------------------------------------------
// Edit (101) + delete (102)
// ---------------------------------------------------------------------------

// handleChatEdit edits the caller's own message: decrypt (when enc), filter,
// update, and broadcast chat_edited with the re-sealed body so the wire
// format stays uniform with normal chat.
func (s *TCPServer) handleChatEdit(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatEdit
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_edit: "+err.Error())
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
		return s.sendError(client, errCodePermissionDenied, "you can only edit your own messages")
	}

	plain := msg.NewText
	if msg.Enc {
		if s.chatKeys == nil {
			return s.sendError(client, errCodeUnavailable, "chat key manager unavailable")
		}
		p, err := s.chatKeys.open(stored.ChannelID, msg.NewText)
		if err != nil {
			return s.sendError(client, errCodeMalformed, "message decryption failed (stale key?)")
		}
		plain = p
	}
	if utf8.RuneCountInString(plain) > s.cfg.ChatMaxLength && s.cfg.ChatMaxLength > 0 {
		return s.sendError(client, errCodeMalformed, fmt.Sprintf("message too long (max %d characters)", s.cfg.ChatMaxLength))
	}
	if err := s.moderateBody(plain); err != nil {
		return s.sendError(client, errCodeMalformed, err.Error())
	}

	if err := s.deps.Chat.EditChatMessage(ctx, msg.MessageID, plain); err != nil {
		return s.sendError(client, errCodeNotFound, "edit failed: "+err.Error())
	}

	// Broadcast the edit re-sealed with the current scope key (uniform wire
	// format: clients decrypt like normal chat).
	outText := plain
	var keyID uint32
	if msg.Enc && s.chatKeys != nil {
		keyID, outText = s.chatKeys.seal(stored.ChannelID, plain)
	}
	s.broadcastScope(stored.ChannelID, eventChatEdited, map[string]any{
		"message_id": msg.MessageID,
		"channel_id": stored.ChannelID,
		"body":       outText,
		"enc":        msg.Enc,
		"key_id":     keyID,
		"edited_by":  client.UniqueID,
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
	s.broadcastScope(stored.ChannelID, eventChatDeleted, map[string]any{
		"message_id": msg.MessageID,
		"channel_id": stored.ChannelID,
		"deleted_by": client.UniqueID,
	})
	return nil
}

// broadcastScope broadcasts an event to a channel's members (channelID 0 =
// everyone). Note the two broadcast paths differ: BroadcastToChannel takes a
// pre-wrapped envelope, BroadcastEvent wraps the data itself.
func (s *TCPServer) broadcastScope(channelID int64, eventType string, data any) {
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
	s.deps.Broadcast.BroadcastToChannel(channelID, payload)
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
	s.broadcastScope(msg.ChannelID, event, map[string]any{
		"message_id": msg.MessageID,
		"channel_id": msg.ChannelID,
		"by":         client.UniqueID,
	})
	return nil
}

// handleChatPins lists a channel's pins.
func (s *TCPServer) handleChatPins(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChatPins
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed chat_pins: "+err.Error())
	}
	if s.deps == nil || s.deps.Chat == nil {
		return s.sendError(client, errCodeUnavailable, "chat store unavailable")
	}
	pins, err := s.deps.Chat.ChatPins(ctx, msg.ChannelID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "pins query failed")
	}
	resp := netproto.ChatPinsResponse{ChannelID: msg.ChannelID, Pins: []netproto.ChatPinEntry{}}
	for _, p := range pins {
		entry := netproto.ChatPinEntry{MessageID: p.MessageID, PinnedBy: p.PinnedBy, PinnedAt: p.PinnedAt.Unix()}
		if p.Message != nil {
			m := chatHistoryEntry(*p.Message, nil)
			entry.Message = &m
		}
		resp.Pins = append(resp.Pins, entry)
	}
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
	s.broadcastScope(stored.ChannelID, eventChatReaction, map[string]any{
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

// handleTyping relays a typing indicator (not stored). Clients throttle to
// ~3s between sends.
func (s *TCPServer) handleTyping(_ context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.Typing
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed typing: "+err.Error())
	}
	if s.deps == nil || s.deps.Broadcast == nil {
		return s.sendError(client, errCodeUnavailable, "broadcast backend unavailable")
	}
	data := map[string]any{
		"client_id":  client.ID,
		"unique_id":  client.UniqueID,
		"nickname":   client.Username,
		"channel_id": msg.ChannelID,
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
		payload, err := eventEnvelope(eventTyping, data)
		if err != nil {
			return err
		}
		s.deps.Broadcast.BroadcastToChannel(msg.ChannelID, payload)
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

// emojiDir returns the emoji storage directory under the file root.
func (s *TCPServer) emojiDir() string {
	return filepath.Join(s.cfg.FileRoot, "emojis")
}

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
	dir := s.emojiDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return s.sendError(client, errCodeUnavailable, "emoji storage unavailable")
	}
	fileName := msg.Name + ext
	if err := os.WriteFile(filepath.Join(dir, fileName), raw, 0o644); err != nil {
		return s.sendError(client, errCodeUnavailable, "emoji write failed")
	}
	s.broadcastEvent(eventEmojiAdded, map[string]any{"name": msg.Name, "file_name": fileName, "by": client.UniqueID})
	return nil
}

// handleEmojiList lists the uploaded custom emojis.
func (s *TCPServer) handleEmojiList(_ context.Context, client *Client, f *netproto.Frame) error {
	entries, err := os.ReadDir(s.emojiDir())
	if err != nil {
		// No emoji directory yet is an empty list, not an error.
		return s.writeMessage(client, netproto.MsgEmojiListResponse, netproto.EmojiListResponse{Emojis: []netproto.EmojiEntry{}})
	}
	out := netproto.EmojiListResponse{Emojis: []netproto.EmojiEntry{}}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := filepath.Ext(name)
		out.Emojis = append(out.Emojis, netproto.EmojiEntry{
			Name:     strings.TrimSuffix(name, ext),
			FileName: name,
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
	entries, err := os.ReadDir(s.emojiDir())
	if err != nil {
		return s.sendError(client, errCodeNotFound, "emoji not found")
	}
	for _, e := range entries {
		name := e.Name()
		if strings.TrimSuffix(name, filepath.Ext(name)) != msg.Name {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.emojiDir(), name))
		if err != nil {
			return s.sendError(client, errCodeNotFound, "emoji not found")
		}
		return s.writeMessage(client, netproto.MsgEmojiData, netproto.EmojiData{
			Name:        msg.Name,
			DataBase64:  base64.StdEncoding.EncodeToString(raw),
			ContentType: http.DetectContentType(raw),
		})
	}
	return s.sendError(client, errCodeNotFound, "emoji not found")
}

// ---------------------------------------------------------------------------
// Server settings: MOTD + announcement (132/133)
// ---------------------------------------------------------------------------

// serverSetting returns a server setting ("" when unset or no store).
func (s *TCPServer) serverSetting(ctx context.Context, key string) string {
	if s.deps == nil || s.deps.Chat == nil {
		return ""
	}
	v, err := s.deps.Chat.GetServerSetting(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

// SetServerSettingAndAnnounce stores a server setting; the "announcement"
// key is additionally broadcast to all online clients.
func (s *TCPServer) SetServerSettingAndAnnounce(ctx context.Context, key, value string) error {
	if s.deps == nil || s.deps.Chat == nil {
		return errors.New("chat store unavailable")
	}
	if err := s.deps.Chat.SetServerSetting(ctx, key, value); err != nil {
		return err
	}
	if key == "announcement" && value != "" {
		s.broadcastEvent(eventAnnouncement, map[string]any{"text": value})
	}
	return nil
}

// parseChannelID parses a string channel id ("" = 0).
func parseChannelID(s string) int64 {
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}
