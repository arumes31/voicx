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
        status: p.status, error: p.error || "", resumed: p.resumed || 0,
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

// isChatAttachment reports a row the browser cannot make sense of: a chat
// attachment is sealed with a per-file key that exists only inside the
// encrypted chat message linking it, so downloading it here yields ciphertext
// that looks like corruption (91-135). The server's flag is authoritative
// when set, but it is omitempty on the wire — false and absent look identical
// — so the content-derived ".vcx" suffix stays as the fallback. Both err
// toward showing the lock, which is the safe direction.
function isChatAttachment(e) {
    return !!e.encrypted || (e.name || "").toLowerCase().endsWith(".vcx");
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
    const sealed = isChatAttachment(e);
    const nameCell = tr.querySelector(".fb-name");
    nameCell.textContent = sealed ? "🔒 chat attachment" : e.name;
    if (sealed) nameCell.title = e.name;
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
    const dl = btn("⬇", "download", () => downloadFile(e));
    const link = btn("🔗", "copy download link (15 min)", () => linkFile(e));
    if (sealed) {
        dl.disabled = true;
        dl.title = "open it from the chat message that carries its key";
        link.disabled = true;
        link.title = "sealed files cannot be linked directly";
    }
    btn("✓", "verify checksum (re-downloads and compares)", () => verifyFile(e, tr));
    btn("▾", "versions (264)", () => toggleVersions(e, tr));
    btn("✎", "rename / move within this channel", () => renameFile(e));
    btn("⇄", "move to another channel (262)", () => moveToChannel(e));
    btn("🗑", "delete", () => deleteFile(e));
    return tr;
}

// --- row actions ------------------------------------------------------------

let xferSeq = 0;

// startDownload runs one download attempt and remembers its arguments so the
// transfer list can retry it — a retry into the same destination resumes from
// the partial file rather than starting over (259).
async function startDownload(args, id) {
    id = id || `dl-${++xferSeq}`;
    const err = await App().DownloadFileProgress(id, args.channelID, args.folder, args.name, args.path, args.size || 0);
    if (err) {
        V().toast("download failed: " + err, "warn");
        return;
    }
    downloadArgs.set(id, args);
    V().toast("downloading " + args.name);
}

// downloadArgs keeps the retry payload per transfer id.
const downloadArgs = new Map();

async function downloadFile(e) {
    // The configured download folder (Downloads settings) wins; only fall back
    // to the save dialog when the user has not set one.
    let path = await App().DownloadPath(e.name);
    if (!path) path = await App().PickSavePath(e.name);
    if (!path) return;
    startDownload({ channelID: fb.channelID, folder: fb.folder, name: e.name, path, size: e.size || 0 });
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
            dl.onclick = () => downloadFile({ ...e, name: v.name, size: v.size });
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
    const err = await App().FileRename(fb.channelID, fb.folder, e.name, newFolder, newName, 0);
    if (err) V().toast("rename failed: " + err, "warn");
    setTimeout(refreshFiles, 400);
}

