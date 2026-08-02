// permkeys.go validates permission identifiers accepted by channeladdperm
// (220). Without it a typo ("i_client_needed_talkpower") silently creates a
// permission row nothing ever resolves, so the operator sees a stored,
// audited, permanently inert setting.
//
// The registry is built from the permissions package constants rather than
// from string literals, so it cannot drift from the resolver's vocabulary; it
// lives here because the permissions package exposes no lookup of its own.
package query

import "voicx/internal/permissions"

// knownPermKeys is the set of permission identifiers the resolver understands.
var knownPermKeys = func() map[string]bool {
	keys := []permissions.PermissionKey{
		permissions.PermissionKeyChannelJoinPower,
		permissions.PermissionKeyChannelNeededJoinPower,
		permissions.PermissionKeyChannelJoinIgnorePassword,
		permissions.PermissionKeyChannelSubscribePower,
		permissions.PermissionKeyChannelNeededSubscribePower,
		permissions.PermissionKeyChannelCreateChild,
		permissions.PermissionKeyChannelCreatePermanent,
		permissions.PermissionKeyChannelCreateSemiPermanent,
		permissions.PermissionKeyChannelCreateTemporary,
		permissions.PermissionKeyChannelDelete,
		permissions.PermissionKeyChannelModify,
		permissions.PermissionKeyChannelModifyPower,
		permissions.PermissionKeyChannelNeededModifyPower,
		permissions.PermissionKeyClientKickFromChannelPower,
		permissions.PermissionKeyClientNeededKickFromChannelPower,
		permissions.PermissionKeyClientKickFromServerPower,
		permissions.PermissionKeyClientNeededKickFromServerPower,
		permissions.PermissionKeyClientBan,
		permissions.PermissionKeyClientBanPower,
		permissions.PermissionKeyClientNeededBanPower,
		permissions.PermissionKeyClientMovePower,
		permissions.PermissionKeyClientNeededMovePower,
		permissions.PermissionKeyClientUseChannelCommand,
		permissions.PermissionKeyClientTalkPower,
		permissions.PermissionKeyClientNeededTalkPower,
		permissions.PermissionKeyClientWhisperPower,
		permissions.PermissionKeyClientNeededWhisperPower,
		permissions.PermissionKeyClientVideoPublish,
		permissions.PermissionKeyClientPrioritySpeaker,
		permissions.PermissionKeyClientIgnoreAntiflood,
		permissions.PermissionKeyClientRequestTalker,
		permissions.PermissionKeyFTFileUploadPower,
		permissions.PermissionKeyFTNeededFileUploadPower,
		permissions.PermissionKeyFTFileDownloadPower,
		permissions.PermissionKeyFTNeededFileDownloadPower,
		permissions.PermissionKeyFTFileDelete,
		permissions.PermissionKeyClientPokePower,
		permissions.PermissionKeyClientNeededPokePower,
		permissions.PermissionKeyClientPoke,
		permissions.PermissionKeyClientServerQueryView,
		permissions.PermissionKeyClientRemoteAddressView,
		permissions.PermissionKeyPermissionModifyPower,
		permissions.PermissionKeyPermissionModifyPowerI,
		permissions.PermissionKeyVirtualserverSelect,
		permissions.PermissionKeyVirtualserverInfoView,
		permissions.PermissionKeyVirtualserverConnectionInfoView,
		permissions.PermissionKeyVirtualserverRecording,
		permissions.PermissionKeyVirtualserverTokenList,
		permissions.PermissionKeyVirtualserverTokenAdd,
		permissions.PermissionKeyVirtualserverTokenUse,
		permissions.PermissionKeyVirtualserverTokenDelete,
		permissions.PermissionKeyChatDeleteAny,
		permissions.PermissionKeyChatMentionAll,
		permissions.PermissionKeyChatSlowmodeBypass,
		permissions.PermissionKeyEmojiManage,
		permissions.PermissionKeyChatFilterManage,
		permissions.PermissionKeyClientIsBot,
		permissions.PermissionKeyServerGroupManage,
		permissions.PermissionKeyChannelGroupManage,
		permissions.PermissionKeyPermissionManage,
		permissions.PermissionKeyAuditView,
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[string(k)] = true
	}
	return set
}()

// knownPermKey reports whether key is a permission the resolver evaluates.
func knownPermKey(key string) bool { return knownPermKeys[key] }
