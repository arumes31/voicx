// Package channels implements voicx channel lifecycle management: creation,
// deletion, type transitions, and automatic cleanup of temporary channels when
// they become empty.
//
// A temporary channel (ChannelTypeTemporary) is deleted automatically after a
// configurable grace period once it has zero members. Semi-permanent and
// permanent channels persist until explicitly deleted.
package channels

import (
	"fmt"

	"voicx/internal/safecast"
)

// ChannelType distinguishes temporary, semi-permanent, and permanent channels.
// The integer values mirror the channel_type SMALLINT column in the channels
// table (see internal/store/migrations/001_init.sql).
type ChannelType int8

const (
	// ChannelTypeTemporary is a channel that is automatically deleted after a
	// grace period once it becomes empty.
	ChannelTypeTemporary = 0
	// ChannelTypeSemiPermanent is a channel that persists across restarts but
	// is not protected from explicit deletion.
	ChannelTypeSemiPermanent = 1
	// ChannelTypePermanent is a channel that persists across restarts and is
	// intended to be long-lived (e.g. the server's default channel).
	ChannelTypePermanent = 2
)

// String returns a human-readable name for the channel type: "temporary",
// "semi-permanent", or "permanent". Unknown values return "unknown".
func (t ChannelType) String() string {
	switch t {
	case ChannelTypeTemporary:
		return "temporary"
	case ChannelTypeSemiPermanent:
		return "semi-permanent"
	case ChannelTypePermanent:
		return "permanent"
	default:
		return "unknown"
	}
}

// Valid reports whether t is one of the defined channel types.
func (t ChannelType) Valid() bool {
	switch t {
	case ChannelTypeTemporary, ChannelTypeSemiPermanent, ChannelTypePermanent:
		return true
	default:
		return false
	}
}

// ParseChannelType validates an integer before converting it to ChannelType.
// It is used at wire and database boundaries where narrowing first could turn
// an out-of-range value such as 256 into a valid temporary-channel value.
func ParseChannelType(value int) (ChannelType, error) {
	switch value {
	case int(ChannelTypeTemporary):
		return ChannelTypeTemporary, nil
	case int(ChannelTypeSemiPermanent):
		return ChannelTypeSemiPermanent, nil
	case int(ChannelTypePermanent):
		return ChannelTypePermanent, nil
	default:
		return 0, fmt.Errorf("invalid channel type %d", value)
	}
}

// ChannelSpec describes a channel to be created. It is the input to
// ChannelManager.CreateChannel.
type ChannelSpec struct {
	// Name is the channel display name. Must be non-empty.
	Name string
	// Topic is an optional channel topic/description.
	Topic string
	// ParentID is the parent channel ID. 0 means the channel is a root channel.
	ParentID int64
	// OrderIndex controls the sort order among siblings.
	OrderIndex int
	// Type is the channel type (temporary, semi-permanent, permanent).
	Type ChannelType
	// MaxClients is the maximum number of clients allowed in the channel. 0 or
	// negative means unlimited (stored as NULL).
	MaxClients int
	// Password is the plaintext channel password. If non-empty it is hashed
	// with auth.HashPassword before being stored. If empty, no password is set.
	Password string
	// NeededJoinPower is the i_channel_join_power a client must meet or exceed
	// to join the channel. 0 means no power requirement.
	NeededJoinPower int
	// CreatedBy is the user ID of the channel creator. 0 means unknown.
	CreatedBy int64

	// Per-channel Opus audio quality (migration 005). OpusBitrate is the
	// target bitrate in bits/s; 0 means the server default (32000).
	OpusBitrate int
	OpusFEC     bool
	OpusDTX     bool
	OpusStereo  bool
}

// ChannelUpdate describes the editable fields of a channel. Nil pointers
// leave the corresponding column untouched. It is the input to
// ChannelManager.UpdateChannel.
type ChannelUpdate struct {
	Topic           *string
	MaxClients      *int
	OpusBitrate     *int
	OpusFEC         *bool
	OpusDTX         *bool
	OpusStereo      *bool
	SlowModeSeconds *int
	Description     *string
	// NeededJoinPower (160), OrderIndex (163) and ParentID (168) are editable
	// after creation; ParentID 0 moves the channel to the root.
	NeededJoinPower *int
	OrderIndex      *int
	ParentID        *int64
	// InheritPermissions toggles resolving the parent's channel permissions
	// before the channel's own (157).
	InheritPermissions *bool
}

// Validate checks the spec for obvious errors and returns a descriptive error
// if any are found. It does not verify the parent channel's existence (that is
// done by the manager, which has access to the database).
func (s ChannelSpec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("channel name must not be empty")
	}
	if !s.Type.Valid() {
		return fmt.Errorf("invalid channel type %d", s.Type)
	}
	if s.MaxClients > 0 {
		if _, err := safecast.IntToInt32(s.MaxClients); err != nil {
			return fmt.Errorf("max clients is out of range: %w", err)
		}
	}
	return nil
}