// moveToChannel moves a file into another channel (262). The server checks
// both channels, so a target the user cannot upload to is refused there.
async function moveToChannel(e) {
    const channels = (V().state.channels || []).filter((c) => c.ChannelID !== fb.channelID);
    if (channels.length === 0) {
        V().toast("no other channel to move into", "warn");
        return;
    }
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3>Move to channel</h3>
            <div class="dlg-text fb-move-what"></div>
            <label class="dlg-label">target channel</label>
            <select class="dlg-input fb-move-ch"></select>
            <label class="dlg-label">folder in the target (blank = root)</label>
            <input class="dlg-input fb-move-folder" placeholder="docs/2024" />
            <div class="dlg-buttons">
                <button class="dlg-ok">Move</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    overlay.querySelector(".fb-move-what").textContent = e.name;
    const sel = overlay.querySelector(".fb-move-ch");
    for (const c of channels) {
        const opt = document.createElement("option");
        opt.value = String(c.ChannelID);
        opt.textContent = c.Name;
        sel.appendChild(opt);
    }
    const close = () => overlay.remove();
    overlay.querySelector(".dlg-cancel").onclick = close;
    overlay.onclick = (ev) => { if (ev.target === overlay) close(); };
    overlay.querySelector(".dlg-ok").onclick = async () => {
        const target = parseInt(sel.value, 10);
        const folder = overlay.querySelector(".fb-move-folder").value.replace(/^\/+|\/+$/g, "");
        close();
        const err = await App().FileRename(fb.channelID, fb.folder, e.name, folder, e.name, target);
        if (err) V().toast("move failed: " + err, "warn");
        else V().toast(`moved ${e.name} to ${sel.options[sel.selectedIndex].textContent}`);
        setTimeout(refreshFiles, 400);
    };
    document.body.appendChild(overlay);
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
const UPLOAD_WAIT_TIMEOUT_MS = 10 * 60 * 1000;

// DROP_MAX caps the browser-File route: a dropped File has no path on disk we
// can hand to Go, so its bytes must travel base64 inside one IPC argument.
// Past a few MiB that argument stops arriving at all, so refuse it loudly and
// point at the picker, which streams (259).
const DROP_MAX = 4 * 1024 * 1024;

// queueUploads takes browser File objects (drag & drop).
function queueUploads(files) {
    if (!fb.channelID) {
        V().toast("join a channel first", "warn");
        return;
    }
    let queued = 0;
    for (const f of files) {
        if (f.size > DROP_MAX) {
            V().toast(`${f.name} is too large to drop (${humanBytes(f.size)}) — use ⬆ to upload it`, "warn");
            continue;
        }
        uploadQueue.push({ file: f, folder: fb.folder });
        queued++;
    }
    if (queued) V().toast(queued + " file(s) queued for upload");
    pumpUploads();
}

// queueUploadPaths takes native paths from the picker: those stream off disk.
function queueUploadPaths(paths) {
    if (!fb.channelID) {
        V().toast("join a channel first", "warn");
        return;
    }
    for (const p of paths) uploadQueue.push({ path: p, folder: fb.folder });
    V().toast(paths.length + " file(s) queued for upload");
    pumpUploads();
}

// awaitUpload keeps the queue sequential (260): the next file starts only once
// this one has left the active state.
function awaitUpload(id) {
    const deadline = Date.now() + UPLOAD_WAIT_TIMEOUT_MS;
    const check = setInterval(() => {
        const t = transfers.get(id);
        if (t && t.status !== "active") {
            clearInterval(check);
            uploadActive = false;
            if (t.status === "done") refreshFiles();
            pumpUploads();
        } else if (Date.now() >= deadline) {
            clearInterval(check);
            uploadActive = false;
            V().toast("upload status timed out; continuing the queue", "warn");
            pumpUploads();
        }
    }, 300);
}

function bytesToBase64(bytes) {
    const chunks = [];
    const chunkSize = 0x8000;
    for (let offset = 0; offset < bytes.length; offset += chunkSize) {
        chunks.push(String.fromCharCode(...bytes.subarray(offset, offset + chunkSize)));
    }
    return btoa(chunks.join(""));
}

async function pumpUploads() {
    if (uploadActive || uploadQueue.length === 0) return;
    uploadActive = true;
    const { file, path, folder } = uploadQueue.shift();
    const id = `up-${++xferSeq}`;
    const label = path ? path.split(/[\\/]/).pop() : file.name;
    try {
        let err;
        if (path) {
            err = await App().UploadPathProgress(id, fb.channelID, folder, path);
        } else {
            const buf = await file.arrayBuffer();
            const b64 = bytesToBase64(new Uint8Array(buf));
            err = await App().UploadFileProgress(id, fb.channelID, folder, file.name, b64);
        }
        if (err) {
            V().toast("upload " + label + " failed: " + err, "warn");
            uploadActive = false;
            pumpUploads();
            return;
        }
        awaitUpload(id);
    } catch (err) {
        V().toast("reading " + label + " failed: " + err, "warn");
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
        const resumed = t.resumed > 0 ? ` · resumed at ${humanBytes(t.resumed)}` : "";
        row.innerHTML = `
            <span class="tr-dir">${t.direction === "upload" ? "⬆" : "⬇"}</span>
            <span class="tr-name" title="${esc(t.name)}">${esc(t.name)}</span>
            <span class="tr-bar"><span class="tr-fill" style="width:${pct}%"></span></span>
            <span class="tr-meta mono">${pct}% · ${humanBytes(t.bps || 0)}/s ${eta ? "· " + eta : ""}${resumed}</span>
            <span class="tr-status mono">${t.status}${t.error ? ": " + esc(t.error) : ""}</span>`;
        if (t.status === "active") {
            const cancel = document.createElement("button");
            cancel.className = "icon-btn";
            cancel.textContent = "✕";
            cancel.title = "cancel transfer";
            cancel.onclick = () => App().CancelTransfer(t.id);
            row.appendChild(cancel);
        } else if (t.status !== "done" && downloadArgs.has(t.id)) {
            // (259) retrying into the same destination picks up where the
            // interrupted attempt stopped instead of re-fetching the whole file.
            const retry = document.createElement("button");
            retry.className = "icon-btn";
            retry.textContent = "↻";
            retry.title = "resume from where this stopped";
            retry.onclick = () => {
                const args = downloadArgs.get(t.id);
                t.history = [];
                startDownload(args, t.id);
            };
            row.appendChild(retry);
        }
        list.appendChild(row);
    }
}

// --- server icon + banner (270) ------------------------------------------------

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
    // Both halves of the server's branding load on the same trigger (connect
    // and menu refresh), so the banner rides along here.
    loadServerBanner();
}

