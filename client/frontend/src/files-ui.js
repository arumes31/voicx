// files-ui.js — wave-7 files & media: the channel file browser (256) with
// folders (261), drag & drop upload queue (257/260) with progress and
// cancel (258), quota bar (265), versions (264), rename/delete (262/263),
// download links (267), checksum display + verify (279/280), and the
// transfers window with a throughput sparkline (277/278). Folders are
// virtual (derived from file rows — empty folders do not persist).
import { humanBytes } from "./clientinfo.js";

const V = () => window.__voicx;
const App = () => window.go.main.App;

const fb = {
    channelID: 0,
    folder: "",
    folders: [],
    entries: [],
    used: 0,
    quota: 0,
    open: false,
};

// ---------------------------------------------------------------------------
// Transfers registry (278) + sparkline data (277)
// ---------------------------------------------------------------------------

const transfers = new Map(); // id -> record
const RECENT_KEEP = 20;
let sparkSamples = []; // aggregate bytes/sec samples (~1 per progress event burst)
let sparkTimer = null;

function trackTransfer(p) {
    let t = transfers.get(p.id);
    if (!t) {
        t = { id: p.id, direction: p.direction, name: p.name, history: [], started: Date.now() };
        transfers.set(p.id, t);
    }
    Object.assign(t, {
        transferred: p.transferred, total: p.total, bps: p.bytes_per_sec,
        status: p.status, error: p.error || "",
    });
    if (p.status === "active") {
        t.history.push(p.bytes_per_sec || 0);
        if (t.history.length > 60) t.history.shift();
    }
    trimTransfers();
    if (trWin.open) renderTransfers();
}

function trimTransfers() {
    const done = [...transfers.values()].filter((t) => t.status !== "active");
    if (done.length > RECENT_KEEP) {
        done.sort((a, b) => b.started - a.started);
        for (const t of done.slice(RECENT_KEEP)) transfers.delete(t.id);
    }
}

// activeBps aggregates the throughput of active transfers (sparkline).
function activeBps() {
    let sum = 0;
    for (const t of transfers.values()) if (t.status === "active") sum += t.bps || 0;
    return sum;
}

// ---------------------------------------------------------------------------
// File browser pane (256)
// ---------------------------------------------------------------------------

