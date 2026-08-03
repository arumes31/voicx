// perms-ui.js — wave 6b: Permission Manager (editable grid, trace, templates,
// export), group management (CRUD, members, drag & drop, timed assigns,
// icons), the audit log viewer, and the ban list dialog. All views degrade
// gracefully for non-privileged users (read-only notice instead of controls).
import { pickIcon } from "./image-tools.js";

const V = () => window.__voicx;
const App = () => window.go.main.App;

// Permission key catalog (internal/permissions/types.go). The grid shows
// these plus any extra keys present on the selected target.
const PERMISSION_KEYS = [
    "i_channel_join_power", "i_channel_needed_join_power", "b_channel_join_ignore_password",
    "i_channel_subscribe_power", "i_channel_needed_subscribe_power",
    "b_channel_create_child", "b_channel_create_permanent", "b_channel_create_semi_permanent",
    "b_channel_create_temporary", "b_channel_delete", "b_channel_modify",
    "i_channel_modify_power", "i_channel_needed_modify_power",
    "i_client_kick_from_channel_power", "i_client_needed_kick_from_channel_power",
    "i_client_kick_from_server_power", "i_client_needed_kick_from_server_power",
    "b_client_ban", "i_client_ban_power", "i_client_needed_ban_power",
    "i_client_move_power", "i_client_needed_move_power",
    "b_client_use_channel_command", "i_client_talk_power", "i_client_needed_talk_power",
    "i_client_whisper_power", "i_client_needed_whisper_power",
    "b_client_video_publish", "b_client_issue_screenshare_1080p", "b_client_priority_speaker", "b_client_ignore_antiflood",
    "b_client_request_talker", "b_client_is_bot",
    "i_ft_file_upload_power", "i_ft_needed_file_upload_power",
    "i_ft_file_download_power", "i_ft_needed_file_download_power",
    "i_ft_quota_mb_upload_per_client", "b_ft_delete", "b_client_avatar_upload",
    "i_client_poke_power", "i_client_needed_poke_power", "b_client_poke",
    "b_client_remoteaddress_view", "i_permission_modify_power", "b_permission_modify_power_ignore",
    "b_virtualserver_info_view", "b_virtualserver_connectioninfo_view", "b_virtualserver_recording",
    "b_virtualserver_token_list", "b_virtualserver_token_add", "b_virtualserver_token_use",
    "b_virtualserver_token_delete",
    "b_chat_delete_any", "b_chat_mention_all", "b_chat_slowmode_bypass", "b_chat_filter_manage",
    "b_emoji_manage",
    "b_server_group_manage", "b_channel_group_manage", "b_permission_manage", "b_audit_view",
];

// Resolver evaluation order for the trace view (137/155).
const TRACE_TIERS = ["server_group", "client_specific", "channel_specific", "channel", "channel_group", "channel_client"];

const TEMPLATES = ["guest", "member", "moderator", "admin"];

// --- privilege helpers ---------------------------------------------------------

// myPerm returns the caller's resolved entry for a key (state.myPerms is
// filled by refreshPermissions in main.js).
function myPerm(key) {
    const p = V().state.myPerms;
    return p ? p.get(key) : undefined;
}

function hasPerm(key) {
    if (V().state.isAdmin) return true;
    const e = myPerm(key);
    return !!e && e.value !== 0 && !e.negate;
}

function hasPower(key) {
    if (V().state.isAdmin) return true;
    const e = myPerm(key);
    return !!e && e.value > 0 && !e.negate;
}

const canPermManage = () => hasPerm("b_permission_manage");
const canAuditView = () => hasPerm("b_audit_view");
const canChatFilterManage = () => hasPerm("b_chat_filter_manage");
const canGroupManage = (type) => hasPerm(type === "channel" ? "b_channel_group_manage" : "b_server_group_manage");
const canBan = () => hasPerm("b_client_ban") || hasPower("i_client_ban_power");
const canKickChannel = () => hasPower("i_client_kick_from_channel_power");
const canKickServer = () => hasPower("i_client_kick_from_server_power");

// --- small dialog helpers --------------------------------------------------------

function modal(cls, html) {
    const o = document.createElement("div");
    o.className = "dlg-overlay";
    o.innerHTML = `<div class="dlg ${cls}"></div>`;
    const d = o.firstElementChild;
    d.innerHTML = html;
    o.onclick = (e) => { if (e.target === o) o.remove(); };
    document.body.appendChild(o);
    return { overlay: o, dlg: d, q: (sel) => d.querySelector(sel) };
}

function confirmDlg(title, bodyHtml, okLabel, danger) {
    return new Promise((resolve) => {
        const { overlay, dlg, q } = modal("confirm-dlg", `
            <h3></h3>
            <div class="dlg-text confirm-body"></div>
            <div class="dlg-buttons">
                <button class="dlg-cancel">Cancel</button>
                <button class="dlg-ok ${danger ? "danger-btn" : ""}"></button>
            </div>`);
        dlg.querySelector("h3").textContent = title;
        q(".confirm-body").innerHTML = bodyHtml;
        q(".dlg-ok").textContent = okLabel;
        q(".dlg-ok").onclick = () => { overlay.remove(); resolve(true); };
        q(".dlg-cancel").onclick = () => { overlay.remove(); resolve(false); };
    });
}

// denyNotice renders the graceful-degradation empty state.
function denyNotice(perm) {
    return `<div class="empty-state">Requires <span class="mono">${perm}</span> (or server admin) — the server also enforces this.</div>`;
}

