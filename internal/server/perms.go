// perms.go implements the permission middleware for the TCP control server.
// Instead of ad-hoc checks scattered through the handlers, every privileged
// action goes through a permChecker: a per-request wrapper around the
// caller's TieredPermissions (loaded via permissions.Loader for the caller's
// current channel context) and the shared permissions.Resolver.
//
// Semantics applied here, on top of the resolver:
//   - Server admins (users.is_admin) bypass all checks.
//   - ErrPermissionNotSet on a caller's own power/boolean permission means
//     DENIED for privileged actions (create, delete, kick, ban, move).
//   - ErrPermissionNotSet on a target's needed_*_power means 0, matching the
//     TS3 default needed power.
//   - Joining a channel defaults to allowed: an unset i_channel_join_power
//     counts as 0 and the needed join power is 0 (channels carry no
//     needed-power storage yet).
package server

import (
	"context"
	"errors"
	"fmt"

	"voicx/internal/permissions"
)

// errPermsUnavailable is returned by permCheckerFor when the permission
// backend is not wired into the server.
var errPermsUnavailable = errors.New("permission backend unavailable")

// permChecker resolves permissions for one client in one request context.
// It is read-only after construction and safe to use within a single handler.
type permChecker struct {
	resolver *permissions.Resolver
	tp       permissions.TieredPermissions
	admin    bool
}

// permCheckerFor loads the client's tiered permissions (in the context of the
// channel the client currently occupies) and wraps them in a permChecker.
// Guests (userID 0, no users row) short-circuit to an empty set without any
// database access: deny-on-unset semantics then apply like for everyone else.
func (s *TCPServer) permCheckerFor(ctx context.Context, client *Client) (*permChecker, error) {
	if s.deps == nil || s.deps.Perms == nil || s.deps.Resolver == nil {
		return nil, errPermsUnavailable
	}
	if client.UserID == 0 {
		return &permChecker{
			resolver: s.deps.Resolver,
			tp:       permissions.NewTieredPermissions(),
			admin:    false,
		}, nil
	}
	var channelID int64
	if s.deps.State != nil {
		if sc, ok := s.deps.State.GetClient(client.ID); ok {
			channelID = sc.ChannelID
		}
	}
	tp, err := s.deps.Perms.LoadForClient(ctx, client.UserID, channelID)
	if err != nil {
		return nil, fmt.Errorf("loading permissions: %w", err)
	}
	return &permChecker{
		resolver: s.deps.Resolver,
		tp:       tp,
		admin:    client.isAdmin(),
	}, nil
}

// granted resolves a boolean (b_*) permission. ErrPermissionNotSet means the
// permission is not granted.
func (p *permChecker) granted(key permissions.PermissionKey) bool {
	if p.admin {
		return true
	}
	ok, _, err := p.resolver.ResolveBool(p.tp, key)
	if err != nil {
		return false
	}
	return ok
}

// powerAtLeast reports whether the client's resolved integer (i_*) power
// permission meets or exceeds the needed value. ErrPermissionNotSet means the
// power is not granted (privileged actions default to denied).
func (p *permChecker) powerAtLeast(key permissions.PermissionKey, needed int) bool {
	if p.admin {
		return true
	}
	ok, _, err := p.resolver.IsGranted(p.tp, key, needed)
	if err != nil {
		return false
	}
	return ok
}

// neededPower resolves a needed_*_power permission, defaulting to 0 when it
// is not set (the TS3 default needed power).
func (p *permChecker) neededPower(key permissions.PermissionKey) int {
	v, _, err := p.resolver.ResolveInt(p.tp, key)
	if err != nil {
		return 0
	}
	return v
}

// power resolves an integer (i_*) power permission's value, defaulting to 0
// when it is not set.
func (p *permChecker) power(key permissions.PermissionKey) int {
	v, _, err := p.resolver.ResolveInt(p.tp, key)
	if err != nil {
		return 0
	}
	return v
}

// joinAllowed reports whether the client may join a channel whose needed join
// power is needed. An unset i_channel_join_power counts as the default
// power 0.
func (p *permChecker) joinAllowed(needed int) bool {
	if p.admin {
		return true
	}
	return p.power(permissions.PermissionKeyChannelJoinPower) >= needed
}

// talkAllowed reports whether the client may transmit audio. An unset
// i_client_talk_power means allowed (the TS3 default talk power 0 meets the
// default needed talk power 0); an explicitly negated entry denies.
func (p *permChecker) talkAllowed() bool {
	return p.notNegated(permissions.PermissionKeyClientTalkPower)
}

// whisperAllowed reports whether the client may configure a whisper list.
// Same semantics as talkAllowed with i_client_whisper_power.
func (p *permChecker) whisperAllowed() bool {
	return p.notNegated(permissions.PermissionKeyClientWhisperPower)
}

// videoPublishAllowed reports whether the client may publish video. An unset
// b_client_video_publish means allowed (open default, consistent with the
// talk gate); a negated entry or an explicit value of 0 denies.
func (p *permChecker) videoPublishAllowed() bool {
	if p.admin {
		return true
	}
	perm, _, err := p.resolver.Resolve(p.tp, permissions.PermissionKeyClientVideoPublish)
	if errors.Is(err, permissions.ErrPermissionNotSet) {
		return true
	}
	if err != nil {
		return false
	}
	return !perm.Negate && perm.Value != 0
}

// ftUploadAllowed reports whether the client may upload files. An unset
// i_ft_file_upload_power means allowed (TS3 default); a negated entry denies.
func (p *permChecker) ftUploadAllowed() bool {
	return p.notNegated(permissions.PermissionKeyFTFileUploadPower)
}

// ftDownloadAllowed reports whether the client may download or list files.
// Same semantics as ftUploadAllowed with i_ft_file_download_power.
func (p *permChecker) ftDownloadAllowed() bool {
	return p.notNegated(permissions.PermissionKeyFTFileDownloadPower)
}

// notNegated resolves key and reports allowed unless the winning entry is an
// explicit negate. An unset key means allowed.
func (p *permChecker) notNegated(key permissions.PermissionKey) bool {
	if p.admin {
		return true
	}
	perm, _, err := p.resolver.Resolve(p.tp, key)
	if errors.Is(err, permissions.ErrPermissionNotSet) {
		return true
	}
	if err != nil {
		return false
	}
	return !perm.Negate
}