function esc(s) {
    return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function fmtDate(ts) {
    if (!ts) return "";
    return new Date(ts).toLocaleDateString() + " " + new Date(ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// refreshFiles reloads the current folder listing.
async function refreshFiles() {
    const pane = document.getElementById("files-pane");
    if (!fb.channelID) {
        pane.querySelector(".fb-list").innerHTML = `<div class="empty-state">Join a channel to browse its files</div>`;
        renderFbChrome();
        return;
    }
    try {
        const resp = await App().FileList(fb.channelID, fb.folder);
        fb.entries = resp.entries || [];
        fb.folders = resp.folders || [];
        fb.used = resp.used_bytes || 0;
        fb.quota = resp.quota_bytes || 0;
    } catch (err) {
        pane.querySelector(".fb-list").innerHTML = `<div class="empty-state">file list failed: ${esc(err)}</div>`;
        return;
    }
    renderFbChrome();
    renderFbList();
}

// subfoldersOf returns the direct child folders of the current folder.
function subfoldersOf() {
    const prefix = fb.folder ? fb.folder + "/" : "";
    const subs = new Set();
    for (const f of fb.folders) {
        if (!f.startsWith(prefix)) continue;
        const rest = f.slice(prefix.length);
        if (rest && !rest.includes("/")) subs.add(rest);
        else if (rest) subs.add(rest.split("/")[0]);
    }
    return [...subs].sort();
}

function renderFbChrome() {
    const pane = document.getElementById("files-pane");
    const ch = V().state.channels.find((c) => c.ChannelID === fb.channelID);
    // Breadcrumb.
    const crumb = pane.querySelector(".fb-crumb");
    crumb.innerHTML = "";
    const mk = (label, folder) => {
        const a = document.createElement("a");
        a.textContent = label;
        a.onclick = () => { fb.folder = folder; refreshFiles(); };
        crumb.appendChild(a);
        crumb.appendChild(document.createTextNode(" / "));
    };
    mk(ch ? "# " + ch.Name : "channel", "");
    const segs = fb.folder ? fb.folder.split("/") : [];
    segs.forEach((s, i) => mk(s, segs.slice(0, i + 1).join("/")));
    if (crumb.lastChild) crumb.removeChild(crumb.lastChild);

    // Quota bar (265).
    const q = pane.querySelector(".fb-quota");
    if (fb.quota > 0) {
        const pct = Math.min(100, Math.round(fb.used / fb.quota * 100));
        q.innerHTML = `<div class="fb-quota-fill${pct > 90 ? " hot" : ""}" style="width:${pct}%"></div>`;
        q.title = `${humanBytes(fb.used)} of ${humanBytes(fb.quota)} used (${pct}%)`;
        q.classList.remove("hidden");
    } else {
        q.classList.add("hidden");
        q.title = `${humanBytes(fb.used)} used (no quota)`;
    }
}

function renderFbList() {
    const list = document.getElementById("files-pane").querySelector(".fb-list");
    list.innerHTML = "";
    const subs = subfoldersOf();
    if (subs.length === 0 && fb.entries.length === 0) {
        list.innerHTML = `<div class="empty-state">Empty folder — drop files here to upload</div>`;
        return;
    }
    const table = document.createElement("table");
    table.className = "perm-grid fb-grid";
    table.innerHTML = `<thead><tr><th>name</th><th>size</th><th>uploader</th><th>date</th><th>sha-256</th><th></th></tr></thead><tbody></tbody>`;
    const tbody = table.querySelector("tbody");

    for (const sub of subs) {
        const tr = document.createElement("tr");
        tr.className = "fb-folder";
        tr.innerHTML = `<td colspan="6">📁 <a class="fb-folder-link"></a></td>`;
        const link = tr.querySelector(".fb-folder-link");
        link.textContent = sub + "/";
        link.onclick = () => { fb.folder = (fb.folder ? fb.folder + "/" : "") + sub; refreshFiles(); };
        tbody.appendChild(tr);
    }

    for (const e of fb.entries) {
        tbody.appendChild(fileRow(e));
    }
    list.appendChild(table);
}

function fileRow(e) {
    const tr = document.createElement("tr");
    tr.innerHTML = `
        <td class="fb-name"></td>
        <td class="mono">${humanBytes(e.size)}</td>
        <td class="mono fb-up" title=""></td>
        <td class="mono fb-date">${fmtDate(e.uploaded_at)}</td>
        <td class="mono fb-sha" title="click to copy"></td>
        <td class="fb-actions"></td>`;
    tr.querySelector(".fb-name").textContent = e.name;
    const up = tr.querySelector(".fb-up");
    up.textContent = (e.uploader || "").slice(0, 8) + (e.uploader ? "…" : "");
    up.title = e.uploader || "";
    const sha = tr.querySelector(".fb-sha");
    sha.textContent = (e.sha256 || "").slice(0, 8);
    sha.title = e.sha256 || "";
    sha.onclick = () => navigator.clipboard.writeText(e.sha256).then(() => V().toast("SHA-256 copied"));

    const act = tr.querySelector(".fb-actions");
    const btn = (label, title, fn) => {
        const b = document.createElement("button");
        b.className = "icon-btn";
        b.textContent = label;
        b.title = title;
        b.onclick = fn;
        act.appendChild(b);
        return b;
    };
    btn("⬇", "download", () => downloadFile(e));
    btn("🔗", "copy download link (15 min)", () => linkFile(e));
    btn("✓", "verify checksum (re-downloads and compares)", () => verifyFile(e, tr));
    btn("▾", "versions (264)", () => toggleVersions(e, tr));
    btn("✎", "rename / move", () => renameFile(e));
    btn("🗑", "delete", () => deleteFile(e));
    return tr;
}

// --- row actions ------------------------------------------------------------

let xferSeq = 0;

function downloadFile(e) {
    App().PickSavePath(e.name).then((path) => {
        if (!path) return;
        const id = `dl-${++xferSeq}`;
        const err = App().DownloadFileProgress(id, fb.channelID, fb.folder, e.name, path);
        if (err) V().toast("download failed: " + err, "warn");
        else V().toast("downloading " + e.name);
    });
}

async function linkFile(e) {
    try {
        const resp = await App().FileLink(fb.channelID, fb.folder, e.name);
        // The client builds the URL from its own control host (the server
        // cannot know its published address behind Docker/NAT).
        const host = (V().state.lastConnect?.addr || location.host).split(":")[0];
        const url = `http://${host}:${resp.health_port}${resp.path}`;
        await navigator.clipboard.writeText(url);
        V().toast("download link copied (valid until " + fmtDate(resp.expires_at * 1000) + ")");
    } catch (err) {
        V().toast("link failed: " + err, "warn");
    }
}

async function verifyFile(e, tr) {
    const sha = tr.querySelector(".fb-sha");
    sha.textContent = "…";
    try {
        const ok = await App().VerifyFile(fb.channelID, fb.folder, e.name, e.sha256);
        sha.textContent = ok ? "✓ ok" : "✗ BAD";
        sha.className = "mono fb-sha " + (ok ? "verify-ok" : "verify-bad");
        setTimeout(() => { sha.textContent = e.sha256.slice(0, 8); sha.className = "mono fb-sha"; }, 4000);
    } catch (err) {
        sha.textContent = "err";
        V().toast("verify failed: " + err, "warn");
    }
}

async function toggleVersions(e, tr) {
    const next = tr.nextSibling;
    if (next && next.classList && next.classList.contains("fb-versions")) {
        next.remove();
        return;
    }
    const vr = document.createElement("tr");
    vr.className = "fb-versions";
    vr.innerHTML = `<td colspan="6"><div class="fb-ver-list">loading…</div></td>`;
    tr.after(vr);
    try {
        const resp = await App().FileVersions(fb.channelID, fb.folder, e.name);
        const list = vr.querySelector(".fb-ver-list");
        if (!resp.entries || resp.entries.length === 0) {
            list.textContent = "no old versions";
            return;
        }
        list.innerHTML = "";
        for (const v of resp.entries) {
            const row = document.createElement("div");
            row.className = "fb-ver-row";
            row.innerHTML = `<span class="mono fb-ver-name"></span><span class="mono">${humanBytes(v.size)}</span><span class="mono fb-date">${fmtDate(v.uploaded_at)}</span>`;
            row.querySelector(".fb-ver-name").textContent = v.name;
            const dl = document.createElement("button");
            dl.className = "icon-btn";
            dl.textContent = "⬇";
            dl.title = "download this version";
            dl.onclick = () => downloadFile({ ...e, name: v.name });
            row.appendChild(dl);
            list.appendChild(row);
        }
    } catch (err) {
        vr.querySelector(".fb-ver-list").textContent = "versions failed: " + err;
    }
}

async function renameFile(e) {
    const name = prompt("New name (or folder/name to move):", (fb.folder ? fb.folder + "/" : "") + e.name);
    if (!name || name === e.name) return;
    let newFolder = "";
    let newName = name;
    const i = name.lastIndexOf("/");
    if (i >= 0) {
        newFolder = name.slice(0, i);
        newName = name.slice(i + 1);
    }
    const err = await App().FileRename(fb.channelID, fb.folder, e.name, newFolder, newName);
    if (err) V().toast("rename failed: " + err, "warn");
    setTimeout(refreshFiles, 400);
}

async function deleteFile(e) {
    if (!confirm(`Delete ${e.name}?`)) return;
    const err = await App().FileDelete(fb.channelID, fb.folder, e.name);
    if (err) V().toast("delete failed: " + err, "warn");
    setTimeout(refreshFiles, 400);
}

// --- upload queue (257/260) -----------------------------------------------------

const uploadQueue = [];
let uploadActive = false;

function queueUploads(files) {
    if (!fb.channelID) {
        V().toast("join a channel first", "warn");
        return;
    }
    for (const f of files) {
        uploadQueue.push({ file: f, folder: fb.folder });
    }
    V().toast(uploadQueue.length + " file(s) queued for upload");
    pumpUploads();
}

async function pumpUploads() {
    if (uploadActive || uploadQueue.length === 0) return;
    uploadActive = true;
    const { file, folder } = uploadQueue.shift();
    try {
        const buf = await file.arrayBuffer();
        const b64 = btoa(String.fromCharCode(...new Uint8Array(buf)));
        const id = `up-${++xferSeq}`;
        const err = App().UploadFileProgress(id, fb.channelID, folder, file.name, b64);
        if (err) {
            V().toast("upload " + file.name + " failed: " + err, "warn");
            uploadActive = false;
            pumpUploads();
            return;
        }
        // Wait for this transfer to finish before starting the next
        // (sequential queue, 260).
        const check = setInterval(() => {
            const t = transfers.get(id);
            if (t && t.status !== "active") {
                clearInterval(check);
                uploadActive = false;
                if (t.status === "done") refreshFiles();
                pumpUploads();
            }
        }, 300);
    } catch (err) {
        V().toast("reading " + file.name + " failed: " + err, "warn");
        uploadActive = false;
        pumpUploads();
    }
}

// --- transfers window (278) + sparkline (277) -------------------------------------

const trWin = { open: false, overlay: null };

function openTransfers() {
    if (trWin.open) return;
    trWin.open = true;
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg transfers">
            <div class="pm-head">
                <h3>Transfers</h3>
                <button class="icon-btn tr-close" title="Close">✕</button>
            </div>
            <canvas class="tr-spark" width="640" height="48"></canvas>
            <div class="tr-list"></div>
        </div>`;
    trWin.overlay = overlay;
    overlay.querySelector(".tr-close").onclick = closeTransfers;
    overlay.onclick = (e) => { if (e.target === overlay) closeTransfers(); };
    document.body.appendChild(overlay);
    renderTransfers();
    sparkTimer = setInterval(() => {
        sparkSamples.push(activeBps());
        if (sparkSamples.length > 60) sparkSamples.shift();
        drawSpark();
    }, 1000);
}

function closeTransfers() {
    if (!trWin.open) return;
    trWin.open = false;
    clearInterval(sparkTimer);
    trWin.overlay.remove();
}

function drawSpark() {
    if (!trWin.open) return;
    const canvas = trWin.overlay.querySelector(".tr-spark");
    const ctx = canvas.getContext("2d");
    ctx.fillStyle = "#0b0f14";
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    const max = Math.max(1, ...sparkSamples);
    ctx.strokeStyle = "#2ee6a8";
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    sparkSamples.forEach((v, i) => {
        const x = i / 59 * canvas.width;
        const y = canvas.height - (v / max) * (canvas.height - 4) - 2;
        if (i === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
    });
    ctx.stroke();
    ctx.fillStyle = "#8494a6";
    ctx.font = "10px 'JetBrains Mono Variable', monospace";
    ctx.fillText(humanBytes(activeBps()) + "/s", 6, 12);
}

function renderTransfers() {
    if (!trWin.open) return;
    const list = trWin.overlay.querySelector(".tr-list");
    list.innerHTML = "";
    const rows = [...transfers.values()].sort((a, b) => b.started - a.started);
    if (rows.length === 0) {
        list.innerHTML = `<div class="empty-state">No transfers yet</div>`;
        return;
    }
    for (const t of rows) {
        const row = document.createElement("div");
        row.className = "tr-row " + t.status;
        const pct = t.total > 0 ? Math.min(100, Math.round(t.transferred / t.total * 100)) : 0;
        const eta = t.status === "active" && t.bps > 0 && t.total > 0
            ? Math.max(0, Math.round((t.total - t.transferred) / t.bps)) + "s"
            : "";
        row.innerHTML = `
            <span class="tr-dir">${t.direction === "upload" ? "⬆" : "⬇"}</span>
            <span class="tr-name" title="${esc(t.name)}">${esc(t.name)}</span>
            <span class="tr-bar"><span class="tr-fill" style="width:${pct}%"></span></span>
            <span class="tr-meta mono">${pct}% · ${humanBytes(t.bps || 0)}/s ${eta ? "· " + eta : ""}</span>
            <span class="tr-status mono">${t.status}${t.error ? ": " + esc(t.error) : ""}</span>`;
        if (t.status === "active") {
            const cancel = document.createElement("button");
            cancel.className = "icon-btn";
            cancel.textContent = "✕";
            cancel.title = "cancel transfer";
            cancel.onclick = () => App().CancelTransfer(t.id);
            row.appendChild(cancel);
        }
        list.appendChild(row);
    }
}

// --- server icon (270) ---------------------------------------------------------

async function loadServerIcon() {
    try {
        const data = await App().ServerIconGet();
        const el = document.getElementById("server-icon");
        if (data && data.data_base64) {
            el.src = `data:${data.content_type};base64,${data.data_base64}`;
            el.classList.remove("hidden");
        } else {
            el.classList.add("hidden");
        }
    } catch { /* no icon */ }
}

// --- wiring ---------------------------------------------------------------------

export function initFilesUI() {
    const pane = document.getElementById("files-pane");
    pane.innerHTML = `
        <div class="fb-toolbar">
            <span class="fb-crumb"></span>
            <span class="fb-spacer"></span>
            <button class="icon-btn fb-refresh" title="Refresh">⟳</button>
            <button class="icon-btn fb-upload" title="Upload files">⬆</button>
            <button class="icon-btn fb-mkdir" title="New folder (virtual — persists only while it contains files)">📁+</button>
            <button class="icon-btn fb-transfers" title="Transfers">⇅</button>
        </div>
        <div class="fb-quota hidden"><div class="fb-quota-fill"></div></div>
        <div class="fb-list"></div>`;

    pane.querySelector(".fb-refresh").onclick = refreshFiles;
    pane.querySelector(".fb-transfers").onclick = openTransfers;
    pane.querySelector(".fb-upload").onclick = () => {
        const input = document.createElement("input");
        input.type = "file";
        input.multiple = true;
        input.onchange = () => queueUploads([...input.files]);
        input.click();
    };
    pane.querySelector(".fb-mkdir").onclick = () => {
        const name = prompt("New folder name (virtual — persists only while it contains files):");
        if (!name) return;
        const clean = name.replace(/^\/+|\/+$/g, "");
        if (!clean || clean.includes("..") || clean.includes("\\")) {
            V().toast("invalid folder name", "warn");
            return;
        }
        fb.folder = fb.folder ? fb.folder + "/" + clean : clean;
        refreshFiles();
        V().toast("folder ready — upload or move files into it");
    };

    // (257) drag & drop upload onto the whole pane.
    pane.addEventListener("dragover", (e) => {
        e.preventDefault();
        pane.classList.add("drop-active");
    });
    pane.addEventListener("dragleave", () => pane.classList.remove("drop-active"));
    pane.addEventListener("drop", (e) => {
        e.preventDefault();
        pane.classList.remove("drop-active");
        if (e.dataTransfer.files.length) queueUploads([...e.dataTransfer.files]);
    });

    // Tab switching (chat <-> files).
    const tabChat = document.getElementById("tab-chat");
    const tabFiles = document.getElementById("tab-files");
    const setTab = (files) => {
        fb.open = files;
        document.getElementById("center").classList.toggle("files-active", files);
        tabChat.classList.toggle("active", !files);
        tabFiles.classList.toggle("active", files);
        if (files) {
            fb.channelID = V().state.myChannelID;
            refreshFiles();
        }
    };
    tabChat.onclick = () => setTab(false);
    tabFiles.onclick = () => setTab(true);
    document.getElementById("tab-transfers").onclick = openTransfers;

    window.runtime.EventsOn("ft_progress", trackTransfer);

    window.__voicxFiles = {
        refreshFiles, openTransfers, loadServerIcon,
        // Follows channel changes (256): browsing follows the channel I'm in.
        onChannelChanged() {
            if (fb.open) {
                fb.channelID = V().state.myChannelID;
                fb.folder = "";
                refreshFiles();
            }
        },
    };
}
