// chat-ui.js — wave-5b rich chat UI: markdown messages (91-93), link preview
// cards (94), emoji picker + custom server emoji (95), reactions (97), file
// paste/drop/upload + inline rendering (98-100), edit/delete (101/102),
// history scrollback (103), unread badges (104), mentions (106), reply-to
// (107), pins (109), search (110), topic/description header (111-113), read
// state (121), PM tabs + offline badge + receipts (122-124), export (125),
// display prefs (126-129), join/leave collapsing (131), announcement banner
// (132), MOTD (133), scroll lock (134) and the Ctrl+K quick switcher (135).
//
// Threads (108) are deliberately descoped: reply-chains (107) cover the
// basics; full thread panels are future work.
import { renderMarkdown, escapeHTML, EMOJI } from "./markdown.js";
import { playEvent } from "./sounds.js";

const V = () => window.__voicx;
const $ = (id) => document.getElementById(id);
const app = () => window.go.main.App;

const PAGE = 50; // history page size (103)

// ---------------------------------------------------------------------------
// Module state
// ---------------------------------------------------------------------------

// view: {kind:"channel"} (follows state.myChannelID) | {kind:"global"} |
// {kind:"dm", uid}. store key: "ch:<id>" ("ch:0" = global) or "dm:<uid>".
let view = { kind: "channel" };
const store = new Map(); // key -> {msgs, hasMore, end, loading, loaded}
const unread = new Map(); // channelID -> {n, mention}
const pmTabs = new Map(); // uid -> {uid, nick, unread, offline, pendingRead}
const receipts = new Map(); // clientMsgID -> "delivered" | "read"
const myReactions = new Map(); // messageID -> Set(emoji) I toggled this session.
// NOTE (97): history only carries reaction COUNTS, not who reacted, so the
// own-reaction highlight only knows about toggles from this session.

let atBottom = true;
let newCount = 0;
let replyTo = null; // message object being replied to (107)
let pendingFiles = []; // {name, dataBase64, isImage, dataURL} staged for send (98/100)
let lastDMTarget = ""; // last unique ID we sent a DM to (echo routing)
let pinsPanel = null; // open pins panel element (109)
let searchQ = ""; // live search filter (110)

// Custom server emoji (95): list + data-URL cache.
let customEmoji = [];
let customDirty = true;
const emojiURLs = new Map(); // name -> dataURL | "" (failed)

// Offline-DM toast batching (123).
const offlineBatch = new Map(); // uid -> {n, nick, timer}

// Persist debounce for settings writes (last-read, dismissed announcement).
let persistTimer = null;

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

function getStore(key) {
    if (!store.has(key)) {
        store.set(key, { msgs: [], hasMore: true, end: false, loading: false, loaded: false });
    }
    return store.get(key);
}

function activeKey() {
    const st = V().state;
    if (view.kind === "global") return "ch:0";
    if (view.kind === "dm") return "dm:" + view.uid;
    return "ch:" + (st.myChannelID || 0);
}

// normalize converts a live ChatBroadcast or a ChatHistoryEntry into the
// message shape the renderer uses.
function normalize(d, chID) {
    let ts = Date.now();
    if (typeof d.sent_at === "number") ts = d.sent_at < 1e12 ? d.sent_at * 1000 : d.sent_at;
    else if (typeof d.sent_at === "string" && d.sent_at) ts = Date.parse(d.sent_at) || ts;
    const channelID = chID !== undefined ? chID
        : (d.channel_id !== undefined && d.channel_id !== null && d.channel_id !== "" ? Number(d.channel_id) : null);
    return {
        id: Number(d.id) || 0,
        from: d.from ?? d.from_nickname ?? "?",
        fromUID: d.from_unique_id || "",
        text: d.text ?? d.body ?? "",
        ts,
        edited: !!(d.edited_at || d.edited),
        deleted: !!d.deleted,
        reactions: d.reactions || null,
        mentions: d.mentions || [],
        e2e: !!d.e2e,
        // enc_verified is set by the Go layer after it opened the body itself:
        // without it a history message would draw no badge at all and look
        // exactly like one the server handed over in the clear (91-135).
        enc: !!d.enc || !!d.enc_verified,
        offline: !!d.offline,
        clientMsgID: d.client_msg_id || "",
        channelID,
        self: false,
        mentioned: false,
    };
}

function findMsg(id) {
    for (const st of store.values()) {
        const m = st.msgs.find((x) => x.id === Number(id));
        if (m) return m;
    }
    return null;
}

function fmtTime(ts) {
    const mode = V().state.settings?.chat_timestamps || "absolute";
    if (mode === "off") return "";
    const d = new Date(ts);
    if (mode === "relative") {
        const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
        if (s < 60) return "now";
        if (s < 3600) return Math.floor(s / 60) + "m";
        if (s < 86400) return Math.floor(s / 3600) + "h";
        return Math.floor(s / 86400) + "d";
    }
    const hm = String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
    if (d.toDateString() === new Date().toDateString()) return hm;
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")} ${hm}`;
}

