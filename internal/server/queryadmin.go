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

// EffectiveMaxClients returns the current connection cap. Runtime server UI
// changes are folded into cfg under configMu and persisted for restart.
func (s *TCPServer) EffectiveMaxClients(ctx context.Context) int {
	s.configMu.RLock()
	max := s.cfg.MaxClients
	s.configMu.RUnlock()
	if value := s.serverSetting(ctx, "max_clients_override"); value != "" {
		if override, err := strconv.Atoi(value); err == nil && override >= 0 {
			return override
		}
	}
	return max
}
