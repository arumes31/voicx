// social-ui.js — wave-8b social/UX features: presence status picker +
// auto-away display (307-309), contacts & block list (316-318), hover cards
// (323/324), poke dialog (321/322), per-user notes + avatar lightbox + copy
// chips in Client Info (314/315/325), the server news pane (313), recent
// channels (320), tree filter + collapse/expand wiring (302/319), and
// multi-select batch actions (306).
import { humanBytes } from "./clientinfo.js";

const V = () => window.__voicx;
const App = () => window.go.main.App;

const STATUS_LABELS = { "": "online", away: "away", busy: "busy" };

// ---------------------------------------------------------------------------
// Tree tools (302/319)
// ---------------------------------------------------------------------------

function initTreeTools() {
    const { state, $ } = V();
    $("tree-filter").oninput = (e) => {
        state.treeFilter = e.target.value.trim();
        V().renderTree();
    };
    $("tree-collapse").onclick = () => {
        for (const ch of state.channels) state.collapsedChannels.add(ch.ChannelID);
        V().renderTree();
    };
    $("tree-expand").onclick = () => {
        state.collapsedChannels.clear();
        V().renderTree();
    };
    // (302) double-click a channel row toggles its collapse; (349) in
    // windowed mode it toggles the explicit expansion instead.
    document.getElementById("channel-tree").addEventListener("dblclick", (e) => {
        const row = e.target.closest(".channel");
        if (!row) return;
        const id = Number(row.dataset.chid);
        if (window.__voicxPolish?.virtualizeEnabled?.()) {
            if (state.expandedVirtual.has(id)) state.expandedVirtual.delete(id);
            else state.expandedVirtual.add(id);
        } else if (state.collapsedChannels.has(id)) {
            state.collapsedChannels.delete(id);
        } else {
            state.collapsedChannels.add(id);
        }
        V().renderTree();
    });
}

// ---------------------------------------------------------------------------
// Presence (307-309)
// ---------------------------------------------------------------------------

// openStatusPicker shows the status dialog (Self menu).
function openStatusPicker() {
    const { state } = V();
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3>My status</h3>
            <select class="dlg-input st-sel">
                <option value="online">🟢 online</option>
                <option value="away">🕐 away</option>
                <option value="busy">⛔ busy</option>
                ${V().state.isAdmin ? '<option value="invisible">👻 invisible (admin only)</option>' : ""}
            </select>
            <label class="dlg-label">Status message (optional)</label>
            <input class="dlg-input st-msg" maxlength="200" placeholder="e.g. in a meeting" />
            <div class="dlg-buttons">
                <button class="dlg-ok">Set</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    const sel = overlay.querySelector(".st-sel");
    sel.value = state.myStatus || "online";
    overlay.querySelector(".dlg-ok").onclick = async () => {
        const status = sel.value;
        const msg = overlay.querySelector(".st-msg").value.trim();
        overlay.remove();
        const err = await App().SetStatus(status, msg);
        if (err) V().toast("status failed: " + err, "warn");
        else {
            state.myStatus = status === "online" ? "" : status;
            V().sysMsg("status: " + STATUS_LABELS[state.myStatus] + (msg ? " — " + msg : ""));
        }
    };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
}

// ---------------------------------------------------------------------------
// Contacts + block list (316-318)
// ---------------------------------------------------------------------------