function fmtFull(ts) {
    const d = new Date(ts);
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")} ` +
        `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

// persistSettings saves state.settings (debounced) after silent mutations
// like last-read pointers (121) or a dismissed announcement (132).
function persistSettings() {
    if (persistTimer) clearTimeout(persistTimer);
    persistTimer = setTimeout(() => {
        persistTimer = null;
        const s = V().state.settings;
        if (s) app().SaveSettings(s);
    }, 1200);
}

function hashStr(s) {
    let h = 5381;
    for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) >>> 0;
    return h.toString(16);
}

// ---------------------------------------------------------------------------
// Custom server emoji (95)
// ---------------------------------------------------------------------------

async function ensureCustomEmoji() {
    if (!customDirty) return;
    customDirty = false;
    try {
        const r = await app().EmojiList();
        customEmoji = r.emojis || [];
    } catch {
        customEmoji = [];
    }
}

async function emojiURL(name) {
    if (emojiURLs.has(name)) return emojiURLs.get(name);
    emojiURLs.set(name, ""); // marks "loading/failed until proven otherwise"
    try {
        const d = await app().EmojiGet(name);
        const u = `data:${d.content_type};base64,${d.data_base64}`;
        emojiURLs.set(name, u);
        return u;
    } catch {
        return "";
    }
}

// applyCustomEmoji swaps :name: tokens for cached custom-emoji images. A name
// without a cached URL triggers a background load; the emoji shows up on the
// next render (messages render often enough that this is acceptable).
function applyCustomEmoji(html) {
    if (customDirty) ensureCustomEmoji();
    for (const e of customEmoji) {
        const url = emojiURLs.get(e.name);
        if (url) {
            html = html.split(":" + e.name + ":").join(
                `<img class="md-emoji" src="${url}" alt=":${escapeHTML(e.name)}:">`);
        } else if (url === undefined) {
            emojiURL(e.name);
        }
    }
    return html;
}

// ---------------------------------------------------------------------------
// Message rendering (91-93, 97, 99, 101/102, 106, 107)
// ---------------------------------------------------------------------------

// UNOPENED_BODIES are the exact strings the Go layer substitutes for a body it
// could not show. They must match client/e2e.go character for character — a
// drift silently downgrades the ⚠ badge back to a normal shield (91-135).
const UNOPENED_BODIES = new Set([
    "[encrypted message — key unavailable]",
    "[encrypted message — you do not have access]",
    "[refused: server sent plaintext history]",
    "[encrypted message — decryption failed]",
]);

// CHAT_ENCRYPTION_HELP is the single source for what the shield actually
// promises, so the channel info panel and the README cannot drift apart.
export const CHAT_ENCRYPTION_HELP =
    "Channel and global messages are encrypted with a key the server holds, so it can " +
    "apply moderation at send time but keeps no readable copy: history, pins and " +
    "attachments are stored sealed. Direct messages are end-to-end encrypted and the " +
    "server never holds their key. Search runs in this client over decrypted messages — " +
    "the server cannot match on content. Attachments carry their own key inside the " +
    "encrypted message body, so a file is exactly as private as the message that links it.";

function renderMsg(m) {
    const el = document.createElement("div");
    el.className = "msg rich";
    if (m.deleted) el.classList.add("deleted");
    if (m.mentioned) el.classList.add("mentioned");
    if (m.self) el.classList.add("own");
    if (m.id) el.dataset.msgId = m.id;
    el.dataset.ts = m.ts;

    const time = document.createElement("span");
    time.className = "msg-time mono";
    time.textContent = fmtTime(m.ts);
    time.title = fmtFull(m.ts);
    el.appendChild(time);

    const from = document.createElement("span");
    from.className = "msg-from" + (m.self ? " self" : "");
    from.textContent = m.from + ":";
    el.appendChild(from);

    const tag = document.createElement("span");
    tag.className = "msg-tag";
    tag.textContent = m.e2e ? (m.offline ? "dm · offline" : "dm") : (m.channelID ? "channel" : "global");
    el.appendChild(tag);

    // (4b/91-135) lock semantics. A placeholder body means the message stayed
    // sealed, which must not look like an ordinary encrypted message.
    const unopened = !m.deleted && UNOPENED_BODIES.has(m.text);
    if (unopened) el.classList.add("missing-key");
    if (m.e2e || m.enc || unopened) {
        const lock = document.createElement("span");
        lock.className = "msg-lock";
        if (unopened) {
            lock.textContent = "⚠";
            lock.title = "this message is still encrypted — its key is not available to this client";
        } else if (m.e2e) {
            lock.textContent = "🔒";
            lock.title = "end-to-end encrypted — only you and the other user can read this";
        } else {
            lock.textContent = "🛡";
            lock.title = "encrypted with this channel's key — stored encrypted. The server holds " +
                "the channel key so it can moderate at send time; it does not keep the text.";
        }
        el.appendChild(lock);
    }

    const body = document.createElement("span");
    body.className = "msg-text";
    if (m.deleted) {
        body.classList.add("tombstone");
        body.textContent = "message deleted";
    } else {
        renderBody(body, m);
    }
    el.appendChild(body);

    if (m.edited && !m.deleted) {
        const em = document.createElement("span");
        em.className = "msg-edited";
        em.textContent = "(edited)";
        el.appendChild(em);
    }

    // (124) delivery/read receipt tick on my outgoing DMs.
    if (m.self && m.e2e && m.clientMsgID) {
        const tick = document.createElement("span");
        tick.className = "msg-receipt";
        tick.dataset.cmid = m.clientMsgID;
        const r = receipts.get(m.clientMsgID);
        tick.textContent = r === "read" ? "✓✓" : r === "delivered" ? "✓" : "";
        if (r === "read") tick.classList.add("read");
        el.appendChild(tick);
    }

    if (!m.deleted && m.id) {
        el.appendChild(renderReacts(m));
        el.appendChild(renderActions(m));
    }
    return el;
}

// renderBody renders markdown text plus [file:<name>] attachment refs (99)
// and the leading reply quote (107). All user text passes through
// renderMarkdown (which escapes first); file names only ever go into
// textContent or DOM properties, never raw HTML.
function renderBody(container, m) {
    let text = m.text;
    let quoteNick = null;
    const qm = text.match(/^↪ ([^\n:]{1,64}): /);
    if (qm) {
        quoteNick = qm[1];
        text = text.slice(qm[0].length);
    }
    const parts = text.split(/\[file:([^\]]+)\]/);
    let html = "";
    for (let i = 0; i < parts.length; i += 2) html += renderMarkdown(parts[i]);
    container.innerHTML = applyCustomEmoji(html);
    if (quoteNick) {
        const q = document.createElement("span");
        q.className = "msg-quote";
        q.textContent = "↪ " + quoteNick + ":";
        q.title = "jump to the quoted message";
        container.insertBefore(q, container.firstChild);
    }
    for (let i = 1; i < parts.length; i += 2) attachFileRef(container, m, parts[i]);
}

const IMAGE_EXTS = ["png", "jpg", "jpeg", "gif", "webp"];

// parseFileRef splits a [file:<capture>] token into its three parts. It is
// total and never throws: zero or one separators is a legacy plain reference,
// which is what keeps pre-encryption messages rendering. Mirrors
// parseFileRef in client/chat.go.
export function parseFileRef(cap) {
    const i = cap.indexOf("#");
    if (i < 0) return { storage: cap, key: "", name: cap };       // legacy [file:photo.png]
    const j = cap.indexOf("#", i + 1);
    if (j < 0) return { storage: cap, key: "", name: cap };       // malformed -> treat as plain
    return { storage: cap.slice(0, i), key: cap.slice(i + 1, j), name: cap.slice(j + 1) };
}

// attachFileRef takes the RAW capture and parses it here rather than in
// renderBody, so the file key never crosses the split boundary. The key goes
// straight into the Go call: never into textContent, an attribute or a src.
function attachFileRef(container, m, cap) {
    const chID = m.channelID ?? V().state.myChannelID ?? 0;
    const ref = parseFileRef(cap);
    const name = ref.name;
    const ext = (name.split(".").pop() || "").toLowerCase();
    const wrap = document.createElement("span");
    wrap.className = "msg-file";
    if (IMAGE_EXTS.includes(ext)) {
        wrap.textContent = "loading image " + name + " …";
        app().DownloadChatAttachment(chID, ref.storage, ref.key).then((b64) => {
            wrap.textContent = "";
            if (!b64) {
                wrap.textContent = "📎 " + name + " (unavailable)";
                return;
            }
            const img = document.createElement("img");
            img.className = "msg-img";
            img.alt = name;
            img.title = name + " — click to zoom";
            img.src = `data:image/${ext === "jpg" ? "jpeg" : ext};base64,${b64}`;
            img.onclick = () => openLightbox(img.src);
            wrap.appendChild(img);
        }).catch(() => {
            wrap.textContent = "📎 " + name + " (download failed)";
        });
    } else {
        const b = document.createElement("button");
        b.className = "file-chip";
        b.textContent = "📎 " + name;
        b.title = "download " + name;
        b.onclick = async () => {
            try {
                const b64 = await app().DownloadChatAttachment(chID, ref.storage, ref.key);
                const a = document.createElement("a");
                a.href = "data:application/octet-stream;base64," + b64;
                a.download = name;
                a.click();
            } catch (e) {
                V().toast("download failed: " + e, "warn");
            }
        };
        wrap.appendChild(b);
    }
    container.appendChild(wrap);
}

function openLightbox(src) {
    const ov = document.createElement("div");
    ov.className = "dlg-overlay lightbox";
    const img = document.createElement("img");
    img.src = src;
    ov.appendChild(img);
    ov.onclick = () => ov.remove();
    document.body.appendChild(ov);
}

// --- reactions (97) -----------------------------------------------------------

const QUICK_REACTS = ["👍", "❤️", "😂", "😮", "😢", "🎉", "🔥", "👀"];

function renderReacts(m) {
    const row = document.createElement("span");
    row.className = "msg-reacts";
    const rx = m.reactions || {};
    for (const [emoji, count] of Object.entries(rx)) {
        if (!count) continue;
        const chip = document.createElement("button");
        chip.className = "react-chip";
        if (myReactions.get(m.id)?.has(emoji)) chip.classList.add("own");
        chip.textContent = `${emoji} ${count}`;
        chip.title = "toggle your reaction";
        chip.onclick = (e) => {
            e.stopPropagation();
            toggleReaction(m, emoji);
        };
        row.appendChild(chip);
    }
    return row;
}

async function toggleReaction(m, emoji) {
    const err = await app().ChatReact(m.id, emoji);
    if (err) {
        V().toast("reaction failed: " + err, "warn");
        return;
    }
    // Optimistic local toggle; the chat_reaction broadcast carries the new
    // counts and re-syncs (including the own-highlight via `by`).
    let set = myReactions.get(m.id);
    if (!set) {
        set = new Set();
        myReactions.set(m.id, set);
    }
    if (set.has(emoji)) set.delete(emoji);
    else set.add(emoji);
    const rx = { ...(m.reactions || {}) };
    rx[emoji] = Math.max(0, (rx[emoji] || 0) + (set.has(emoji) ? 1 : -1));
    m.reactions = rx;
    refreshReacts(m);
}

function refreshReacts(m) {
    const el = document.querySelector(`#chat-log .msg[data-msg-id="${m.id}"]`);
    if (!el) return;
    const old = el.querySelector(".msg-reacts");
    if (old) old.replaceWith(renderReacts(m));
}

let reactStrip = null;

function closeReactStrip() {
    if (reactStrip) {
        reactStrip.remove();
        reactStrip = null;
    }
}

function openReactStrip(m, anchorEl) {
    closeReactStrip();
    reactStrip = document.createElement("div");
    reactStrip.className = "react-strip";
    for (const emoji of QUICK_REACTS) {
        const b = document.createElement("button");
        b.textContent = emoji;
        b.onclick = () => {
            closeReactStrip();
            toggleReaction(m, emoji);
        };
        reactStrip.appendChild(b);
    }
    // Custom server emoji join the strip once loaded.
    ensureCustomEmoji().then(async () => {
        if (!reactStrip) return;
        for (const e of customEmoji.slice(0, 8)) {
            const url = await emojiURL(e.name);
            if (!url || !reactStrip) continue;
            const b = document.createElement("button");
            const img = document.createElement("img");
            img.src = url;
            img.alt = ":" + e.name + ":";
            b.appendChild(img);
            b.onclick = () => {
                closeReactStrip();
                toggleReaction(m, ":" + e.name + ":");
            };
            reactStrip.appendChild(b);
        }
    });
    const r = anchorEl.getBoundingClientRect();
    reactStrip.style.left = Math.min(r.left, window.innerWidth - 320) + "px";
    reactStrip.style.top = Math.max(8, r.top - 44) + "px";
    reactStrip.onclick = (e) => e.stopPropagation();
    document.body.appendChild(reactStrip);
}

