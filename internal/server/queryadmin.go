// queryadmin.go implements the ServerQuery-facing additions of wave 10a:
// the resolved-permission overview (219) and the max-clients override
// enforcement (217).
package server

import (
	"context"
	"strconv"

	"voicx/internal/permissions"
)

// ResolvedPerm is one resolved permission of a user (219 permoverview).
type ResolvedPerm struct {
	Key   string
	Value int
	Grant int
	Tier  string
}

// PermOverview returns the resolved permission set of a user in an optional
// channel context: one entry per key present in any tier, resolved through
// the tier hierarchy (same rules as the client-facing permissions_query).
func (s *TCPServer) PermOverview(ctx context.Context, uniqueID string, channelID int64) ([]ResolvedPerm, error) {
	if s.deps == nil || s.deps.Auth == nil || s.deps.Perms == nil || s.deps.Resolver == nil {
		return nil, errPermsUnavailable
	}
	user, err := s.deps.Auth.LookupUser(ctx, uniqueID)
	if err != nil {
		return nil, err
	}
	tp, err := s.deps.Perms.LoadForClient(ctx, user.ID, channelID)
	if err != nil {
		return nil, err
	}
	var out []ResolvedPerm
	seen := make(map[permissions.PermissionKey]bool)
	for tier := permissions.Tier(0); tier <= permissions.TierChannel; tier++ {
		set, ok := tp.Get(tier)
		if !ok || set == nil {
			continue
		}
		for _, key := range set.Keys() {
			if seen[key] {
				continue
			}
			seen[key] = true
			p, winTier, err := s.deps.Resolver.Resolve(tp, key)
			if err != nil {
				continue
			}
			out = append(out, ResolvedPerm{
				Key:   string(p.Key),
				Value: p.Value,
				Grant: p.Grant,
				Tier:  winTier.String(),
			})
		}
	}
	return out, nil
}

// EffectiveMaxClients returns the connection cap: the configured max_clients,
// tightened by the serveredit override when one is set (217).
func (s *TCPServer) EffectiveMaxClients(ctx context.Context) int {
	max := s.cfg.MaxClients
	if v := s.serverSetting(ctx, "max_clients_override"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && (max <= 0 || n < max) {
			max = n
		}
	}
	return max
}