function openContacts() {
    const { state } = V();
    const s = state.settings || {};
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const render = () => {
        const contacts = s.contacts || [];
        const list = overlay.querySelector(".ct-list");
        list.innerHTML = contacts.length ? "" : `<div class="empty-state">No contacts yet</div>`;
        for (const c of contacts) {
            const online = state.clients.find((x) => x.unique_id === c.unique_id);
            const row = document.createElement("div");
            row.className = "ct-row";
            row.innerHTML = `
                <span class="ct-dot ${online ? "on" : ""}" title="${online ? "online" : "offline"}"></span>
                <span class="ct-name"></span>
                <span class="ct-hist mono" title="nickname history (318)"></span>
                <button class="ct-block" title="block/unblock (317)"></button>
                <button class="ct-del" title="remove">✕</button>`;
            row.querySelector(".ct-name").textContent = (c.label || online?.nickname || c.unique_id.slice(0, 12)) + (online ? " — " + online.nickname : "");
            row.querySelector(".ct-hist").textContent = (c.nick_history || []).slice(-3).join(", ");
            const blocked = (s.blocked_users || []).includes(c.unique_id);
            // (383) buddy alert toggle.
            const watch = document.createElement("button");
            watch.textContent = c.notify_online ? "🔔" : "🔕";
            watch.title = c.notify_online ? "notify when online: on" : "notify when online: off";
            watch.onclick = async () => {
                c.notify_online = !c.notify_online;
                await App().SaveSettings(s);
                render();
            };
            row.insertBefore(watch, row.querySelector(".ct-block"));
            const blockBtn = row.querySelector(".ct-block");
            blockBtn.textContent = blocked ? "🚫" : "🔇";
            blockBtn.title = blocked ? "unblock" : "block (hide chat + mute voice)";
            blockBtn.onclick = async () => {
                if (blocked) s.blocked_users = (s.blocked_users || []).filter((u) => u !== c.unique_id);
                else s.blocked_users = [...(s.blocked_users || []), c.unique_id];
                await App().SaveSettings(s);
                render();
            };
            row.querySelector(".ct-del").onclick = async () => {
                s.contacts = contacts.filter((x) => x !== c);
                await App().SaveSettings(s);
                render();
            };
            list.appendChild(row);
        }
    };
    overlay.innerHTML = `
        <div class="dlg dlg-wide">
            <h3>Contacts</h3>
            <div class="ct-add">
                <input class="dlg-input ct-uid" placeholder="unique ID" />
                <input class="dlg-input ct-label" placeholder="label (optional)" />
                <button class="ct-add-btn">Add</button>
            </div>
            <div class="ct-list"></div>
            <div class="dlg-buttons"><button class="dlg-ok">Close</button></div>
        </div>`;
    overlay.querySelector(".ct-add-btn").onclick = async () => {
        const uid = overlay.querySelector(".ct-uid").value.trim();
        if (!uid) return;
        if ((s.contacts || []).some((c) => c.unique_id === uid)) {
            V().toast("contact already exists", "warn");
            return;
        }
        s.contacts = [...(s.contacts || []), { unique_id: uid, label: overlay.querySelector(".ct-label").value.trim() }];
        await App().SaveSettings(s);
        overlay.querySelector(".ct-uid").value = "";
        overlay.querySelector(".ct-label").value = "";
        render();
    };
    overlay.querySelector(".dlg-ok").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    render();
}

// ---------------------------------------------------------------------------
// Hover cards (323/324)
// ---------------------------------------------------------------------------

let hoverCard = null;
let hoverTimer = null;

function showHoverCard(x, y, html) {
    hideHoverCard();
    hoverCard = document.createElement("div");
    hoverCard.className = "hover-card";
    hoverCard.innerHTML = html;
    hoverCard.style.left = Math.min(x, window.innerWidth - 260) + "px";
    hoverCard.style.top = Math.min(y, window.innerHeight - 160) + "px";
    document.body.appendChild(hoverCard);
}

function hideHoverCard() {
    if (hoverTimer) {
        clearTimeout(hoverTimer);
        hoverTimer = null;
    }
    if (hoverCard) {
        hoverCard.remove();
        hoverCard = null;
    }
}