// bannerEl returns the banner image, creating it under the sidebar brand on
// first use. It is built here rather than in the page markup so the whole
// feature stays in one file.
function bannerEl() {
    let el = document.getElementById("server-banner");
    if (el) return el;
    const sidebar = document.getElementById("sidebar");
    const brand = sidebar && sidebar.querySelector(".brand");
    if (!brand) return null;
    el = document.createElement("img");
    el.id = "server-banner";
    el.alt = "";
    el.title = "server banner";
    el.style.cssText = "display:none;width:100%;max-height:96px;object-fit:cover;border-radius:6px;margin:6px 0";
    brand.after(el);
    return el;
}

async function loadServerBanner() {
    const el = bannerEl();
    if (!el) return;
    try {
        const data = await App().ServerBannerGet();
        if (data && data.data_base64) {
            el.src = `data:${data.content_type};base64,${data.data_base64}`;
            el.style.display = "";
        } else {
            el.removeAttribute("src");
            el.style.display = "none";
        }
    } catch {
        el.style.display = "none";
    }
}

// setServerBanner uploads a new banner (admin only). Reachable from the files
// toolbar; the server refuses non-admins.
async function setServerBanner() {
    const { pickIcon } = await import("./image-tools.js");
    // Banners are wide, so allow more pixels than a 256px icon; the quality
    // loop still keeps it under the server's 256 KiB cap (274).
    const img = await pickIcon(1600, 0.85);
    if (!img) return;
    const err = await App().ServerBannerSet(img.dataBase64);
    if (err) {
        V().toast("banner failed: " + err, "warn");
        return;
    }
    V().toast("server banner updated");
    setTimeout(loadServerBanner, 400);
}

// --- channel icons (271) --------------------------------------------------------

// channelIcons caches one entry per channel: a data URL, or null for "asked,
// none set". Without the cache every tree re-render would re-fetch.
const channelIcons = new Map();

// decorateChannelIcons swaps the placeholder glyph in the channel tree for the
// real icon. The tree is rendered elsewhere and re-rendered often, so this
// runs from an observer rather than being woven into the render.
function decorateChannelIcons() {
    const rows = document.querySelectorAll("#channel-tree .channel[data-chid]");
    for (const row of rows) {
        const id = Number(row.dataset.chid);
        const slot = row.querySelector(".ch-icon");
        if (!slot || slot.dataset.iconFor === String(id)) continue;
        if (!channelIcons.has(id)) {
            channelIcons.set(id, null); // claim it so concurrent passes do not refetch
            App().ChannelIconGet(id).then((d) => {
                if (d && d.data_base64) {
                    channelIcons.set(id, `data:${d.content_type};base64,${d.data_base64}`);
                    decorateChannelIcons();
                }
            }).catch(() => { /* no icon */ });
            continue;
        }
        const url = channelIcons.get(id);
        if (!url) continue;
        slot.dataset.iconFor = String(id);
        slot.textContent = "";
        const img = document.createElement("img");
        img.src = url;
        img.alt = "";
        img.style.cssText = "width:14px;height:14px;border-radius:3px;object-fit:cover;vertical-align:-2px";
        slot.appendChild(img);
    }
}

// watchChannelIcons re-decorates after any tree re-render.
function watchChannelIcons() {
    const tree = document.getElementById("channel-tree");
    if (!tree) return;
    let queued = false;
    new MutationObserver(() => {
        if (queued) return;
        queued = true;
        setTimeout(() => { queued = false; decorateChannelIcons(); }, 50);
    }).observe(tree, { childList: true, subtree: true });
    decorateChannelIcons();
}

// --- emoji manager (272) --------------------------------------------------------

