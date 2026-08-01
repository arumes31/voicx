// tabs.js — wave-8a multi-server tabs (281): the TS3-style server tab bar
// above the panes, tab switching with state reset/replay, unread/mention
// badges, quick connect (285), and auto-connect on startup (286). The
// backend (tabs.go) owns one connManager per tab and replays journaled
// state frames on activation, so the frontend keeps its single-state model:
// on "tab_reset" we clear chat/tree and the replay rebuilds them.
const V = () => window.__voicx;
const App = () => window.go.main.App;

// renderTabs draws the server tab bar from the backend's tab model. With no
// tabs left, the login dialog comes back up.
function renderTabs(tabs) {
    const bar = document.getElementById("server-tabs");
    bar.innerHTML = "";
    if (!tabs || tabs.length === 0) {
        V().showLogin();
        renderRecents();
    } else if (tabs.some((t) => t.connected)) {
        document.getElementById("login-overlay").classList.add("hidden");
        document.getElementById("app").classList.remove("hidden");
    }
    for (const t of tabs || []) {
        const el = document.createElement("div");
        el.className = "srv-tab" + (t.active ? " active" : "") + (t.connected ? "" : " offline");
        el.dataset.tabId = t.id;
        const label = document.createElement("span");
        label.className = "srv-tab-label";
        label.textContent = (t.nickname || "?") + " @ " + (t.addr || "?");
        label.title = t.id + (t.connected ? "" : " (offline)");
        el.appendChild(label);
        if (t.mentions > 0) {
            const b = document.createElement("span");
            b.className = "srv-badge mention";
            b.textContent = t.mentions;
            b.title = t.mentions + " unread mention(s)";
            el.appendChild(b);
        } else if (t.unread > 0) {
            const b = document.createElement("span");
            b.className = "srv-badge";
            b.textContent = t.unread > 99 ? "99+" : t.unread;
            b.title = t.unread + " unread message(s)";
            el.appendChild(b);
        }
        const x = document.createElement("button");
        x.className = "srv-tab-x";
        x.textContent = "✕";
        x.title = "disconnect and close tab";
        x.onclick = (e) => {
            e.stopPropagation();
            App().CloseTab(t.id);
        };
        el.appendChild(x);
        el.onclick = () => {
            if (!t.active) App().SetActiveTab(t.id);
        };
        bar.appendChild(el);
    }
    // "+" tab opens the login dialog for a new connection.
    const plus = document.createElement("button");
    plus.id = "srv-tab-plus";
    plus.className = "srv-tab plus";
    plus.textContent = "+";
    plus.title = "connect to another server (new tab)";
    plus.onclick = () => V().showLogin();
    bar.appendChild(plus);
}

// onTabReset clears all per-server view state before the backend replays
// the newly activated tab's journaled frames.
function onTabReset() {
    const { state, $ } = V();
    // Voice is active-tab only this wave: tear it down on every switch.
    if (state.pc) {
        try { state.pc.close(); } catch { /* already closed */ }
        state.pc = null;
    }
    state.channels = [];
    state.clients = [];
    window.__voicxNotify?.resetBuddyWatch(); // (383) buddy alerts re-arm per connect
    state.myChannelID = 0;
    state.selectedClientID = "";
    state.trackUsers.clear();
    window.__voicxChat?.resetView?.();
    V().renderTree();
    V().refreshPermissions();
}

// autoConnect fires flagged bookmarks at startup (286). Passwords are never
// stored, so account bookmarks can only prefill the login dialog; guest
// logins (empty password) connect directly. Documented limitation.
async function autoConnectBookmarks() {
    const flagged = (V().state.settings?.bookmarks || []).filter((b) => b.auto_connect);
    for (const b of flagged) {
        // (334) the per-server nickname override is what gets sent, so the
        // bookmark must be named explicitly for the backend to find it.
        const nick = b.nickname_override || b.nickname;
        const err = await App().ConnectGuestBookmarkTab(b.name, b.addr, nick);
        if (err !== "") {
            // Account login needed: prefill for the user.
            const { $ } = V();
            $("login-addr").value = b.addr;
            $("login-nick").value = nick;
            V().state.pendingBookmark = { name: b.name, addr: b.addr };
            V().sysMsg?.("auto-connect needs your password for " + b.addr);
        }
    }
}

// quickConnectLast connects the most recently used bookmark in a new tab
// (285, default Ctrl+Shift+C).
async function quickConnectLast() {
    const bms = V().state.settings?.bookmarks || [];
    const recents = V().state.settings?.recents || [];
    const target = bms[bms.length - 1] || recents[0];
    if (!target) {
        V().toast("no bookmark or recent server to quick-connect", "warn");
        return;
    }
    // Passwords are never stored: guest logins connect directly, account
    // bookmarks prefill the login dialog. (334) the override is the nickname
    // actually sent, so the bookmark name goes along; recents have neither.
    const nick = target.nickname_override || target.nickname;
    const err = await App().ConnectGuestBookmarkTab(target.name || "", target.addr, nick);
    if (err !== "") {
        const { $ } = V();
        $("login-addr").value = target.addr;
        $("login-nick").value = nick;
        V().showLogin();
        // stashed after showLogin, which drops the previous login's stash: a
        // recent has no bookmark name and must leave none behind (334).
        if (target.name) V().state.pendingBookmark = { name: target.name, addr: target.addr };
    }
}

// renderRecents draws the recent-servers list in the login dialog (282)
// with a bookmark star toggle.
function renderRecents() {
    const area = document.getElementById("login-recents");
    if (!area) return;
    const s = V().state.settings || {};
    const recents = s.recents || [];
    const bms = s.bookmarks || [];
    area.innerHTML = recents.length ? "" : `<div class="empty-state">no recent servers</div>`;
    for (const r of recents) {
        const row = document.createElement("div");
        row.className = "recent-row";
        const starred = bms.some((b) => b.addr === r.addr && b.nickname === r.nickname);
        row.innerHTML = `<button class="recent-star" title="bookmark">${starred ? "★" : "☆"}</button>
            <span class="recent-label"></span>`;
        row.querySelector(".recent-label").textContent = (r.nickname || "?") + " @ " + r.addr;
        row.querySelector(".recent-label").onclick = () => {
            document.getElementById("login-addr").value = r.addr;
            document.getElementById("login-nick").value = r.nickname || "";
        };
        row.querySelector(".recent-star").onclick = async () => {
            if (starred) {
                s.bookmarks = bms.filter((b) => !(b.addr === r.addr && b.nickname === r.nickname));
            } else {
                s.bookmarks = [...bms, { name: (r.nickname || "?") + " @ " + r.addr, addr: r.addr, nickname: r.nickname }];
            }
            const err = await App().SaveSettings(s);
            if (err) V().toast("save failed: " + err, "warn");
            // (282) re-read the merged truth: recents recorded while the save
            // was in flight are not in the copy we sent.
            else V().state.settings = await App().GetSettings();
            renderRecents();
        };
        area.appendChild(row);
    }
}

export function initTabs() {
    window.runtime.EventsOn("tab_update", renderTabs);
    window.runtime.EventsOn("tab_reset", onTabReset);

    // "+" in an empty app also means: show the tab bar only when connected.
    renderTabs([]);
    renderRecents();

    // (286) auto-connect flagged bookmarks once settings are loaded.
    setTimeout(autoConnectBookmarks, 300);

    window.__voicxTabs = { renderTabs, quickConnectLast, renderRecents };
}