// --- hover actions (97/101/102/107/109) --------------------------------------

function renderActions(m) {
    const acts = document.createElement("span");
    acts.className = "msg-actions";
    const mk = (label, title, fn) => {
        const b = document.createElement("button");
        b.textContent = label;
        b.title = title;
        b.onclick = (e) => {
            e.stopPropagation();
            fn();
        };
        acts.appendChild(b);
        return b;
    };
    mk("😊", "react", () => openReactStrip(m, acts));
    mk("↩", "reply", () => setReply(m));
    if (m.self && !m.e2e) mk("✎", "edit", () => startEdit(m));
    if (m.self) mk("🗑", "delete", () => deleteMsg(m));
    if (!m.e2e) mk("📌", "pin / unpin", () => pinMsg(m));
    return acts;
}

function refreshMsgEl(m) {
    const el = document.querySelector(`#chat-log .msg[data-msg-id="${m.id}"]`);
    if (el) el.replaceWith(renderMsg(m));
}

// --- edit / delete (101/102) ---------------------------------------------------

function startEdit(m) {
    const el = document.querySelector(`#chat-log .msg[data-msg-id="${m.id}"]`);
    if (!el) return;
    const body = el.querySelector(".msg-text");
    const input = document.createElement("input");
    input.className = "msg-edit-input";
    input.value = m.text;
    body.replaceWith(input);
    input.focus();
    input.setSelectionRange(input.value.length, input.value.length);
    input.onkeydown = async (e) => {
        e.stopPropagation();
        if (e.key === "Escape") refreshMsgEl(m);
        if (e.key !== "Enter") return;
        const t = input.value.trim();
        refreshMsgEl(m); // the chat_edited broadcast refreshes the text
        if (t && t !== m.text) {
            const err = await app().ChatEditMessage(m.channelID ?? 0, m.id, t);
            if (err) V().toast("edit failed: " + err, "warn");
        }
    };
    input.onblur = () => refreshMsgEl(m);
}

async function deleteMsg(m) {
    if (!confirm("Delete this message?")) return;
    const err = await app().ChatDeleteMessage(m.id);
    if (err) V().toast("delete failed: " + err, "warn");
    // The chat_deleted broadcast renders the tombstone.
}

async function pinMsg(m) {
    const err = await app().ChatPinMessage(m.channelID ?? 0, m.id, true);
    if (err) V().toast("pin failed: " + err, "warn"); // permission errors land here (109)
}

// --- reply-to (107) ------------------------------------------------------------
// The server protocol has no reply field, so a reply is sent as plain text
// prefixed "↪ <nick>: " — clients render that prefix as a clickable quote.

function setReply(m) {
    replyTo = m;
    const bar = $("reply-bar");
    bar.innerHTML = "";
    const label = document.createElement("span");
    label.className = "reply-label";
    label.textContent = "↪ " + m.from + ": " + m.text.slice(0, 80);
    const x = document.createElement("button");
    x.textContent = "✕";
    x.title = "cancel reply";
    x.onclick = clearReply;
    bar.appendChild(label);
    bar.appendChild(x);
    bar.classList.remove("hidden");
    $("chat-text").focus();
}

function clearReply() {
    replyTo = null;
    $("reply-bar").classList.add("hidden");
}

function scrollToQuote(quoteEl) {
    const msgEl = quoteEl.closest(".msg");
    const nick = quoteEl.textContent.replace(/^↪ /, "").replace(/:$/, "");
    const ts = Number(msgEl?.dataset.ts) || Infinity;
    const st = getStore(activeKey());
    // Best effort: the newest loaded message from that nick before this one.
    let target = null;
    for (const m of st.msgs) {
        if (m.from === nick && m.ts <= ts && m.id) target = m;
    }
    if (!target) {
        V().toast("quoted message not loaded", "info", "alert");
        return;
    }
    const el = document.querySelector(`#chat-log .msg[data-msg-id="${target.id}"]`);
    if (el) {
        el.scrollIntoView({ block: "center" });
        el.classList.add("flash");
        setTimeout(() => el.classList.remove("flash"), 1200);
    }
}

// ---------------------------------------------------------------------------
// Views, PM tabs (122) and rendering
// ---------------------------------------------------------------------------

function setView(v) {
    view = v;
    const key = activeKey();
    if (key.startsWith("ch:")) {
        const chID = Number(key.slice(3));
        if (unread.delete(chID)) V().renderTree();
    }
    if (v.kind === "dm") {
        const tab = pmTabs.get(v.uid);
        if (tab) {
            tab.unread = 0;
            tab.offline = false;
        }
    }
    renderTabs();
    renderView();
    sendPendingReads();
}

export function openPM(uid, nick) {
    if (!uid) return;
    if (!pmTabs.has(uid)) pmTabs.set(uid, { uid, nick: nick || uid, unread: 0, offline: false, pendingRead: "" });
    if (nick) pmTabs.get(uid).nick = nick;
    activatePM(uid);
}

function activatePM(uid) {
    // Keep the scope selector in sync so sendChat routes to the tab's user.
    $("chat-scope").value = "direct";
    $("chat-target").classList.remove("hidden");
    $("chat-target").value = uid;
    setView({ kind: "dm", uid });
}

function closePM(uid) {
    pmTabs.delete(uid);
    store.delete("dm:" + uid);
    if (view.kind === "dm" && view.uid === uid) {
        $("chat-scope").value = "channel";
        $("chat-target").classList.add("hidden");
        setView({ kind: "channel" });
        return;
    }
    renderTabs();
}

function renderTabs() {
    const bar = $("pm-tabs");
    bar.innerHTML = "";
    bar.classList.toggle("hidden", pmTabs.size === 0);
    for (const tab of pmTabs.values()) {
        const el = document.createElement("div");
        el.className = "pm-tab" + (view.kind === "dm" && view.uid === tab.uid ? " active" : "");
        const name = document.createElement("span");
        name.className = "pm-tab-name";
        name.textContent = tab.nick;
        el.appendChild(name);
        if (tab.offline) {
            const b = document.createElement("span");
            b.className = "pm-badge offline";
            b.textContent = "offline";
            b.title = "offline messages received (123)";
            el.appendChild(b);
        }
        if (tab.unread > 0) {
            const dot = document.createElement("span");
            dot.className = "pm-unread";
            dot.textContent = tab.unread;
            el.appendChild(dot);
        }
        const x = document.createElement("button");
        x.className = "pm-close";
        x.textContent = "✕";
        x.title = "close tab";
        x.onclick = (e) => {
            e.stopPropagation();
            closePM(tab.uid);
        };
        el.appendChild(x);
        el.onclick = () => activatePM(tab.uid);
        bar.appendChild(el);
    }
}

function scrollToBottom() {
    const log = $("chat-log");
    log.scrollTop = log.scrollHeight;
}

function resetNewCount() {
    newCount = 0;
    $("chat-newpill").classList.add("hidden");
}

function showNewPill() {
    const pill = $("chat-newpill");
    pill.textContent = `↓ ${newCount} new message${newCount === 1 ? "" : "s"}`;
    pill.classList.remove("hidden");
}

function renderView(keepScrollFrom) {
    const log = $("chat-log");
    log.innerHTML = "";
    const key = activeKey();
    const st = getStore(key);

    if (view.kind === "channel" && !V().state.myChannelID) {
        const hint = document.createElement("div");
        hint.className = "empty-state";
        hint.textContent = "Join a channel to see its chat — or pick global/direct below.";
        log.appendChild(hint);
        updateHeader();
        return;
    }

    if (view.kind !== "dm") {
        if (!st.loaded && !st.loading) {
            ensureHistory(key).then(() => {
                if (activeKey() === key) renderView();
            });
        }
        if (st.end && st.loaded && st.msgs.length) {
            const end = document.createElement("div");
            end.className = "chat-history-end";
            end.textContent = "— beginning of history —";
            log.appendChild(end);
        }
    }

    // (121) "— new messages —" divider at the first message newer than the
    // persisted last-read pointer (fresh loads of a channel view).
    const lastRead = lastReadFor(key);
    let dividerPlaced = false;

    for (const m of st.msgs) {
        if (!dividerPlaced && lastRead !== null && m.id > lastRead && !m.self) {
            const div = document.createElement("div");
            div.className = "chat-new-divider";
            div.textContent = "— new messages —";
            log.appendChild(div);
            dividerPlaced = true;
        }
        log.appendChild(renderMsg(m));
    }

    applySearchFilter();
    updateHeader();

    if (keepScrollFrom !== undefined) {
        log.scrollTop = log.scrollHeight - keepScrollFrom;
    } else {
        scrollToBottom();
        resetNewCount();
    }
    markRead(key);
}