// The picker and its quick-upload live in the chat pane; this is the full
// asset manager for the same files — previews, rename, delete and upload in
// one place, reached from the media (files) pane.
async function openEmojiManager() {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <div class="pm-head">
                <h3>Custom emoji</h3>
                <button class="icon-btn em-close" title="Close">✕</button>
            </div>
            <div class="em-list">loading…</div>
            <div class="dlg-buttons">
                <button class="em-add">Upload emoji…</button>
            </div>
        </div>`;
    const close = () => overlay.remove();
    overlay.querySelector(".em-close").onclick = close;
    overlay.onclick = (e) => { if (e.target === overlay) close(); };
    overlay.querySelector(".em-add").onclick = () => addEmoji(overlay);
    document.body.appendChild(overlay);
    renderEmojiList(overlay);
}

async function renderEmojiList(overlay) {
    const list = overlay.querySelector(".em-list");
    let resp;
    try {
        resp = await App().EmojiList();
    } catch (err) {
        list.textContent = "emoji list failed: " + err;
        return;
    }
    const emojis = resp.emojis || [];
    if (emojis.length === 0) {
        list.innerHTML = `<div class="empty-state">No custom emoji yet</div>`;
        return;
    }
    list.innerHTML = "";
    for (const e of emojis) {
        const row = document.createElement("div");
        row.className = "em-row";
        row.style.cssText = "display:flex;align-items:center;gap:8px;padding:4px 0";
        const img = document.createElement("img");
        img.style.cssText = "width:24px;height:24px;object-fit:contain";
        img.alt = e.name;
        App().EmojiGet(e.name)
            .then((d) => { if (d && d.data_base64) img.src = `data:${d.content_type};base64,${d.data_base64}`; })
            .catch(() => { /* preview is optional */ });
        const name = document.createElement("span");
        name.className = "mono";
        name.style.flex = "1";
        name.textContent = ":" + e.name + ":";
        const ren = document.createElement("button");
        ren.className = "icon-btn";
        ren.textContent = "✎";
        ren.title = "rename (messages already sent keep the old name)";
        ren.onclick = async () => {
            const next = prompt("New emoji name (a-z 0-9 _ -):", e.name);
            if (!next || next === e.name) return;
            const err = await App().EmojiRename(e.name, next);
            if (err) V().toast("rename failed: " + err, "warn");
            setTimeout(() => renderEmojiList(overlay), 400);
        };
        const del = document.createElement("button");
        del.className = "icon-btn";
        del.textContent = "🗑";
        del.title = "delete";
        del.onclick = async () => {
            if (!confirm(`Delete :${e.name}:?`)) return;
            const err = await App().EmojiDelete(e.name);
            if (err) V().toast("delete failed: " + err, "warn");
            setTimeout(() => renderEmojiList(overlay), 400);
        };
        row.append(img, name, ren, del);
        list.appendChild(row);
    }
}

async function addEmoji(overlay) {
    const name = prompt("Emoji name (a-z 0-9 _ -):");
    if (!name) return;
    const { pickIcon } = await import("./image-tools.js");
    const img = await pickIcon(128, 0.9);
    if (!img) return;
    const err = await App().EmojiUpload(name, img.dataBase64);
    if (err) {
        V().toast("upload failed: " + err, "warn");
        return;
    }
    setTimeout(() => renderEmojiList(overlay), 400);
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
            <button class="icon-btn fb-emoji" title="Custom emoji manager (272)">😀</button>
            <button class="icon-btn fb-banner" title="Set the server banner (admin, 270)">🖼</button>
            <button class="icon-btn fb-transfers" title="Transfers">⇅</button>
        </div>
        <div class="fb-quota hidden"><div class="fb-quota-fill"></div></div>
        <div class="fb-list"></div>`;

    pane.querySelector(".fb-refresh").onclick = refreshFiles;
    pane.querySelector(".fb-transfers").onclick = openTransfers;
    pane.querySelector(".fb-emoji").onclick = openEmojiManager;
    pane.querySelector(".fb-banner").onclick = setServerBanner;
    pane.querySelector(".fb-upload").onclick = async () => {
        // Native picker, not <input type=file>: it yields paths, which upload
        // by streaming instead of by base64 blob (259).
        const paths = await App().PickUploadPaths();
        if (paths && paths.length) queueUploadPaths(paths);
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

    // (270/271) branding changes announced by the server: drop the cached copy
    // so the next paint shows the new image instead of waiting for a reconnect.
    window.runtime.EventsOn("event", (json) => {
        let env;
        try {
            env = JSON.parse(json);
        } catch {
            return;
        }
        if (env.type === "server_banner_changed") {
            loadServerBanner();
        } else if (env.type === "channel_icon_changed") {
            const id = (env.data || {}).channel_id;
            if (!id) return;
            channelIcons.delete(id);
            document.querySelectorAll(`#channel-tree .channel[data-chid="${id}"] .ch-icon`)
                .forEach((el) => { delete el.dataset.iconFor; });
            decorateChannelIcons();
        }
    });
    watchChannelIcons();

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
