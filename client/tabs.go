// tabs.go implements multi-server tabs (281). Architecture (the low-churn
// option): the App keeps one connManager per tab; the active tab's manager
// is what all existing bindings talk to (a.cm), so the bound API stays
// unchanged. State-carrying events from every tab are journaled in Go, while
// events from the ACTIVE tab also go to the frontend under their plain names
// (snapshot/event/channellist/...), so the frontend keeps its single-state
// model. The bounded journal is replayed as plain events when a tab becomes
// active — after a "tab_reset" marker that tells the frontend to clear its
// chat/tree state first. Background chat events also bump the tab's
// unread/mention badges (emitted as "tab_update").
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"unicode"
	"unicode/utf8"

	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"voicx/internal/netproto"
)

// ConnectTabResult identifies the tab created by a successful connection.
type ConnectTabResult struct {
	TabID string `json:"tab_id"`
	Error string `json:"error"`
}

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
	cm      *connManager
	info    TabInfo
	journal []journalEntry // events since the last snapshot
}

// tabsMu guards tabs/activeID and every mutable tabState field.

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

// relayTabEvent forwards active-tab events to the frontend and journals every
// tab's state-carrying events. Active events must be journaled too: they are
// the state that has to be replayed after switching away and back.
func (a *App) relayTabEvent(tabID, name string, payload any) {
	// Snapshot the nickname before taking tabsMu: tab state is protected by
	// tabsMu, while connection state belongs to the manager. They must not be
	// nested in that order.
	a.tabsMu.Lock()
	cm := (*connManager)(nil)
	if ts := a.tabs[tabID]; ts != nil {
		cm = ts.cm
	}
	a.tabsMu.Unlock()
	nickname := ""
	if cm != nil {
		cm.mu.Lock()
		nickname = cm.nickname
		cm.mu.Unlock()
	}

	a.tabsMu.Lock()
	active := a.activeID == tabID
	generation := a.activationGeneration
	ts := a.tabs[tabID]
	if ts == nil {
		a.tabsMu.Unlock()
		// A manager may finish an async transfer/decrypt after its tab has
		// closed. It is never safe to route that event to the current tab.
		return
	}
	mention := false
	if name == "disconnected" {
		ts.info.Connected = false
	}
	text, _ := payload.(string)
	switch name {
	case "snapshot":
		ts.journal = ts.journal[:0]
		ts.journal = append(ts.journal, journalEntry{name, text})
	case "channellist", "subscriptions", "server_rules", "event":
		ts.journal = append(ts.journal, journalEntry{name, text})
		if len(ts.journal) > journalCap {
			ts.journal = ts.journal[len(ts.journal)-journalCap:]
		}
		if !active && name == "event" {
			mention = a.countBadge(ts, text, nickname)
		}
	}
	a.tabsMu.Unlock()

	if mention {
		// (290) mention in a background tab: flash the taskbar + badge.
		a.FlashWindow()
		trayAddMention()
	}
	if active {
		// Publication shares the activation sequencer. Revalidate after
		// acquiring it: an old active relay cannot escape after a newer reset,
		// and a new relay cannot precede its reset batch.
		a.activationPublishMu.Lock()
		a.tabsMu.Lock()
		stillActive := a.activeID == tabID && a.activationGeneration == generation && a.tabs[tabID] == ts
		a.tabsMu.Unlock()
		if stillActive {
			if name == "disconnected" {
				traySetConnected(false)
			}
			a.emitPlain(name, payload)
			if name == "disconnected" {
				a.emitTabsUpdate()
			}
		}
		a.activationPublishMu.Unlock()
		return
	}
	if name != "snapshot" && name != "channellist" && name != "subscriptions" &&
		name != "server_rules" && name != "event" && name != "disconnected" {
		// ice/offer/avatar/servererror from background tabs: drop (voice is
		// active-tab only this wave).
		return
	}
	a.emitTabsUpdate()
}