function lastReadFor(key) {
    if (!key.startsWith("ch:")) return null;
    const lr = V().state.settings?.last_read_channels;
    if (!lr) return null;
    const v = lr[key.slice(3)];
    return v ? Number(v) : null;
}

function markRead(key) {
    if (!key.startsWith("ch:")) return;
    const st = getStore(key);
    const newest = st.msgs.length ? st.msgs[st.msgs.length - 1].id : 0;
    if (!newest) return;
    const s = V().state.settings;
    if (!s) return;
    s.last_read_channels = s.last_read_channels || {};
    if ((s.last_read_channels[key.slice(3)] || 0) >= newest) return;
    s.last_read_channels[key.slice(3)] = newest;
    persistSettings();
}

// ---------------------------------------------------------------------------
// History scrollback (103)
// ---------------------------------------------------------------------------

async function ensureHistory(key) {
    const st = getStore(key);
    if (st.loaded || st.loading || !key.startsWith("ch:")) return;
    st.loading = true;
    try {
        await loadPage(key, 0);
        st.loaded = true;
    } catch {
        // History unavailable (old server / no permission) — live chat works.
        st.loaded = true;
        st.end = true;
    } finally {
        st.loading = false;
    }
}

async function loadPage(key, beforeID) {
    const st = getStore(key);
    const chID = Number(key.slice(3));
    const resp = await app().ChatHistory(chID, beforeID, PAGE);
    const page = (resp.messages || []).map((x) => normalize(x, chID)).reverse(); // newest-first → oldest-first
    if (beforeID === 0) {
        st.msgs = page;
    } else {
        st.msgs = page.concat(st.msgs);
    }
    st.end = page.length < PAGE;
    return page.length;
}

async function maybeLoadOlder() {
    if (view.kind === "dm") return;
    const key = activeKey();
    const st = getStore(key);
    if (st.loading || st.end || !st.loaded || !st.msgs.length) return;
    st.loading = true;
    const log = $("chat-log");
    const spinner = document.createElement("div");
    spinner.className = "chat-spinner";
    spinner.textContent = "loading older messages…";
    log.insertBefore(spinner, log.firstChild);
    const prevH = log.scrollHeight;
    try {
        await loadPage(key, st.msgs[0].id);
    } catch { /* keep what we have */ }
    st.loading = false;
    if (activeKey() !== key) return;
    // Anchor the viewport: content was prepended, so the old scrollHeight
    // as "distance from the new bottom" lands back on the same message.
    renderView(prevH);
}

// ---------------------------------------------------------------------------
// Live incoming chat (addChat entry point, called from main.js)
// ---------------------------------------------------------------------------

export function addChat(d) {
    const st = V().state;
    const m = normalize(d);
    m.self = m.fromUID ? m.fromUID === st.myUniqueID : m.from === st.myNickname;

    if (m.e2e) {
        routeDM(d, m);
        return;
    }

    const chID = m.channelID ?? 0;
    const key = "ch:" + chID;
    if (m.mentions.includes(st.myUniqueID) && !m.self) {
        m.mentioned = true; // (106) accent highlight
        window.__voicxNotify?.notify("mention", (m.from || "someone") + ": " + (m.text || "").slice(0, 80),
            { channelID: m.channelID, className: "mentions", kind: "warn" });
    } else if (!m.self) {
        // (388) keyword highlights: whole-word match gets mention treatment.
        const kw = window.__voicxNotify?.matchKeyword(m.text || "");
        if (kw) {
            m.mentioned = true;
            window.__voicxNotify?.notify("keyword", `[${kw}] ` + (m.from || "someone") + ": " + (m.text || "").slice(0, 80),
                { channelID: m.channelID, className: "mentions", kind: "warn" });
        }
    }
    pushMsg(key, m);
    if (key === activeKey()) {
        appendLive(m);
    } else if (!m.self) {
        // (104) unread badge for a non-selected channel.
        const u = unread.get(chID) || { n: 0, mention: false };
        u.n++;
        u.mention = u.mention || m.mentioned;
        unread.set(chID, u);
        V().renderTree();
    }
}

function pushMsg(key, m) {
    const st = getStore(key);
    st.msgs.push(m);
    const max = V().state.settings?.chat_max_lines || 200;
    while (st.msgs.length > Math.max(max, PAGE)) st.msgs.shift();
}

function appendLive(m) {
    const log = $("chat-log");
    log.appendChild(renderMsg(m));
    const max = V().state.settings?.chat_max_lines || 200;
    while (log.children.length > max) log.firstChild.remove();
    if (searchQ) applySearchFilter();
    if (atBottom) {
        scrollToBottom();
        markRead(activeKey());
    } else {
        newCount++;
        showNewPill(); // (134) scroll lock: user is reading scrollback
    }
}

// --- DM routing, tabs, offline badge, receipts (122-124) -----------------------

function routeDM(d, m) {
    const st = V().state;
    // Peer: incoming → sender; own echo → the user we last DMed (the
    // broadcast carries no to_unique_id client-side).
    const peer = m.self ? (lastDMTarget || (view.kind === "dm" ? view.uid : "")) : (m.fromUID || "");
    if (!peer) return;
    const tab = pmTabs.get(peer) || { uid: peer, nick: m.self ? peer : m.from, unread: 0, offline: false, pendingRead: "" };
    if (!pmTabs.has(peer)) pmTabs.set(peer, tab);
    if (!m.self) tab.nick = m.from;

    const key = "dm:" + peer;
    pushMsg(key, m);

    if (m.offline && !m.self) {
        // (123) offline-spooled DM: tab badge + batched toast.
        tab.offline = true;
        const b = offlineBatch.get(peer) || { n: 0, nick: m.from, timer: null };
        b.n++;
        if (b.timer) clearTimeout(b.timer);
        b.timer = setTimeout(() => {
            offlineBatch.delete(peer);
            V().toast(`${b.n} offline message${b.n === 1 ? "" : "s"} from ${b.nick}`, "info", "alert");
        }, 700);
        offlineBatch.set(peer, b);
    }

    if (!m.self && m.clientMsgID) {
        // (124) delivery receipt immediately; read receipt when the tab is
        // visible and focused (otherwise queued for activation/focus).
        app().SendChatDelivered(m.fromUID, m.clientMsgID);
        if (view.kind === "dm" && view.uid === peer && document.hasFocus()) {
            app().SendChatRead(m.fromUID, m.clientMsgID);
        } else {
            tab.pendingRead = m.clientMsgID;
        }
    }

    if (key === activeKey()) {
        appendLive(m);
    } else if (!m.self) {
        tab.unread++;
    }
    renderTabs();
}

function sendPendingReads() {
    if (view.kind !== "dm" || !document.hasFocus()) return;
    const tab = pmTabs.get(view.uid);
    if (tab?.pendingRead) {
        app().SendChatRead(tab.uid, tab.pendingRead);
        tab.pendingRead = "";
    }
}

export function onDelivered(d) {
    if (d.client_msg_id) {
        receipts.set(d.client_msg_id, "delivered");
        updateReceiptTick(d.client_msg_id);
    }
}

export function onRead(d) {
    if (d.client_msg_id) {
        receipts.set(d.client_msg_id, "read");
        updateReceiptTick(d.client_msg_id);
    }
}

function updateReceiptTick(cmid) {
    const r = receipts.get(cmid);
    document.querySelectorAll(`#chat-log .msg-receipt[data-cmid="${CSS.escape(cmid)}"]`).forEach((el) => {
        el.textContent = r === "read" ? "✓✓" : "✓";
        el.classList.toggle("read", r === "read");
    });
}

// ---------------------------------------------------------------------------
// Live edit/delete/pin/reaction events
// ---------------------------------------------------------------------------

export function onChatEdited(d) {
    // The Go layer unseals chat_edited before emitting it, so the new body is
    // on the event itself: no history refetch, and an edit further back than
    // one page is covered like any other (91-135).
    const m = findMsg(d.message_id);
    if (!m) return;
    m.text = d.body ?? "";
    m.edited = true;
    refreshMsgEl(m);
}

export function onChatDeleted(d) {
    const m = findMsg(d.message_id);
    if (!m) return;
    m.deleted = true;
    m.text = "";
    refreshMsgEl(m); // tombstone style (102)
}

export function onChatPinned(d, pinned) {
    V().sysMsg(pinned ? "a message was pinned" : "a message was unpinned");
    if (pinsPanel) loadPinsPanel(); // refresh the open panel (109)
}

export function onChatReaction(d) {
    const m = findMsg(d.message_id);
    if (!m) return;
    // The event carries the authoritative counts; the own-highlight only
    // tracks toggles made in this session (history has counts only, 97).
    m.reactions = d.reactions || {};
    refreshReacts(m);
}

