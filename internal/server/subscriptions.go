// subscriptions.go implements channel subscriptions (312): a client that
// holds i_channel_subscribe_power over a channel's
// i_channel_needed_subscribe_power receives that channel's chat without
// standing in it.
//
// Key distribution is the whole difficulty. Channel chat is sealed under the
// channel's scope key and the server relays ciphertext, so a subscriber that
// does not hold the generation would be handed bytes it can never open. A
// subscription therefore means exactly one thing for keys: an entitled
// subscriber is treated as a member of that scope by the sealed-key path —
// it receives the current generation on subscribe, every later generation on
// rotation, and archival generations through the normal bundle request. A
// caller that has published no X25519 key is REFUSED outright rather than
// subscribed into a stream of unreadable ciphertext.
//
// Entitlement is re-checked on the relay path, not only at subscribe time:
// the permission write sites push MsgPermsInvalid but have no seam that could
// tell this registry a grant was revoked, so the first message after a
// revocation is what drops the subscription.
package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"voicx/internal/netproto"
	"voicx/internal/permissions"
)

// maxSubscribeTargets caps one ChannelSubscribe request and maxSubscriptions
// caps a client's standing set. Each accepted target costs a persisted scope
// key generation on first use plus one box.SealAnonymous per rotation, so an
// uncapped set is a self-inflicted amplifier.
const (
	maxSubscribeTargets = 64
	maxSubscriptions    = 64
)

// handleChannelSubscribe adds or removes channel subscriptions and answers
// with the authoritative full set. The reply is never a delta: a client that
// applied a delta it half-received would drift from the server's view with
// nothing to correct it.
func (s *TCPServer) handleChannelSubscribe(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.ChannelSubscribe
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed channel_subscribe: "+err.Error())
	}
	if len(msg.ChannelIDs) == 0 || len(msg.ChannelIDs) > maxSubscribeTargets {
		return s.sendError(client, errCodeMalformed,
			fmt.Sprintf("channel_ids count must be 1..%d", maxSubscribeTargets))
	}
	if s.deps == nil || s.deps.State == nil {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}
	if client.rulesBlocked() {
		return s.sendError(client, errCodePermissionDenied,
			"accept the server rules before subscribing to channel chat")
	}
	// Same bucket as a chat send: an accepted target seals a key and a
	// dropped one is re-checked on the next relay, so the loop has a cost.
	if s.chatRate != nil && !s.chatRate.allow(client.UniqueID, time.Now()) {
		return s.sendError(client, errCodeMalformed, "chat rate limit exceeded — slow down")
	}

	if !msg.Subscribe {
		// The channel the caller stands in is implicit, so it survives this
		// unconditionally: it is not in the explicit set to begin with.
		s.deps.State.Unsubscribe(client.ID, msg.ChannelIDs)
		return s.sendSubscriptionState(client)
	}

	currentChannelID, e2ePublicKey, ok := s.deps.State.ClientChannelState(client.ID)
	if !ok {
		return s.sendError(client, errCodeUnavailable, "state backend unavailable")
	}
	if s.chatKeys == nil || !s.chatKeys.configured() {
		return s.sendError(client, errCodeUnavailable, "chat key manager unavailable")
	}
	if e2ePublicKey == "" {
		// Relaying to this client would be ciphertext it provably cannot
		// open, and a silent subscription is worse than none (312).
		return s.sendError(client, errCodePermissionDenied,
			"publish an encryption key before subscribing — a subscriber that cannot be sealed to could not read the channel")
	}

	held := map[int64]bool{}
	for _, id := range s.deps.State.Subscriptions(client.ID) {
		held[id] = true
	}
	var add []int64
	var refused []string
	for _, id := range msg.ChannelIDs {
		switch {
		case id == globalChatScope:
			refused = append(refused, "0 (global chat already reaches every client)")
		case id == currentChannelID:
			// Implicitly subscribed; the reply carries it either way.
		case held[id]:
			// Already subscribed.
		default:
			if _, ok := s.deps.State.GetChannel(id); !ok {
				refused = append(refused, fmt.Sprintf("%d (no such channel)", id))
				continue
			}
			if !s.subscribeAllowed(ctx, client, id) {
				refused = append(refused, fmt.Sprintf("%d (insufficient %s)", id,
					permissions.PermissionKeyChannelSubscribePower))
				continue
			}
			held[id] = true
			add = append(add, id)
		}
	}
	if len(held) > maxSubscriptions {
		return s.sendError(client, errCodeMalformed,
			fmt.Sprintf("too many subscriptions (max %d) — unsubscribe first", maxSubscriptions))
	}

	s.deps.State.Subscribe(client.ID, add)
	for _, id := range add {
		// The caller is entitled to this scope, which is what authorises
		// minting its first generation here.
		if err := s.deliverScopeKey(ctx, client, id); err != nil {
			s.deps.State.Unsubscribe(client.ID, []int64{id})
			refused = append(refused, fmt.Sprintf("%d (key delivery failed)", id))
			continue
		}
		if client.UserID != 0 && s.deps.Groups != nil {
			groupID, applied, err := s.deps.Groups.ApplyChannelGroupAutoAssignment(ctx, client.UserID, id)
			if err != nil {
				s.logger.Warn("channel-group auto assignment failed", zap.Int64("channel_id", id), zap.Error(err))
			} else if applied {
				if s.deps.Perms != nil {
					s.deps.Perms.Invalidate(client.UserID, id)
				}
				s.audit(ctx, "system", "channel_group_auto_assign", client.UniqueID,
					fmt.Sprintf("channel=%d group=%d", id, groupID))
				s.notifyPermsInvalid("channel_group_auto_assign", []string{client.UniqueID})
			}
		}
	}
	if len(refused) > 0 {
		_ = s.sendError(client, errCodePermissionDenied,
			"not subscribed to channel(s): "+strings.Join(refused, ", "))
	}
	return s.sendSubscriptionState(client)
}