// countBadge increments unread/mention counters for background chat events.
func (a *App) countBadge(ts *tabState, eventJSON, nickname string) bool {
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(eventJSON), &env); err != nil || env.Type != "chat" {
		return false
	}
	var chat struct {
		From string `json:"from"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(env.Data, &chat); err != nil {
		return false
	}
	ts.info.Unread++
	if nickname != "" && containsMention(chat.Text, nickname) {
		ts.info.Mentions++
		return true
	}
	return false
}

// containsMention recognizes an exact Unicode case-insensitive @nickname.
// A word-like suffix would address somebody else (@annex is not @ann), while
// punctuation, whitespace, or end-of-text cleanly terminates the mention.
func containsMention(text, nickname string) bool {
	if nickname == "" {
		return false
	}
	nicknameRunes := utf8.RuneCountInString(nickname)
	for offset, r := range text {
		if r != '@' {
			continue
		}
		start := offset + utf8.RuneLen(r)
		end := start
		for range nicknameRunes {
			if end == len(text) {
				break
			}
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
		}
		if utf8.RuneCountInString(text[start:end]) != nicknameRunes || !strings.EqualFold(text[start:end], nickname) {
			continue
		}
		if end == len(text) {
			return true
		}
		next, _ := utf8.DecodeRuneInString(text[end:])
		if !unicode.IsLetter(next) && !unicode.IsDigit(next) && next != '_' && next != '-' {
			return true
		}
	}
	return false
}

// emitPlain forwards an event to the Wails runtime under its plain name.
func (a *App) emitPlain(name string, payload any) {
	if a.eventEmit != nil {
		a.eventEmit(name, payload)
		return
	}
	if a.ctx != nil {
		wailsRuntime.EventsEmit(a.ctx, name, payload)
	}
}

// emitTabsUpdate sends the tab bar model to the frontend.
func (a *App) emitTabsUpdate() {
	a.emitPlain("tab_update", a.ListTabs())
}

// tabs/accessors ---------------------------------------------------------

// tabsRegistry returns a detached snapshot of the tab registry.
func (a *App) tabsRegistry() map[string]*tabState {
	a.tabsMu.Lock()
	defer a.tabsMu.Unlock()
	copy := make(map[string]*tabState, len(a.tabs))
	for id, ts := range a.tabs {
		copy[id] = ts
	}
	return copy
}

// newTab creates a tab with a fresh connManager and registers it.
func (a *App) newTab() (string, *tabState) {
	a.settingsMu.Lock()
	allowPlaintext := a.settings.AllowPlaintext
	a.settingsMu.Unlock()
	id := fmt.Sprintf("tab-%d", a.tabSeq.Add(1))
	cm := newConnManager(a.ctx)
	cm.tabID = id
	cm.sink = tabSink{app: a, tabID: id}
	cm.allowPlaintext = allowPlaintext
	if a.knownServers != nil {
		cm.knownServers = a.knownServers
	}
	ts := &tabState{cm: cm, info: TabInfo{ID: id}}
	a.tabsMu.Lock()
	if a.tabs == nil {
		a.tabs = make(map[string]*tabState)
	}
	a.tabs[id] = ts
	a.tabOrder = append(a.tabOrder, id)
	a.tabsMu.Unlock()
	return id, ts
}

// activate switches the active tab. Its state commit is atomic under tabsMu;
// UI side effects run only after the committed snapshot has been released.
func (a *App) activate(tabID string) {
	// Take the publication sequencer before committing activeID. That makes a
	// reset/replay batch indivisible with respect to relayed active events.
	a.activationPublishMu.Lock()
	defer a.activationPublishMu.Unlock()
	a.tabsMu.Lock()
	activeCM, journal, generation, ok := a.activateLocked(tabID)
	a.tabsMu.Unlock()
	if !ok {
		return
	}
	a.finishActivateSerialized(tabID, activeCM, journal, generation)
}

// activateLocked commits the active binding. The caller holds tabsMu.
func (a *App) activateLocked(tabID string) (*connManager, []journalEntry, uint64, bool) {
	ts := a.tabs[tabID]
	if tabID != "" && ts == nil {
		return nil, nil, 0, false
	}
	a.activationGeneration++
	generation := a.activationGeneration
	a.activeID = tabID
	var activeCM *connManager
	var journal []journalEntry
	if ts != nil {
		a.cmStore(ts.cm)
		activeCM = ts.cm
		ts.info.Unread = 0
		ts.info.Mentions = 0
		journal = append(journal, ts.journal...)
	} else {
		a.cmStore(nil)
	}
	return activeCM, journal, generation, true
}

// finishActivateSerialized publishes a committed activation while
// activationPublishMu is held.
func (a *App) finishActivateSerialized(tabID string, activeCM *connManager, journal []journalEntry, generation uint64) {
	if !a.isCurrentActivation(generation) {
		return
	}
	traySetConnected(activeCM != nil && activeCM.connected())

	// The frontend clears chat/tree on tab_reset; the replay below rebuilds
	// state from the journaled frames in order.
	trayClearMentions()
	a.emitPlain("tab_reset", tabID)
	for _, e := range journal {
		if !a.isCurrentActivation(generation) {
			return
		}
		a.emitPlain(e.name, e.payload)
	}
	if !a.isCurrentActivation(generation) {
		return
	}
	a.emitTabsUpdate()
}

func (a *App) isCurrentActivation(generation uint64) bool {
	a.tabsMu.Lock()
	defer a.tabsMu.Unlock()
	return a.activationGeneration == generation
}

// --- bound API ---------------------------------------------------------------

// ListTabs returns the tab bar model.
func (a *App) ListTabs() []TabInfo {
	a.tabsMu.Lock()
	tabs := make([]*tabState, 0, len(a.tabOrder))
	activeID := a.activeID
	for _, id := range a.tabOrder {
		if ts := a.tabs[id]; ts != nil {
			tabs = append(tabs, ts)
		}
	}
	a.tabsMu.Unlock()

	out := make([]TabInfo, 0, len(tabs))
	for _, ts := range tabs {
		a.tabsMu.Lock()
		info := ts.info
		a.tabsMu.Unlock()
		info.Active = info.ID == activeID
		if ts.cm != nil {
			info.Connected = ts.cm.connected()
			ts.cm.mu.Lock()
			addr, nickname := ts.cm.addr, ts.cm.nickname
			ts.cm.mu.Unlock()
			if info.Addr == "" {
				info.Addr = addr
			}
			if info.Nickname == "" {
				info.Nickname = nickname
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
	return a.ConnectBookmarkTabWithID(bookmark, addr, nickname, password, serverPassword).Error
}

// ConnectBookmarkTabWithID connects in a new tab and returns that tab's ID.
func (a *App) ConnectBookmarkTabWithID(bookmark, addr, nickname, password, serverPassword string) ConnectTabResult {
	if addr == "" || nickname == "" {
		return ConnectTabResult{Error: "server address and nickname are required"}
	}
	id, ts := a.newTab()
	err := ts.cm.connect(addr, nickname, password, serverPassword)
	if err != "" {
		a.removeTab(id)
		if err == errFingerprintMismatch.Error() {
			return ConnectTabResult{Error: fingerprintMismatchMessage(ts.cm)}
		}
		return ConnectTabResult{Error: err}
	}
	a.tabsMu.Lock()
	ts.info.Addr = addr
	ts.info.Nickname = nickname
	a.tabsMu.Unlock()
	a.onTabConnected(ts.cm, bookmark, addr, nickname)
	a.activate(id)
	return ConnectTabResult{TabID: id}
}

// ConnectGuestTab connects as a guest in a NEW tab (mirroring ConnectGuest).
func (a *App) ConnectGuestTab(addr, nickname string) string {
	return a.ConnectGuestBookmarkTab("", addr, nickname)
}

// ConnectGuestBookmarkTab is ConnectGuestTab with the originating bookmark's
// Name (see ConnectBookmarkTab).
func (a *App) ConnectGuestBookmarkTab(bookmark, addr, nickname string) string {
	return a.ConnectGuestBookmarkTabWithID(bookmark, addr, nickname).Error
}

// ConnectGuestBookmarkTabWithID connects as a guest and returns the new tab's ID.
func (a *App) ConnectGuestBookmarkTabWithID(bookmark, addr, nickname string) ConnectTabResult {
	if addr == "" || nickname == "" {
		return ConnectTabResult{Error: "server address and nickname are required"}
	}
	id, ts := a.newTab()
	if err := ts.cm.connect(addr, nickname, "", ""); err != "" {
		a.removeTab(id)
		if err == errFingerprintMismatch.Error() {
			return ConnectTabResult{Error: fingerprintMismatchMessage(ts.cm)}
		}
		return ConnectTabResult{Error: err}
	}
	a.tabsMu.Lock()
	ts.info.Addr = addr
	ts.info.Nickname = nickname
	a.tabsMu.Unlock()
	a.onTabConnected(ts.cm, bookmark, addr, nickname)
	a.activate(id)
	return ConnectTabResult{TabID: id}
}

func fingerprintMismatchMessage(cm *connManager) string {
	_, presented, _ := cm.securitySnapshot()
	return fmt.Sprintf("%s: the server certificate changed (presented: %s). Possible MITM — only trust it if you expected the change",
		errFingerprintMismatch, presented)
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
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	var match *Bookmark
	for i := range a.settings.Bookmarks {
		b := a.settings.Bookmarks[i]
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
		copy := b
		match = &copy
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
	a.activate(tabID)
}

// CloseTab disconnects and removes a tab; when it was active, the next tab
// (if any) becomes active.
func (a *App) CloseTab(tabID string) {
	// Lock publication before mutating the active binding so relays cannot
	// publish between the new active commit and its reset marker.
	a.activationPublishMu.Lock()
	defer a.activationPublishMu.Unlock()
	a.tabsMu.Lock()
	ts := a.tabs[tabID]
	wasActive := a.activeID == tabID
	if ts == nil {
		a.tabsMu.Unlock()
		return
	}
	idx := -1
	for i, id := range a.tabOrder {
		if id == tabID {
			idx = i
			break
		}
	}
	delete(a.tabs, tabID)
	if idx >= 0 {
		a.tabOrder = append(a.tabOrder[:idx], a.tabOrder[idx+1:]...)
	}
	var next string
	var activeCM *connManager
	var journal []journalEntry
	var generation uint64
	if wasActive {
		if idx >= 0 && idx < len(a.tabOrder) {
			next = a.tabOrder[idx] // right neighbour now occupies this slot.
		} else if len(a.tabOrder) > 0 {
			next = a.tabOrder[len(a.tabOrder)-1]
		}
		// Remove + replacement selection + active binding are one tabsMu
		// transaction, so a concurrent SetActiveTab cannot be overwritten.
		activeCM, journal, generation, _ = a.activateLocked(next)
	}
	a.tabsMu.Unlock()

	// Teardown can emit callbacks; the registry already has no entry so a
	// late callback is dropped by relayTabEvent.
	ts.cm.disconnect()
	if wasActive {
		a.finishActivateSerialized(next, activeCM, journal, generation)
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
	for i, id := range a.tabOrder {
		if id == tabID {
			a.tabOrder = append(a.tabOrder[:i], a.tabOrder[i+1:]...)
			break
		}
	}
	a.tabsMu.Unlock()
}
