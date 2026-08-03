// social.go defines the Wails-bound API for wave-8b presence and social
// features: presence status, pokes, and the public server-info query.
package main

import (
	"time"

	"voicx/internal/netproto"
)

// SetStatus sets the caller's presence status ("online"|"away"|"busy") and
// optional status message (307-309).
func (a *App) SetStatus(status, message string) string {
	// (390) the idle timer publishes the autoAwaySentinel; the text other
	// clients actually see comes from the user's configured away message.
	if status == "away" && message == autoAwaySentinel {
		message = a.autoAwayMessage()
	}
	if err := a.cmLoad().write(netproto.MsgSetStatus, netproto.SetStatus{Status: status, Message: message}); err != nil {
		return err.Error()
	}
	return ""
}

// Poke sends a poke to a client (321/322; server enforces permission +
// cooldown, errors arrive via servererror).
func (a *App) Poke(clientID, message string) string {
	if err := a.cmLoad().write(netproto.MsgPoke, netproto.Poke{ClientID: clientID, Message: message}); err != nil {
		return err.Error()
	}
	return ""
}

// ServerInfo returns the server's public information (313).
func (a *App) ServerInfo() (netproto.ServerInfoResponse, error) {
	f, err := a.cmLoad().request(netproto.MsgServerInfoQuery, netproto.MsgServerInfoResponse,
		netproto.ServerInfoQuery{}, 5*time.Second)
	if err != nil {
		return netproto.ServerInfoResponse{}, err
	}
	var resp netproto.ServerInfoResponse
	if err := decodeJSON(f, &resp); err != nil {
		return netproto.ServerInfoResponse{}, err
	}
	return resp, nil
}