export function onEmojiAdded() {
    customDirty = true; // re-list on next use (95)
}

// ---------------------------------------------------------------------------
// Sending (reply prefix 107, file uploads 98/100)
// ---------------------------------------------------------------------------

export async function sendMessage() {
    const st = V().state;
    const input = $("chat-text");
    let text = input.value.trim();
    const files = pendingFiles.splice(0);
    renderFilePreview();
    if (!text && files.length === 0) return;

    const scope = $("chat-scope").value;
    let target = "";
    if (scope === "channel") target = String(st.myChannelID || "");
    if (scope === "direct") {
        target = $("chat-target").value.trim();
        lastDMTarget = target;
        if (target && !pmTabs.has(target)) openPMKeepView(target);
    }

    // Files are sealed with their own key and uploaded under a content-derived
    // name; the returned token carries that key inside the encrypted message
    // body, so the attachment is as private as the message (91-135).
    const chID = scope === "channel" ? st.myChannelID : 0;
    for (const f of files) {
        let token;
        try {
            token = await app().UploadChatAttachment(chID, f.name, f.dataBase64);
        } catch (e) {
            V().sysMsg("upload failed: " + e);
            continue;
        }
        await app().SendChat(scope, target, token);
    }

    if (text) {
        if (replyTo) {
            text = `↪ ${replyTo.from}: ${text}`;
            clearReply();
        }
        const err = await app().SendChat(scope, target, text);
        if (err) V().sysMsg("chat failed: " + err);
    }
    input.value = "";
}

function openPMKeepView(uid) {
    const c = V().state.clients.find((x) => x.unique_id === uid);
    pmTabs.set(uid, { uid, nick: c?.nickname || uid, unread: 0, offline: false, pendingRead: "" });
    renderTabs();
}

// --- file staging: paste / drop (98/100) ---------------------------------------

function stageFiles(fileList) {
    for (const f of fileList) {
        const reader = new FileReader();
        reader.onload = () => {
            const dataURL = reader.result;
            pendingFiles.push({
                // ']' would break the [file:<name>] wire syntax.
                name: (f.name || "paste.bin").replace(/[\[\]]/g, "_"),
                dataBase64: dataURL.split(",")[1] || "",
                isImage: f.type.startsWith("image/"),
                dataURL: f.type.startsWith("image/") ? dataURL : null,
            });
            renderFilePreview();
        };
        reader.readAsDataURL(f);
    }
}

function renderFilePreview() {
    const row = $("file-preview-row");
    row.innerHTML = "";
    row.classList.toggle("hidden", pendingFiles.length === 0);
    pendingFiles.forEach((f, i) => {
        const item = document.createElement("div");
        item.className = "file-preview";
        if (f.dataURL) {
            const img = document.createElement("img");
            img.src = f.dataURL;
            img.alt = f.name;
            item.appendChild(img);
        } else {
            const chip = document.createElement("span");
            chip.className = "file-chip";
            chip.textContent = "📎 " + f.name;
            item.appendChild(chip);
        }
        const x = document.createElement("button");
        x.textContent = "✕";
        x.title = "remove";
        x.onclick = () => {
            pendingFiles.splice(i, 1);
            renderFilePreview();
        };
        item.appendChild(x);
        row.appendChild(item);
    });
}

// ---------------------------------------------------------------------------
// Emoji picker (95)
// ---------------------------------------------------------------------------

const EMOJI_CATS = [
    ["Smileys", ["😄", "😃", "😁", "😆", "😅", "😂", "🤣", "😉", "😊", "😍", "😎", "🤔", "😐", "🙄", "😢", "😭", "😠", "🤬", "😱", "😴", "🤓", "😇", "🙃", "🫡"]],
    ["Gestures", ["👍", "👎", "👌", "✌️", "🤞", "👏", "🙌", "👋", "💪", "🙏", "🫶", "🤝"]],
    ["Hearts", ["❤️", "🧡", "💛", "💚", "💙", "💜", "🖤", "🤍", "💔", "💯", "💥"]],
    ["Objects", ["🔥", "✨", "⭐", "🎉", "🎊", "🚀", "💡", "🔒", "🎙️", "🔊", "💻", "🐛", "🍕", "☕", "🍺"]],
    ["Symbols", ["✅", "❌", "⚠️", "❓", "❗", "💤", "🔔", "🔕", "⬆️", "⬇️"]],
];

let emojiPanel = null;

function closeEmojiPanel() {
    if (emojiPanel) {
        emojiPanel.remove();
        emojiPanel = null;
    }
}

function insertAtCursor(text) {
    const input = $("chat-text");
    const pos = input.selectionStart ?? input.value.length;
    input.value = input.value.slice(0, pos) + text + input.value.slice(input.selectionEnd ?? pos);
    input.selectionStart = input.selectionEnd = pos + text.length;
    input.focus();
}

function toggleEmojiPanel() {
    if (emojiPanel) {
        closeEmojiPanel();
        return;
    }
    emojiPanel = document.createElement("div");
    emojiPanel.className = "emoji-panel";
    emojiPanel.onclick = (e) => e.stopPropagation();

    for (const [cat, list] of EMOJI_CATS) {
        const head = document.createElement("div");
        head.className = "emoji-cat";
        head.textContent = cat;
        emojiPanel.appendChild(head);
        const grid = document.createElement("div");
        grid.className = "emoji-grid";
        for (const e of list) {
            const b = document.createElement("button");
            b.textContent = e;
            const code = Object.entries(EMOJI).find(([, v]) => v === e)?.[0];
            if (code) b.title = ":" + code + ":";
            b.onclick = () => insertAtCursor(e);
            grid.appendChild(b);
        }
        emojiPanel.appendChild(grid);
    }

    // Custom server emoji, cached as data URLs.
    const head = document.createElement("div");
    head.className = "emoji-cat";
    head.textContent = "Server emoji";
    emojiPanel.appendChild(head);
    const grid = document.createElement("div");
    grid.className = "emoji-grid";
    emojiPanel.appendChild(grid);
    ensureCustomEmoji().then(async () => {
        if (!emojiPanel) return;
        if (customEmoji.length === 0) {
            const hint = document.createElement("div");
            hint.className = "set-hint";
            hint.textContent = "none on this server";
            grid.appendChild(hint);
            return;
        }
        for (const e of customEmoji) {
            const url = await emojiURL(e.name);
            if (!url || !emojiPanel) continue;
            const b = document.createElement("button");
            b.title = ":" + e.name + ":";
            const img = document.createElement("img");
            img.src = url;
            img.alt = ":" + e.name + ":";
            b.appendChild(img);
            b.onclick = () => insertAtCursor(":" + e.name + ":");
            grid.appendChild(b);
        }
    });

    const r = $("chat-emoji").getBoundingClientRect();
    emojiPanel.style.right = Math.max(8, window.innerWidth - r.right) + "px";
    emojiPanel.style.bottom = (window.innerHeight - r.top + 8) + "px";
    document.body.appendChild(emojiPanel);
}

// ---------------------------------------------------------------------------
// Link preview cards (94)
// ---------------------------------------------------------------------------
// LIMITATION: the card tries fetch(url) to scrape the <title>, but CORS
// blocks almost every cross-origin page in the webview, so in practice this
// nearly always falls back to a domain-only card. No external preview
// services are used (privacy).

const linkCards = new Map(); // url -> {title: string | null}
let linkCardEl = null;
let linkCardTimer = null;

function closeLinkCard() {
    if (linkCardTimer) {
        clearTimeout(linkCardTimer);
        linkCardTimer = null;
    }
    if (linkCardEl) {
        linkCardEl.remove();
        linkCardEl = null;
    }
}

async function linkPreview(url) {
    if (linkCards.has(url)) return linkCards.get(url);
    let card = { title: null };
    try {
        const resp = await fetch(url);
        const html = await resp.text();
        const m = html.match(/<title[^>]*>([^<]*)<\/title>/i);
        if (m) card.title = m[1].trim().slice(0, 120);
    } catch { /* CORS etc. — domain-only card */ }
    linkCards.set(url, card);
    return card;
}

function scheduleLinkCard(linkEl) {
    closeLinkCard();
    const url = linkEl.getAttribute("data-url");
    if (!url) return;
    linkCardTimer = setTimeout(async () => {
        let domain = url;
        try {
            domain = new URL(url).host;
        } catch { /* show the raw URL */ }
        const card = await linkPreview(url);
        linkCardEl = document.createElement("div");
        linkCardEl.className = "link-card";
        const t = document.createElement("div");
        t.className = "link-card-title";
        t.textContent = card.title || domain;
        const d = document.createElement("div");
        d.className = "link-card-domain";
        d.textContent = card.title ? domain : url;
        linkCardEl.appendChild(t);
        linkCardEl.appendChild(d);
        const r = linkEl.getBoundingClientRect();
        linkCardEl.style.left = Math.min(r.left, window.innerWidth - 300) + "px";
        linkCardEl.style.top = Math.max(8, r.top - 64) + "px";
        document.body.appendChild(linkCardEl);
    }, 350);
}

