// tabs.go implements multi-server tabs (281). Architecture (the low-churn
// option): the App keeps one connManager per tab; the active tab's manager
// is what all existing bindings talk to (a.cm), so the bound API stays
// unchanged. Events from the ACTIVE tab go to the frontend under their
// plain names (snapshot/event/channellist/...), so the frontend keeps its
// single-state model. Events from BACKGROUND tabs are journaled in Go
// (bounded, state-carrying frames only) and replayed as plain events when
// their tab becomes active — after a "tab_reset" marker that tells the
// frontend to clear its chat/tree state first. Background chat events only
// bump the tab's unread/mention badges (emitted as "tab_update").
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
)

// TabInfo describes one server tab for the tab bar.
type TabInfo struct {
	ID        string `json:"id"`
	Addr      string `json:"addr"`
	Nickname  string `json:"nickname"`
	Connected bool   `json:"connected"`
	Active    bool   `json:"active"`
	Unread    int    `json:"unread"`
	Mentions  int    `json:"mentions"`
}

// journalEntry is one buffered state-carrying event of a background tab.
type journalEntry struct {
	name    string
	payload string
}

// tabState bundles a tab's manager with its replay journal and badges.
type tabState struct {
	cm       *connManager
	info     TabInfo
	journal  []journalEntry // events since the last snapshot
	maxItems int
}

// tabsMu guards tabs/activeID. cmMu (app.go) guards the active cm pointer.
var (
	nextTabSeq = 0
)

// journalCap bounds the per-tab replay buffer (chatty channels could
// otherwise grow it without limit).
const journalCap = 2000

// tabSink routes a tab's events through the App relay instead of directly
// to the Wails runtime.
type tabSink struct {
	app   *App
	tabID string
}

// Emit implements eventSink.
func (s tabSink) Emit(name string, payload any) {
	s.app.relayTabEvent(s.tabID, name, payload)
}

// relayTabEvent forwards active-tab events to the frontend and journals
// background-tab events, updating badges.
func (a *App) relayTabEvent(tabID, name string, payload any) {
	a.tabsMu.Lock()
	active := a.activeID == tabID
	ts := a.tabs[tabID]
	if name == "disconnected" && ts != nil {
		ts.info.Connected = false
	}
	a.tabsMu.Unlock()

	if active || ts == nil {
		a.emitPlain(name, payload)
		if name == "disconnected" {
			a.emitTabsUpdate()
		}
		return
	}

	// Background tab: journal state-carrying frames for replay.
	text, _ := payload.(string)
	switch name {
	case "snapshot":
		ts.journal = ts.journal[:0]
		ts.journal = append(ts.journal, journalEntry{name, text})
	case "channellist", "event":
		ts.journal = append(ts.journal, journalEntry{name, text})
		if len(ts.journal) > journalCap {
			ts.journal = ts.journal[len(ts.journal)-journalCap:]
		}
		if name == "event" {
			a.countBadge(ts, text)
		}
	case "disconnected":
		ts.info.Connected = false
		a.emitTabsUpdate()
		return
	default:
		// ice/offer/avatar/servererror from background tabs: drop (voice is
		// active-tab only this wave).
		return
	}
	a.emitTabsUpdate()
}

