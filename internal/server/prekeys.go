package server

import (
	"context"
	"fmt"

	"voicx/internal/e2ee"
	"voicx/internal/netproto"
	"voicx/internal/store"
)

const maxPublishedOneTimePreKeys = 100

func (s *TCPServer) handlePreKeyPublish(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PreKeyPublish
	if err := netproto.Decode(f, &msg); err != nil {
		return s.sendError(client, errCodeMalformed, "malformed prekey publish")
	}
	if client.UserID <= 0 {
		return s.sendError(client, errCodePermissionDenied, "registered account required for asynchronous prekeys")
	}
	if s.deps == nil || s.deps.PreKeys == nil {
		return s.sendError(client, errCodeUnavailable, "prekey store unavailable")
	}
	bundle := e2ee.PreKeyBundle{
		IdentityDH: msg.IdentityDH, SigningPublic: msg.SigningPublic,
		SignedPreKeyID: msg.SignedPreKeyID, SignedPreKey: msg.SignedPreKey, Signature: msg.Signature,
	}
	if msg.SignedPreKeyID == 0 || !e2ee.VerifyPreKeyBundle(bundle) || len(msg.OneTimePreKeys) > maxPublishedOneTimePreKeys {
		return s.sendError(client, errCodeMalformed, "invalid signed prekey bundle")
	}
	seen := make(map[uint32]bool, len(msg.OneTimePreKeys))
	oneTime := make([]store.PreKey, 0, len(msg.OneTimePreKeys))
	for _, key := range msg.OneTimePreKeys {
		if key.KeyID == 0 || len(key.PublicKey) != 32 || seen[key.KeyID] {
			return s.sendError(client, errCodeMalformed, "invalid or duplicate one-time prekey")
		}
		seen[key.KeyID] = true
		oneTime = append(oneTime, store.PreKey{KeyID: key.KeyID, PublicKey: key.PublicKey, OneTime: true})
	}
	if err := s.deps.PreKeys.PublishPreKeyBundle(ctx, client.UserID, store.PreKeyBundle{
		IdentityDH: msg.IdentityDH, SigningPublic: msg.SigningPublic,
		SignedPreKeyID: msg.SignedPreKeyID, SignedPreKey: msg.SignedPreKey, Signature: msg.Signature,
	}, oneTime); err != nil {
		return s.sendError(client, errCodeUnavailable, "publishing prekeys failed")
	}
	s.audit(ctx, client.UniqueID, "e2ee_prekeys_publish", client.UniqueID, fmt.Sprintf("one_time=%d", len(oneTime)))
	return nil
}

func (s *TCPServer) handlePreKeyQuery(ctx context.Context, client *Client, f *netproto.Frame) error {
	var msg netproto.PreKeyQuery
	if err := netproto.Decode(f, &msg); err != nil || msg.UniqueID == "" {
		return s.sendError(client, errCodeMalformed, "target unique ID is required")
	}
	if s.deps == nil || s.deps.PreKeys == nil || s.deps.Auth == nil {
		return s.sendError(client, errCodeUnavailable, "prekey directory unavailable")
	}
	target, err := s.deps.Auth.LookupUser(ctx, msg.UniqueID)
	if err != nil {
		return s.sendError(client, errCodeNotFound, "prekey bundle not found")
	}
	bundle, err := s.deps.PreKeys.ConsumePreKeyBundle(ctx, target.ID)
	if err != nil {
		return s.sendError(client, errCodeUnavailable, "loading prekey bundle failed")
	}
	if bundle == nil {
		return s.sendError(client, errCodeNotFound, "prekey bundle not found")
	}
	response := netproto.PreKeyBundle{
		UniqueID: msg.UniqueID, IdentityDH: bundle.IdentityDH, SigningPublic: bundle.SigningPublic,
		SignedPreKeyID: bundle.SignedPreKeyID, SignedPreKey: bundle.SignedPreKey, Signature: bundle.Signature,
	}
	if bundle.OneTimePreKey != nil {
		response.OneTimeKeyID = bundle.OneTimePreKey.KeyID
		response.OneTimePreKey = bundle.OneTimePreKey.PublicKey
	}
	return s.writeMessage(client, netproto.MsgPreKeyBundle, response)
}