// subscribeAllowed reports whether client may subscribe to channelID.
//
// Both sides resolve in the TARGET channel's context: the needed power is a
// channel-tier permission, so loading it for the channel the caller happens
// to stand in would gate every target on an unrelated channel's setting.
//
// An unset i_channel_subscribe_power is DENIED rather than treated as 0.
// Subscribing hands out a channel's scope key, so it follows the house rule
// for privileged actions (kick/move/ban) and not the open default of join —
// otherwise granting nothing would silently make every channel's chat
// readable by everyone the moment this shipped.
func (s *TCPServer) subscribeAllowed(ctx context.Context, client *Client, channelID int64) bool {
	if client.isAdmin() {
		return true
	}
	if s.deps == nil || s.deps.Perms == nil || s.deps.Resolver == nil {
		return false
	}
	tp := permissions.NewTieredPermissions()
	if client.UserID == 0 {
		if set, ok := s.guestGroupSet(ctx); ok {
			tp.Set(permissions.TierServerGroup, set)
		}
	} else {
		loaded, err := s.deps.Perms.LoadForClient(ctx, client.UserID, channelID)
		if err != nil {
			return false
		}
		tp = loaded
	}
	pc := &permChecker{resolver: s.deps.Resolver, tp: tp}
	return pc.powerAtLeast(permissions.PermissionKeyChannelSubscribePower,
		pc.neededPower(permissions.PermissionKeyChannelNeededSubscribePower))
}

// subscriptionSet returns the authoritative set for a client: its explicit
// subscriptions plus the channel it stands in, which is implicit and cannot
// be unsubscribed.
func (s *TCPServer) subscriptionSet(client *Client) []int64 {
	out := []int64{}
	if s.deps == nil || s.deps.State == nil {
		return out
	}
	seen := map[int64]bool{}
	if channelID, _, ok := s.deps.State.ClientChannelState(client.ID); ok && channelID != 0 {
		seen[channelID] = true
		out = append(out, channelID)
	}
	for _, id := range s.deps.State.Subscriptions(client.ID) {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sendSubscriptionState pushes the authoritative set to one client.
func (s *TCPServer) sendSubscriptionState(client *Client) error {
	return s.writeMessage(client, netproto.MsgSubscriptionState,
		netproto.SubscriptionState{ChannelIDs: s.subscriptionSet(client)})
}

// channelSubscribers returns the connected clients that receive channelID's
// chat WITHOUT standing in it. Clients that lost the power since subscribing
// are dropped here and told: this is the only place a revocation is observed.
func (s *TCPServer) channelSubscribers(ctx context.Context, channelID int64) []*Client {
	if channelID == globalChatScope || s.deps == nil || s.deps.State == nil {
		return nil
	}
	var out, revoked []*Client
	for _, sc := range s.deps.State.ChannelSubscribers(channelID) {
		if sc.ChannelID == channelID {
			continue // a member is already served by the channel fan-out
		}
		client, ok := s.clientByID(sc.ClientID)
		if !ok {
			continue
		}
		if !s.subscribeAllowed(ctx, client, channelID) {
			revoked = append(revoked, client)
			continue
		}
		out = append(out, client)
	}
	for _, c := range revoked {
		s.deps.State.Unsubscribe(c.ID, []int64{channelID})
		_ = s.sendSubscriptionState(c)
	}
	return out
}

// broadcastChannelScoped delivers a pre-wrapped event envelope to a channel's
// members and to its subscribers.
func (s *TCPServer) broadcastChannelScoped(ctx context.Context, channelID int64, payload []byte) {
	if s.deps == nil || s.deps.Broadcast == nil {
		return
	}
	s.deps.Broadcast.BroadcastToChannel(channelID, payload)
	for _, c := range s.channelSubscribers(ctx, channelID) {
		_ = s.deps.Broadcast.BroadcastToClient(c.ID, payload)
	}
}

// pushSubscriptionStateTo re-pushes the authoritative set to the named
// clients, so a drop they did not ask for still reaches them.
func (s *TCPServer) pushSubscriptionStateTo(clientIDs []string) {
	for _, id := range clientIDs {
		if client, ok := s.clientByID(id); ok {
			_ = s.sendSubscriptionState(client)
		}
	}
}
