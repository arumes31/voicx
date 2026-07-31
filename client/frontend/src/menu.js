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
                <div>voice ops console — client v0.1.0</div>
                <div class="mono about-uid"></div>
                <div class="about-links">
                    <a href="https://github.com/voicx" target="_blank">project</a> ·
                    <a href="https://github.com/voicx/issues" target="_blank">issues</a>
                </div>
            </div>
            <div class="dlg-buttons"><button class="dlg-ok">Close</button></div>
        </div>`;
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
    V().state.settings = s;
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
    $("login-nick").value = b.nickname;
    $("login-password").value = "";
    $("login-serverpw").value = "";
    state.lastConnect = null;
    V().showLogin();
    toast("bookmark loaded — enter password to connect");
}

function manageBookmarks() {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const render = () => {
        const bms = currentSettings().bookmarks || [];
        overlay.querySelector(".bm-list").innerHTML = bms.length === 0
            ? `<div class="empty-state">No bookmarks yet</div>`
            : "";
        bms.forEach((b, i) => {
            const row = document.createElement("div");
            row.className = "bm-row";
            row.innerHTML = `<span class="bm-name"></span><span class="bm-addr mono"></span>
                <button class="bm-rename">Rename</button><button class="bm-del">Delete</button>`;
            row.querySelector(".bm-name").textContent = b.name;
            row.querySelector(".bm-addr").textContent = b.addr;
            row.querySelector(".bm-rename").onclick = () => {
                const nn = prompt("Bookmark name:", b.name);
                if (nn) {
                    bms[i].name = nn;
                    saveSettings({ bookmarks: bms }).then(render);
                }
            };
            row.querySelector(".bm-del").onclick = () => {
                bms.splice(i, 1);
                saveSettings({ bookmarks: bms }).then(render);
            };
            overlay.querySelector(".bm-list").appendChild(row);
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

// --- Self actions --------------------------------------------------------------

function setAvatarFile() {
    const input = document.createElement("input");
    input.type = "file";
    input.accept = "image/png,image/jpeg,image/gif,image/webp";
    input.onchange = async () => {
        const file = input.files[0];
        if (!file) return;
        if (file.size > 256 * 1024) {
            V().toast("avatar too large (max 256 KiB)", "warn");
            return;
        }
        const buf = await file.arrayBuffer();
        const b64 = btoa(String.fromCharCode(...new Uint8Array(buf)));
        const err = await window.go.main.App.SetAvatar(b64);
        if (err) V().toast("avatar failed: " + err, "warn");
        else V().toast("avatar updated");
    };
    input.click();
}

// --- menu bar -------------------------------------------------------------------

export function initMenu() {
    const { $ } = V();

    const connections = buildMenu("Connections", [
        menuAction("Connect…", () => V().showLogin()),
        menuAction("Disconnect", () => V().disconnect()),
        divider(),
        menuAction("Quit", () => window.runtime.Quit()),
    ]);

    const bookmarkItems = [menuAction("Bookmark current server", bookmarkCurrent), divider()];
    const bookmarkList = document.createElement("div");
    bookmarkList.className = "bm-menu-list";
    const renderBookmarkMenu = () => {
        bookmarkList.innerHTML = "";
        for (const b of currentSettings().bookmarks || []) {
            bookmarkList.appendChild(menuAction(b.name, () => connectBookmark(b)));
        }
    };
    const bookmarks = buildMenu("Bookmarks", [
        ...bookmarkItems,
        bookmarkList,
        menuAction("Manage bookmarks…", manageBookmarks),
    ]);
    const bmItem = bookmarks;
    const origClick = bmItem.onclick;
    bmItem.onclick = (e) => { renderBookmarkMenu(); origClick(e); };

    const self = buildMenu("Self", [
        menuAction("Change nickname…", () => {
            dlgPrompt("Change nickname", "Nickname for your next connect:", V().state.myNickname, (v) => {
                if (v) {
                    $("login-nick").value = v;
                    V().state.myNickname = v;
                    V().sysMsg("nickname set to " + v + " (applies on next connect)");
                }
            });
        }),
        menuAction("Set avatar…", setAvatarFile),
        divider(),
        menuAction("Toggle mute", () => $("voice-mute").click()),
        menuAction("Toggle deafen", () => {
            const { state, setDeafened, sysMsg } = V();
            setDeafened(!state.deafened);
            sysMsg(state.deafened ? "deafened (incoming audio off)" : "undeafened");
        }),
    ]);

    const permissions = buildMenu("Permissions", [
        menuAction("View my permissions", () => {
            V().refreshPermissions();
            $("details").scrollIntoView({ behavior: "smooth" });
        }),
    ]);

    const tools = buildMenu("Tools", [
        menuAction("Settings…", () => window.__voicx.openSettings("application")),
        menuAction("Whisper lists…", () => window.__voicx.openSettings("whisper")),
    ]);

    const help = buildMenu("Help", [
        menuAction("About voicx", dlgAbout),
        menuAction("Open log folder", async () => {
            const err = await window.go.main.App.OpenLogFolder();
            if (err) V().toast("cannot open log folder: " + err, "warn");
        }),
    ]);

    const bar = $("menubar");
    for (const m of [connections, bookmarks, self, permissions, tools, help]) bar.appendChild(m);
    document.addEventListener("click", closeMenus);
}
