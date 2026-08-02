// menu.js — TS3-style menu bar with dropdown menus.
const V = () => window.__voicx;

let openMenu = null;

function closeMenus() {
    document.querySelectorAll(".menu-dropdown.open").forEach((d) => d.classList.remove("open"));
    document.querySelectorAll(".menu-item.open").forEach((d) => d.classList.remove("open"));
    openMenu = null;
}

function menuAction(label, fn, opts = {}) {
    const a = document.createElement("a");
    a.textContent = label;
    if (opts.disabled) {
        a.className = "disabled";
        a.title = opts.tooltip || "coming soon";
        return a;
    }
    a.onclick = () => { closeMenus(); fn(); };
    return a;
}

function divider() {
    const d = document.createElement("div");
    d.className = "menu-divider";
    return d;
}

function buildMenu(label, items) {
    const item = document.createElement("div");
    item.className = "menu-item";
    item.innerHTML = `<span>${label}</span>`;
    const drop = document.createElement("div");
    drop.className = "menu-dropdown";
    for (const it of items) drop.appendChild(it);
    item.appendChild(drop);
    item.onclick = (e) => {
        e.stopPropagation();
        const wasOpen = openMenu === item;
        closeMenus();
        if (!wasOpen) {
            item.classList.add("open");
            drop.classList.add("open");
            openMenu = item;
        }
    };
    return item;
}

// --- small dialogs -----------------------------------------------------------

function dlgPrompt(title, label, initial, cb) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3></h3>
            <label class="dlg-label"></label>
            <input type="text" class="dlg-input" />
            <div class="dlg-buttons">
                <button class="dlg-ok">OK</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    overlay.querySelector("h3").textContent = title;
    overlay.querySelector(".dlg-label").textContent = label;
    const input = overlay.querySelector(".dlg-input");
    input.value = initial || "";
    overlay.querySelector(".dlg-ok").onclick = () => { cb(input.value.trim()); overlay.remove(); };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    input.focus();
}

