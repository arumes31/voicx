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
    "b_client_video_publish", "b_client_priority_speaker", "b_client_ignore_antiflood",
    "b_client_request_talker",
    "i_ft_file_upload_power", "i_ft_needed_file_upload_power",
    "i_ft_file_download_power", "i_ft_needed_file_download_power",
    "i_client_poke_power", "i_client_needed_poke_power", "b_client_poke",
    "b_client_remoteaddress_view", "i_permission_modify_power",
    "b_virtualserver_info_view", "b_virtualserver_connectioninfo_view", "b_virtualserver_recording",
    "b_virtualserver_token_list", "b_virtualserver_token_add", "b_virtualserver_token_use",
    "b_virtualserver_token_delete",
    "b_chat_delete_any", "b_chat_mention_all", "b_chat_slowmode_bypass", "b_emoji_manage",
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
                    <button class="pm-export icon-btn" title="Export this target's permissions as JSON (148)">⬇</button>
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
    q(".pm-export").onclick = exportPerms;
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
            <td>${e ? [e.skip ? "skip" : "", e.negate ? "negate" : ""].filter(Boolean).join(",") : ""}</td>`;
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
            <label title="skip: prevents LOWER tiers from overriding this entry (multi-group merge)">
                <input type="checkbox" class="pe-skip" ${e && e.skip ? "checked" : ""} /> skip</label>
            <label title="negate: explicitly DENIES this permission, overriding any positive lower-tier value">
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

// exportPerms saves the current target's grid as JSON (148).
async function exportPerms() {
    if (!pm.target) {
        V().toast("select a target first", "warn");
        return;
    }
    const out = {
        tier: pm.target.tier,
        target: { group_id: pm.target.groupID || 0, unique_id: pm.target.uniqueID || "", channel_id: pm.target.channelID || 0, label: pm.target.label },
        exported_at: new Date().toISOString(),
        entries: [...pm.entries.entries()].map(([key, e]) => ({ key, ...e })),
    };
    const name = "permissions-" + (pm.target.label || "target").replace(/[^a-z0-9_-]+/gi, "_") + ".json";
    const err = await App().ExportChat(name, JSON.stringify(out, null, 2));
    if (err) V().toast("export failed: " + err, "warn");
    else V().toast("permissions exported");
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
        <button class="ga-icon" title="Upload group icon (177)">Icon</button>`;
    renderTemplateButton(actions, tabTier(pm.tab));

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

// --- wiring ------------------------------------------------------------------

export function initPermsUI() {
    // Grant-cap / permission errors from fire-and-forget writes arrive here
    // (the server sends MsgError); surface them as toasts.
    window.runtime.EventsOn("servererror", (msg) => {
        if (String(msg).includes("grant cap") || String(msg).includes("insufficient permission")) {
            V().toast("server: " + msg, "warn");
        }
    });
    window.__voicxPerms = {
        openPermissionManager, openAuditViewer, openBanList,
        refreshGroups, groupColorFor, primaryGroup, hoistedGroups, groupIconURL,
        canPermManage, canAuditView, canGroupManage, canBan, canKickChannel, canKickServer,
        esc,
    };
}