// ---------------------------------------------------------------------------
// Pins panel (109)
// ---------------------------------------------------------------------------

function closePinsPanel() {
    if (pinsPanel) {
        pinsPanel.remove();
        pinsPanel = null;
    }
}

async function togglePinsPanel() {
    if (pinsPanel) {
        closePinsPanel();
        return;
    }
    if (view.kind === "dm") return;
    pinsPanel = document.createElement("div");
    pinsPanel.className = "chat-pop pins-panel";
    $("chat-wrap").appendChild(pinsPanel);
    await loadPinsPanel();
}

async function loadPinsPanel() {
    if (!pinsPanel) return;
    const chID = view.kind === "global" ? 0 : V().state.myChannelID;
    pinsPanel.innerHTML = "";
    const head = document.createElement("div");
    head.className = "chat-pop-head";
    head.textContent = "Pinned messages";
    const x = document.createElement("button");
    x.textContent = "✕";
    x.onclick = closePinsPanel;
    head.appendChild(x);
    pinsPanel.appendChild(head);
    let pins = [];
    try {
        const resp = await app().ChatPins(chID);
        pins = resp.pins || [];
    } catch (e) {
        const err = document.createElement("div");
        err.className = "set-hint warn";
        err.textContent = "pins unavailable: " + e;
        pinsPanel.appendChild(err);
        return;
    }
    if (pins.length === 0) {
        const hint = document.createElement("div");
        hint.className = "set-hint";
        hint.textContent = "Nothing pinned yet — hover a message and hit 📌.";
        pinsPanel.appendChild(hint);
        return;
    }
    for (const p of pins) {
        const row = document.createElement("div");
        row.className = "pin-row";
        const body = document.createElement("span");
        body.className = "pin-body";
        const msg = p.message || {};
        body.textContent = `${msg.from_nickname || "?"}: ${msg.deleted ? "(deleted)" : (msg.body || "").slice(0, 120)}`;
        row.appendChild(body);
        const jump = document.createElement("button");
        jump.textContent = "↩";
        jump.title = "jump to message";
        jump.onclick = () => {
            const el = document.querySelector(`#chat-log .msg[data-msg-id="${p.message_id}"]`);
            if (el) {
                el.scrollIntoView({ block: "center" });
                el.classList.add("flash");
                setTimeout(() => el.classList.remove("flash"), 1200);
            } else {
                V().toast("message not loaded — scroll up to load older history", "info", "alert");
            }
        };
        row.appendChild(jump);
        const unpin = document.createElement("button");
        unpin.textContent = "✕";
        unpin.title = "unpin";
        unpin.onclick = async () => {
            const err = await app().ChatPinMessage(chID, p.message_id, false);
            if (err) V().toast("unpin failed: " + err, "warn");
            loadPinsPanel();
        };
        row.appendChild(unpin);
        pinsPanel.appendChild(row);
    }
}

// ---------------------------------------------------------------------------
// Search (110)
// ---------------------------------------------------------------------------

function openSearch() {
    $("chat-search-row").classList.remove("hidden");
    $("chat-search").focus();
}

function closeSearch() {
    $("chat-search-row").classList.add("hidden");
    $("chat-search").value = "";
    searchQ = "";
    applySearchFilter();
}

function applySearchFilter() {
    const log = $("chat-log");
    const q = searchQ.toLowerCase();
    clearMarks(log);
    log.querySelectorAll(".msg").forEach((el) => {
        const show = !q || el.textContent.toLowerCase().includes(q);
        el.classList.toggle("hidden", !show);
        if (show && q) {
            const body = el.querySelector(".msg-text");
            if (body) markIn(body, q);
        }
    });
}

function clearMarks(root) {
    root.querySelectorAll("mark").forEach((mk) => {
        const parent = mk.parentNode;
        parent.replaceChild(document.createTextNode(mk.textContent), mk);
        parent.normalize();
    });
}

function markIn(el, q) {
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    const nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    for (const node of nodes) {
        const idx = node.nodeValue.toLowerCase().indexOf(q);
        if (idx < 0) continue;
        const after = node.splitText(idx);
        after.splitText(q.length);
        const mark = document.createElement("mark");
        mark.textContent = after.nodeValue;
        after.parentNode.replaceChild(mark, after);
    }
}

// SEARCH_MAX mirrors the server's chat_search_max_messages default; the Go
// layer applies its own cap, this is only what we ask for.
const SEARCH_MAX = 2000;

async function searchServer() {
    const q = $("chat-search").value.trim();
    if (!q) return;
    if (view.kind === "dm") {
        V().toast("server search covers channel/global history only (DMs are E2E)", "info", "alert");
        return;
    }
    const chID = view.kind === "global" ? 0 : V().state.myChannelID;
    const btn = $("chat-search-server");
    btn.disabled = true;
    btn.textContent = "searching…";
    // The paging loop lives in Go now: the keys never cross into the webview
    // and a page can use the store's cap of 200 instead of the UI's 50 (110).
    const unsub = window.runtime.EventsOn("chatsearch:progress",
        (n) => { btn.textContent = `searching… ${n}`; });
    let res = { messages: [], undecryptable: 0 };
    try {
        res = await app().ChatSearch(chID, q, SEARCH_MAX);
    } catch (e) {
        V().toast("search failed: " + e, "warn");
    }
    if (unsub) unsub();
    btn.disabled = false;
    btn.textContent = "search server history";
    showSearchResults(q.toLowerCase(), res.messages || [], res.undecryptable || 0);
}

function showSearchResults(q, results, undecryptable) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const dlg = document.createElement("div");
    dlg.className = "dlg dlg-wide search-results";
    const h = document.createElement("h3");
    // A partial search must never look complete: messages under a generation
    // this client cannot obtain are counted, not silently dropped.
    h.textContent = undecryptable > 0
        ? `${results.length} matches (${undecryptable} messages could not be decrypted)`
        : `${results.length} matches`;
    dlg.appendChild(h);
    const list = document.createElement("div");
    list.className = "search-list";
    if (results.length === 0) {
        const hint = document.createElement("div");
        hint.className = "set-hint";
        hint.textContent = "no matches in the paged history";
        list.appendChild(hint);
    }
    for (const r of results) {
        const row = document.createElement("div");
        row.className = "search-hit";
        // Escape first, then wrap the match in <mark> (XSS discipline).
        const esc = escapeHTML((r.body || "").slice(0, 200));
        const eq = escapeHTML(q).toLowerCase();
        const idx = esc.toLowerCase().indexOf(eq);
        row.innerHTML = idx >= 0
            ? esc.slice(0, idx) + "<mark>" + esc.slice(idx, idx + eq.length) + "</mark>" + esc.slice(idx + eq.length)
            : esc;
        const meta = document.createElement("span");
        meta.className = "search-meta";
        meta.textContent = `${r.from_nickname || "?"} · ${fmtFull((typeof r.sent_at === "number" ? r.sent_at * 1000 : Date.parse(r.sent_at)) || 0)}`;
        row.insertBefore(meta, row.firstChild);
        row.onclick = () => {
            overlay.remove();
            const el = document.querySelector(`#chat-log .msg[data-msg-id="${r.id}"]`);
            if (el) {
                el.scrollIntoView({ block: "center" });
                el.classList.add("flash");
                setTimeout(() => el.classList.remove("flash"), 1200);
            } else {
                V().toast("message not in the loaded view", "info", "alert");
            }
        };
        list.appendChild(row);
    }
    dlg.appendChild(list);
    const btns = document.createElement("div");
    btns.className = "dlg-buttons";
    const ok = document.createElement("button");
    ok.className = "dlg-ok";
    ok.textContent = "Close";
    ok.onclick = () => overlay.remove();
    btns.appendChild(ok);
    dlg.appendChild(btns);
    overlay.appendChild(dlg);
    overlay.onclick = (e) => {
        if (e.target === overlay) overlay.remove();
    };
    document.body.appendChild(overlay);
}

// ---------------------------------------------------------------------------
// Header: topic (111), description (112/113), export (125)
// ---------------------------------------------------------------------------

export function refreshHeader() {
    updateHeader();
}

function updateHeader() {
    const st = V().state;
    let title = "Chat", topic = "";
    let showChanBtns = false;
    if (view.kind === "global") {
        title = "Global chat";
    } else if (view.kind === "dm") {
        title = "DM — " + (pmTabs.get(view.uid)?.nick || view.uid);
    } else {
        const ch = st.channels.find((c) => c.ChannelID === st.myChannelID);
        if (ch) {
            title = "# " + ch.Name;
            topic = ch.Topic || "";
            showChanBtns = true;
        }
    }
    $("chat-head-title").textContent = title;
    const topicEl = $("chat-topic");
    topicEl.textContent = topic;
    topicEl.title = topic; // (111) tooltip carries the full topic
    topicEl.classList.toggle("hidden", !topic);
    $("chat-pins-btn").classList.toggle("hidden", view.kind === "dm");
    $("chat-info-btn").classList.toggle("hidden", !showChanBtns);
}