function esc(s) {
    return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function initHoverCards() {
    const tree = document.getElementById("channel-tree");
    tree.addEventListener("mouseover", (e) => {
        const clientRow = e.target.closest(".client");
        const chRow = e.target.closest(".channel");
        hideHoverCard();
        if (clientRow) {
            const c = V().state.clients.find((x) => x.client_id === clientRow.dataset.clid);
            if (!c) return;
            hoverTimer = setTimeout(() => {
                const g = window.__voicxPerms.primaryGroup(c.unique_id);
                const av = V().state.avatars.get(c.unique_id);
                showHoverCard(e.clientX + 12, e.clientY + 12, `
                    <div class="hc-head">
                        ${av ? `<img class="hc-av" src="${av}">` : ""}
                        <b>${esc(c.nickname || c.unique_id)}</b>
                    </div>
                    <div class="hc-line mono">${esc((c.unique_id || "").slice(0, 20))}…</div>
                    ${g ? `<div class="hc-line">group: ${esc(g.name)}</div>` : ""}
                    ${c.status ? `<div class="hc-line">status: ${esc(c.status)}${c.status_message ? " — " + esc(c.status_message) : ""}</div>` : ""}
                    <div class="hc-line">channel: ${esc(V().state.channels.find((x) => x.ChannelID === c.channel_id)?.Name || "none")}</div>`);
            }, 500);
        } else if (chRow) {
            const ch = V().state.channels.find((x) => x.ChannelID === Number(chRow.dataset.chid));
            if (!ch) return;
            hoverTimer = setTimeout(() => {
                const quality = ch.OpusBitrate ? Math.round(ch.OpusBitrate / 1000) + " kbps" : "default 32 kbps";
                showHoverCard(e.clientX + 12, e.clientY + 12, `
                    <div class="hc-head"><b># ${esc(ch.Name)}</b></div>
                    ${ch.Topic ? `<div class="hc-line">${esc(ch.Topic)}</div>` : ""}
                    <div class="hc-line">clients: ${ch.ClientCount}${ch.MaxClients ? "/" + ch.MaxClients : ""}${ch.HasPassword ? " · 🔒" : ""}</div>
                    <div class="hc-line">codec: Opus ${quality}${ch.OpusStereo ? " stereo" : ""}${ch.OpusFEC ? " +FEC" : ""}</div>`);
            }, 500);
        }
    });
    tree.addEventListener("mouseout", hideHoverCard);
    tree.addEventListener("mousedown", hideHoverCard);
}

// ---------------------------------------------------------------------------
// Poke (321/322)
// ---------------------------------------------------------------------------

function openPoke(client) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3>Poke</h3>
            <div class="dlg-text poke-target"></div>
            <input class="dlg-input poke-msg" maxlength="200" placeholder="message (optional)" />
            <div class="dlg-buttons">
                <button class="dlg-ok">Poke</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    overlay.querySelector(".poke-target").textContent = client.nickname || client.unique_id;
    overlay.querySelector(".dlg-ok").onclick = async () => {
        const msg = overlay.querySelector(".poke-msg").value.trim();
        overlay.remove();
        const err = await App().Poke(client.client_id, msg);
        if (err) V().toast("poke failed: " + err, "warn");
    };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    overlay.querySelector(".poke-msg").focus();
}

// ---------------------------------------------------------------------------
// News pane (313)
// ---------------------------------------------------------------------------

async function refreshNews() {
    const area = document.getElementById("news-area");
    if (!V().state.myClientID) {
        area.innerHTML = `<div class="empty-state">offline</div>`;
        return;
    }
    try {
        const info = await App().ServerInfo();
        const motd = await App().MOTD();
        const up = Math.floor(info.uptime_seconds / 60);
        area.innerHTML = `
            <div class="news-line"><b>${esc(info.name)}</b></div>
            <div class="news-line mono">${esc(info.version)} · ${info.clients_online}/${info.max_clients} clients · ${info.channels_online} channels · up ${up}m</div>
            ${motd ? `<div class="news-motd">${esc(motd)}</div>` : ""}`;
    } catch {
        area.innerHTML = `<div class="empty-state">server info unavailable</div>`;
    }
}

// ---------------------------------------------------------------------------
// Client Info additions (314/315/325)
// ---------------------------------------------------------------------------

// avatarLightbox shows the full avatar (314).
function avatarLightbox(dataUrl) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `<div class="dlg avatar-full"><img src="${dataUrl}" alt=""></div>`;
    overlay.onclick = () => overlay.remove();
    document.body.appendChild(overlay);
}

// userNote loads/saves the local per-user note (315).
function userNote(uid) {
    return (V().state.settings?.user_notes || {})[uid] || "";
}

async function saveUserNote(uid, note) {
    const s = V().state.settings;
    s.user_notes = s.user_notes || {};
    if (note) s.user_notes[uid] = note;
    else delete s.user_notes[uid];
    await App().SaveSettings(s);
}

// ---------------------------------------------------------------------------
// init + exports
// ---------------------------------------------------------------------------

export function initSocialUI() {
    initTreeTools();
    initHoverCards();
    window.__voicxSocial = {
        openStatusPicker, openContacts, openPoke, refreshNews,
        avatarLightbox, userNote, saveUserNote, esc,
    };
}

void humanBytes; // shared formatting helper kept for future rows