function dlgAbout() {
    const { state, $ } = V();
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3>About voicx</h3>
            <div class="about-body">
                <div class="wordmark" style="font-size:26px">voicx</div>
                <div class="mono about-version"></div>
                <div class="mono about-uid"></div>
                <div class="about-links">
                    <a href="https://github.com/voicx" target="_blank">project</a> ·
                    <a href="https://github.com/voicx/issues" target="_blank">issues</a>
                </div>
            </div>
            <div class="dlg-buttons"><button class="dlg-ok">Close</button></div>
        </div>`;
    window.go.main.App.ClientVersion().then((v) => {
        overlay.querySelector(".about-version").textContent = "version " + v;
    });
    const uidEl = overlay.querySelector(".about-uid");
    uidEl.textContent = state.myUniqueID || "(not connected)";
    overlay.querySelector(".dlg-ok").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
}

// --- bookmarks ---------------------------------------------------------------

function currentSettings() {
    return V().state.settings || {};
}

async function saveSettings(patch) {
    const s = Object.assign({}, currentSettings(), patch);
    const err = await window.go.main.App.SaveSettings(s);
    if (err) {
        V().toast("save failed: " + err, "warn");
        return false;
    }
    // (282) the blob we sent is a copy taken before the save: re-read the
    // merged truth so Go-owned fields (recents) written meanwhile survive.
    V().state.settings = await window.go.main.App.GetSettings();
    return true;
}

async function bookmarkCurrent() {
    const { state, toast } = V();
    if (!state.lastConnect) {
        toast("not connected", "warn");
        return;
    }
    const c = state.lastConnect;
    const name = c.nick + " @ " + c.addr;
    const bookmarks = (currentSettings().bookmarks || []).filter((b) => !(b.addr === c.addr && b.nickname === c.nick));
    bookmarks.push({ name, addr: c.addr, nickname: c.nick });
    if (await saveSettings({ bookmarks })) toast("bookmark saved: " + name);
}

async function connectBookmark(b) {
    const { state, toast, $ } = V();
    $("login-addr").value = b.addr;
    // (334) per-server nickname override applies at connect.
    $("login-nick").value = b.nickname_override || b.nickname;
    $("login-password").value = "";
    $("login-serverpw").value = "";
    state.lastConnect = null;
    V().showLogin();
    // (334) the override is what gets sent as the login nickname, so the
    // connect must carry the bookmark name to stay identifiable. Stashed
    // after showLogin, which drops the previous login's stash.
    state.pendingBookmark = { name: b.name, addr: b.addr };
    toast("bookmark loaded — enter password to connect");
}

// sortedBookmarks returns bookmarks grouped by folder, ordered within (284).
function sortedBookmarks() {
    const bms = [...(currentSettings().bookmarks || [])];
    bms.sort((a, b) =>
        (a.folder || "").localeCompare(b.folder || "") ||
        (a.order || 0) - (b.order || 0) ||
        a.name.localeCompare(b.name));
    return bms;
}

function manageBookmarks() {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const persist = async () => {
        const s = currentSettings();
        // Renumber the order within each folder (284).
        const groups = {};
        for (const b of s.bookmarks) {
            const f = b.folder || "";
            groups[f] = groups[f] || [];
            groups[f].push(b);
        }
        for (const f of Object.keys(groups)) groups[f].forEach((b, i) => { b.order = i; });
        return saveSettings({ bookmarks: s.bookmarks });
    };
    const render = () => {
        const bms = sortedBookmarks();
        const list = overlay.querySelector(".bm-list");
        list.innerHTML = bms.length === 0 ? `<div class="empty-state">No bookmarks yet</div>` : "";
        let lastFolder = null;
        bms.forEach((b) => {
            const folder = b.folder || "";
            if (folder !== lastFolder) {
                lastFolder = folder;
                const head = document.createElement("div");
                head.className = "bm-folder";
                head.textContent = folder || "(ungrouped)";
                list.appendChild(head);
            }
            const idx = currentSettings().bookmarks.indexOf(b);
            const row = document.createElement("div");
            row.className = "bm-row";
            row.innerHTML = `
                <span class="bm-dot" title="color (284)"></span>
                <span class="bm-name"></span>
                <span class="bm-addr mono"></span>
                <button class="bm-up" title="move up">↑</button>
                <button class="bm-down" title="move down">↓</button>
                <button class="bm-edit">Edit</button>
                <button class="bm-del">Delete</button>`;
            row.querySelector(".bm-name").textContent = b.name;
            row.querySelector(".bm-addr").textContent = b.addr + (b.auto_connect ? " ⚡" : "");
            const dot = row.querySelector(".bm-dot");
            dot.style.background = b.color || "var(--text-faint)";
            dot.onclick = () => {
                const c = prompt("Bookmark color (hex, e.g. #2ee6a8; empty = none):", b.color || "");
                if (c === null) return;
                b.color = c.trim();
                persist().then(render);
            };
            const s = currentSettings();
            row.querySelector(".bm-up").onclick = () => {
                if (idx > 0) {
                    [s.bookmarks[idx - 1], s.bookmarks[idx]] = [s.bookmarks[idx], s.bookmarks[idx - 1]];
                    persist().then(render);
                }
            };
            row.querySelector(".bm-down").onclick = () => {
                if (idx < s.bookmarks.length - 1) {
                    [s.bookmarks[idx + 1], s.bookmarks[idx]] = [s.bookmarks[idx], s.bookmarks[idx + 1]];
                    persist().then(render);
                }
            };
            row.querySelector(".bm-edit").onclick = () => editBookmark(b, persist, render);
            row.querySelector(".bm-del").onclick = () => {
                s.bookmarks.splice(idx, 1);
                persist().then(render);
            };
            list.appendChild(row);
        });
    };
    overlay.innerHTML = `
        <div class="dlg dlg-wide">
            <h3>Manage bookmarks</h3>
            <div class="bm-list"></div>
            <div class="dlg-buttons"><button class="dlg-ok">Close</button></div>
        </div>`;
    overlay.querySelector(".dlg-ok").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    render();
}

// editBookmark edits one bookmark's extended fields (283/284/286/300).
function editBookmark(b, persist, render) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3>Edit bookmark</h3>
            <label class="dlg-label">Name</label>
            <input class="dlg-input bm-f-name" />
            <label class="dlg-label">Folder (283)</label>
            <input class="dlg-input bm-f-folder" placeholder="e.g. work / friends" />
            <label class="dlg-label">Hotkey profile (300)</label>
            <input class="dlg-input bm-f-profile" placeholder="default" />
            <label class="dlg-label">Nickname override (334)</label>
            <input class="dlg-input bm-f-nick" placeholder="use bookmark nickname" />
            <label class="dlg-label">Avatar override (335)</label>
            <div class="bm-f-avatar-row">
                <button class="icon-btn bm-f-avatar-btn">choose image…</button>
                <span class="bm-f-avatar-state mono"></span>
            </div>
            <label class="dlg-label"><input type="checkbox" class="bm-f-auto" /> auto-connect on startup (286; guest/prefill only — passwords are never stored)</label>
            <div class="dlg-buttons">
                <button class="dlg-ok">Save</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    const q = (sel) => overlay.querySelector(sel);
    q(".bm-f-name").value = b.name;
    q(".bm-f-folder").value = b.folder || "";
    q(".bm-f-profile").value = b.profile || "";
    q(".bm-f-nick").value = b.nickname_override || "";
    q(".bm-f-auto").checked = !!b.auto_connect;
    q(".bm-f-avatar-state").textContent = b.avatar_override_b64 ? "set (" + Math.round(b.avatar_override_b64.length / 1366) + " KB)" : "none";
    q(".bm-f-avatar-btn").onclick = async () => {
        const img = await pickIcon(256, 0.85);
        if (!img) return;
        b.avatar_override_b64 = img.dataBase64;
        q(".bm-f-avatar-state").textContent = "set";
    };
    q(".dlg-ok").onclick = async () => {
        b.name = q(".bm-f-name").value.trim() || b.name;
        b.folder = q(".bm-f-folder").value.trim();
        b.profile = q(".bm-f-profile").value.trim();
        b.nickname_override = q(".bm-f-nick").value.trim();
        b.auto_connect = q(".bm-f-auto").checked;
        overlay.remove();
        await persist();
        render();
    };
    q(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
}

// --- Self actions --------------------------------------------------------------

// menu.js — TS3-style menu bar with dropdown menus.
import { t } from "./i18n.js";
import { pickAvatar, pickIcon } from "./image-tools.js";

// --- Self actions --------------------------------------------------------------

// setAvatarFile opens the avatar crop dialog (268): preview, zoom/reposition,
// 256x256 canvas resize; animated GIF/WebP pass through untouched (269).
async function setAvatarFile() {
    const img = await pickAvatar();
    if (!img) return;
    const err = await window.go.main.App.SetAvatar(img.dataBase64);
    if (err) V().toast("avatar failed: " + err, "warn");
    else V().toast("avatar updated");
}

// setServerIcon uploads a compressed server icon (admin, 270/274).
async function setServerIcon() {
    const img = await pickIcon();
    if (!img) return;
    const err = await window.go.main.App.ServerIconSet(img.dataBase64);
    if (err) {
        V().toast("server icon failed: " + err, "warn");
        return;
    }
    V().toast("server icon updated");
    window.__voicxFiles.loadServerIcon();
}

// --- menu bar -------------------------------------------------------------------

export function initMenu() {
    const { $ } = V();
    $("menubar").innerHTML = ""; // rebuilds on language change (336)

    const connections = buildMenu(t("menu.connections"), [
        menuAction(t("menu.connect"), () => V().showLogin()),
        menuAction(t("menu.disconnect"), () => V().disconnect()),
        divider(),
        menuAction(t("menu.quit"), () => window.runtime.Quit()),
    ]);

    const bookmarkItems = [menuAction("Bookmark current server", bookmarkCurrent), divider()];
    const bookmarkList = document.createElement("div");
    bookmarkList.className = "bm-menu-list";
    const renderBookmarkMenu = () => {
        bookmarkList.innerHTML = "";
        let lastFolder = null;
        for (const b of sortedBookmarks()) {
            const folder = b.folder || "";
            if (folder !== lastFolder) {
                lastFolder = folder;
                const head = document.createElement("div");
                head.className = "bm-menu-folder";
                head.textContent = folder;
                bookmarkList.appendChild(head);
            }
            const item = menuAction(b.name, () => connectBookmark(b));
            if (b.color) {
                // (284) same swatch the tab bar uses, so a bookmark reads the
                // same in the menu as it does once connected.
                const dot = document.createElement("span");
                dot.className = "srv-tab-dot";
                dot.style.background = b.color;
                item.prepend(dot);
            }
            bookmarkList.appendChild(item);
        }
    };
    const bookmarks = buildMenu(t("menu.bookmarks"), [
        ...bookmarkItems,
        bookmarkList,
        menuAction("Manage bookmarks…", manageBookmarks),
    ]);
    const bmItem = bookmarks;
    const origClick = bmItem.onclick;
    bmItem.onclick = (e) => { renderBookmarkMenu(); origClick(e); };

    const self = buildMenu(t("menu.self"), [
        menuAction("Change nickname…", () => {
            dlgPrompt("Change nickname", "Nickname for your next connect:", V().state.myNickname, (v) => {
                if (v) {
                    $("login-nick").value = v;
                    V().state.myNickname = v;
                    V().sysMsg("nickname set to " + v + " (applies on next connect)");
                }
            });
        }),
        menuAction(t("menu.setStatus"), () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxSocial.openStatusPicker();
        }),
        menuAction(t("menu.contacts"), () => window.__voicxSocial.openContacts()),
        menuAction(t("menu.setAvatar"), setAvatarFile),
        menuAction(t("menu.setServerIcon"), () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            if (!V().state.isAdmin) return V().toast("server icon is admin-only", "warn");
            setServerIcon();
        }),
        divider(),
        menuAction("Toggle mute", () => $("voice-mute").click()),
        menuAction("Toggle deafen", () => {
            const { state, setDeafened, sysMsg } = V();
            setDeafened(!state.deafened);
            sysMsg(state.deafened ? "deafened (incoming audio off)" : "undeafened");
        }),
    ]);

    const permissions = buildMenu(t("menu.permissions"), [
        menuAction(t("menu.viewMyPerms"), () => {
            V().refreshPermissions();
            $("details").scrollIntoView({ behavior: "smooth" });
        }),
        menuAction(t("menu.permManager"), () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxPerms.openPermissionManager();
        }),
    ]);

    const tools = buildMenu(t("menu.tools"), [
        menuAction(t("menu.settings"), () => window.__voicx.openSettings("application")),
        menuAction(t("menu.whisperLists"), () => window.__voicx.openSettings("whisper")),
        divider(),
        menuAction(t("menu.auditLog"), () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxPerms.openAuditViewer();
        }),
        menuAction(t("menu.bans"), () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxPerms.openBanList();
        }),
        menuAction("Chat filters…", () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxPerms.openChatFilters();
        }),
        // (173) complaint review, gated the same way as the audit viewer.
        menuAction("Complaints…", () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxPerms.openComplaints();
        }),
        // (174/175/176) privilege key management and handoff.
        menuAction("Privilege keys…", () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxPerms.openTokenManager();
        }),
        menuAction("Use a privilege key…", () => window.__voicxPerms.openTokenRedeem()),
        divider(),
        menuAction(t("menu.debugConsole"), () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxMeta.openDebugConsole();
        }),
        menuAction(t("menu.connStats"), () => {
            if (!V().state.myClientID) return V().toast("not connected", "warn");
            window.__voicxMeta.openStatsPage();
        }),
    ]);

    // View menu: window/integration toggles (wave 8a/8c).
    const view = buildMenu(t("menu.view"), [
        menuAction(t("menu.compact"), () => V().toggleCompact()),
        menuAction(t("menu.zen"), () => window.__voicxPolish.toggleZen()),
        menuAction("Pop out chat", () => window.__voicxPolish.toggleChatPopout()),
        menuAction("Do not disturb", async () => {
            const s = V().state.settings;
            s.dnd_enabled = !s.dnd_enabled;
            await window.go.main.App.SaveSettings(s);
            V().toast("do not disturb " + (s.dnd_enabled ? "on" : "off"));
            V().renderTree();
        }),
        menuAction("Always on top", async () => {
            const on = !(V().state.settings?.always_on_top);
            await window.go.main.App.SetAlwaysOnTop(on);
            if (V().state.settings) V().state.settings.always_on_top = on;
            V().toast("always on top " + (on ? "on" : "off"));
        }),
        menuAction("Window opacity…", () => {}, {
            disabled: true,
            tooltip: "not supported by the Wails v2 WebView2 backend (292)",
        }),
        divider(),
        menuAction("Theme: dark", () => setTheme("dark")),
        menuAction("Theme: light", () => setTheme("light")),
        menuAction("Theme: high contrast", () => setTheme("hc")),
    ]);

    const help = buildMenu(t("menu.help"), [
        menuAction(t("menu.checkUpdates"), () => window.__voicx.checkForUpdatesInteractive()),
        menuAction(t("menu.exportLogs"), async () => {
            const err = await window.go.main.App.ExportLogs();
            if (err) V().toast("export failed: " + err, "warn");
            else V().toast("logs exported");
        }),
        divider(),
        menuAction(t("menu.about"), dlgAbout),
        menuAction("Open log folder", async () => {
            const err = await window.go.main.App.OpenLogFolder();
            if (err) V().toast("cannot open log folder: " + err, "warn");
        }),
    ]);

    const bar = $("menubar");
    for (const m of [connections, bookmarks, self, view, permissions, tools, help]) bar.appendChild(m);
    document.addEventListener("click", closeMenus);
}

// setTheme switches the UI theme (294/295) and persists it.
async function setTheme(theme) {
    const s = Object.assign({}, V().state.settings || {}, { theme });
    const err = await window.go.main.App.SaveSettings(s);
    if (err) {
        V().toast("save failed: " + err, "warn");
        return;
    }
    // (282) same as saveSettings: the merged blob is the authoritative cache.
    V().state.settings = await window.go.main.App.GetSettings();
    V().applyAppearance();
}