function openDescription() {
    const st = V().state;
    const ch = st.channels.find((c) => c.ChannelID === st.myChannelID);
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const dlg = document.createElement("div");
    dlg.className = "dlg dlg-wide chan-desc";
    const h = document.createElement("h3");
    h.textContent = ch ? "# " + ch.Name : "Channel";
    dlg.appendChild(h);
    const body = document.createElement("div");
    body.className = "chan-desc-body";
    if (ch && ch.Description) {
        body.innerHTML = renderMarkdown(ch.Description); // escaped inside (112/113)
    } else {
        body.classList.add("set-hint");
        body.textContent = "This channel has no description.";
    }
    dlg.appendChild(body);
    // (91-135) one source for the encryption story, so the client and the
    // README cannot drift apart on what the shield actually promises.
    const encH = document.createElement("h4");
    encH.textContent = "How chat encryption works here";
    dlg.appendChild(encH);
    const enc = document.createElement("div");
    enc.className = "set-hint";
    enc.textContent = CHAT_ENCRYPTION_HELP;
    dlg.appendChild(enc);
    const btns = document.createElement("div");
    btns.className = "dlg-buttons";
    const ok = document.createElement("button");
    ok.className = "dlg-ok";
    ok.textContent = "Close";
    ok.onclick = () => overlay.remove();
    btns.appendChild(ok);
    dlg.appendChild(btns);
    overlay.appendChild(dlg);
    overlay.onclick = (e) => {
        if (e.target === overlay) overlay.remove();
    };
    document.body.appendChild(overlay);
}

// exportChat writes the loaded transcript out. Everything on screen is
// decrypted, so a plain export is the one place this client can undo the
// storage guarantee — it takes an explicit confirm, and the encrypted
// container is offered first (125).
async function exportChat() {
    const key = activeKey();
    const st = getStore(key);
    const lines = st.msgs.map((m) =>
        `[${fmtFull(m.ts)}] ${m.from}: ${m.deleted ? "(deleted)" : m.text}`);
    const contents = lines.join("\n") + "\n";
    const name = view.kind === "dm" ? "dm-" + view.uid.slice(0, 8)
        : view.kind === "global" ? "global"
        : (V().state.channels.find((c) => c.ChannelID === V().state.myChannelID)?.Name || "channel");

    const pass = await askExportPassphrase();
    if (pass === null) return; // cancelled
    if (pass !== "") {
        try {
            await app().ExportChatEncrypted(`voicx-${name}.voicxchat`, contents, pass);
        } catch (e) {
            V().toast("export failed: " + e, "warn");
        }
        return;
    }
    const err = await app().ExportChat(`voicx-${name}.txt`, contents);
    if (err) V().toast("export failed: " + err, "warn");
}

// askExportPassphrase resolves to a passphrase (encrypted export), "" (plain
// export, explicitly confirmed) or null (cancelled).
function askExportPassphrase() {
    return new Promise((resolve) => {
        const overlay = document.createElement("div");
        overlay.className = "dlg-overlay";
        const dlg = document.createElement("div");
        dlg.className = "dlg";
        const h = document.createElement("h3");
        h.textContent = "Export chat";
        dlg.appendChild(h);
        const warn = document.createElement("div");
        warn.className = "set-hint warn";
        warn.textContent = "A plain export writes DECRYPTED messages to a file on your disk. " +
            "Enter a passphrase to write an encrypted .voicxchat instead.";
        dlg.appendChild(warn);
        const inp = document.createElement("input");
        inp.type = "password";
        inp.placeholder = "passphrase (leave empty for a plain export)";
        dlg.appendChild(inp);
        const btns = document.createElement("div");
        btns.className = "dlg-buttons";
        const done = (v) => { overlay.remove(); resolve(v); };
        const cancel = document.createElement("button");
        cancel.textContent = "Cancel";
        cancel.onclick = () => done(null);
        const ok = document.createElement("button");
        ok.className = "dlg-ok";
        ok.textContent = "Export";
        ok.onclick = () => {
            const p = inp.value;
            if (p === "" && !confirm("Write an UNENCRYPTED copy of this chat to disk?")) return;
            done(p);
        };
        btns.appendChild(cancel);
        btns.appendChild(ok);
        dlg.appendChild(btns);
        overlay.appendChild(dlg);
        overlay.onclick = (e) => { if (e.target === overlay) done(null); };
        document.body.appendChild(overlay);
        inp.focus();
    });
}

// ---------------------------------------------------------------------------
// Announcement banner (132) + MOTD (133)
// ---------------------------------------------------------------------------

export function onAnnouncement(d) {
    const box = $("banner-box");
    box.innerHTML = "";
    const text = d.text || "";
    if (!text) return;
    // (385) announcements go through the notification matrix.
    window.__voicxNotify?.notify("announcement", "announcement: " + text.slice(0, 100), { className: "messages", kind: "warn" });
    const h = hashStr(text);
    const s = V().state.settings;
    if (s?.dismissed_announcement === h) return; // already dismissed
    const b = document.createElement("div");
    b.className = "announce-banner";
    const t = document.createElement("span");
    t.textContent = "📢 " + text;
    const x = document.createElement("button");
    x.textContent = "✕";
    x.title = "dismiss";
    x.onclick = () => {
        b.remove();
        if (s) {
            s.dismissed_announcement = h;
            persistSettings();
        }
    };
    b.appendChild(t);
    b.appendChild(x);
    box.appendChild(b);
}

export async function onConnect() {
    const st = V().state;
    // myUniqueID drives mention matching and own-message detection.
    try {
        st.myUniqueID = await app().IdentityUID();
    } catch { /* best-effort */ }
    // (133) surface the server MOTD once per connect. It is a server notice,
    // not a message: styling it as one would put it next to a shield or a lock
    // it has not earned.
    try {
        const motd = await app().MOTD();
        if (motd) V().sysMsg("server notice — " + motd);
    } catch { /* MOTD is best-effort */ }
}

export function onMyChannelChanged() {
    const st = V().state;
    if (unread.delete(st.myChannelID)) V().renderTree();
    if (view.kind === "channel") renderView();
    else updateHeader();
}

// resetView clears all per-server chat state (281 tab switch): message
// stores, unread badges, PM tabs, and the rendered panes. The tab journal
// replay rebuilds the view from server frames afterwards.
export function resetView() {
    view = { kind: "channel" };
    store.clear();
    unread.clear();
    pmTabs.clear();
    replyTo = null;
    pendingFiles = [];
    lastDMTarget = "";
    newCount = 0;
    jl.verb = null;
    jl.names = [];
    jl.el = null;
    $("chat-log").innerHTML = "";
    $("pm-tabs").innerHTML = "";
    $("pm-tabs").classList.add("hidden");
    $("reply-bar").classList.add("hidden");
    $("file-preview-row").classList.add("hidden");
    $("chat-head-title").textContent = "Chat";
    $("chat-topic").classList.add("hidden");
    renderTabs();
}

// ---------------------------------------------------------------------------
// Join/leave system lines with collapsing (130/131)
// ---------------------------------------------------------------------------

const jl = { verb: null, names: [], ts: 0, el: null };

export function sysJoinLeave(nick, verb) {
    const s = V().state.settings;
    if (s && s.sys_join_leave === false) return; // (130) category filter
    const now = Date.now();
    // (131) consecutive same-verb lines inside 60s collapse into one line.
    if (jl.verb === verb && jl.el && jl.el.isConnected && now - jl.ts < 60000) {
        jl.names.push(nick);
        jl.ts = now;
        jl.el.textContent = "— " + jl.names.join(", ") + " " + verb + " —";
        return;
    }
    jl.verb = verb;
    jl.names = [nick];
    jl.ts = now;
    const el = document.createElement("div");
    el.className = "msg sys";
    el.textContent = "— " + nick + " " + verb + " —";
    $("chat-log").appendChild(el);
    jl.el = el;
    if (atBottom) scrollToBottom();
}

// ---------------------------------------------------------------------------
// Display prefs (126-129)
// ---------------------------------------------------------------------------

export function applyChatPrefs() {
    const s = V().state.settings || {};
    const log = $("chat-log");
    log.classList.toggle("chat-compact", (s.chat_density || "comfortable") === "compact");
    log.classList.toggle("chat-bubbles", (s.chat_layout || "irc") === "bubbles");
    log.style.setProperty("--chat-font-size", (s.chat_font_size || 14) + "px");
    // Timestamps re-render live when the mode changes (126).
    log.querySelectorAll(".msg[data-ts]").forEach((el) => {
        const t = el.querySelector(".msg-time");
        if (t) t.textContent = fmtTime(Number(el.dataset.ts));
    });
}