// esc for HTML interpolation of user data.
function esc(s) {
    return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function fmtTime(unix) {
    if (!unix) return "";
    return new Date(unix * 1000).toLocaleString();
}

// --- group state (tree presentation + own groups) ------------------------------

// refreshGroups reloads server groups and their memberships into state:
// state.serverGroups (list), state.groupByUID (uid -> [{...group}] sorted by
// sort_id). Called on connect and after group events.
async function refreshGroups() {
    const { state } = V();
    try {
        const list = await App().GroupList("server");
        state.serverGroups = list.groups || [];
        const byUID = new Map();
        for (const g of state.serverGroups) {
            if (g.member_count === 0) continue;
            try {
                const members = await App().GroupMembers("server", g.id, 0);
                for (const m of members.members || []) {
                    if (!byUID.has(m.unique_id)) byUID.set(m.unique_id, []);
                    byUID.get(m.unique_id).push(g);
                }
            } catch { /* member listing is best-effort */ }
        }
        for (const arr of byUID.values()) arr.sort((a, b) => a.sort_id - b.sort_id);
        state.groupByUID = byUID;
    } catch {
        state.serverGroups = [];
        state.groupByUID = new Map();
    }
}

// primaryGroup returns the first applicable group (by sort_id) for a user.
function primaryGroup(uid) {
    const arr = V().state.groupByUID?.get(uid);
    return arr && arr.length ? arr[0] : null;
}

// groupColorFor returns the nickname color for a user ("" = default).
function groupColorFor(uid) {
    const g = primaryGroup(uid);
    return g && g.color ? g.color : "";
}

// hoistedGroups returns hoisted groups with their online members.
function hoistedGroups() {
    const { state } = V();
    const out = [];
    for (const g of state.serverGroups || []) {
        if (!g.hoist) continue;
        const members = state.clients.filter((c) => (state.groupByUID?.get(c.unique_id) || []).some((x) => x.id === g.id));
        if (members.length > 0) out.push({ group: g, members });
    }
    return out;
}

// groupIconURL fetches+caches a group icon as a data URL (state.groupIcons).
async function groupIconURL(groupID) {
    const { state } = V();
    if (!state.groupIcons) state.groupIcons = new Map();
    if (state.groupIcons.has(groupID)) return state.groupIcons.get(groupID);
    try {
        const data = await App().GroupIconGet(groupID);
        const url = data && data.data_base64 ? `data:${data.content_type};base64,${data.data_base64}` : null;
        state.groupIcons.set(groupID, url);
        return url;
    } catch {
        state.groupIcons.set(groupID, null);
        return null;
    }
}

// --- Permission Manager ----------------------------------------------------------

// pm holds the open manager's state (null when closed).
let pm = null;

function openPermissionManager() {
    if (pm) return;
    pm = {
        tab: "server",       // server | clients | channel | channelGroups
        target: null,        // {tier, groupID, uniqueID, channelID, label}
        entries: new Map(),  // key -> {value, grant, skip, negate}
        filter: "",
        editKey: "",
        channelGroupChan: 0, // channel context for channel-group members
    };
    const { overlay, dlg, q } = modal("pm", `
        <div class="pm-head">
            <h3>Permission Manager</h3>
            <button class="icon-btn pm-close" title="Close">✕</button>
        </div>
        <div class="pm-tabs">
            <button data-tab="server" class="active">Server Groups</button>
            <button data-tab="clients">Clients</button>
            <button data-tab="channel">Channel</button>
            <button data-tab="channelGroups">Channel Groups</button>
        </div>
        <div class="pm-body">
            <div class="pm-left">
                <div class="pm-actions"></div>
                <div class="pm-targets"></div>
                <div class="pm-members"></div>
            </div>
            <div class="pm-right">
                <div class="pm-right-head">
                    <input class="pm-filter dlg-input" placeholder="filter permissions… (154)" />
                    <button class="pm-matrix icon-btn" title="Compare this client's effective permissions across sub-channels (726)">▦</button>
                    <button class="pm-export icon-btn" title="Export this target's permissions as JSON (148)">⬇</button>
                    <button class="pm-export-csv icon-btn" title="Export this target's permissions as CSV (148)">CSV</button>
                </div>
                <div class="pm-grid"></div>
                <div class="pm-trace"></div>
            </div>
        </div>`);
    pm.overlay = overlay;
    pm.dlg = dlg;
    pm.q = q;
    q(".pm-close").onclick = () => closePermissionManager();
    q(".pm-filter").oninput = (e) => { pm.filter = e.target.value.trim().toLowerCase(); renderGrid(); };
    q(".pm-matrix").onclick = () => openChannelPermissionMatrix();
    q(".pm-export").onclick = () => exportPerms("json");
    q(".pm-export-csv").onclick = () => exportPerms("csv");
    dlg.querySelectorAll(".pm-tabs button").forEach((b) => {
        b.onclick = () => {
            pm.tab = b.dataset.tab;
            pm.target = null;
            pm.entries = new Map();
            pm.editKey = "";
            dlg.querySelectorAll(".pm-tabs button").forEach((x) => x.classList.toggle("active", x === b));
            renderTargets();
            renderGrid();
        };
    });
    renderTargets();
    renderGrid();
}

function closePermissionManager() {
    if (!pm) return;
    pm.overlay.remove();
    pm = null;
}

// openChannelPermissionMatrix compares a selected client's effective values
// across several channel contexts. Each cell comes from the server's trace
// resolver, so inherited tiers and overrides match enforcement exactly.
async function openChannelPermissionMatrix() {
    const uid = pm?.target?.uniqueID || "";
    if (!uid) return V().toast("select a client target first", "warn");
    const channels = [...(V().state.channels || [])].sort((a, b) =>
        (a.ParentID - b.ParentID) || (a.OrderIndex - b.OrderIndex) || (a.ChannelID - b.ChannelID));
    if (channels.length < 2) return V().toast("at least two channels are required for comparison", "warn");
    const { overlay, q } = modal("pm-matrix-dialog", `
        <h3>Cross-channel permission matrix</h3>
        <div class="dlg-text pm-matrix-who"></div>
        <div class="pm-matrix-channels"></div>
        <label class="dlg-label">Permission keys (comma-separated)</label>
        <textarea class="dlg-input pm-matrix-keys" rows="3"></textarea>
        <div class="pm-matrix-result"></div>
        <div class="dlg-buttons"><button class="dlg-cancel">Close</button><button class="dlg-ok">Compare</button></div>`);
    q(".pm-matrix-who").textContent = pm.target.label + " — values include inherited permissions";
    const chooser = q(".pm-matrix-channels");
    const current = V().state.myChannelID || 0;
    const defaults = new Set(channels.filter((ch) => ch.ChannelID === current || ch.ParentID === current).slice(0, 8).map((ch) => ch.ChannelID));
    if (defaults.size < 2) for (const ch of channels.slice(0, 2)) defaults.add(ch.ChannelID);
    for (const ch of channels) {
        const label = document.createElement("label");
        label.className = "pm-matrix-choice";
        const input = document.createElement("input");
        input.type = "checkbox";
        input.value = ch.ChannelID;
        input.checked = defaults.has(ch.ChannelID);
        label.append(input, document.createTextNode(" " + ch.Name + " (" + ch.ChannelID + ")"));
        chooser.appendChild(label);
    }
    const common = ["i_channel_join_power", "i_client_talk_power", "i_client_whisper_power",
        "b_client_use_channel_command", "b_client_video_publish", "b_client_issue_screenshare_1080p",
        "i_ft_file_upload_power", "i_ft_file_download_power", "b_channel_modify", "b_channel_delete"];
    q(".pm-matrix-keys").value = [...new Set([...pm.entries.keys(), ...common])].slice(0, 20).join(", ");
    q(".dlg-cancel").onclick = () => overlay.remove();
    q(".dlg-ok").onclick = async () => {
        const selected = [...chooser.querySelectorAll("input:checked")].map((x) => Number(x.value)).slice(0, 12);
        const keys = q(".pm-matrix-keys").value.split(",").map((x) => x.trim()).filter((x) => PERMISSION_KEYS.includes(x)).slice(0, 30);
        if (selected.length < 2 || keys.length === 0) return V().toast("choose at least two channels and one known permission", "warn");
        const result = q(".pm-matrix-result");
        result.innerHTML = `<div class="empty-state">resolving ${selected.length * keys.length} cells…</div>`;
        const values = new Map();
        const pending = selected.flatMap((channelID) => keys.map((key) => ({ channelID, key })));
        let next = 0;
        const worker = async () => {
            while (next < pending.length) {
                const { channelID, key } = pending[next++];
                try {
                    const trace = await App().PermTrace(uid, key, channelID);
                    values.set(channelID + ":" + key, { value: trace.effective, tier: trace.effective_tier || "unset" });
                } catch {
                    values.set(channelID + ":" + key, { value: "?", tier: "error" });
                }
            }
        };
        await Promise.all(Array.from({ length: Math.min(6, pending.length) }, worker));
        let html = `<table class="perm-grid trace-grid"><thead><tr><th>permission</th>`;
        for (const id of selected) html += `<th>${esc(channels.find((ch) => ch.ChannelID === id)?.Name || id)}</th>`;
        html += "</tr></thead><tbody>";
        for (const key of keys) {
            html += `<tr><td class="mono">${esc(key)}</td>`;
            const rowValues = selected.map((id) => values.get(id + ":" + key));
            const actualValues = rowValues.filter((v) => v?.value !== "?").map((v) => String(v?.value));
            const differs = new Set(actualValues).size > 1;
            for (const v of rowValues) html += `<td class="${differs ? "winning" : ""}" title="source: ${esc(v?.tier)}"><b>${esc(v?.value)}</b><br><span class="pm-dim">${esc(v?.tier)}</span></td>`;
            html += "</tr>";
        }
        result.innerHTML = html + "</tbody></table>";
    };
}

// targetTier maps the tab to the write-path tier.
function tabTier(tab) {
    switch (tab) {
        case "clients": return "client";
        case "channel": return "channel";
        case "channelGroups": return "channel_group";
        default: return "server_group";
    }
}

async function renderTargets() {
    const list = pm.q(".pm-targets");
    const actions = pm.q(".pm-actions");
    const members = pm.q(".pm-members");
    list.innerHTML = `<div class="empty-state">loading…</div>`;
    actions.innerHTML = "";
    members.innerHTML = "";

    if (pm.tab === "server" || pm.tab === "channelGroups") {
        const type = pm.tab === "server" ? "server" : "channel";
        renderGroupActions(actions, type);
        let groups = [];
        try {
            const resp = await App().GroupList(type);
            groups = resp.groups || [];
        } catch { /* handled below */ }
        list.innerHTML = groups.length ? "" : `<div class="empty-state">No ${type} groups</div>`;
        for (const g of groups) {
            const row = document.createElement("div");
            row.className = "pm-target" + (pm.target?.groupID === g.id ? " active" : "");
            row.innerHTML = `<span class="pm-target-name"></span><span class="pm-target-count mono"></span>`;
            row.querySelector(".pm-target-name").textContent = g.name;
            row.querySelector(".pm-target-count").textContent = g.member_count + "👤";
            row.style.borderLeftColor = g.color || "transparent";
            row.onclick = () => selectTarget({ tier: tabTier(pm.tab), groupID: g.id, label: g.name, group: g });
            list.appendChild(row);
        }
        if (pm.target?.groupID) renderMembers();
        return;
    }

    if (pm.tab === "clients") {
        renderTemplateButton(actions, "client");
        renderCopyButton(actions);
        const clients = V().state.clients.filter((c) => c.unique_id);
        list.innerHTML = clients.length ? "" : `<div class="empty-state">No online users</div>`;
        for (const c of clients) {
            const row = document.createElement("div");
            row.className = "pm-target" + (pm.target?.uniqueID === c.unique_id ? " active" : "");
            row.innerHTML = `<span class="pm-target-name"></span>`;
            row.querySelector(".pm-target-name").textContent = c.nickname || c.unique_id;
            row.title = c.unique_id;
            row.onclick = () => selectTarget({ tier: "client", uniqueID: c.unique_id, label: c.nickname || c.unique_id });
            list.appendChild(row);
        }
        // Free-form unique ID entry (offline users).
        const custom = document.createElement("div");
        custom.className = "pm-custom-uid";
        custom.innerHTML = `<input class="dlg-input" placeholder="unique ID (offline user)…" /><button>select</button>`;
        custom.querySelector("button").onclick = () => {
            const uid = custom.querySelector("input").value.trim();
            if (uid) selectTarget({ tier: "client", uniqueID: uid, label: uid.slice(0, 14) + "…" });
        };
        list.appendChild(custom);
        return;
    }

    // Channel tab: channel-tier entries (e.g. per-channel needed talk power).
    const channels = V().state.channels;
    list.innerHTML = channels.length ? "" : `<div class="empty-state">No channels</div>`;
    for (const ch of channels) {
        const row = document.createElement("div");
        row.className = "pm-target" + (pm.target?.channelID === ch.ChannelID ? " active" : "");
        row.innerHTML = `<span class="pm-target-name"></span>`;
        row.querySelector(".pm-target-name").textContent = "# " + ch.Name;
        row.onclick = () => selectTarget({ tier: "channel", channelID: ch.ChannelID, label: "# " + ch.Name });
        list.appendChild(row);
    }
}

function selectTarget(t) {
    pm.target = t;
    pm.editKey = "";
    pm.q(".pm-trace").innerHTML = "";
    renderTargets();
    loadEntries();
}

// loadEntries reads the target's current entries via PermList (6b read path).
async function loadEntries() {
    pm.entries = new Map();
    renderGrid();
    if (!pm.target || !canPermManage()) return;
    try {
        const resp = await App().PermList(pm.target.tier, pm.target.groupID || 0, pm.target.uniqueID || "", pm.target.channelID || 0);
        for (const e of resp.entries || []) {
            pm.entries.set(e.key, { value: e.value, grant: e.grant, skip: !!e.skip, negate: !!e.negate });
        }
    } catch { /* stale/unknown target: show empty grid */ }
    renderGrid();
}

function renderGrid() {
    const grid = pm.q(".pm-grid");
    if (!pm.target) {
        grid.innerHTML = `<div class="empty-state">Select a target on the left</div>`;
        return;
    }
    if (!canPermManage()) {
        grid.innerHTML = denyNotice("b_permission_manage");
        return;
    }
    const keys = new Set(PERMISSION_KEYS);
    for (const k of pm.entries.keys()) keys.add(k);
    const rows = [...keys].sort().filter((k) => !pm.filter || k.includes(pm.filter));
    grid.innerHTML = "";
    const table = document.createElement("table");
    table.className = "perm-grid pm-edit-grid";
    table.innerHTML = `<thead><tr><th>key</th><th>value</th><th>grant</th><th>flags</th></tr></thead><tbody></tbody>`;
    const tbody = table.querySelector("tbody");
    for (const key of rows) {
        const e = pm.entries.get(key);
        const tr = document.createElement("tr");
        tr.className = (e ? "set" : "unset") + (pm.editKey === key ? " editing" : "");
        tr.innerHTML = `
            <td class="mono">${key}</td>
            <td>${e ? `<span class="pill-val">${e.value}</span>` : '<span class="pm-dim">—</span>'}</td>
            <td>${e && e.grant ? `<span class="pill-val grant">${e.grant}</span>` : '<span class="pm-dim">—</span>'}</td>
            <td title="${e?.negate ? "negate is an explicit denial and wins over positive lower tiers" : e?.skip ? "skip locks this value against lower-tier overrides" : ""}">${e ? [e.skip ? "skip" : "", e.negate ? "negate" : ""].filter(Boolean).join(",") : ""}</td>`;
        tr.onclick = () => { pm.editKey = pm.editKey === key ? "" : key; renderGrid(); };
        tbody.appendChild(tr);
        if (pm.editKey === key) tbody.appendChild(editorRow(key, e));
    }
    grid.appendChild(table);
}

// editorRow is the inline value editor (136/152/153): value + grant inputs,
// skip/negate checkboxes with TS3 tooltips, Set/Unset, and (for clients)
// Trace.
function editorRow(key, e) {
    const tr = document.createElement("tr");
    tr.className = "pm-editor-row";
    const td = document.createElement("td");
    td.colSpan = 4;
    td.innerHTML = `
        <div class="pm-editor">
            <label>value <input type="number" class="dlg-input pe-value" value="${e ? e.value : 0}" /></label>
            <label title="grant value: a non-admin may only set values ≤ their own grant for this key (TS3-lite)">grant
                <input type="number" class="dlg-input pe-grant" value="${e ? e.grant : 0}" /></label>
            <label title="skip: locks this entry against lower-tier overrides; use only when inheritance must stop here">
                <input type="checkbox" class="pe-skip" ${e && e.skip ? "checked" : ""} /> skip</label>
            <label title="negate: an explicit denial; it forces effective value 0 and overrides positive lower tiers">
                <input type="checkbox" class="pe-negate" ${e && e.negate ? "checked" : ""} /> negate</label>
            <button class="dlg-ok pe-set">Set</button>
            <button class="dlg-cancel pe-unset" ${e ? "" : "disabled"}>Unset</button>
            ${pm.tab === "clients" ? '<button class="dlg-cancel pe-trace">Trace</button>' : ""}
        </div>`;
    const t = pm.target;
    td.querySelector(".pe-set").onclick = async () => {
        const value = parseInt(td.querySelector(".pe-value").value, 10) || 0;
        const grant = parseInt(td.querySelector(".pe-grant").value, 10) || 0;
        const skip = td.querySelector(".pe-skip").checked;
        const negate = td.querySelector(".pe-negate").checked;
        const err = await App().PermSet(t.tier, t.groupID || 0, t.uniqueID || "", t.channelID || 0, key, value, grant, skip, negate);
        if (err) V().toast("perm set failed: " + err, "warn");
        // Writes are fire-and-forget; grant-cap/permission errors arrive as
        // servererror toasts. Refresh shortly after (and once more to be sure).
        setTimeout(loadEntries, 400);
        setTimeout(loadEntries, 1500);
    };
    td.querySelector(".pe-unset").onclick = async () => {
        const err = await App().PermUnset(t.tier, t.groupID || 0, t.uniqueID || "", t.channelID || 0, key);
        if (err) V().toast("perm unset failed: " + err, "warn");
        setTimeout(loadEntries, 400);
        setTimeout(loadEntries, 1500);
    };
    const traceBtn = td.querySelector(".pe-trace");
    if (traceBtn) traceBtn.onclick = () => renderTrace(key);
    tr.appendChild(td);
    return tr;
}

// renderTrace shows the "why" view (137/155): winning tier highlighted, every
// tier's contribution in resolver order.
async function renderTrace(key) {
    const area = pm.q(".pm-trace");
    area.innerHTML = `<div class="empty-state">tracing…</div>`;
    let resp;
    try {
        resp = await App().PermTrace(pm.target.uniqueID || "", key, pm.target.channelID || 0);
    } catch (err) {
        area.innerHTML = `<div class="empty-state">trace failed: ${esc(err)}</div>`;
        return;
    }
    const byTier = new Map((resp.entries || []).map((x) => [x.tier, x]));
    let html = `<div class="trace-head">why <span class="mono">${esc(key)}</span> = <span class="pill-val">${resp.effective}</span>`;
    html += resp.effective_tier ? ` (from <b>${esc(resp.effective_tier)}</b>)` : " (not set anywhere)";
    html += `</div><table class="perm-grid trace-grid"><tbody>`;
    for (const tier of TRACE_TIERS) {
        const e = byTier.get(tier);
        const win = e && e.winning;
        html += `<tr class="${win ? "winning" : ""}">
            <td class="mono">${tier}</td>
            <td>${e && e.present ? `<span class="pill-val">${e.value}</span>` : '<span class="pm-dim">—</span>'}</td>
            <td>${e && e.present && e.grant ? e.grant : ""}</td>
            <td>${e && e.present ? [e.skip ? "skip" : "", e.negate ? "negate" : ""].filter(Boolean).join(",") : ""}</td>
            <td>${win ? "◀ wins" : ""}</td>
        </tr>`;
    }
    area.innerHTML = html + "</tbody></table>";
}

// csvCell quotes a CSV field: RFC 4180 doubling, and always quoted so a key
// or label containing a separator cannot shift the columns.
function csvCell(v) {
    let cell = String(v ?? "");
    if (/^[=+\-@]/.test(cell)) cell = "'" + cell;
    return '"' + cell.replace(/"/g, '""') + '"';
}

// exportPerms saves the current target's grid (148). JSON round-trips into an
// importer; CSV is the shape a spreadsheet or a diff of two servers wants.
async function exportPerms(format) {
    if (!pm.target) {
        V().toast("select a target first", "warn");
        return;
    }
    const t = pm.target;
    const rows = [...pm.entries.entries()].map(([key, e]) => ({ key, ...e }));
    const base = "permissions-" + (t.label || "target").replace(/[^a-z0-9_-]+/gi, "_");
    let name, body;
    if (format === "csv") {
        const head = ["tier", "group_id", "unique_id", "channel_id", "key", "value", "grant", "skip", "negate"];
        const lines = [head.join(",")];
        for (const r of rows) {
            lines.push([t.tier, t.groupID || 0, t.uniqueID || "", t.channelID || 0,
                r.key, r.value, r.grant, r.skip ? 1 : 0, r.negate ? 1 : 0].map(csvCell).join(","));
        }
        name = base + ".csv";
        body = lines.join("\r\n") + "\r\n";
    } else {
        name = base + ".json";
        body = JSON.stringify({
            tier: t.tier,
            target: { group_id: t.groupID || 0, unique_id: t.uniqueID || "", channel_id: t.channelID || 0, label: t.label },
            exported_at: new Date().toISOString(),
            entries: rows,
        }, null, 2);
    }
    const err = await App().ExportChat(name, body);
    if (err) V().toast("export failed: " + err, "warn");
    else V().toast("permissions exported (" + (format === "csv" ? "CSV" : "JSON") + ")");
}

// --- group management ----------------------------------------------------------

function renderGroupActions(actions, type) {
    const manage = canGroupManage(type);
    if (!manage) {
        actions.innerHTML = denyNotice(type === "channel" ? "b_channel_group_manage" : "b_server_group_manage");
        return;
    }
    actions.innerHTML = `
        <button class="ga-new" title="Create group">+ New</button>
        <button class="ga-rename" title="Rename selected">Rename</button>
        <button class="ga-delete" title="Delete selected">Delete</button>
        <button class="ga-icon" title="Upload group icon (177)">Icon</button>
        ${type === "server" ? `<button class="ga-look" title="Nickname colour, hoisting and sort order (178/179)">Appearance…</button>` : ""}`;
    renderTemplateButton(actions, tabTier(pm.tab));
    renderCopyButton(actions);

    actions.querySelector(".ga-new").onclick = async () => {
        const name = prompt("Group name:");
        if (!name) return;
        try {
            await App().GroupCreate(type, name, 0);
            toastAudit("group created");
            renderTargets();
            refreshGroups().then(() => V().renderTree());
        } catch (err) { V().toast("group create failed: " + err, "warn"); }
    };
    actions.querySelector(".ga-rename").onclick = async () => {
        if (!pm.target?.groupID) return V().toast("select a group first", "warn");
        const name = prompt("New name:", pm.target.label);
        if (!name) return;
        const err = await App().GroupRename(type, pm.target.groupID, name);
        if (err) return V().toast("rename failed: " + err, "warn");
        setTimeout(() => { renderTargets(); refreshGroups().then(() => V().renderTree()); }, 400);
    };
    actions.querySelector(".ga-delete").onclick = async () => {
        if (!pm.target?.groupID) return V().toast("select a group first", "warn");
        const g = pm.target.group;
        const forceNote = g && g.member_count > 0
            ? `<p class="warn">Group "${esc(g.name)}" has ${g.member_count} member(s) — deleting requires force.</p>`
            : "";
        const ok = await confirmDlg("Delete group",
            `<p>Delete ${type} group <b>${esc(pm.target.label)}</b>?</p>` + forceNote,
            "Delete", true);
        if (!ok) return;
        const err = await App().GroupDelete(type, pm.target.groupID, !!(g && g.member_count > 0));
        if (err) return V().toast("delete failed: " + err, "warn");
        pm.target = null;
        setTimeout(() => { renderTargets(); renderGrid(); refreshGroups().then(() => V().renderTree()); }, 400);
    };
    actions.querySelector(".ga-icon").onclick = async () => {
        if (!pm.target?.groupID) return V().toast("select a group first", "warn");
        const img = await pickIcon();
        if (!img) return;
        const err = await App().GroupIconSet(pm.target.groupID, img.dataBase64);
        if (err) V().toast("icon failed: " + err, "warn");
        else V().toast("group icon updated");
    };
    const lookBtn = actions.querySelector(".ga-look");
    if (lookBtn) {
        lookBtn.onclick = () => {
            if (!pm.target?.groupID) return V().toast("select a group first", "warn");
            openGroupAppearance(pm.target.group);
        };
    }
}

// openGroupAppearance edits a server group's cosmetics (178 colour, 179
// hoisting, sort order). The columns have existed since migration 009 and the
// tree already renders them; this is the only thing that can write them.
function openGroupAppearance(group) {
    const g = group || {};
    const { overlay, q } = modal("confirm-dlg", `
        <h3>Group appearance</h3>
        <div class="dlg-text">
            <div class="ga-look-name"></div>
            <label class="dlg-label">Nickname colour</label>
            <div class="ce-icon-row">
                <input type="color" class="ga-look-color" />
                <input type="text" class="dlg-input mono ga-look-hex" placeholder="#rrggbb" maxlength="7" />
                <button class="ga-look-clear" title="Clear back to the theme default">Clear</button>
            </div>
            <label class="dlg-label"><input type="checkbox" class="ga-look-hoist" /> hoist — show this group as its own section above the channel tree</label>
            <label class="dlg-label">Sort order (lower sorts first; the lowest applicable group picks the nickname colour)</label>
            <input type="number" class="dlg-input ga-look-sort" />
        </div>
        <div class="dlg-buttons">
            <button class="dlg-cancel">Cancel</button>
            <button class="dlg-ok">Save</button>
        </div>`);
    q(".ga-look-name").textContent = g.name || "";
    const hex = q(".ga-look-hex");
    const swatch = q(".ga-look-color");
    hex.value = g.color || "";
    swatch.value = /^#[0-9a-f]{6}$/i.test(g.color || "") ? g.color : "#7f7f7f";
    q(".ga-look-hoist").checked = !!g.hoist;
    q(".ga-look-sort").value = g.sort_id || 0;

    swatch.oninput = () => { hex.value = swatch.value; };
    hex.oninput = () => { if (/^#[0-9a-f]{6}$/i.test(hex.value)) swatch.value = hex.value; };
    q(".ga-look-clear").onclick = () => { hex.value = ""; };
    q(".dlg-cancel").onclick = () => overlay.remove();
    q(".dlg-ok").onclick = async () => {
        const color = hex.value.trim().toLowerCase();
        // The server rejects anything else with errCodeMalformed; catching it
        // here keeps the dialog open with the bad value still visible.
        if (color && !/^#[0-9a-f]{6}$/.test(color)) {
            return V().toast("colour must be #rrggbb (or empty for the theme default)", "warn");
        }
        try {
            await App().GroupEdit(g.id, color, q(".ga-look-hoist").checked,
                parseInt(q(".ga-look-sort").value, 10) || 0);
        } catch (err) {
            return V().toast("group appearance failed: " + err, "warn");
        }
        overlay.remove();
        toastAudit("group appearance updated");
        renderTargets();
        refreshGroups().then(() => V().renderTree());
    };
}

// renderCopyButton adds the copy-permissions action (141). The current target
// is the source; the destination is picked in the dialog.
function renderCopyButton(actions) {
    if (!canPermManage()) return;
    const btn = document.createElement("button");
    btn.textContent = "Copy perms…";
    btn.title = "Copy this target's permission entries onto another target (141)";
    btn.onclick = () => {
        if (!pm.target) return V().toast("select a source target first", "warn");
        const kind = copyKindOf(pm.target.tier);
        if (!kind) return V().toast("channel-tier entries cannot be copied", "warn");
        openPermCopy(kind, copyIDOf(pm.target), pm.target.label);
    };
    actions.appendChild(btn);
}

// copyKindOf maps a grid tier onto a perm_copy kind ("" = unsupported).
function copyKindOf(tier) {
    switch (tier) {
        case "server_group": return "servergroup";
        case "channel_group": return "channelgroup";
        case "client": return "client";
        default: return "";
    }
}

// copyIDOf renders a target's perm_copy id: a decimal group id, or the unique
// ID for a client.
function copyIDOf(t) {
    return t.tier === "client" ? (t.uniqueID || "") : String(t.groupID || 0);
}

// openPermCopy asks for the destination and issues the copy. The server caps
// every entry against the caller's own grant in both directions, so a denial
// is a normal outcome and arrives as a servererror toast.
async function openPermCopy(fromKind, fromID, fromLabel) {
    const { overlay, q } = modal("confirm-dlg", `
        <h3>Copy permissions</h3>
        <div class="dlg-text">
            <div>From <b class="pc-from"></b> <span class="mono pc-from-id"></span></div>
            <label class="dlg-label">To</label>
            <select class="dlg-input pc-kind">
                <option value="servergroup">server group</option>
                <option value="channelgroup">channel group</option>
                <option value="client">client (unique ID)</option>
            </select>
            <select class="dlg-input pc-group"></select>
            <input class="dlg-input pc-uid hidden" placeholder="destination unique ID" />
            <label class="dlg-label">Channel scope (channel groups, and the per-channel client tier on both sides)</label>
            <select class="dlg-input pc-channel">
                <option value="0">— none —</option>
            </select>
            <label class="dlg-label"><input type="checkbox" class="pc-replace" /> replace — clear the destination's own entries first, so the result is an exact copy rather than a merge</label>
        </div>
        <div class="dlg-buttons">
            <button class="dlg-cancel">Cancel</button>
            <button class="dlg-ok">Copy</button>
        </div>`);
    q(".pc-from").textContent = fromLabel || fromID;
    q(".pc-from-id").textContent = fromKind + ":" + fromID;
    for (const c of V().state.channels) {
        const opt = document.createElement("option");
        opt.value = c.ChannelID;
        opt.textContent = "# " + c.Name;
        q(".pc-channel").appendChild(opt);
    }
    const kindSel = q(".pc-kind");
    const groupSel = q(".pc-group");
    kindSel.value = fromKind;

    const fillGroups = async () => {
        groupSel.innerHTML = "";
        if (kindSel.value === "client") {
            groupSel.classList.add("hidden");
            q(".pc-uid").classList.remove("hidden");
            return;
        }
        groupSel.classList.remove("hidden");
        q(".pc-uid").classList.add("hidden");
        let groups = [];
        try {
            const resp = await App().GroupList(kindSel.value === "channelgroup" ? "channel" : "server");
            groups = resp.groups || [];
        } catch { /* leave the select empty */ }
        for (const g of groups) {
            const opt = document.createElement("option");
            opt.value = g.id;
            opt.textContent = g.name;
            groupSel.appendChild(opt);
        }
    };
    kindSel.onchange = fillGroups;
    await fillGroups();

    q(".dlg-cancel").onclick = () => overlay.remove();
    q(".dlg-ok").onclick = async () => {
        const toKind = kindSel.value;
        const toID = toKind === "client" ? q(".pc-uid").value.trim() : groupSel.value;
        if (!toID) return V().toast("pick a destination", "warn");
        const channelID = parseInt(q(".pc-channel").value, 10) || 0;
        if (toKind === fromKind && toID === fromID) {
            return V().toast("source and destination are the same target", "warn");
        }
        const err = await App().PermCopy(fromKind, fromID, toKind, toID, channelID, q(".pc-replace").checked);
        overlay.remove();
        if (err) return V().toast("copy failed: " + err, "warn");
        toastAudit("permissions copied");
        setTimeout(loadEntries, 500);
    };
}

function renderTemplateButton(actions, tier) {
    if (!canPermManage()) return;
    const btn = document.createElement("button");
    btn.textContent = "Template…";
    btn.title = "Apply a built-in permission template (142)";
    btn.onclick = async () => {
        if (!pm.target) return V().toast("select a target first", "warn");
        const pick = await templatePicker();
        if (!pick) return;
        const ok = await confirmDlg("Apply template",
            `<p>Apply the <b>${pick}</b> template to <b>${esc(pm.target.label)}</b>?</p>
             <p class="pm-dim">This writes the template's permission bundle through the normal write path (audited).</p>`,
            "Apply", false);
        if (!ok) return;
        const err = await App().PermTemplateApply(pick, tier, pm.target.groupID || 0, pm.target.uniqueID || "");
        if (err) return V().toast("template failed: " + err, "warn");
        toastAudit("template applied");
        setTimeout(loadEntries, 500);
    };
    actions.appendChild(btn);
}

function templatePicker() {
    return new Promise((resolve) => {
        const { overlay, dlg, q } = modal("confirm-dlg", `
            <h3>Permission template</h3>
            <div class="dlg-text">
                <select class="dlg-input tpl-select">
                    ${TEMPLATES.map((t) => `<option value="${t}">${t}</option>`).join("")}
                </select>
            </div>
            <div class="dlg-buttons">
                <button class="dlg-cancel">Cancel</button>
                <button class="dlg-ok">Select</button>
            </div>`);
        q(".dlg-ok").onclick = () => { const v = q(".tpl-select").value; overlay.remove(); resolve(v); };
        q(".dlg-cancel").onclick = () => { overlay.remove(); resolve(null); };
    });
}

// toastAudit notes the audit-log side effect of a write (149).
function toastAudit(what) {
    V().toast(what + " — recorded in the audit log");
}

// renderMembers shows the selected group's members with assign/unassign and
// the drag & drop target (140/141) plus timed assigns (145).
async function renderMembers() {
    const area = pm.q(".pm-members");
    const g = pm.target?.group;
    if (!g) {
        area.innerHTML = "";
        return;
    }
    const type = pm.tab === "server" ? "server" : "channel";
    let channelID = 0;
    let channelPicker = "";
    if (type === "channel") {
        // Channel-group membership is channel-scoped: pick the channel.
        channelPicker = `<select class="dlg-input mem-channel">
            <option value="0">— pick a channel for membership —</option>
            ${V().state.channels.map((c) => `<option value="${c.ChannelID}" ${pm.channelGroupChan === c.ChannelID ? "selected" : ""}>${esc(c.Name)}</option>`).join("")}
        </select>`;
        channelID = pm.channelGroupChan;
    }
    area.innerHTML = `
        <div class="pm-members-head">Members</div>
        ${channelPicker}
        <div class="pm-member-list"><div class="empty-state">loading…</div></div>
        <div class="pm-assign">
            <select class="dlg-input mem-user">
                <option value="">— online user —</option>
                ${V().state.clients.filter((c) => c.unique_id).map((c) =>
                    `<option value="${esc(c.unique_id)}">${esc(c.nickname || c.unique_id)}</option>`).join("")}
            </select>
            <input class="dlg-input mem-uid" placeholder="or unique ID" />
            <input class="dlg-input mem-min" type="number" min="0" value="0" title="duration in minutes (0 = permanent) (145)" />
            <button class="mem-add">Assign</button>
        </div>
        <div class="pm-drop-hint">drag users from the tree here to assign (140)</div>`;

    const chanSel = area.querySelector(".mem-channel");
    if (chanSel) {
        chanSel.onchange = () => {
            pm.channelGroupChan = parseInt(chanSel.value, 10) || 0;
            renderMembers();
        };
        if (!channelID) {
            area.querySelector(".pm-member-list").innerHTML = `<div class="empty-state">pick a channel above</div>`;
        }
    }

    const listEl = area.querySelector(".pm-member-list");
    if (type === "server" || channelID) {
        try {
            const resp = await App().GroupMembers(type, g.id, channelID);
            const members = resp.members || [];
            listEl.innerHTML = members.length ? "" : `<div class="empty-state">no members</div>`;
            for (const m of members) {
                const row = document.createElement("div");
                row.className = "pm-member";
                row.innerHTML = `<span class="pm-member-name"></span>
                    ${m.expires_at ? `<span class="pm-member-exp mono" title="timed membership (145)">→ ${fmtTime(m.expires_at)}</span>` : ""}
                    <button class="mem-del" title="Unassign">✕</button>`;
                row.querySelector(".pm-member-name").textContent = m.nickname || m.unique_id;
                row.title = m.unique_id;
                row.querySelector(".mem-del").onclick = async () => {
                    const err = await App().GroupUnassign(type, g.id, m.unique_id, channelID);
                    if (err) V().toast("unassign failed: " + err, "warn");
                    setTimeout(() => { renderMembers(); refreshGroups().then(() => V().renderTree()); }, 400);
                };
                listEl.appendChild(row);
            }
        } catch {
            listEl.innerHTML = `<div class="empty-state">member list unavailable</div>`;
        }
    }

    const doAssign = async (uid) => {
        if (!uid) return;
        const minutes = parseInt(area.querySelector(".mem-min").value, 10) || 0;
        const err = await App().GroupAssign(type, g.id, uid, channelID, minutes * 60);
        if (err) V().toast("assign failed: " + err, "warn");
        else toastAudit("assigned to " + g.name);
        setTimeout(() => { renderMembers(); renderTargets(); refreshGroups().then(() => V().renderTree()); }, 400);
    };
    area.querySelector(".mem-add").onclick = () => {
        const uid = area.querySelector(".mem-uid").value.trim() || area.querySelector(".mem-user").value;
        doAssign(uid);
    };

    // Drag & drop from the channel tree (140): rows carry text/voicx-uid.
    area.ondragover = (e) => {
        if (e.dataTransfer.types.includes("text/voicx-uid")) {
            e.preventDefault();
            area.classList.add("drop-active");
        }
    };
    area.ondragleave = () => area.classList.remove("drop-active");
    area.ondrop = (e) => {
        e.preventDefault();
        area.classList.remove("drop-active");
        const uid = e.dataTransfer.getData("text/voicx-uid");
        doAssign(uid);
    };
}

// --- audit log viewer (149/197) -------------------------------------------------

async function openAuditViewer() {
    const { overlay, dlg, q } = modal("audit", `
        <div class="pm-head">
            <h3>Audit Log</h3>
            <button class="icon-btn audit-close" title="Close">✕</button>
        </div>
        <div class="audit-filter-row">
            <input class="dlg-input audit-filter" placeholder="filter by action (e.g. perm_set, group_assign, ban)…" />
        </div>
        <div class="audit-list"></div>
        <div class="dlg-buttons"><button class="dlg-cancel audit-older">Load older</button></div>`);
    q(".audit-close").onclick = () => overlay.remove();
    if (!canAuditView()) {
        q(".audit-list").innerHTML = denyNotice("b_audit_view");
        q(".audit-older").remove();
        return;
    }
    const state = { entries: [], oldest: 0, filter: "" };
    const renderRows = () => {
        const list = q(".audit-list");
        const rows = state.entries.filter((e) => !state.filter || e.action.includes(state.filter));
        list.innerHTML = rows.length ? "" : `<div class="empty-state">no entries</div>`;
        const table = document.createElement("table");
        table.className = "perm-grid audit-grid";
        table.innerHTML = `<thead><tr><th>time</th><th>actor</th><th>action</th><th>target</th><th>detail</th></tr></thead><tbody></tbody>`;
        const tbody = table.querySelector("tbody");
        for (const e of rows) {
            const tr = document.createElement("tr");
            tr.innerHTML = `
                <td class="mono audit-time">${fmtTime(e.created_at)}</td>
                <td class="mono" title="${esc(e.actor)}">${esc((e.actor || "").slice(0, 10))}…</td>
                <td class="mono">${esc(e.action)}</td>
                <td class="mono">${esc(e.target)}</td>
                <td class="mono audit-detail" title="${esc(e.detail)}">${esc(e.detail)}</td>`;
            tbody.appendChild(tr);
        }
        list.appendChild(table);
    };
    const load = async () => {
        try {
            const resp = await App().AuditLog(state.oldest, 50);
            const entries = resp.entries || [];
            if (entries.length === 0) {
                q(".audit-older").disabled = true;
                return;
            }
            state.oldest = entries[entries.length - 1].id;
            state.entries = state.entries.concat(entries);
            renderRows();
        } catch (err) {
            V().toast("audit load failed: " + err, "warn");
        }
    };
    q(".audit-filter").oninput = (e) => { state.filter = e.target.value.trim(); renderRows(); };
    q(".audit-older").onclick = load;
    load();
}

// --- chat filter manager (117/118) ------------------------------------------------

// openChatFilters edits the runtime word/link moderation lists. Values are put
// into textareas through .value, never interpolated into HTML: the lists are
// operator-authored text and the whole point of the dialog is to type into it.
async function openChatFilters() {
    const { overlay, q } = modal("audit", `
        <div class="pm-head">
            <h3>Chat Filters</h3>
            <button class="icon-btn cf-close" title="Close">✕</button>
        </div>
        <div class="cf-source empty-state"></div>
        <div class="cf-body">
            <div class="cf-field">
                <b>Word filter</b>
                <div class="pm-dim">comma-separated; each entry is a case-insensitive substring of the message</div>
                <textarea class="dlg-input cf-words" rows="3"></textarea>
            </div>
            <div class="cf-field">
                <b>Link blacklist</b>
                <div class="pm-dim">comma-separated hosts; matches the host itself or any subdomain of it</div>
                <textarea class="dlg-input cf-black" rows="3"></textarea>
            </div>
            <div class="cf-field">
                <b>Link whitelist</b>
                <div class="pm-dim">comma-separated hosts; while non-empty EVERY link in a message must match one</div>
                <textarea class="dlg-input cf-white" rows="3"></textarea>
            </div>
        </div>
        <div class="dlg-buttons">
            <button class="dlg-cancel cf-reload">Reload</button>
            <button class="dlg-ok cf-save">Save</button>
        </div>`);
    q(".cf-close").onclick = () => overlay.remove();
    if (!canChatFilterManage()) {
        q(".cf-body").innerHTML = denyNotice("b_chat_filter_manage");
        q(".cf-source").remove();
        q(".dlg-buttons").remove();
        return;
    }
    let lastFilters = null;
    const fill = (resp) => {
        lastFilters = resp;
        q(".cf-words").value = resp.word_filter || "";
        q(".cf-black").value = resp.link_blacklist || "";
        q(".cf-white").value = resp.link_whitelist || "";
        q(".cf-source").textContent = resp.from_config
            ? "In force from config.yaml — no runtime override stored yet. Saving copies these lists into the server database, after which config.yaml is ignored."
            : "In force from the server database (runtime override). The chat filter settings in config.yaml are no longer applied.";
    };
    const load = async () => {
        try {
            fill(await App().ChatFilterGet());
        } catch (err) {
            q(".cf-source").textContent = "loading filters failed: " + err;
        }
    };
    q(".cf-reload").onclick = load;
    q(".cf-save").onclick = async () => {
        if (lastFilters?.from_config) {
            const ok = confirm("Saving will copy these lists into the server database and override config.yaml going forward. Proceed?");
            if (!ok) return;
        }
        const saveBtn = q(".cf-save");
        saveBtn.disabled = true;
        try {
            fill(await App().ChatFilterSet(q(".cf-words").value, q(".cf-black").value, q(".cf-white").value));
            toastAudit("chat filters updated");
        } catch (err) {
            V().toast("chat filter save failed: " + err, "warn");
        } finally {
            saveBtn.disabled = false;
        }
    };
    load();
}

// --- ban list dialog (172) --------------------------------------------------------

async function openBanList() {
    const { overlay, dlg, q } = modal("audit", `
        <div class="pm-head">
            <h3>Bans</h3>
            <button class="icon-btn ban-close" title="Close">✕</button>
        </div>
        <div class="ban-list"></div>`);
    q(".ban-close").onclick = () => overlay.remove();
    if (!canBan()) {
        q(".ban-list").innerHTML = denyNotice("b_client_ban / i_client_ban_power");
        return;
    }
    const render = async () => {
        const list = q(".ban-list");
        let bans = [];
        try {
            const resp = await App().BanList();
            bans = resp.bans || [];
        } catch (err) {
            list.innerHTML = `<div class="empty-state">ban list failed: ${esc(err)}</div>`;
            return;
        }
        list.innerHTML = bans.length ? "" : `<div class="empty-state">no bans</div>`;
        const table = document.createElement("table");
        table.className = "perm-grid audit-grid";
        table.innerHTML = `<thead><tr><th>target</th><th>reason</th><th>banned by</th><th>expires</th><th></th></tr></thead><tbody></tbody>`;
        const tbody = table.querySelector("tbody");
        for (const b of bans) {
            const tr = document.createElement("tr");
            const expired = b.expires_at && b.expires_at * 1000 < Date.now();
            tr.className = expired ? "pm-dim" : "";
            tr.innerHTML = `
                <td class="mono" title="${esc(b.value)}">${esc((b.value || "").slice(0, 16))}…</td>
                <td>${esc(b.reason)}</td>
                <td class="mono">${esc((b.banned_by || "").slice(0, 10))}</td>
                <td class="mono">${b.expires_at ? fmtTime(b.expires_at) : "permanent"}</td>
                <td><button class="mem-del ban-lift" title="Lift ban">✕</button></td>`;
            tr.querySelector(".ban-lift").onclick = async () => {
                const ok = await confirmDlg("Lift ban", `<p>Lift the ban on <span class="mono">${esc(b.value.slice(0, 24))}…</span>?</p>`, "Lift", true);
                if (!ok) return;
                const err = await App().BanRemove(b.id);
                if (err) V().toast("ban lift failed: " + err, "warn");
                setTimeout(render, 400);
            };
            tbody.appendChild(tr);
        }
        list.appendChild(table);
    };
    render();
}

// --- complaint list (173) ---------------------------------------------------------

// openComplaints reviews filed complaints. The server rides the ban gate
// rather than a key of its own, so the client gates the same way.
async function openComplaints() {
    const { overlay, q } = modal("audit", `
        <div class="pm-head">
            <h3>Complaints</h3>
            <button class="icon-btn cp-close" title="Close">✕</button>
        </div>
        <div class="cp-list"></div>`);
    q(".cp-close").onclick = () => overlay.remove();
    if (!canBan()) {
        q(".cp-list").innerHTML = denyNotice("b_client_ban / i_client_ban_power");
        return;
    }
    // Nicknames are best-effort server-side and may be "": the unique ID is
    // the only identifier that always exists.
    const who = (nick, uid) => nick || uid;
    const render = (entries) => {
        const list = q(".cp-list");
        list.innerHTML = entries.length ? "" : `<div class="empty-state">no complaints</div>`;
        if (!entries.length) return;
        const table = document.createElement("table");
        table.className = "perm-grid audit-grid";
        table.innerHTML = `<thead><tr><th>against</th><th>from</th><th>reason</th><th>filed</th><th></th></tr></thead><tbody></tbody>`;
        const tbody = table.querySelector("tbody");
        for (const e of entries) {
            const tr = document.createElement("tr");
            tr.innerHTML = `<td class="cp-target"></td><td class="cp-from"></td><td class="cp-reason"></td>
                <td class="mono">${fmtTime(e.created_at)}</td>
                <td><button class="mem-del cp-one" title="Clear this complaint">✕</button>
                    <button class="mem-del cp-all" title="Clear every complaint against this user">✕ all</button></td>`;
            tr.querySelector(".cp-target").textContent = who(e.target_nickname, e.target_unique_id);
            tr.querySelector(".cp-target").title = e.target_unique_id;
            tr.querySelector(".cp-from").textContent = who(e.from_nickname, e.from_unique_id);
            tr.querySelector(".cp-from").title = e.from_unique_id;
            tr.querySelector(".cp-reason").textContent = e.reason || "";
            tr.querySelector(".cp-one").onclick = () => clear(e.target_unique_id, e.from_unique_id);
            tr.querySelector(".cp-all").onclick = async () => {
                const ok = await confirmDlg("Clear complaints",
                    `<p>Clear <b>every</b> complaint against <span class="mono">${esc(e.target_unique_id.slice(0, 24))}…</span>?</p>`,
                    "Clear all", true);
                if (ok) clear(e.target_unique_id, "");
            };
            tbody.appendChild(tr);
        }
        list.appendChild(table);
    };
    const clear = async (target, from) => {
        try {
            render((await App().ComplaintClear(target, from)).entries || []);
            toastAudit("complaints cleared");
        } catch (err) {
            V().toast("clear failed: " + err, "warn");
        }
    };
    try {
        render((await App().ComplaintList()).entries || []);
    } catch (err) {
        q(".cp-list").innerHTML = `<div class="empty-state">complaint list failed: ${esc(err)}</div>`;
    }
}

// --- invite links (176) -----------------------------------------------------------

// inviteLink builds the handoff URL for a privilege key. Registering the
// voicx:// scheme with the OS is an installer concern; generation and parsing
// live here so a pasted link works even without the protocol handler.
function inviteLink(addr, token) {
    return "voicx://" + addr + "?token=" + encodeURIComponent(token);
}

// parseInviteLink reads voicx://host:port?token=… back into its parts
// (null when the string is not an invite link).
function parseInviteLink(url) {
    const m = /^voicx:\/\/([^/?#]+)\/?(?:\?(.*))?$/i.exec(String(url || "").trim());
    if (!m) return null;
    const token = new URLSearchParams(m[2] || "").get("token") || "";
    return { addr: m[1], token };
}

// --- QR codes (175) ---------------------------------------------------------------
// Self-contained byte-mode QR encoder, versions 1-10 at EC level M. An npm
// package or a remote generator is not an option here: the app ships with no
// network CSP allowance and has to work offline.

const QR_EC_PER_BLOCK = [10, 16, 26, 18, 24, 16, 18, 22, 22, 26];
const QR_BLOCKS = [1, 1, 1, 2, 2, 4, 4, 4, 5, 5];
const QR_TOTAL_CW = [26, 44, 70, 100, 134, 172, 196, 242, 292, 346];
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34], [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50]];
const QR_VERSION_BITS = { 7: 0x07c94, 8: 0x085bc, 9: 0x09a99, 10: 0x0a4d3 };

let QR_EXP = null;
let QR_LOG = null;

// qrTables builds the GF(256) log/antilog tables (primitive polynomial 0x11d).
function qrTables() {
    if (QR_EXP) return;
    QR_EXP = new Uint8Array(512);
    QR_LOG = new Uint8Array(256);
    let x = 1;
    for (let i = 0; i < 255; i++) {
        QR_EXP[i] = x;
        QR_LOG[x] = i;
        x <<= 1;
        if (x & 0x100) x ^= 0x11d;
    }
    for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
}

function qrMul(a, b) {
    return a === 0 || b === 0 ? 0 : QR_EXP[QR_LOG[a] + QR_LOG[b]];
}

// qrGenPoly returns the Reed-Solomon generator polynomial of degree n,
// highest coefficient first.
function qrGenPoly(n) {
    let g = [1];
    for (let i = 0; i < n; i++) {
        const res = new Array(g.length + 1).fill(0);
        for (let k = 0; k < g.length; k++) res[k] ^= g[k];
        for (let k = 0; k < g.length; k++) res[k + 1] ^= qrMul(g[k], QR_EXP[i]);
        g = res;
    }
    return g;
}

function qrEC(data, n) {
    const g = qrGenPoly(n);
    const res = new Array(data.length + n).fill(0);
    for (let i = 0; i < data.length; i++) res[i] = data[i];
    for (let i = 0; i < data.length; i++) {
        const lead = res[i];
        if (!lead) continue;
        for (let j = 0; j < g.length; j++) res[i + j] ^= qrMul(g[j], lead);
    }
    return res.slice(data.length);
}

function qrMaskFn(k) {
    switch (k) {
        case 0: return (r, c) => (r + c) % 2 === 0;
        case 1: return (r) => r % 2 === 0;
        case 2: return (r, c) => c % 3 === 0;
        case 3: return (r, c) => (r + c) % 3 === 0;
        case 4: return (r, c) => (Math.floor(r / 2) + Math.floor(c / 3)) % 2 === 0;
        case 5: return (r, c) => ((r * c) % 2) + ((r * c) % 3) === 0;
        case 6: return (r, c) => (((r * c) % 2) + ((r * c) % 3)) % 2 === 0;
        default: return (r, c) => (((r + c) % 2) + ((r * c) % 3)) % 2 === 0;
    }
}

// qrFormatBits is the 15-bit BCH format word for EC level M (00) and a mask.
function qrFormatBits(mask) {
    const data = mask;
    let rem = data;
    for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
    return ((data << 10) | rem) ^ 0x5412;
}

// qrPenalty scores a masked matrix by the four standard rules; the lowest
// score wins, which is what keeps big flat areas from confusing scanners.
function qrPenalty(mod, size) {
    let score = 0;
    const lines = [];
    for (let r = 0; r < size; r++) {
        let row = "";
        for (let c = 0; c < size; c++) row += mod[r][c] ? "1" : "0";
        lines.push(row);
    }
    for (let c = 0; c < size; c++) {
        let col = "";
        for (let r = 0; r < size; r++) col += mod[r][c] ? "1" : "0";
        lines.push(col);
    }
    for (const line of lines) {
        let run = 1;
        for (let i = 1; i <= line.length; i++) {
            if (i < line.length && line[i] === line[i - 1]) {
                run++;
                continue;
            }
            if (run >= 5) score += 3 + (run - 5);
            run = 1;
        }
        for (const pat of ["10111010000", "00001011101"]) {
            let at = line.indexOf(pat);
            while (at !== -1) {
                score += 40;
                at = line.indexOf(pat, at + 1);
            }
        }
    }
    let dark = 0;
    for (let r = 0; r < size - 1; r++) {
        for (let c = 0; c < size - 1; c++) {
            const v = mod[r][c];
            if (v === mod[r][c + 1] && v === mod[r + 1][c] && v === mod[r + 1][c + 1]) score += 3;
        }
    }
    for (let r = 0; r < size; r++) for (let c = 0; c < size; c++) dark += mod[r][c];
    score += 10 * Math.floor(Math.abs((dark * 100) / (size * size) - 50) / 5);
    return score;
}

// qrEncode returns the module matrix for text, or null when it does not fit a
// version-10 code (far beyond any invite link).
function qrEncode(text) {
    qrTables();
    const bytes = new TextEncoder().encode(text);
    let version = 0;
    let dataCw = 0;
    for (let v = 1; v <= 10; v++) {
        const cw = QR_TOTAL_CW[v - 1] - QR_EC_PER_BLOCK[v - 1] * QR_BLOCKS[v - 1];
        if (4 + (v <= 9 ? 8 : 16) + bytes.length * 8 <= cw * 8) {
            version = v;
            dataCw = cw;
            break;
        }
    }
    if (!version) return null;

    const bits = [];
    const put = (val, len) => { for (let i = len - 1; i >= 0; i--) bits.push((val >>> i) & 1); };
    put(4, 4);
    put(bytes.length, version <= 9 ? 8 : 16);
    for (const b of bytes) put(b, 8);
    for (let i = 0; i < 4 && bits.length < dataCw * 8; i++) bits.push(0);
    while (bits.length % 8) bits.push(0);
    const data = [];
    for (let i = 0; i < bits.length; i += 8) {
        let b = 0;
        for (let j = 0; j < 8; j++) b = (b << 1) | bits[i + j];
        data.push(b);
    }
    for (let i = 0; data.length < dataCw; i++) data.push(i % 2 ? 0x11 : 0xec);

    const nBlocks = QR_BLOCKS[version - 1];
    const ecLen = QR_EC_PER_BLOCK[version - 1];
    const shortLen = Math.floor(dataCw / nBlocks);
    const nLong = dataCw % nBlocks;
    const dataBlocks = [];
    const ecBlocks = [];
    let off = 0;
    for (let i = 0; i < nBlocks; i++) {
        const len = shortLen + (i >= nBlocks - nLong ? 1 : 0);
        const blk = data.slice(off, off + len);
        off += len;
        dataBlocks.push(blk);
        ecBlocks.push(qrEC(blk, ecLen));
    }
    const out = [];
    for (let i = 0; i <= shortLen; i++) for (const b of dataBlocks) if (i < b.length) out.push(b[i]);
    for (let i = 0; i < ecLen; i++) for (const b of ecBlocks) out.push(b[i]);

    const size = version * 4 + 17;
    const mod = Array.from({ length: size }, () => new Uint8Array(size));
    const fixed = Array.from({ length: size }, () => new Uint8Array(size));
    const setF = (r, c, v) => {
        if (r < 0 || c < 0 || r >= size || c >= size) return;
        mod[r][c] = v ? 1 : 0;
        fixed[r][c] = 1;
    };
    const finder = (r0, c0) => {
        for (let r = -1; r <= 7; r++) {
            for (let c = -1; c <= 7; c++) {
                const ring = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6);
                const core = r >= 2 && r <= 4 && c >= 2 && c <= 4;
                setF(r0 + r, c0 + c, ring || core);
            }
        }
    };
    finder(0, 0);
    finder(0, size - 7);
    finder(size - 7, 0);
    for (let i = 8; i < size - 8; i++) {
        setF(6, i, i % 2 === 0);
        setF(i, 6, i % 2 === 0);
    }
    const ap = QR_ALIGN[version - 1];
    for (const r of ap) {
        for (const c of ap) {
            if ((r <= 8 && c <= 8) || (r <= 8 && c >= size - 9) || (r >= size - 9 && c <= 8)) continue;
            for (let dr = -2; dr <= 2; dr++) {
                for (let dc = -2; dc <= 2; dc++) setF(r + dr, c + dc, Math.max(Math.abs(dr), Math.abs(dc)) !== 1);
            }
        }
    }
    if (version >= 7) {
        const vb = QR_VERSION_BITS[version];
        for (let i = 0; i < 18; i++) {
            const bit = (vb >>> i) & 1;
            const r = Math.floor(i / 3);
            const c = i % 3;
            setF(size - 11 + c, r, bit);
            setF(r, size - 11 + c, bit);
        }
    }
    // Reserve the format areas before the data walk so the zig-zag skips them.
    for (let i = 0; i <= 8; i++) {
        if (!fixed[8][i]) setF(8, i, 0);
        if (!fixed[i][8]) setF(i, 8, 0);
    }
    for (let i = 0; i < 8; i++) {
        setF(8, size - 1 - i, 0);
        setF(size - 1 - i, 8, 0);
    }

    let idx = 0;
    let bitIdx = 0;
    let upward = true;
    for (let col = size - 1; col > 0; col -= 2) {
        if (col === 6) col--;
        for (let i = 0; i < size; i++) {
            const r = upward ? size - 1 - i : i;
            for (let j = 0; j < 2; j++) {
                const c = col - j;
                if (fixed[r][c]) continue;
                let bit = 0;
                if (idx < out.length) bit = (out[idx] >>> (7 - bitIdx)) & 1;
                if (++bitIdx === 8) {
                    bitIdx = 0;
                    idx++;
                }
                mod[r][c] = bit;
            }
        }
        upward = !upward;
    }

    let best = null;
    let bestScore = Infinity;
    for (let mask = 0; mask < 8; mask++) {
        const fn = qrMaskFn(mask);
        const cand = mod.map((row) => Uint8Array.from(row));
        for (let r = 0; r < size; r++) {
            for (let c = 0; c < size; c++) if (!fixed[r][c] && fn(r, c)) cand[r][c] ^= 1;
        }
        const fb = qrFormatBits(mask);
        const gb = (i) => (fb >>> i) & 1;
        const setFmt = (r, c, v) => { cand[r][c] = v; };
        for (let i = 0; i <= 5; i++) setFmt(i, 8, gb(i));
        setFmt(7, 8, gb(6));
        setFmt(8, 8, gb(7));
        setFmt(8, 7, gb(8));
        for (let i = 9; i < 15; i++) setFmt(8, 14 - i, gb(i));
        for (let i = 0; i < 8; i++) setFmt(8, size - 1 - i, gb(i));
        for (let i = 8; i < 15; i++) setFmt(size - 15 + i, 8, gb(i));
        setFmt(size - 8, 8, 1);
        const score = qrPenalty(cand, size);
        if (score < bestScore) {
            bestScore = score;
            best = cand;
        }
    }
    return best;
}

// qrSVG renders text as an inline SVG. The payload only ever reaches the
// markup as path geometry — the key itself is never written into an attribute
// or a label.
function qrSVG(text, px) {
    const m = qrEncode(text);
    if (!m) return null;
    const size = m.length;
    const quiet = 4;
    const total = size + quiet * 2;
    let d = "";
    for (let r = 0; r < size; r++) {
        for (let c = 0; c < size; c++) if (m[r][c]) d += `M${c + quiet} ${r + quiet}h1v1h-1z`;
    }
    return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${total} ${total}" width="${px}" height="${px}"` +
        ` shape-rendering="crispEdges" role="img" aria-label="invite QR code">` +
        `<rect width="${total}" height="${total}" fill="#ffffff"/><path d="${d}" fill="#000000"/></svg>`;
}

// --- privilege token manager (174/175/176) ----------------------------------------

const canTokenList = () => hasPerm("b_virtualserver_token_list");
const canTokenAdd = () => hasPerm("b_virtualserver_token_add");
const canTokenDelete = () => hasPerm("b_virtualserver_token_delete");

// copyText copies to the clipboard and reports without echoing the value —
// a privilege key must never reach a toast or a log line.
async function copyText(value, what) {
    try {
        await navigator.clipboard.writeText(value);
        V().toast(what + " copied to the clipboard");
    } catch {
        V().toast("clipboard unavailable", "warn");
    }
}

// openTokenShare shows the handoff surface for one key: the raw key, the
// invite link, and a QR of the link.
function openTokenShare(token) {
    const addr = V().state.lastConnect?.addr || "";
    const link = inviteLink(addr, token);
    const { overlay, q } = modal("confirm-dlg", `
        <h3>Share privilege key</h3>
        <div class="dlg-text">
            <label class="dlg-label">Key</label>
            <input class="dlg-input mono tk-share-key" readonly />
            <label class="dlg-label">Invite link</label>
            <input class="dlg-input mono tk-share-link" readonly />
            <div class="tk-qr"></div>
            <div class="pm-dim tk-qr-note"></div>
        </div>
        <div class="dlg-buttons">
            <button class="dlg-cancel tk-copy-key">Copy key</button>
            <button class="dlg-cancel tk-copy-link">Copy link</button>
            <button class="dlg-ok">Close</button>
        </div>`);
    // .value, never innerHTML: the key is a credential.
    q(".tk-share-key").value = token;
    q(".tk-share-link").value = link;
    const svg = qrSVG(link, 200);
    if (svg) {
        q(".tk-qr").innerHTML = svg;
        q(".tk-qr-note").textContent = addr
            ? "Scan or paste the link into voicx to redeem."
            : "Not connected — the link has no server address; copy the key instead.";
    } else {
        q(".tk-qr-note").textContent = "link too long for a QR code — copy it instead";
    }
    q(".tk-copy-key").onclick = () => copyText(token, "key");
    q(".tk-copy-link").onclick = () => copyText(link, "invite link");
    q(".dlg-ok").onclick = () => overlay.remove();
}

// openTokenManager lists, mints and revokes privilege keys. All three
// messages answer with the full list, so the view cannot drift.
async function openTokenManager() {
    const { overlay, q } = modal("audit", `
        <div class="pm-head">
            <h3>Privilege Keys</h3>
            <button class="icon-btn tk-close" title="Close">✕</button>
        </div>
        <div class="tk-add"></div>
        <div class="tk-list"></div>`);
    q(".tk-close").onclick = () => overlay.remove();
    if (!canTokenList()) {
        q(".tk-list").innerHTML = denyNotice("b_virtualserver_token_list");
        q(".tk-add").remove();
        return;
    }

    let known = new Set();
    const channelName = (id) => V().state.channels.find((c) => c.ChannelID === id)?.Name || String(id);

    const render = (entries) => {
        const list = q(".tk-list");
        list.innerHTML = entries.length ? "" : `<div class="empty-state">no privilege keys</div>`;
        if (!entries.length) return;
        const table = document.createElement("table");
        table.className = "perm-grid audit-grid";
        table.innerHTML = `<thead><tr><th>key</th><th>grants</th><th>channel</th><th>note</th><th>created</th><th>used by</th><th></th></tr></thead><tbody></tbody>`;
        const tbody = table.querySelector("tbody");
        for (const e of entries) {
            const tr = document.createElement("tr");
            tr.className = e.used_by ? "pm-dim" : "";
            tr.innerHTML = `<td class="mono tk-key"></td><td class="tk-group"></td><td class="tk-chan"></td>
                <td class="tk-desc"></td><td class="mono">${fmtTime(e.created_at)}</td><td class="mono tk-used"></td>
                <td><button class="tk-share" title="Copy key / invite link / QR">Share…</button>
                    ${canTokenDelete() ? `<button class="mem-del tk-del" title="Revoke this key">✕</button>` : ""}</td>`;
            tr.querySelector(".tk-key").textContent = e.token;
            tr.querySelector(".tk-group").textContent = e.group_id ? (e.group_name || "group " + e.group_id) : "server admin";
            tr.querySelector(".tk-chan").textContent = e.channel_id ? "# " + channelName(e.channel_id) : "";
            tr.querySelector(".tk-desc").textContent = e.description || "";
            tr.querySelector(".tk-used").textContent = e.used_by || "";
            tr.querySelector(".tk-share").onclick = () => openTokenShare(e.token);
            const del = tr.querySelector(".tk-del");
            if (del) {
                del.onclick = async () => {
                    const ok = await confirmDlg("Revoke key", `<p>Revoke this privilege key? Anyone holding it can no longer redeem it.</p>`, "Revoke", true);
                    if (!ok) return;
                    try {
                        const resp = await App().TokenDelete(e.token);
                        known = new Set((resp.entries || []).map((x) => x.token));
                        render(resp.entries || []);
                        toastAudit("privilege key revoked");
                    } catch (err) {
                        V().toast("revoke failed: " + err, "warn");
                    }
                };
            }
            tbody.appendChild(tr);
        }
        list.appendChild(table);
    };

    if (canTokenAdd()) {
        q(".tk-add").innerHTML = `
            <select class="dlg-input tk-new-group"><option value="0">server admin (admin only)</option></select>
            <select class="dlg-input tk-new-chan"><option value="0">— no channel —</option></select>
            <input class="dlg-input tk-new-desc" placeholder="note (optional)" />
            <button class="tk-new">+ Create key</button>`;
        try {
            for (const g of (await App().GroupList("server")).groups || []) {
                const opt = document.createElement("option");
                opt.value = g.id;
                opt.textContent = g.name;
                q(".tk-new-group").appendChild(opt);
            }
        } catch { /* the admin option alone still works */ }
        for (const c of V().state.channels) {
            const opt = document.createElement("option");
            opt.value = c.ChannelID;
            opt.textContent = "# " + c.Name;
            q(".tk-new-chan").appendChild(opt);
        }
        q(".tk-new").onclick = async () => {
            const groupID = parseInt(q(".tk-new-group").value, 10) || 0;
            const chanID = parseInt(q(".tk-new-chan").value, 10) || 0;
            let resp;
            try {
                resp = await App().TokenAdd(groupID, chanID, q(".tk-new-desc").value.trim());
            } catch (err) {
                return V().toast("key creation failed: " + err, "warn");
            }
            const entries = resp.entries || [];
            // The reply is the whole list, oldest-first: the new key is the
            // one that was not there before.
            const fresh = entries.find((e) => !known.has(e.token));
            known = new Set(entries.map((e) => e.token));
            render(entries);
            q(".tk-new-desc").value = "";
            toastAudit("privilege key created");
            if (fresh) openTokenShare(fresh.token);
        };
    }

    try {
        const resp = await App().TokenList();
        known = new Set((resp.entries || []).map((e) => e.token));
        render(resp.entries || []);
    } catch (err) {
        q(".tk-list").innerHTML = `<div class="empty-state">key list failed: ${esc(err)}</div>`;
    }
}

// --- token redemption -------------------------------------------------------------

// pendingToken holds a key handed over while offline; the first snapshot after
// the next login redeems it.
let pendingToken = "";

// redeemToken sends MsgTokenUse. Success is silent — the grant arrives as the
// token_used event, which is where the permission refetch happens.
async function redeemToken(token) {
    if (!token) return;
    if (!V().state.myClientID) {
        pendingToken = token;
        V().toast("not connected — the key will be redeemed after you log in");
        return;
    }
    const err = await App().TokenUse(token);
    if (err) V().toast("redeem failed: " + err, "warn");
}

function redeemPendingToken() {
    if (!pendingToken || !V().state.myClientID) return;
    const token = pendingToken;
    pendingToken = "";
    redeemToken(token);
}

// openTokenRedeem takes a raw key or a voicx:// invite link. This is the only
// path that can redeem the bootstrap admin key printed at first server start.
function openTokenRedeem() {
    const { overlay, q } = modal("confirm-dlg", `
        <h3>Use a privilege key</h3>
        <div class="dlg-text">
            <p class="pm-dim">Paste a privilege key or a <span class="mono">voicx://</span> invite link. Any connected user can redeem a valid key.</p>
            <input class="dlg-input mono tk-use-input" placeholder="key or voicx://host:port?token=…" />
            <div class="pm-dim tk-use-hint"></div>
        </div>
        <div class="dlg-buttons">
            <button class="dlg-cancel">Cancel</button>
            <button class="dlg-ok">Redeem</button>
        </div>`);
    const input = q(".tk-use-input");
    input.oninput = () => {
        const link = parseInviteLink(input.value);
        q(".tk-use-hint").textContent = link ? "invite link for " + link.addr : "";
    };
    q(".dlg-cancel").onclick = () => overlay.remove();
    q(".dlg-ok").onclick = () => {
        const raw = input.value.trim();
        if (!raw) return;
        const link = parseInviteLink(raw);
        overlay.remove();
        if (!link) return redeemToken(raw);
        if (!link.token) return V().toast("that invite link carries no key", "warn");
        const addr = V().state.lastConnect?.addr || "";
        if (V().state.myClientID && (!link.addr || link.addr === addr)) return redeemToken(link.token);
        // A link for another server: stash the key and point the user at the
        // right address; the redemption fires on the next snapshot.
        pendingToken = link.token;
        const el = document.getElementById("login-addr");
        if (el) el.value = link.addr;
        V().showLogin();
        V().toast("connect to " + link.addr + " — the key is redeemed automatically");
    };
    input.focus();
}

// --- wiring ------------------------------------------------------------------

export function initPermsUI() {
    // Fire-and-forget failures are surfaced once by main.js. Keeping a second
    // permission-specific listener here used to duplicate the same warning.
    // (151) push-based cache invalidation: the server tells every client whose
    // resolution could have moved to refetch. group_edit also carries the
    // cosmetics, so it refreshes the group list and the tree as well.
    window.runtime.EventsOn("perms_invalid", (reason) => {
        V().refreshPermissions();
        if (String(reason) === "group_edit") {
            refreshGroups().then(() => V().renderTree());
        }
    });

    // Redemption is silent on the wire; the grant lands as this event.
    window.runtime.EventsOn("event", (json) => {
        let env;
        try {
            env = JSON.parse(json);
        } catch {
            return;
        }
        if (env.type !== "token_used") return;
        const d = env.data || {};
        if (d.client_id && d.client_id !== V().state.myClientID) return;
        if (d.promoted) V().state.isGuest = false;
        if (!d.group_id) V().state.isAdmin = true;
        V().toast(d.group_id ? "privilege key redeemed" : "privilege key redeemed — you are now a server admin");
        V().refreshPermissions();
        refreshGroups().then(() => V().renderTree());
    });

    // A key handed over while offline (invite link) redeems on the first
    // snapshot after login — the point at which the account row exists.
    window.runtime.EventsOn("snapshot", () => {
        redeemPendingToken();
    });

    window.__voicxPerms = {
        openPermissionManager, openAuditViewer, openBanList, openChatFilters,
        openComplaints, openTokenManager, openTokenRedeem,
        refreshGroups, groupColorFor, primaryGroup, hoistedGroups, groupIconURL,
        canPermManage, canAuditView, canGroupManage, canBan, canKickChannel, canKickServer,
        canChatFilterManage, canTokenList,
        inviteLink, parseInviteLink, redeemPendingToken,
        esc,
    };
}
