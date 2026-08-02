// admin.go binds the wave-5 server administration surfaces the client could
// not reach: the complaint list (173) and privilege token management
// (174/175/176), plus token redemption (MsgTokenUse), which until now was
// only reachable through ServerQuery — so the bootstrap admin token printed
// at first server start could never be redeemed from the app.
package main

import (
	"time"

	"voicx/internal/netproto"
)

// --- complaints (173) ---------------------------------------------------------

// ComplaintList returns every filed complaint. Gated server-side by the same
// check as the ban list, so a denial arrives as MsgError (code 4).
func (a *App) ComplaintList() (netproto.Complaints, error) {
	f, err := a.cmLoad().request(netproto.MsgComplaintList, netproto.MsgComplaints,
		netproto.ComplaintList{}, 5*time.Second)
	if err != nil {
		return netproto.Complaints{}, err
	}
	var resp netproto.Complaints
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.Complaints{}, err
	}
	return resp, nil
}

// ComplaintClear resolves complaints against a target and returns the
// refreshed list. An empty fromUniqueID clears every complaint against the
// target.
func (a *App) ComplaintClear(targetUniqueID, fromUniqueID string) (netproto.Complaints, error) {
	f, err := a.cmLoad().request(netproto.MsgComplaintClear, netproto.MsgComplaints,
		netproto.ComplaintClear{TargetUniqueID: targetUniqueID, FromUniqueID: fromUniqueID}, 5*time.Second)
	if err != nil {
		return netproto.Complaints{}, err
	}
	var resp netproto.Complaints
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.Complaints{}, err
	}
	return resp, nil
}

// --- privilege tokens (174) ---------------------------------------------------

// TokenList returns the privilege keys. Gated by b_virtualserver_token_list.
func (a *App) TokenList() (netproto.Tokens, error) {
	return a.tokenRequest(netproto.MsgTokenList, netproto.TokenList{})
}

// TokenAdd mints a privilege key and returns the refreshed list; the new key
// is the entry that was not present before (the list is oldest-first).
// channelID 0 makes a plain server-group key; groupID 0 mints an admin key
// and is admin-only.
func (a *App) TokenAdd(groupID, channelID int64, description string) (netproto.Tokens, error) {
	return a.tokenRequest(netproto.MsgTokenAdd, netproto.TokenAdd{
		GroupID: groupID, ChannelID: channelID, Description: description,
	})
}

// TokenDelete revokes a key and returns the refreshed list.
func (a *App) TokenDelete(token string) (netproto.Tokens, error) {
	return a.tokenRequest(netproto.MsgTokenDelete, netproto.TokenDelete{Token: token})
}

// tokenRequest is the shared round trip: all three token messages answer with
// MsgTokens, so the manager view can never drift from the server.
func (a *App) tokenRequest(mt netproto.MessageType, msg any) (netproto.Tokens, error) {
	f, err := a.cmLoad().request(mt, netproto.MsgTokens, msg, 5*time.Second)
	if err != nil {
		return netproto.Tokens{}, err
	}
	var resp netproto.Tokens
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.Tokens{}, err
	}
	return resp, nil
}

// TokenUse redeems a privilege key for the logged-in account. Success is
// silent: the grant arrives as the "token_used" event and the client refetches
// its permissions from there. Failures arrive as MsgError — 6 unknown key,
// 2 exhausted, 4 guest (redemption writes against an account row, and a guest
// has none).
func (a *App) TokenUse(token string) string {
	if err := a.cmLoad().write(netproto.MsgTokenUse, netproto.TokenUse{Token: token}); err != nil {
		return err.Error()
	}
	return ""
}