// ---------------------------------------------------------------------------
// Quick switcher (135)
// ---------------------------------------------------------------------------

let qsOverlay = null;

function qsScore(q, s) {
    q = q.toLowerCase();
    s = s.toLowerCase();
    if (!q) return 0;
    const i = s.indexOf(q);
    if (i >= 0) return 100 - i; // substring beats subsequence
    let qi = 0;
    for (const ch of s) if (ch === q[qi]) qi++;
    return qi === q.length ? 50 - s.length * 0.05 : -1;
}

function closeQS() {
    if (qsOverlay) {
        qsOverlay.remove();
        qsOverlay = null;
    }
}

function openQS() {
    closeQS();
    const st = V().state;
    const items = [];
    for (const ch of st.channels) {
        items.push({ label: "# " + ch.Name, hint: "channel", action: () => app().JoinChannel(ch.ChannelID) });
    }
    for (const c of st.clients) {
        if (c.unique_id && c.unique_id !== st.myUniqueID) {
            items.push({ label: "@ " + (c.nickname || c.unique_id), hint: "user → PM", action: () => openPM(c.unique_id, c.nickname) });
        }
    }
    for (const tab of pmTabs.values()) {
        items.push({ label: "✉ " + tab.nick, hint: "PM tab", action: () => activatePM(tab.uid) });
    }

    qsOverlay = document.createElement("div");
    qsOverlay.className = "dlg-overlay qs-overlay";
    const box = document.createElement("div");
    box.className = "qs-box";
    const input = document.createElement("input");
    input.placeholder = "jump to channel, user or PM tab…";
    const list = document.createElement("div");
    list.className = "qs-list";
    box.appendChild(input);
    box.appendChild(list);
    qsOverlay.appendChild(box);
    qsOverlay.onclick = (e) => {
        if (e.target === qsOverlay) closeQS();
    };
    document.body.appendChild(qsOverlay);

    let sel = 0;
    let matches = [];
    const renderList = () => {
        const q = input.value.trim();
        matches = items
            .map((it) => ({ it, score: qsScore(q, it.label) }))
            .filter((x) => x.score >= 0)
            .sort((a, b) => b.score - a.score)
            .slice(0, 12)
            .map((x) => x.it);
        sel = Math.min(sel, Math.max(0, matches.length - 1));
        list.innerHTML = "";
        matches.forEach((it, i) => {
            const row = document.createElement("div");
            row.className = "qs-row" + (i === sel ? " sel" : "");
            const l = document.createElement("span");
            l.textContent = it.label;
            const h = document.createElement("span");
            h.className = "qs-hint";
            h.textContent = it.hint;
            row.appendChild(l);
            row.appendChild(h);
            row.onclick = () => {
                closeQS();
                it.action();
            };
            list.appendChild(row);
        });
    };
    input.oninput = () => {
        sel = 0;
        renderList();
    };
    input.onkeydown = (e) => {
        e.stopPropagation();
        if (e.key === "Escape") closeQS();
        if (e.key === "ArrowDown") {
            e.preventDefault();
            sel = Math.min(sel + 1, matches.length - 1);
            renderList();
        }
        if (e.key === "ArrowUp") {
            e.preventDefault();
            sel = Math.max(sel - 1, 0);
            renderList();
        }
        if (e.key === "Enter" && matches[sel]) {
            closeQS();
            matches[sel].action();
        }
    };
    renderList();
    input.focus();
}

// ---------------------------------------------------------------------------
// @nickname tab-completion in the chat input (106)
// ---------------------------------------------------------------------------

let tabCycle = null; // {start, base, matches, idx}

function handleTabComplete(e) {
    const input = $("chat-text");
    const pos = input.selectionStart ?? input.value.length;
    const before = input.value.slice(0, pos);
    const m = before.match(/@([\w-]*)$/);
    if (!m) {
        tabCycle = null;
        return;
    }
    const base = m[1].toLowerCase();
    const names = V().state.clients
        .filter((c) => c.client_id !== V().state.myClientID)
        .map((c) => c.nickname || c.unique_id)
        .filter((n) => n && n.toLowerCase().startsWith(base));
    if (names.length === 0) {
        tabCycle = null;
        return;
    }
    e.preventDefault();
    if (tabCycle && tabCycle.start === pos - m[0].length && tabCycle.base === base) {
        tabCycle.idx = (tabCycle.idx + 1) % tabCycle.matches.length; // cycle
    } else {
        tabCycle = { start: pos - m[0].length, base, matches: names, idx: 0 };
    }
    const pick = tabCycle.matches[tabCycle.idx];
    input.value = input.value.slice(0, tabCycle.start) + "@" + pick + " " + input.value.slice(pos);
    input.selectionStart = input.selectionEnd = tabCycle.start + pick.length + 2;
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

export function initChat() {
    const log = $("chat-log");

    applyChatPrefs();

    // Scope selector drives the active view (channel/global/direct).
    $("chat-scope").addEventListener("change", () => {
        const scope = $("chat-scope").value;
        if (scope === "global") setView({ kind: "global" });
        else if (scope === "channel") setView({ kind: "channel" });
        else {
            const uid = $("chat-target").value.trim();
            if (uid) openPM(uid);
        }
    });
    $("chat-target").addEventListener("change", () => {
        const uid = $("chat-target").value.trim();
        if ($("chat-scope").value === "direct" && uid) openPM(uid);
    });

    // (134) scroll lock + (103) scroll-up history paging.
    log.addEventListener("scroll", () => {
        atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 30;
        if (atBottom) {
            resetNewCount();
            markRead(activeKey());
        }
        if (log.scrollTop < 40) maybeLoadOlder();
    });
    $("chat-newpill").onclick = () => {
        scrollToBottom();
        resetNewCount();
        atBottom = true;
    };

    // Click delegation: external links (93), reply quotes (107).
    log.addEventListener("click", (e) => {
        const link = e.target.closest(".md-link");
        if (link) {
            e.preventDefault();
            const url = link.getAttribute("data-url");
            if (url) window.runtime.BrowserOpenURL(url);
            return;
        }
        const q = e.target.closest(".msg-quote");
        if (q) scrollToQuote(q);
    });
    // (94) hover preview cards.
    log.addEventListener("mouseover", (e) => {
        const link = e.target.closest(".md-link");
        if (link) scheduleLinkCard(link);
    });
    log.addEventListener("mouseout", (e) => {
        if (e.target.closest(".md-link")) closeLinkCard();
    });

    // Header buttons.
    $("chat-pins-btn").onclick = togglePinsPanel;
    $("chat-info-btn").onclick = openDescription;
    $("chat-export-btn").onclick = exportChat;
    $("chat-search-btn").onclick = openSearch;
    $("chat-search-close").onclick = closeSearch;
    $("chat-search").addEventListener("input", () => {
        searchQ = $("chat-search").value.trim();
        applySearchFilter();
    });
    $("chat-search").addEventListener("keydown", (e) => {
        e.stopPropagation();
        if (e.key === "Escape") closeSearch();
    });
    $("chat-search-server").onclick = searchServer;

    // Emoji picker (95).
    $("chat-emoji").onclick = (e) => {
        e.stopPropagation();
        toggleEmojiPanel();
    };
    document.addEventListener("click", () => {
        closeEmojiPanel();
        closeReactStrip();
    });

    // File paste + drop (98/100).
    $("chat-text").addEventListener("paste", (e) => {
        const files = [...(e.clipboardData?.files || [])];
        if (files.length) {
            e.preventDefault();
            stageFiles(files);
        }
    });
    const wrap = $("chat-wrap");
    wrap.addEventListener("dragover", (e) => e.preventDefault());
    wrap.addEventListener("drop", (e) => {
        e.preventDefault();
        if (e.dataTransfer?.files?.length) stageFiles(e.dataTransfer.files);
    });

    // @nick completion (106).
    $("chat-text").addEventListener("keydown", (e) => {
        if (e.key === "Tab") handleTabComplete(e);
    });

    // Read receipts need a focused window (124).
    window.addEventListener("focus", sendPendingReads);

    // Ctrl+F search (110), Ctrl+K quick switcher (135).
    document.addEventListener("keydown", (e) => {
        if (e.ctrlKey && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "f") {
            e.preventDefault();
            openSearch();
        }
        if (e.ctrlKey && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "k") {
            e.preventDefault();
            openQS();
        }
    });

    // Share helpers through the module namespace (clientinfo.js pattern).
    Object.assign(window.__voicx, {
        openPM,
        applyChatPrefs,
        chatUnread: (chID) => unread.get(chID) || null,
        refreshHeader,
    });

    renderTabs();
    renderView();
}

export function unreadFor(chID) {
    return unread.get(chID) || null;
}