// countBadge increments unread/mention counters for background chat events.
func (a *App) countBadge(ts *tabState, eventJSON string) {
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(eventJSON), &env); err != nil || env.Type != "chat" {
		return
	}
	var chat struct {
		From string `json:"from"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(env.Data, &chat); err != nil {
		return
	}
	ts.info.Unread++
	nick := ts.cm.nickname
	if nick != "" && strings.Contains(chat.Text, "@"+nick) {
		ts.info.Mentions++
		// (290) mention in a background tab: flash the taskbar + badge.
		a.FlashWindow()
		trayAddMention()
	}
}

// emitPlain forwards an event to the Wails runtime under its plain name.
func (a *App) emitPlain(name string, payload any) {
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, name, payload)
	}
}

// emitTabsUpdate sends the tab bar model to the frontend.
func (a *App) emitTabsUpdate() {
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, "tab_update", a.ListTabs())
	}
}

// tabs/accessors ---------------------------------------------------------

// tabsRegistry returns the tab map (for tests).
func (a *App) tabsRegistry() map[string]*tabState {
	a.tabsMu.Lock()
	defer a.tabsMu.Unlock()
	return a.tabs
}

// activeCM returns the active tab's connManager (may be nil).
func (a *App) activeCM() *connManager {
	a.tabsMu.Lock()
	defer a.tabsMu.Unlock()
	if ts := a.tabs[a.activeID]; ts != nil {
		return ts.cm
	}
	return nil
}

// newTab creates a tab with a fresh connManager and registers it.
func (a *App) newTab() (string, *tabState) {
	a.tabsMu.Lock()
	defer a.tabsMu.Unlock()
	nextTabSeq++
	id := fmt.Sprintf("tab-%d", nextTabSeq)
	cm := newConnManager(a.ctx)
	cm.tabID = id
	cm.sink = tabSink{app: a, tabID: id}
	cm.allowPlaintext = a.settings.AllowPlaintext
	if a.knownServers != nil {
		cm.knownServers = a.knownServers
	}
	ts := &tabState{cm: cm, info: TabInfo{ID: id}}
	a.tabs[id] = ts
	return id, ts
}

// activateLocked switches the active tab (caller holds no locks): swaps the
// binding target, tells the frontend to reset, then replays the journal.
func (a *App) activate(tabID string) {
	a.tabsMu.Lock()
	ts := a.tabs[tabID]
	a.activeID = tabID
	if ts != nil {
		a.cmStore(ts.cm)
		ts.info.Unread = 0
		ts.info.Mentions = 0
	} else {
		a.cmStore(nil)
	}
	a.tabsMu.Unlock()

	// The frontend clears chat/tree on tab_reset; the replay below rebuilds
	// state from the journaled frames in order.
	trayClearMentions()
	a.emitPlain("tab_reset", tabID)
	if ts != nil {
		for _, e := range ts.journal {
			a.emitPlain(e.name, e.payload)
		}
	}
	a.emitTabsUpdate()
}

// --- bound API ---------------------------------------------------------------

// ListTabs returns the tab bar model.
func (a *App) ListTabs() []TabInfo {
	a.tabsMu.Lock()
	defer a.tabsMu.Unlock()
	out := make([]TabInfo, 0, len(a.tabs))
	for id, ts := range a.tabs {
		info := ts.info
		info.ID = id
		info.Active = id == a.activeID
		if ts.cm != nil {
			info.Connected = ts.cm.connected()
			if info.Addr == "" {
				info.Addr = ts.cm.addr
			}
			if info.Nickname == "" {
				info.Nickname = ts.cm.nickname
			}
		}
		out = append(out, info)
	}
	return out
}

// ConnectTab connects to a server in a NEW tab and activates it. It returns
// the tab ID on success or the failure reason (mirroring Connect). The
// bookmark this login came from is unknown here; see ConnectBookmarkTab.
func (a *App) ConnectTab(addr, nickname, password, serverPassword string) string {
	return a.ConnectBookmarkTab("", addr, nickname, password, serverPassword)
}

// ConnectBookmarkTab is ConnectTab with the originating bookmark's Name.
// Bookmarks must be identified explicitly: a nickname override (334)
// replaces the login nickname before connecting, so addr+nickname no longer
// identifies the bookmark that per-server settings (300/335) belong to.
func (a *App) ConnectBookmarkTab(bookmark, addr, nickname, password, serverPassword string) string {
	if addr == "" || nickname == "" {
		return "server address and nickname are required"
	}
	id, ts := a.newTab()
	err := ts.cm.connect(addr, nickname, password, serverPassword)
	if err != "" {
		a.removeTab(id)
		if err == errFingerprintMismatch.Error() {
			_, presented, _ := ts.cm.securitySnapshot()
			return fmt.Sprintf("%s: the server certificate changed (presented: %s). Possible MITM — only trust it if you expected the change", err, presented)
		}
		return err
	}
	ts.info.Addr = addr
	ts.info.Nickname = nickname
	a.onTabConnected(ts.cm, bookmark, addr, nickname)
	a.activate(id)
	return ""
}

// ConnectGuestTab connects as a guest in a NEW tab (mirroring ConnectGuest).
func (a *App) ConnectGuestTab(addr, nickname string) string {
	return a.ConnectGuestBookmarkTab("", addr, nickname)
}

// ConnectGuestBookmarkTab is ConnectGuestTab with the originating bookmark's
// Name (see ConnectBookmarkTab).
func (a *App) ConnectGuestBookmarkTab(bookmark, addr, nickname string) string {
	if addr == "" || nickname == "" {
		return "server address and nickname are required"
	}
	id, ts := a.newTab()
	if err := ts.cm.connect(addr, nickname, "", ""); err != "" {
		a.removeTab(id)
		return err
	}
	ts.info.Addr = addr
	ts.info.Nickname = nickname
	a.onTabConnected(ts.cm, bookmark, addr, nickname)
	a.activate(id)
	return ""
}

// lookupBookmark resolves the bookmark a connection belongs to, by explicit
// Name or (for callers that know no name) by the nickname that was sent as
// the login nickname — the stored one or its override (334). Neither key is
// unique: names are auto-generated ("nick @ addr"), freely editable, and
// nothing rejects a duplicate, so both branches require agreement on addr AND
// resolve only when exactly one bookmark matches. Guessing between candidates
// applies a foreign hotkey profile (300) and uploads a foreign avatar
// override (335) to the server.
func (a *App) lookupBookmark(name, addr, nickname string) *Bookmark {
	var match *Bookmark
	for i := range a.settings.Bookmarks {
		b := &a.settings.Bookmarks[i]
		if b.Addr != addr {
			continue
		}
		if name != "" {
			if b.Name != name {
				continue
			}
		} else if b.Nickname != nickname && b.NicknameOverride != nickname {
			continue
		}
		if match != nil {
			return nil // ambiguous: treat as a plain login
		}
		match = b
	}
	return match
}

// onTabConnected records the recents entry (282), applies the bookmark's
// hotkey profile (300), and uploads the avatar override (335). cm is the
// newly connected tab's manager (it is not active yet); bookmark is the
// originating bookmark's Name ("" = direct login).
func (a *App) onTabConnected(cm *connManager, bookmark, addr, nickname string) {
	a.RecordRecent(addr, nickname)
	b := a.lookupBookmark(bookmark, addr, nickname)
	if b == nil {
		a.ApplyHotkeyProfile("default")
		return
	}
	if b.Profile != "" {
		a.ApplyHotkeyProfile(b.Profile)
	} else {
		a.ApplyHotkeyProfile("default")
	}
	// (335) per-server avatar override: upload after connect.
	if b.AvatarOverrideB64 != "" {
		if err := cm.write(netproto.MsgAvatarSet, netproto.AvatarSet{DataBase64: b.AvatarOverrideB64}); err != nil {
			log.Printf("avatar override upload failed: %v", err)
		}
	}
}

// SetActiveTab switches the active tab (replays its journaled state).
func (a *App) SetActiveTab(tabID string) {
	a.tabsMu.Lock()
	_, ok := a.tabs[tabID]
	a.tabsMu.Unlock()
	if !ok {
		return
	}
	a.activate(tabID)
}

// CloseTab disconnects and removes a tab; when it was active, the next tab
// (if any) becomes active.
func (a *App) CloseTab(tabID string) {
	a.tabsMu.Lock()
	ts := a.tabs[tabID]
	wasActive := a.activeID == tabID
	a.tabsMu.Unlock()
	if ts == nil {
		return
	}
	ts.cm.disconnect()
	a.removeTab(tabID)
	if wasActive {
		a.tabsMu.Lock()
		var next string
		for id := range a.tabs {
			next = id
			break
		}
		a.tabsMu.Unlock()
		a.activate(next) // empty next = no tabs left (frontend shows login)
	} else {
		a.emitTabsUpdate()
	}
}

// appWithCM builds an App whose active connManager is cm (test helper).
func appWithCM(cm *connManager) *App {
	a := &App{tabs: map[string]*tabState{}, hotkeys: map[string]*hotkeyReg{}}
	a.cmStore(cm)
	return a
}

// removeTab deletes a tab from the registry.
func (a *App) removeTab(tabID string) {
	a.tabsMu.Lock()
	delete(a.tabs, tabID)
	a.tabsMu.Unlock()
}
