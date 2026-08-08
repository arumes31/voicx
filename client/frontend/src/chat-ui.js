// chat-ui.js — wave-5b rich chat UI: markdown messages (91-93), link preview
// cards (94), emoji picker + custom server emoji (95/96), reactions (97), file
// paste/drop/upload + inline rendering (98-100), edit/delete (101/102),
// history scrollback (103), unread badges (104), mentions (105/106), reply-to
// (107), thread panels (108), pins (109), search (110), topic/description
// header (111-113), typing indicators (120), read state (121), PM tabs +
// offline badge + receipts (122-124), export (125), display prefs (126-129),
// join/leave collapsing (131), announcement banner (132), MOTD (133), scroll
// lock (134) and the Ctrl+K quick switcher (135).
//
// Threads prefer the protocol reply_to_id field. The legacy quote-prefix
// resolver remains for history written by older clients.
import { renderMarkdown, escapeHTML, EMOJI } from "./markdown.js";
import { playEvent } from "./sounds.js";
import { pickIcon } from "./image-tools.js";
import { closeDialog, isCurrentServerDialog, mountServerDialog } from "./modal.js";
import { imageDataURL } from "./safe-media.js";

const V = () => window.__voicx;
const $ = (id) => document.getElementById(id);
const app = () => window.go.main.App;

const PAGE = 50; // history page size (103)

// ---------------------------------------------------------------------------
// Module state
// ---------------------------------------------------------------------------

// view: {kind:"channel"} (follows state.myChannelID) | {kind:"global"} |
// {kind:"dm", uid} | {kind:"chan", id} (a subscribed channel I am not in,
// 312/311). store key: "ch:<id>" ("ch:0" = global) or "dm:<uid>".
let view = { kind: "channel" };
// subscriptions is the server's authoritative set (312): every id it lists,
// including the channel I stand in, which the server adds implicitly. It is
// replaced wholesale, never merged, so the client cannot accumulate drift.
let subscriptions = [];
const store = new Map(); // key -> {msgs, hasMore, end, loading, loaded}
const unread = new Map(); // channelID -> {n, mention}
const pmTabs = new Map(); // uid -> {uid, nick, unread, offline, pendingRead}
const chanTabs = new Map(); // channelID -> {id}; derived from SubscriptionState
let pendingChannelTab = 0; // activate only after the server confirms subscription
const receipts = new Map(); // clientMsgID -> "delivered" | "read"
const myReactions = new Map(); // messageID -> Set(emoji) I toggled this session.
// NOTE (97): history only carries reaction COUNTS, not who reacted, so the
// own-reaction highlight only knows about toggles from this session.

let atBottom = true;
let newCount = 0;
let replyTo = null; // message object being replied to (107)
let pendingFiles = []; // {name, dataBase64, isImage, dataURL} staged for send (98/100)
let lastDMTarget = ""; // last unique ID we sent a DM to (echo routing)
let emojiPanel = null; // open emoji panel element (95)
let pinsPanel = null; // open pins panel element (109)
let searchQ = ""; // live search filter (110)
let threadPanel = null; // open thread panel element (108)
let threadRootID = 0; // root message id the thread panel is showing (108)

// Pinned message ids per store key (109). Loaded once per scope alongside
// history so the 📌 hover button can actually toggle instead of only pinning.
const pinnedIDs = new Map(); // store key -> Set(messageID)

// Typing indicators (120): store key -> Map(uniqueID -> {nick, expires}).
const typers = new Map();
let typingTimer = null; // single expiry sweep for every scope
let typingSentAt = 0; // last outgoing ping (throttle)
let typingScope = ""; // scope of that ping, so a scope switch re-pings at once
let typingIdle = null; // idle timer that ends the local composing session
let typingDebounce = null;
const sentHistory = [];
let sentHistoryPos = 0;
let sentHistoryDraft = "";

// Custom server emoji (95): list + data-URL cache.
let customEmoji = [];
let customDirty = true;
const emojiURLs = new Map(); // name -> dataURL | "" (failed)
const builtInEmoji = [...new Set(Object.values(EMOJI))];
const EMOJI_CATS = [
    ["Faces", builtInEmoji.slice(0, 30)],
    ["Reactions", builtInEmoji.slice(30, 49)],
    ["Objects & symbols", builtInEmoji.slice(49)],
];

// Offline-DM toast batching (123).
const offlineBatch = new Map(); // uid -> {n, nick, timer}
const reconnectAnnouncementBatches = new Map(); // scope key -> {n, names, event, ctx, timer}
let reconnectAnnouncementUntil = 0;

// Local DM history (122). The sealed on-disk log is a DM's ONLY history — the
// server is E2EE-blind and stores nothing — so a peer's log is replayed into
// its store once per session, and a write failure is reported once rather than
// per message.
let dmPersistWarned = false;

// DM_RESTORE_TABS caps how many stored conversations get a tab back on
// connect. The bar is a single wrapped row, so restoring 200 peers would bury
// the chat rather than restore it.
const DM_RESTORE_TABS = 6;

// Persist debounce for settings writes (last-read, dismissed announcement).
let persistTimer = null;

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

function getStore(key) {
    if (!store.has(key)) {
        // truncated (103): the server capped a page's key bundle, so some rows
        // in it stayed sealed for a reason that is transient, not permanent.
        store.set(key, { msgs: [], hasMore: true, end: false, loading: false, loaded: false, truncated: false });
    }
    return store.get(key);
}

function activeKey() {
    const st = V().state;
    if (view.kind === "global") return "ch:0";
    if (view.kind === "dm") return "dm:" + view.uid;
    if (view.kind === "chan") return "ch:" + view.id;
    return "ch:" + (st.myChannelID || 0);
}

// activeChannelID returns the channel the active view reads, or null for the
// global and DM views.
function activeChannelID() {
    if (view.kind === "chan") return view.id;
    if (view.kind === "channel") return V().state.myChannelID || null;
    return null;
}

function chatSurfaceVisible(key) {
    const workspace = document.getElementById("app");
    const chatTab = document.getElementById("tab-chat");
    return key === activeKey() && document.visibilityState === "visible" && document.hasFocus() &&
        workspace && !workspace.classList.contains("hidden") && workspace.getAttribute("aria-hidden") !== "true" &&
        chatTab?.getAttribute("aria-pressed") === "true";
}

function chatAnnouncementAllowed(event, ctx, key) {
    if (!chatSurfaceVisible(key)) return false;
    return window.__voicxNotify?.notificationOutputAllowed?.(event, ctx, "toast") ?? true;
}

function chatAnnouncementText(message) {
    return `${message.from || "Someone"}: ${String(message.text || "message").slice(0, 160)}`;
}

export function beginReconnectAnnouncementBatch(durationMs = 12000) {
    reconnectAnnouncementUntil = Math.max(reconnectAnnouncementUntil, Date.now() + Math.max(0, durationMs));
}

export function cancelReconnectAnnouncementBatch() {
    reconnectAnnouncementUntil = 0;
    for (const batch of reconnectAnnouncementBatches.values()) {
        if (batch.timer) clearTimeout(batch.timer);
    }
    reconnectAnnouncementBatches.clear();
}

function queueReconnectAnnouncement(key, message, event, ctx) {
    if (Date.now() >= reconnectAnnouncementUntil) return false;
    const batch = reconnectAnnouncementBatches.get(key) || {
        n: 0, names: new Set(), event, ctx, timer: null,
    };
    batch.n++;
    if (message.from) batch.names.add(message.from);
    batch.event = event;
    batch.ctx = ctx;
    if (batch.timer) clearTimeout(batch.timer);
    batch.timer = setTimeout(() => {
        reconnectAnnouncementBatches.delete(key);
        if (!chatAnnouncementAllowed(batch.event, batch.ctx, key)) return;
        const from = batch.names.size === 1 ? ` from ${[...batch.names][0]}` : "";
        V().announceLive(`${batch.n} message${batch.n === 1 ? "" : "s"}${from} received after reconnect`);
    }, 700);
    reconnectAnnouncementBatches.set(key, batch);
    return true;
}

function eligibleChatAnnouncement(message, key, event, ctx, allowed) {
    if (message.self || message.offline || !allowed) return "";
    if (queueReconnectAnnouncement(key, message, event, ctx)) return "";
    return chatAnnouncementText(message);
}

// channelName resolves a channel id to its tree name, falling back to the id
// so a tab for a channel the snapshot has not caught up with still reads.
function channelName(id) {
    const ch = V().state.channels.find((c) => c.ChannelID === id);
    return ch ? ch.Name : String(id);
}

function subscribed(channelID) {
    return subscriptions.includes(Number(channelID));
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
        replyToID: Number(d.reply_to_id) || 0,
        version: Number(d.version) || 1,
        channelID,
        self: false,
        mentioned: false,
    };
}

// attribute fills the two flags the live path derives from the broadcast but
// a history entry cannot: it carries no mentions list and no "is this mine".
// Without this, own messages replayed from history lose edit/delete and every
// mention of me loses its highlight the moment it is reloaded (106).
function attribute(m) {
    const st = V().state;
    m.self = m.fromUID ? m.fromUID === st.myUniqueID : m.from === st.myNickname;
    m.mentioned = !m.self && !m.deleted && mentionsMe(m.text);
    return m;
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
// Mentions (105/106)
// ---------------------------------------------------------------------------

// MENTION_ALL_FORMS are the mass-mention keywords. @channel is listed with
// @here/@everyone because it reaches every member of the scope exactly like
// they do, so the same gate applies to all three.
const MENTION_ALL_FORMS = ["channel", "here", "everyone"];

// canMentionAll resolves b_chat_mention_all locally so the completer only
// OFFERS the mass-mention forms to a user who may use them (105). The server
// enforces the same key independently — this is UI, not security.
function canMentionAll() {
    const st = V().state;
    if (st.isAdmin) return true;
    const e = st.myPerms?.get("b_chat_mention_all");
    return !!e && e.value !== 0 && !e.negate;
}

function reEscape(s) {
    return s.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&");
}

// mentionRe builds the token matcher for my own nickname plus the mass forms.
// It is anchored on a non-word character so "@dan" does not fire on
// "@daniela", which is exactly the substring bug the server side has (105).
function mentionRe() {
    const nick = V().state.myNickname || "";
    const alts = MENTION_ALL_FORMS.slice();
    if (nick) alts.unshift(reEscape(nick));
    return new RegExp("@(" + alts.join("|") + ")(?![\\w-])", "gi");
}

// mentionsMe reports whether a body mentions me. History entries carry no
// mentions list (the server resolves mentions only for the live broadcast),
// so a replayed message is matched locally instead of losing its highlight
// (106). A mass form counts here even if the sender turned out to lack the
// permission — the server dropped it from the live event, and this is only a
// local accent.
function mentionsMe(text) {
    if (!text) return false;
    return mentionRe().test(text);
}

// markMentions wraps @tokens in the rendered body. It walks text nodes only,
// so it can never introduce markup into user text.
function markMentions(container) {
    const re = mentionRe();
    const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT);
    const nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    while (nodes.length) {
        const node = nodes.shift();
        if (node.parentNode?.closest?.(".md-code, .md-pre, .mention-tok")) continue;
        re.lastIndex = 0;
        const hit = re.exec(node.nodeValue);
        if (!hit) continue;
        const token = node.splitText(hit.index);
        const tail = token.splitText(hit[0].length);
        const span = document.createElement("span");
        span.className = "mention-tok";
        span.textContent = token.nodeValue;
        token.parentNode.replaceChild(span, token);
        nodes.unshift(tail); // a body can mention more than one name
    }
}

// ---------------------------------------------------------------------------
// Custom server emoji (95)
// ---------------------------------------------------------------------------

async function ensureCustomEmoji() {
    if (!customDirty) return;
    const generation = V().state.serverGeneration;
    customDirty = false;
    try {
        const r = await app().EmojiList();
        if (generation !== V().state.serverGeneration) return;
        customEmoji = r.emojis || [];
    } catch {
        if (generation !== V().state.serverGeneration) return;
        customEmoji = [];
    }
}

async function emojiURL(name) {
    if (emojiURLs.has(name)) return emojiURLs.get(name);
    const generation = V().state.serverGeneration;
    emojiURLs.set(name, ""); // marks "loading/failed until proven otherwise"
    try {
        const d = await app().EmojiGet(name);
        if (generation !== V().state.serverGeneration) return "";
        const u = imageDataURL(d);
        if (!u) return "";
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
            const img = `<img class="md-emoji" src="${escapeHTML(url)}" alt=":${escapeHTML(e.name)}:">`;
            const parts = html.split(/(<[^>]+>)/g);
            const target = ":" + e.name + ":";
            for (let i = 0; i < parts.length; i++) {
                if (!parts[i].startsWith("<")) {
                    parts[i] = parts[i].split(target).join(img);
                }
            }
            html = parts.join("");
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
    if (!quoteNick && m.replyToID) quoteNick = findMsg(m.replyToID)?.from || "message";
    const parts = text.split(/\[file:([^\]]+)\]/);
    let html = "";
    for (let i = 0; i < parts.length; i += 2) html += renderMarkdown(parts[i]);
    container.innerHTML = applyCustomEmoji(html);
    markMentions(container); // (106) accent the @token itself, not only the row
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
// (99) inline video. Only container formats the webview can actually decode
// are listed — a .mov would render a dead player, so it stays a download chip.
const VIDEO_EXTS = ["mp4", "webm", "ogv"];
const VIDEO_MIME = { mp4: "video/mp4", webm: "video/webm", ogv: "video/ogg" };

// (98/100) staging and inline-preview caps. Everything crosses the Wails
// bridge base64-encoded and is held in memory twice (blob + data URL), so an
// unbounded attachment takes the renderer down with it.
const ATTACH_MAX_BYTES = 25 * 1024 * 1024;
const INLINE_MAX_B64 = 8 * 1024 * 1024; // ~6 MB of media rendered inline

// parseFileRef splits a [file:<capture>] token into its three parts. It is
// total and never throws: zero or one separators is a legacy plain reference,
// which is what keeps pre-encryption messages rendering. Mirrors
// parseFileRef in client/chat.go.
export function parseFileRef(cap) {
    const i = cap.indexOf("#");
    if (i < 0) return { storage: cap, key: "", name: cap, valid: true, legacy: true };       // legacy plain [file:photo.png]
    const j = cap.indexOf("#", i + 1);
    if (j < 0) return { storage: cap, key: "", name: cap, valid: false, legacy: false };      // malformed (1 separator)
    return { storage: cap.slice(0, i), key: cap.slice(i + 1, j), name: cap.slice(j + 1), valid: true, legacy: false };
}

// attachFileRef takes the RAW capture and parses it here rather than in
// renderBody, so the file key never crosses the split boundary. The key goes
// straight into the Go call: never into textContent, an attribute or a src.
function attachFileRef(container, m, cap) {
    const ref = parseFileRef(cap);
    if (!ref.valid) {
        container.appendChild(document.createTextNode(`[file:${cap}]`));
        return;
    }
    const isDM = m.isDM || !m.channelID || m.channelID === "0" || m.channelID === 0;
    const chID = isDM ? 0 : Number(m.channelID);
    const name = ref.name;
    const ext = (name.split(".").pop() || "").toLowerCase();
    const wrap = document.createElement("span");
    wrap.className = "msg-file";
    const inlineImage = IMAGE_EXTS.includes(ext);
    const inlineVideo = VIDEO_EXTS.includes(ext);
    if (inlineImage || inlineVideo) {
        wrap.textContent = `loading ${inlineVideo ? "video" : "image"} ${name} …`;
        app().DownloadChatAttachment(chID, ref.storage, ref.key).then((b64) => {
            wrap.textContent = "";
            if (!b64) {
                wrap.textContent = "📎 " + name + " (unavailable)";
                return;
            }
            // A data: URL costs ~4/3 of the payload again in the renderer, so
            // oversized media stays a download chip rather than wedging the
            // webview (100).
            if (b64.length > INLINE_MAX_B64) {
                wrap.appendChild(downloadChip(chID, ref, name, " (too large to preview)"));
                return;
            }
            const el = inlineVideo
                ? document.createElement("video")
                : document.createElement("img");
            if (inlineVideo) {
                el.className = "msg-video";
                el.controls = true;
                el.preload = "metadata";
                el.src = `data:${VIDEO_MIME[ext]};base64,${b64}`;
            } else {
                el.className = "msg-img";
                el.alt = name;
                el.src = `data:image/${ext === "jpg" ? "jpeg" : ext};base64,${b64}`;
            }
            el.title = inlineVideo ? name : name + " — click to zoom";
            wrap.appendChild(el);
            const zoom = document.createElement("button");
            zoom.className = "media-zoom";
            zoom.textContent = "⤢";
            zoom.title = "open " + name + " full size";
            zoom.onclick = () => openLightbox(el);
            wrap.appendChild(zoom);
            if (!inlineVideo) el.onclick = () => openLightbox(el); // controls own the click on a video
        }).catch(() => {
            wrap.textContent = "📎 " + name + " (download failed)";
        });
    } else {
        wrap.appendChild(downloadChip(chID, ref, name, ""));
    }
    container.appendChild(wrap);
}

// downloadChip builds the "📎 name" button. ref carries the attachment key,
// which is read straight into the Go call and never written to the DOM.
function downloadChip(chID, ref, name, suffix) {
    const b = document.createElement("button");
    b.className = "file-chip";
    b.textContent = "📎 " + name + suffix;
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
    return b;
}

// openLightbox zooms a rendered media element. It clones the node rather than
// re-downloading, so the attachment is fetched and decrypted exactly once.
function openLightbox(node) {
    const ov = document.createElement("div");
    ov.className = "dlg-overlay lightbox";
    const big = node.cloneNode(true);
    big.removeAttribute("class");
    big.removeAttribute("title");
    if (big.tagName === "VIDEO") {
        big.controls = true;
        big.autoplay = true;
    }
    ov.appendChild(big);
    const close = () => closeDialog(ov);
    ov.onclick = (e) => {
        if (e.target === ov) close(); // clicking the video's controls must not close it
    };
    mountServerDialog(ov);
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
    // (108) only messages that are actually part of a chain get the affordance
    // — on everything else a thread button would open a panel of one.
    const th = threadIndex(activeKey());
    const root = th.rootOf.get(m.id) || m.id;
    if (th.replies.get(root)) mk("🧵", "open thread", () => openThread(root));
    if (m.self && !m.e2e) mk("✎", "edit", () => startEdit(m));
    if (m.self) mk("🗑", "delete", () => deleteMsg(m));
    if (!m.e2e) {
        const pinned = isPinned(m);
        mk(pinned ? "📍" : "📌", pinned ? "unpin" : "pin", () => pinMsg(m));
    }
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
            const err = await app().ChatEditMessage(m.channelID ?? 0, m.id, t, m.version || 1);
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

// --- pin state (109) -----------------------------------------------------------

function pinSet(key) {
    if (!pinnedIDs.has(key)) pinnedIDs.set(key, new Set());
    return pinnedIDs.get(key);
}

function isPinned(m) {
    return pinSet("ch:" + (m.channelID ?? 0)).has(m.id);
}

// pinMsg toggles. The hover button used to always send pinned=true, so it
// could pin but never unpin despite saying so (109).
async function pinMsg(m) {
    const chID = m.channelID ?? 0;
    const err = await app().ChatPinMessage(chID, m.id, !isPinned(m));
    if (err) V().toast("pin failed: " + err, "warn"); // permission errors land here (109)
    // The chat_pinned/chat_unpinned broadcast updates the set and the button.
}

// ensurePins loads a scope's pin set once, so every message knows whether it
// is pinned without the panel being open.
async function ensurePins(key) {
    if (pinnedIDs.has(key) || !key.startsWith("ch:")) return;
    const set = pinSet(key);
    try {
        const resp = await app().ChatPins(Number(key.slice(3)));
        for (const p of resp.pins || []) set.add(Number(p.message_id));
    } catch { /* no membership / old server: pins stay unknown, button pins */ }
}

// --- reply-to (107) ------------------------------------------------------------
// New replies carry a stable message id. Quote-prefix parsing below keeps old
// stored messages and mixed-version servers readable.

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

// QUOTE_RE matches the reply prefix. It is the ONLY link between a reply and
// its parent — threads (108) resolve the chain from it rather than adding a
// second mechanism to the wire.
const QUOTE_RE = /^↪ ([^\n:]{1,64}): /;

// quotedNick returns the nick a message replies to, or "".
function quotedNick(m) {
    return (!m.deleted && m.text.match(QUOTE_RE)?.[1]) || "";
}

// resolveParent finds the message a reply quotes: the newest loaded message
// from that nick at or before the reply. The prefix carries no id, so this is
// best-effort by construction — the same rule the quote jump has always used.
function resolveParent(msgs, m) {
    if (m.replyToID) return msgs.find((candidate) => candidate.id === m.replyToID) || null;
    const nick = quotedNick(m);
    if (!nick) return null;
    let target = null;
    for (const c of msgs) {
        if (c === m) break;
        if (c.from === nick && c.ts <= m.ts && c.id) target = c;
    }
    return target;
}

function scrollToQuote(quoteEl) {
    const msgEl = quoteEl.closest(".msg");
    const id = Number(msgEl?.dataset.msgId) || 0;
    const st = getStore(activeKey());
    const self = st.msgs.find((x) => x.id === id);
    const target = self ? resolveParent(st.msgs, self) : null;
    if (!target) {
        V().toast("quoted message not loaded", "info", "alert");
        return;
    }
    flashMsg(target.id);
}

// flashMsg scrolls a message into view and pulses it. Returns false when the
// message is not in the rendered log (scrolled out of the loaded window).
function flashMsg(id) {
    const el = document.querySelector(`#chat-log .msg[data-msg-id="${id}"]`);
    if (!el) return false;
    el.scrollIntoView({ block: "center" });
    el.classList.add("flash");
    setTimeout(() => el.classList.remove("flash"), 1200);
    return true;
}

// ---------------------------------------------------------------------------
// Threads (108)
// ---------------------------------------------------------------------------
// A thread is a reply chain reconstructed from the 107 quote prefix: rootOf
// maps every message to the head of its chain, and replies counts descendants
// per root. The index is memoized per store key and invalidated whenever that
// store's message list changes, because renderActions asks for it once per
// rendered message.

const threadCache = new Map(); // store key -> {stamp, rootOf, replies}

function threadStamp(st) {
    return st.msgs.length + ":" + (st.msgs.length ? st.msgs[0].id + "-" + st.msgs[st.msgs.length - 1].id : "");
}

function threadIndex(key) {
    const st = getStore(key);
    const stamp = threadStamp(st);
    const hit = threadCache.get(key);
    if (hit && hit.stamp === stamp) return hit;

    const rootOf = new Map(), replies = new Map();
    for (const m of st.msgs) {
        if (!m.id) continue;
        const p = resolveParent(st.msgs, m);
        if (!p) {
            rootOf.set(m.id, m.id);
            continue;
        }
        const root = rootOf.get(p.id) || p.id;
        rootOf.set(m.id, root);
        replies.set(root, (replies.get(root) || 0) + 1);
    }
    const idx = { stamp, rootOf, replies };
    threadCache.set(key, idx);
    return idx;
}

function closeThreadPanel() {
    if (threadPanel) {
        threadPanel.remove();
        threadPanel = null;
    }
    threadRootID = 0;
}

function openThread(rootID) {
    closeThreadPanel();
    threadRootID = Number(rootID) || 0;
    threadPanel = document.createElement("div");
    threadPanel.className = "chat-pop thread-panel";
    $("chat-wrap").appendChild(threadPanel);
    renderThreadPanel();
}

// renderThreadPanel draws the chain for threadRootID. Rows reuse renderMsg so
// reactions, locks and the hover actions behave identically; the live refresh
// helpers are scoped to #chat-log, so these copies are a snapshot that this
// function redraws instead.
function renderThreadPanel() {
    if (!threadPanel) return;
    const key = activeKey();
    const st = getStore(key);
    const th = threadIndex(key);
    threadPanel.innerHTML = "";

    const head = document.createElement("div");
    head.className = "chat-pop-head";
    head.textContent = `Thread · ${(th.replies.get(threadRootID) || 0) + 1} messages`;
    const x = document.createElement("button");
    x.textContent = "✕";
    x.title = "close thread";
    x.onclick = closeThreadPanel;
    head.appendChild(x);
    threadPanel.appendChild(head);

    const chain = st.msgs.filter((m) => m.id && (th.rootOf.get(m.id) || m.id) === threadRootID);
    if (chain.length === 0) {
        const hint = document.createElement("div");
        hint.className = "set-hint";
        hint.textContent = "the root of this thread is no longer loaded — scroll up to load older history";
        threadPanel.appendChild(hint);
        return;
    }
    for (const m of chain) {
        const row = document.createElement("div");
        row.className = "thread-row" + (m.id === threadRootID ? " root" : "");
        row.appendChild(renderMsg(m));
        row.onclick = () => flashMsg(m.id);
        threadPanel.appendChild(row);
    }
    const reply = document.createElement("button");
    reply.className = "thread-reply";
    reply.textContent = "↩ reply in thread";
    reply.title = "reply to the last message of this chain";
    reply.onclick = () => setReply(chain[chain.length - 1]);
    threadPanel.appendChild(reply);
}

// refreshThreadFor redraws the panel only when the changed message is part of
// the open chain. A redraw re-runs renderMsg, which re-downloads any
// attachment in the chain, so it must not fire on every unrelated message.
function refreshThreadFor(id) {
    if (!threadPanel || !id) return;
    if ((threadIndex(activeKey()).rootOf.get(Number(id)) || 0) !== threadRootID) return;
    renderThreadPanel();
}

// ---------------------------------------------------------------------------
// Views, PM tabs (122) and rendering
// ---------------------------------------------------------------------------

function setView(v) {
    view = v;
    closeThreadPanel(); // a chain belongs to one scope (108)
    resetTypingOut(); // (120) the next keystroke pings the new scope at once
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
    V().setDirectTargetVisible(true);
    $("chat-target").value = uid;
    setView({ kind: "dm", uid });
}

async function openE2EEDiagnostics() {
    const generation = V().state.serverGeneration;
    const peer = view.kind === "dm" ? view.uid : "";
    let d;
    try { d = await app().E2EEDiagnostics(peer); }
    catch (err) {
        if (generation === V().state.serverGeneration) V().toast("encryption diagnostics failed: " + err, "warn");
        return;
    }
    if (generation !== V().state.serverGeneration) return;
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const verified = peer && d.safety_number && V().state.settings?.e2ee_verified?.[peer] === d.safety_number;
    const changed = peer && V().state.settings?.e2ee_verified?.[peer] && !verified;
    overlay.innerHTML = `<div class="dlg e2ee-diagnostics">
        <h3>End-to-end encryption</h3>
        <div class="e2ee-status ${changed ? "warn" : verified ? "ok" : ""}">${changed ? "⚠ Safety number changed" : verified ? "✓ Safety number verified" : peer ? "Safety number not verified" : "Channel-key diagnostics"}</div>
        <dl>
            <dt>Cipher</dt><dd>${escapeHTML(d.cipher || "unknown")}</dd>
            <dt>Cached peer keys</dt><dd>${d.cached_peers || 0}</dd>
            <dt>Channel generations</dt><dd>${d.scope_keys || 0}</dd>
            <dt>Missing/refused keys</dt><dd>${d.refused_keys || 0}</dd>
            <dt>Handshakes in flight</dt><dd>${d.pending_key_pulls || 0}</dd>
        </dl>
        ${peer ? `<label>Safety number</label><pre class="safety-number">${escapeHTML(d.safety_number || "peer key unavailable")}</pre>` : ""}
        <div class="dlg-buttons">${peer && d.safety_number ? '<button class="dlg-ok verify-safety">Mark verified</button>' : ""}<button class="dlg-cancel">Close</button></div>
    </div>`;
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    overlay.querySelector(".verify-safety")?.addEventListener("click", async () => {
        const settings = V().state.settings;
        const previous = settings.e2ee_verified;
        settings.e2ee_verified = { ...(previous || {}), [peer]: d.safety_number };
        try {
            const err = await app().SaveSettings(settings);
            if (err) throw new Error(err);
        } catch (err) {
            settings.e2ee_verified = previous;
            if (!isCurrentServerDialog(overlay)) return;
            V().toast("saving safety verification failed: " + err, "warn");
            return;
        }
        if (!isCurrentServerDialog(overlay)) return;
        overlay.remove();
        V().toast("safety number verified");
    });
    mountServerDialog(overlay);
}

function activateChannel(channelID) {
    const id = Number(channelID);
    if (!id || !subscribed(id)) return;
    $("chat-scope").value = "channel";
    V().setDirectTargetVisible(false);
    setView(id === V().state.myChannelID ? { kind: "channel" } : { kind: "chan", id });
}

export function isSubscribed(channelID) {
    return subscribed(channelID);
}

export async function setChannelSubscription(channelID, subscribe) {
    const id = Number(channelID);
    if (!id) return false;
    if (!subscribe && id === V().state.myChannelID) {
        V().toast("your joined channel is always subscribed", "info", "alert");
        return false;
    }
    const err = await app().SubscribeChannels([id], !!subscribe);
    if (err) {
        V().toast((subscribe ? "subscribe" : "unsubscribe") + " failed: " + err, "warn");
        return false;
    }
    return true;
}

export async function openChannelTab(channelID) {
    const id = Number(channelID);
    if (!id) return;
    if (subscribed(id)) {
        activateChannel(id);
        return;
    }
    pendingChannelTab = id;
    if (!await setChannelSubscription(id, true)) pendingChannelTab = 0;
}

function closeChannelTab(channelID) {
    const id = Number(channelID);
    if (id === V().state.myChannelID) {
        V().toast("move to another channel before unsubscribing", "info", "alert");
        return;
    }
    setChannelSubscription(id, false);
}

// SubscriptionState is a full authoritative set. Replacing the local model
// makes rejection, revocation, deletion and missed events self-healing.
export function onSubscriptions(json) {
    let state;
    try {
        state = typeof json === "string" ? JSON.parse(json) : json;
    } catch {
        V().toast("server sent malformed subscriptions", "warn");
        return;
    }
    subscriptions = [...new Set((state?.channel_ids || []).map(Number).filter((id) => id > 0))]
        .sort((a, b) => a - b);
    chanTabs.clear();
    for (const id of subscriptions) chanTabs.set(id, { id });

    if (view.kind === "chan" && !subscribed(view.id)) {
        setView({ kind: "channel" });
    } else {
        renderTabs();
        updateHeader();
    }
    if (pendingChannelTab && subscribed(pendingChannelTab)) {
        const id = pendingChannelTab;
        pendingChannelTab = 0;
        activateChannel(id);
    } else if (pendingChannelTab) {
        pendingChannelTab = 0;
    }
}

// closePM closes the TAB. Dropping the in-memory window is safe now that the
// conversation is on disk sealed: the next open replays it from the local log
// instead of starting empty. Destroying that log is a separate, explicit act
// (clearPMHistory) — closing a tab must never delete a conversation (122).
function closePM(uid) {
    pmTabs.delete(uid);
    store.delete("dm:" + uid);
    if (view.kind === "dm" && view.uid === uid) {
        $("chat-scope").value = "channel";
        V().setDirectTargetVisible(false);
        setView({ kind: "channel" });
        return;
    }
    renderTabs();
}

function renderTabs() {
    const bar = $("pm-tabs");
    bar.innerHTML = "";
    bar.classList.toggle("hidden", pmTabs.size === 0 && chanTabs.size === 0);
    for (const tab of chanTabs.values()) {
        const id = tab.id;
        const current = id === V().state.myChannelID;
        const active = activeChannelID() === id && view.kind !== "global" && view.kind !== "dm";
        const el = document.createElement("div");
        el.className = "pm-tab channel-tab" + (active ? " active" : "");
        el.title = current ? "joined channel" : "subscribed channel";
        const name = document.createElement("span");
        name.className = "pm-tab-name";
        name.textContent = "# " + channelName(id);
        el.appendChild(name);
        const badge = unread.get(id);
        if (badge?.n > 0) {
            const dot = document.createElement("span");
            dot.className = "pm-unread" + (badge.mention ? " mention" : "");
            dot.textContent = badge.n;
            el.appendChild(dot);
        }
        if (!current) {
            const x = document.createElement("button");
            x.className = "pm-close";
            x.textContent = "✕";
            x.title = "unsubscribe and close tab";
            x.onclick = (e) => {
                e.stopPropagation();
                closeChannelTab(id);
            };
            el.appendChild(x);
        }
        el.onclick = () => activateChannel(id);
        bar.appendChild(el);
    }
    for (const tab of pmTabs.values()) {
        const el = document.createElement("div");
        el.className = "pm-tab" + (view.kind === "dm" && view.uid === tab.uid ? " active" : "");
        el.title = "right-click to delete the stored history of this conversation";
        el.oncontextmenu = (e) => {
            e.preventDefault();
            clearPMHistory(tab.uid, tab.nick);
        };
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

// --- local DM history (122) ---------------------------------------------------
// A DM is true E2EE and the server keeps nothing, so the sealed per-peer log
// in client/chat.go is the entire history: without these calls a conversation
// exists only until the process exits.

// dmMsg converts one stored DMEntry into the renderer's message shape. The
// local seq is deliberately NOT used as the id: an id addresses a SERVER row,
// and edit/delete/pin/react all key on it, so reusing a local counter there
// would aim those actions at an unrelated message.
function dmMsg(e, peerNick) {
    const st = V().state;
    const self = !!e.self;
    return {
        id: 0,
        from: e.from_nickname || (self ? st.myNickname : peerNick) || "?",
        fromUID: e.from_unique_id || "",
        text: e.body || "",
        ts: (Number(e.sent_at) || 0) * 1000,
        edited: false,
        deleted: false,
        reactions: null,
        mentions: [],
        e2e: true,
        enc: false,
        offline: !!e.offline,
        clientMsgID: e.client_msg_id || "",
        channelID: null,
        self,
        mentioned: false,
    };
}

// ensureDMHistory replays a peer's log into its store once per session. It
// runs in FRONT of whatever already arrived live, so a message that landed
// before the tab was opened stays newest instead of being replayed twice.
async function ensureDMHistory(uid) {
    const st = getStore("dm:" + uid);
    if (st.loaded || st.loading || !uid) return;
    st.loading = true;
    try {
        const rows = await app().DMHistoryLoad(uid);
        const nick = pmTabs.get(uid)?.nick || uid;
        const live = new Set(st.msgs.map((m) => m.clientMsgID).filter(Boolean));
        const older = (rows || []).map((e) => dmMsg(e, nick))
            .filter((m) => !m.clientMsgID || !live.has(m.clientMsgID));
        st.msgs = older.concat(st.msgs);
        const max = V().state.settings?.chat_max_lines || 200;
        while (st.msgs.length > Math.max(max, PAGE)) st.msgs.shift();
    } catch (e) {
        V().toast("DM history unavailable: " + e, "warn");
    } finally {
        st.loading = false;
        st.loaded = true;
    }
}

// dmRecord writes one DM — sent or received — to the peer's sealed log. The
// Go layer de-duplicates on client_msg_id, so an offline spool replayed after
// a reconnect does not double the conversation.
function dmRecord(peer, nick, m) {
    if (!peer) return;
    app().DMHistoryAppend(peer, nick || "", {
        from_unique_id: m.fromUID,
        from_nickname: m.from,
        body: m.text,
        sent_at: Math.floor(m.ts / 1000),
        self: m.self,
        client_msg_id: m.clientMsgID,
        offline: m.offline,
    }).then((err) => {
        if (!err || dmPersistWarned) return;
        dmPersistWarned = true;
        V().toast("DM history is not being saved: " + err, "warn");
    }).catch(() => { /* the tab still works without a log */ });
}

// clearPMHistory is the ONLY path that destroys a conversation, and it says so
// before it does: the server never had a copy to fall back on (122).
async function clearPMHistory(uid, nick) {
    if (!confirm(`Delete the stored history of the conversation with ${nick}?\n\n` +
        "This cannot be undone — these messages are end-to-end encrypted and the server never had a copy.")) return;
    const err = await app().DMHistoryClear(uid);
    if (err) {
        V().toast("could not clear DM history: " + err, "warn");
        return;
    }
    const st = getStore("dm:" + uid);
    st.msgs = [];
    st.loaded = true;
    if (view.kind === "dm" && view.uid === uid) renderView();
    V().toast("stored history with " + nick + " deleted", "info");
}

// restorePMTabs gives conversations with a stored log their tab back, so a
// restart shows them without waiting for the peer to speak first.
async function restorePMTabs() {
    let peers = [];
    try {
        peers = await app().DMHistoryPeers();
    } catch {
        return; // no local storage path yet
    }
    let added = false;
    for (const p of (peers || []).slice(0, DM_RESTORE_TABS)) {
        if (!p.unique_id || pmTabs.has(p.unique_id)) continue;
        pmTabs.set(p.unique_id, {
            uid: p.unique_id, nick: p.nickname || p.unique_id,
            unread: 0, offline: false, pendingRead: "",
        });
        added = true;
    }
    if (added) renderTabs();
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

    renderTyping(); // (120) drop the previous scope's indicator before anything else

    if (view.kind === "channel" && !V().state.myChannelID) {
        const hint = document.createElement("div");
        hint.className = "empty-state";
        hint.textContent = "Join a channel to see its chat — or pick global/direct below.";
        log.appendChild(hint);
        updateHeader();
        return;
    }

    if (view.kind === "dm") {
        // (122) the local sealed log is this conversation's whole history.
        if (!st.loaded && !st.loading) {
            ensureDMHistory(view.uid).then(() => {
                if (activeKey() === key) renderView();
            });
        }
    } else {
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
        // (103) the server capped this page's key bundle, so rows in it stayed
        // sealed for a TRANSIENT reason. Left unsaid, a ⚠ placeholder reads as
        // "you will never see this" instead of "ask again".
        if (st.truncated) {
            const gap = document.createElement("div");
            gap.className = "chat-history-gap";
            gap.textContent = "some older messages are unavailable — the server could not send every key for this page";
            gap.title = "scroll up again to retry the missing keys";
            log.appendChild(gap);
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
    renderThreadPanel(); // (108) the open chain follows the loaded window

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
    ensurePins(key); // (109) pin state for the 📌 toggle, alongside the first page
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
    const page = (resp.messages || []).map((x) => attribute(normalize(x, chID))).reverse(); // newest-first → oldest-first
    if (beforeID === 0) {
        st.msgs = page;
        st.truncated = !!resp.truncated;
    } else {
        st.msgs = page.concat(st.msgs);
        // (103) a gap anywhere in the loaded window is still a gap in it, so
        // one truncated page keeps the notice up until the scope is reloaded.
        st.truncated = st.truncated || !!resp.truncated;
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
    clearTyping(m.fromUID); // (120) their message landed; they are done typing

    if (m.e2e) {
        return routeDM(d, m);
    }

    const chID = m.channelID ?? 0;
    const key = "ch:" + chID;
    const position = pushMsg(key, m);
    if (position === "duplicate") return false;
    const directMention = m.mentions.includes(st.myUniqueID) && !m.self;
    const roleMention = !m.self && /@(admin|moderator|member|guest)\b/i.test(m.text || "");
    const level = st.settings?.chat_notification_level || "all";
    let announcementAllowed = false;
    let announcementEvent = "channel_message";
    let announcementContext = { channelID: m.channelID, className: "messages" };
    if (directMention) {
        m.mentioned = true; // (106) accent highlight
    } else if (!m.self) {
        // (388) keyword highlights: whole-word match gets mention treatment.
        const kw = window.__voicxNotify?.matchKeyword(m.text || "");
        const keywordLevelAllowed = level === "all" || level === "channel_mentions";
        if (kw) {
            m.mentioned = true;
            if (keywordLevelAllowed) {
                const keywordContext = { channelID: m.channelID, className: "mentions" };
                const keywordAnnouncement = chatAnnouncementAllowed("keyword", keywordContext, key);
                if (keywordAnnouncement) {
                    announcementAllowed = true;
                    announcementEvent = "keyword";
                    announcementContext = keywordContext;
                }
                window.__voicxNotify?.notify("keyword", `[${kw}] ` + (m.from || "someone") + ": " + (m.text || "").slice(0, 80),
                    { ...keywordContext, kind: "warn", announce: false });
            }
        }
    }
    if (!m.self) {
        const allowed = level === "all" || (level === "channel_mentions" && directMention) || (level === "role_mentions" && roleMention);
        if (allowed) {
            const event = directMention || roleMention ? "mention" : "channel_message";
            const context = { channelID: m.channelID, className: directMention || roleMention ? "mentions" : "messages" };
            const eventAnnouncement = chatAnnouncementAllowed(event, context, key);
            if (eventAnnouncement) {
                announcementAllowed = true;
                announcementEvent = event;
                announcementContext = context;
            }
            window.__voicxNotify?.notify(event, (m.from || "someone") + ": " + (m.text || "").slice(0, 80),
                { ...context, kind: directMention || roleMention ? "warn" : "info", announce: false });
        }
    }
    if (key === activeKey()) {
        if (position === "append") appendLive(m);
        else renderView();
    } else if (!m.self && !window.__voicxNotify?.channelOverride?.(chID)?.muted) {
        // (104) unread badge for a non-selected channel. A muted channel (387)
        // suppresses the badge too — silencing only the sound would still nag
        // through the tree.
        const u = unread.get(chID) || { n: 0, mention: false };
        u.n++;
        u.mention = u.mention || m.mentioned;
        unread.set(chID, u);
        V().renderTree();
        renderTabs();
    }
    return {
        added: true,
        announcement: eligibleChatAnnouncement(
            m, key, announcementEvent, announcementContext, announcementAllowed),
    };
}

function pushMsg(key, m) {
    const st = getStore(key);
    if (m.id && st.msgs.some((existing) => existing.id === m.id)) return "duplicate";
    if (m.clientMsgID && st.msgs.some((existing) => existing.clientMsgID === m.clientMsgID && existing.self === m.self)) return "duplicate";
    const index = m.id ? st.msgs.findIndex((existing) => existing.id && existing.id > m.id) : -1;
    if (index >= 0) st.msgs.splice(index, 0, m);
    else st.msgs.push(m);
    const max = V().state.settings?.chat_max_lines || 200;
    while (st.msgs.length > Math.max(max, PAGE)) st.msgs.shift();
    return index >= 0 ? "insert" : "append";
}

function appendLive(m) {
    const log = $("chat-log");
    log.appendChild(renderMsg(m));
    const max = V().state.settings?.chat_max_lines || 200;
    while (log.children.length > max) log.firstChild.remove();
    if (searchQ) applySearchFilter();
    refreshThreadFor(m.id); // (108) a reply joins the open chain immediately
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
    if (!peer) return false;
    const tab = pmTabs.get(peer) || { uid: peer, nick: m.self ? peer : m.from, unread: 0, offline: false, pendingRead: "" };
    if (!pmTabs.has(peer)) pmTabs.set(peer, tab);
    if (!m.self) tab.nick = m.from;

    const key = "dm:" + peer;
    if (pushMsg(key, m) === "duplicate") return false;
    if (!m.self && !m.offline) {
        window.__voicxNotify?.notify("dm", (m.from || "someone") + ": " + (m.text || "").slice(0, 80),
            { uid: peer, className: "messages", kind: "info", announce: false });
    }
    dmRecord(peer, tab.nick, m); // (122) both directions, so a restart replays the thread

    if (m.offline && !m.self) {
        // (123) offline-spooled DM: tab badge + batched toast.
        tab.offline = true;
        const b = offlineBatch.get(peer) || { n: 0, nick: m.from, timer: null };
        b.n++;
        if (b.timer) clearTimeout(b.timer);
        b.timer = setTimeout(() => {
            offlineBatch.delete(peer);
            const summary = `${b.n} offline message${b.n === 1 ? "" : "s"} from ${b.nick}`;
            window.__voicxNotify?.notify("dm", summary,
                { uid: peer, className: "messages", kind: "info", announce: false });
            if (chatAnnouncementAllowed("dm", { uid: peer, className: "messages" }, key)) {
                V().announceLive(summary);
            }
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
    return {
        added: true,
        incomingDM: !m.self && !m.offline,
        announcement: eligibleChatAnnouncement(
            m,
            key,
            "dm",
            { uid: peer, className: "messages" },
            chatAnnouncementAllowed("dm", { uid: peer, className: "messages" }, key),
        ),
    };
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
    m.version = Number(d.version) || (m.version + 1);
    m.mentioned = !m.self && mentionsMe(m.text); // an edit can add or drop my name
    threadCache.clear(); // an edit can add or remove the reply prefix (108)
    refreshMsgEl(m);
    refreshThreadFor(m.id);
}

export function onChatDeleted(d) {
    const m = findMsg(d.message_id);
    if (!m) return;
    m.deleted = true;
    m.text = "";
    threadCache.clear();
    refreshMsgEl(m); // tombstone style (102)
    refreshThreadFor(m.id);
}

export function onChatPinned(d, pinned) {
    V().sysMsg(pinned ? "a message was pinned" : "a message was unpinned");
    // (109) keep the per-scope pin set current so the 📌 hover button toggles.
    const key = "ch:" + (Number(d.channel_id) || 0);
    const id = Number(d.message_id);
    if (pinned) pinSet(key).add(id);
    else pinSet(key).delete(id);
    const m = findMsg(id);
    if (m) refreshMsgEl(m);
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
// Typing indicators (120)
// ---------------------------------------------------------------------------
// Wire contract (internal/server/chatx.go handleTyping / typingEvent):
//   send    App.SendTyping(channelID int64, toUniqueID string) -> error string
//           MsgTyping{channel_id, to_unique_id}; exactly one is set, neither
//           set = global scope.
//   receive event envelope {type:"typing", data:{client_id, unique_id,
//           nickname, channel_id}}
// Two consequences of that shape, both handled below. A channel/global relay
// reaches the SENDER too, so an event carrying my own identity is dropped.
// And the event has no to_unique_id: a DM indicator carries channel_id 0 and
// is therefore indistinguishable from a global one, so it lands on the global
// scope — the person who sees it is the DM recipient either way.
// There is no stop message: the relay is fire-and-forget, so the sender pings
// while composing and every receiver expires the entry on its own.

const TYPING_DEBOUNCE_MS = 300;
const TYPING_PING_MS = 2000; // matches the server's per-scope throttle
const TYPING_TTL_MS = 4500; // must exceed the ping interval or it flickers
const TYPING_IDLE_MS = 3000;

// typingScopeOf returns [channelID, toUniqueID] for the active view.
function typingScopeOf() {
    if (view.kind === "dm") return [0, view.uid];
    if (view.kind === "global") return [0, ""];
    return [activeChannelID() || 0, ""];
}

// resetTypingOut ends the local composing session: the next keystroke pings
// immediately instead of waiting out the throttle. Called on send, on blur,
// on an emptied input, on a scope switch and on idle (120).
function resetTypingOut() {
    typingSentAt = 0;
    typingScope = "";
    if (typingIdle) {
        clearTimeout(typingIdle);
        typingIdle = null;
    }
    if (typingDebounce) {
        clearTimeout(typingDebounce);
        typingDebounce = null;
    }
}

// noteTyping pings the relay while the user composes. An empty input is not
// typing, and the ping is throttled so a fast typist sends one frame per
// TYPING_PING_MS rather than one per keystroke.
function noteTyping() {
    if (!$("chat-text").value.trim()) {
        resetTypingOut();
        return;
    }
    const send = () => {
        typingDebounce = null;
        const [chID, uid] = typingScopeOf();
        const scope = chID + "|" + uid;
        const now = Date.now();
        if (scope !== typingScope || now - typingSentAt >= TYPING_PING_MS) {
            typingScope = scope;
            typingSentAt = now;
            app().SendTyping(chID, uid);
        }
    };
    if (!typingDebounce) typingDebounce = setTimeout(send, TYPING_DEBOUNCE_MS);
    if (typingIdle) clearTimeout(typingIdle);
    typingIdle = setTimeout(resetTypingOut, TYPING_IDLE_MS);
}

// onTyping records one relayed indicator, keyed exactly on the channel_id the
// event carries — the only scope information in the contract.
export function onTyping(d) {
    const st = V().state;
    const uid = d.unique_id || "";
    if (!uid || uid === st.myUniqueID || d.client_id === st.myClientID) return;
    const key = "ch:" + (Number(d.channel_id) || 0);
    if (!typers.has(key)) typers.set(key, new Map());
    typers.get(key).set(uid, { nick: d.nickname || uid, expires: Date.now() + TYPING_TTL_MS });
    startTypingSweep();
    renderTyping();
}

// clearTyping drops a user's indicator early — their message arrived, so they
// are demonstrably no longer typing it. It sweeps every scope because a DM
// indicator is filed under the global scope (see the contract note above), so
// the arriving DM would otherwise leave it hanging until it expires.
function clearTyping(uid) {
    if (!uid) return;
    let hit = false;
    for (const m of typers.values()) hit = m.delete(uid) || hit;
    if (hit) renderTyping();
}

// startTypingSweep runs one interval for every scope, stopping itself once
// nothing is pending so an idle client holds no timer.
function startTypingSweep() {
    if (typingTimer) return;
    typingTimer = setInterval(() => {
        const now = Date.now();
        let live = 0;
        for (const [key, m] of typers) {
            for (const [uid, t] of m) if (t.expires <= now) m.delete(uid);
            if (m.size === 0) typers.delete(key);
            else live += m.size;
        }
        renderTyping();
        if (live === 0) {
            clearInterval(typingTimer);
            typingTimer = null;
        }
    }, 1000);
}

function renderTyping() {
    const el = $("chat-typing");
    if (!el) return;
    // "channel view without a channel" shares the store key with global, so it
    // must not borrow global's typers along with it.
    const inNoChannel = view.kind === "channel" && !V().state.myChannelID;
    const m = inNoChannel ? null : typers.get(activeKey());
    const names = m ? [...m.values()].map((t) => t.nick) : [];
    el.classList.toggle("hidden", names.length === 0);
    if (names.length === 0) {
        el.textContent = "";
        return;
    }
    el.textContent = names.length === 1 ? `${names[0]} is typing…`
        : names.length === 2 ? `${names[0]} and ${names[1]} are typing…`
        : `${names.length} people are typing…`;
}

// ---------------------------------------------------------------------------
// Sending (reply ids 601, file uploads 98/100)
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
    if (scope === "channel") target = String(activeChannelID() || "");
    if (scope === "direct") {
        target = $("chat-target").value.trim();
        lastDMTarget = target;
        if (target && !pmTabs.has(target)) openPMKeepView(target);
    }

    // Files are sealed with their own key and uploaded under a content-derived
    // name; the returned token carries that key inside the encrypted message
    // body, so the attachment is as private as the message (91-135).
    const chID = scope === "channel" ? (activeChannelID() || 0) : 0;
    for (const f of files) {
        let token;
        try {
            token = await app().UploadChatAttachment(chID, f.name, f.dataBase64);
        } catch (e) {
            V().sysMsg("upload failed: " + e);
            continue;
        }
        const err = await app().SendChat(scope, target, token);
        if (err) V().sysMsg("chat failed: " + err);
    }

    if (text) {
        const parentID = scope === "direct" ? 0 : (replyTo?.id || 0);
        const err = parentID
            ? await app().SendChatReply(scope, target, text, parentID)
            : await app().SendChat(scope, target, text);
        rememberSent(text);
        if (replyTo) clearReply();
        if (err) V().sysMsg("chat failed: " + err);
    }
    input.value = "";
    resetTypingOut(); // (120) the message landed; stop claiming to be composing
}

function rememberSent(text) {
    if (!text || sentHistory[sentHistory.length - 1] === text) return;
    sentHistory.push(text);
    if (sentHistory.length > 100) sentHistory.shift();
    sentHistoryPos = sentHistory.length;
    sentHistoryDraft = "";
}

function recallSent(e) {
    const input = $("chat-text");
    if (e.key === "ArrowUp" && (input.selectionStart === 0 || sentHistoryPos < sentHistory.length) && sentHistoryPos > 0) {
        e.preventDefault();
        if (sentHistoryPos === sentHistory.length) sentHistoryDraft = input.value;
        input.value = sentHistory[--sentHistoryPos];
        input.selectionStart = input.selectionEnd = input.value.length;
    } else if (e.key === "ArrowDown" && input.selectionEnd === input.value.length && sentHistoryPos < sentHistory.length) {
        e.preventDefault();
        sentHistoryPos++;
        input.value = sentHistoryPos === sentHistory.length ? sentHistoryDraft : sentHistory[sentHistoryPos];
        input.selectionStart = input.selectionEnd = input.value.length;
    }
}
function closeEmojiPanel() {
    if (emojiPanel) {
        emojiPanel.remove();
        emojiPanel = null;
    }
}

function openPMKeepView(uid) {
    const c = V().state.clients.find((x) => x.unique_id === uid);
    pmTabs.set(uid, { uid, nick: c?.nickname || uid, unread: 0, offline: false, pendingRead: "" });
    renderTabs();
}

// --- file staging: paste / drop (98/100) ---------------------------------------

function stageFiles(fileList) {
    for (const f of fileList) {
        if (f.size > ATTACH_MAX_BYTES) {
            V().toast(`${f.name || "file"} is larger than ${Math.round(ATTACH_MAX_BYTES / 1048576)} MB — use the file browser`, "warn");
            continue;
        }
        const reader = new FileReader();
        reader.onload = () => {
            const dataURL = reader.result;
            const rawName = (f.name || "image.png").replace(/[\[\]#]/g, "_");
            const extIdx = rawName.lastIndexOf(".");
            const uniqueId = Date.now().toString(36) + "_" + Math.random().toString(36).slice(2, 6);
            let uniqueName = rawName;
            if (extIdx > 0) {
                uniqueName = rawName.slice(0, extIdx) + "_" + uniqueId + rawName.slice(extIdx);
            } else {
                uniqueName = rawName + "_" + uniqueId;
            }
            pendingFiles.push({
                name: uniqueName,
                dataBase64: dataURL.split(",")[1] || "",
                isImage: f.type.startsWith("image/"),
                dataURL: f.type.startsWith("image/") ? dataURL : null,
            });
            renderFilePreview();
        };
        reader.onerror = () => V().toast("could not read " + (f.name || "file"), "warn");
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
    // (96) upload is permission-gated server-side, so the button is always
    // offered and a denial comes back as an error frame.
    const up = document.createElement("button");
    up.className = "emoji-upload";
    up.textContent = "+ upload";
    up.title = "upload a custom server emoji (needs permission)";
    up.onclick = async (ev) => {
        ev.stopPropagation();
        const generation = V().state.serverGeneration;
        const img = await pickIcon(128, 0.9);
        if (!img?.dataBase64 || generation !== V().state.serverGeneration) return;
        const name = (prompt("Emoji shortcode (letters, digits, _ and -):") || "").trim();
        if (!name || generation !== V().state.serverGeneration) return;
        const err = await app().EmojiUpload(name, img.dataBase64);
        if (generation !== V().state.serverGeneration) return;
        if (err) {
            V().toast("emoji upload failed: " + err, "warn");
            return;
        }
        customDirty = true; // refetch so the new one appears in the picker
        V().toast("emoji :" + name + ": uploaded");
    };
    head.appendChild(up);
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

// metaContent pulls one <meta> value. It accepts the attributes in either
// order, because plenty of pages emit content= before property=.
function metaContent(html, key) {
    const attr = key.startsWith("og:") ? "property" : "name";
    const re = new RegExp(
        `<meta[^>]+(?:${attr}=["']${key}["'][^>]*content=["']([^"']*)["']` +
        `|content=["']([^"']*)["'][^>]*${attr}=["']${key}["'])`, "i");
    const m = html.match(re);
    const v = (m?.[1] ?? m?.[2] ?? "").trim();
    return v || null;
}

async function linkPreview(url) {
    if (linkCards.has(url)) return linkCards.get(url);
    let card = { title: null, desc: null, image: null };
    if (!/^https?:\/\//i.test(url)) {
        linkCards.set(url, card);
        return card;
    }
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 5000);
    try {
        const resp = await fetch(url, { signal: controller.signal });
        let html = "";
        if (resp.body) {
            const reader = resp.body.getReader();
            const decoder = new TextDecoder();
            let loaded = 0;
            const maxBytes = 256 * 1024;
            while (loaded < maxBytes) {
                const { done, value } = await reader.read();
                if (done) break;
                loaded += value.byteLength;
                html += decoder.decode(value, { stream: true });
            }
            reader.cancel().catch(() => {});
        } else {
            html = (await resp.text()).slice(0, 256 * 1024);
        }
        card.title = metaContent(html, "og:title") ||
            html.match(/<title[^>]*>([^<]*)<\/title>/i)?.[1]?.trim() || null;
        card.desc = metaContent(html, "og:description") || metaContent(html, "description");
        const img = metaContent(html, "og:image");
        if (img && /^https:\/\//i.test(img)) card.image = img;
        if (card.title) card.title = card.title.slice(0, 120);
        if (card.desc) card.desc = card.desc.slice(0, 200);
    } catch { /* CORS/timeout — domain-only card */ }
    finally { clearTimeout(timer); }
    linkCards.set(url, card);
    return card;
}

function scheduleLinkCard(linkEl) {
    closeLinkCard();
    const settings = V().settings || {};
    if (settings.link_previews === false) return;
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
        if (card.image) {
            const im = document.createElement("img");
            im.className = "link-card-img";
            im.src = card.image;
            im.onerror = () => im.remove();
            linkCardEl.appendChild(im);
        }
        const t = document.createElement("div");
        t.className = "link-card-title";
        t.textContent = card.title || domain;
        linkCardEl.appendChild(t);
        if (card.desc) {
            const ds = document.createElement("div");
            ds.className = "link-card-desc";
            ds.textContent = card.desc;
            linkCardEl.appendChild(ds);
        }
        const d = document.createElement("div");
        d.className = "link-card-domain";
        d.textContent = card.title ? domain : url;
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
    if (view.kind === "channel" && !V().state.myChannelID) {
        V().toast("join a channel to view its pinned messages — or switch to global", "info", "alert");
        closePinsPanel();
        return;
    }
    const chID = view.kind === "global" ? 0 : (activeChannelID() || 0);
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
        // Pins decrypt through the same Go path as history, and the response
        // carries the generations it references, so bodies arrive plaintext
        // and the panel needs no extra round trip (109).
        const resp = await app().ChatPins(chID);
        pins = resp.pins || [];
        const set = pinSet("ch:" + chID);
        set.clear();
        for (const p of pins) set.add(Number(p.message_id));
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
            if (!flashMsg(p.message_id)) {
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

const SEARCH_LABEL = "search all history";

// searchAll runs the full-history search. It is CLIENT-side: App.ChatSearch
// pages the scope in Go and matches over bodies it decrypted itself, because
// the server stores only ciphertext and cannot match on content (110).
async function searchAll() {
    const generation = V().state.serverGeneration;
    const q = $("chat-search").value.trim();
    if (!q) return;
    const btn = $("chat-search-server");
    if (btn && btn.disabled) return;
    if (view.kind === "channel" && !V().state.myChannelID) {
        V().toast("join a channel to search its history — or switch to global", "info", "alert");
        return;
    }
    if (view.kind === "dm") {
        // (122/110) the server is E2EE-blind for DMs, so the only searchable
        // history is this device's own sealed log.
        btn.disabled = true;
        btn.textContent = "searching…";
        let dm = { messages: [], scanned: 0, undecryptable: 0 };
        try {
            dm = await app().DMSearch(view.uid, q, SEARCH_MAX);
        } catch (e) {
            if (generation === V().state.serverGeneration) V().toast("search failed: " + e, "warn");
        }
        if (generation !== V().state.serverGeneration) return;
        btn.disabled = false;
        btn.textContent = SEARCH_LABEL;
        showSearchResults(q.toLowerCase(), dm.messages || [], dm.scanned || 0, dm.undecryptable || 0);
        return;
    }
    const chID = view.kind === "global" ? 0 : (activeChannelID() || 0);
    btn.disabled = true;
    btn.textContent = "searching…";
    // The paging loop lives in Go now: the keys never cross into the webview
    // and a page can use the store's cap of 200 instead of the UI's 50 (110).
    const unsub = window.runtime.EventsOn("chatsearch:progress",
        (n) => { btn.textContent = `searching… ${n}`; });
    let res = { messages: [], scanned: 0, undecryptable: 0 };
    try {
        res = await app().ChatSearch(chID, q, SEARCH_MAX);
    } catch (e) {
        if (generation === V().state.serverGeneration) V().toast("search failed: " + e, "warn");
    }
    if (unsub) unsub();
    if (generation !== V().state.serverGeneration) return;
    btn.disabled = false;
    btn.textContent = SEARCH_LABEL;
    showSearchResults(q.toLowerCase(), res.messages || [], res.scanned || 0, res.undecryptable || 0);
}

function showSearchResults(q, results, scanned, undecryptable) {
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
    const sub = document.createElement("div");
    sub.className = "set-hint";
    // Scanned is the honest denominator: the search stops at the Go layer's
    // cap, so "0 matches" over a truncated scan is not "not in the history".
    sub.textContent = `searched ${scanned} message${scanned === 1 ? "" : "s"} of this scope` +
        (scanned >= SEARCH_MAX ? ` (stopped at the ${SEARCH_MAX}-message cap — narrow the query to reach further back)` : "");
    dlg.appendChild(sub);
    const list = document.createElement("div");
    list.className = "search-list";
    if (results.length === 0) {
        const hint = document.createElement("div");
        hint.className = "set-hint";
        hint.textContent = "no matches";
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
            if (!flashMsg(r.id)) V().toast("message not in the loaded view — scroll up to load it", "info", "alert");
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
    mountServerDialog(overlay);
}

// ---------------------------------------------------------------------------
// Header: topic (111), description (112/113), export (125)
// ---------------------------------------------------------------------------

export function refreshHeader() {
    updateHeader();
}

// fmtSlowMode renders a slow-mode interval the way a person reads it.
function fmtSlowMode(sec) {
    if (sec % 3600 === 0) return sec / 3600 + "h";
    if (sec % 60 === 0) return sec / 60 + "m";
    return sec + "s";
}

function updateHeader() {
    const st = V().state;
    let title = "Chat", topic = "";
    let showChanBtns = false;
    let slow = 0;
    if (view.kind === "global") {
        title = "Global chat";
    } else if (view.kind === "dm") {
        title = "DM — " + (pmTabs.get(view.uid)?.nick || view.uid);
    } else {
        const ch = st.channels.find((c) => c.ChannelID === activeChannelID());
        if (ch) {
            title = "# " + ch.Name;
            topic = ch.Topic || "";
            slow = ch.SlowModeSeconds || 0;
            showChanBtns = true;
        }
    }
    $("chat-head-title").textContent = title;
    const topicEl = $("chat-topic");
    topicEl.textContent = topic;
    topicEl.title = topic; // (111) tooltip carries the full topic
    topicEl.classList.toggle("hidden", !topic);
    // (114) slow mode is enforced by the server and rejects the send, so a user
    // who cannot see the limit only ever learns about it from a failure.
    const slowEl = $("chat-slowmode");
    slowEl.textContent = slow > 0 ? "🐢 " + fmtSlowMode(slow) : "";
    slowEl.title = slow > 0
        ? `slow mode: one message every ${fmtSlowMode(slow)} in this channel (moderators are exempt)`
        : "";
    slowEl.classList.toggle("hidden", slow <= 0);
    $("chat-pins-btn").classList.toggle("hidden", view.kind === "dm");
    $("chat-info-btn").classList.toggle("hidden", !showChanBtns);
}

function openDescription() {
    const st = V().state;
    const ch = st.channels.find((c) => c.ChannelID === activeChannelID());
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
    mountServerDialog(overlay);
}

// exportProgressDialog reports how far a running export got. Paging a whole
// scope takes long enough that a silent window looks hung, and the header
// button is too small to carry a running count (125).
function exportProgressDialog() {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const dlg = document.createElement("div");
    dlg.className = "dlg";
    const h = document.createElement("h3");
    h.textContent = "Exporting chat";
    const p = document.createElement("div");
    p.className = "set-hint";
    p.textContent = "decrypting history…";
    dlg.appendChild(h);
    dlg.appendChild(p);
    overlay.appendChild(dlg);
    let cleanup = null;
    mountServerDialog(overlay, { onClose: () => cleanup?.() });
    return {
        update: (n) => { p.textContent = `decrypted ${n} messages…`; },
        close: () => closeDialog(overlay),
        setCleanup: (fn) => { cleanup = fn; },
    };
}

// exportChat writes the WHOLE stored history out. The transcript is built in
// Go — ChatExportHistory pages and decrypts the entire scope, DMExportHistory
// reads the sealed local log — because the view holds only the window it
// happened to scroll through, and an export of that window silently drops
// everything else (125/122). Everything it produces is decrypted, so a plain
// export is the one place this client can undo the storage guarantee: it takes
// an explicit confirm, and the encrypted container is offered first.
async function exportChat() {
    const generation = V().state.serverGeneration;
    const exportView = { ...view };
    // Without a joined channel the channel view has no scope of its own, and
    // channel id 0 is GLOBAL: exporting it here would hand over a different
    // conversation than the one on screen.
    if (view.kind === "channel" && !V().state.myChannelID) {
        V().toast("join a channel to export its history — or switch to global", "info", "alert");
        return;
    }
    const name = exportView.kind === "dm" ? "dm-" + exportView.uid.slice(0, 8)
        : exportView.kind === "global" ? "global"
        : (V().state.channels.find((c) => c.ChannelID === activeChannelID())?.Name || "channel");

    const pass = await askExportPassphrase();
    if (pass === null || generation !== V().state.serverGeneration) return; // cancelled or switched

    const prog = exportProgressDialog();
    let unsub = window.runtime.EventsOn("chatexport:progress", (n) => prog.update(n));
    prog.setCleanup(() => {
        if (!unsub) return;
        unsub();
        unsub = null;
    });
    let res = null;
    try {
        res = exportView.kind === "dm"
            ? await app().DMExportHistory(exportView.uid)
            : await app().ChatExportHistory(exportView.kind === "global" ? 0 : (activeChannelID() || 0), 0);
    } catch (e) {
        if (generation === V().state.serverGeneration) V().toast("export failed: " + e, "warn");
    }
    prog.close();
    if (!res || generation !== V().state.serverGeneration) return;

    const contents = res.text || "";
    if (pass !== "") {
        try {
            await app().ExportChatEncrypted(`voicx-${name}.voicxchat`, contents, pass);
        } catch (e) {
            V().toast("export failed: " + e, "warn");
            return;
        }
    } else {
        const err = await app().ExportChat(`voicx-${name}.txt`, contents);
        if (err) {
            V().toast("export failed: " + err, "warn");
            return;
        }
    }
    // The same notice rides inside the file; repeating it here is what stops a
    // partial transcript from being taken for a whole one before it is opened.
    if (res.complete === false || res.undecryptable > 0) {
        V().toast(`exported ${res.messages} messages` +
            (res.undecryptable > 0 ? `, ${res.undecryptable} unreadable (no key)` : "") +
            (res.complete === false ? " — stopped at the export limit" : ""), "warn");
    }
}

// askExportPassphrase resolves to a passphrase (encrypted export), "" (plain
// export, explicitly confirmed) or null (cancelled).
function askExportPassphrase() {
    return new Promise((resolve) => {
        let result = null;
        let settled = false;
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
        const done = (value) => {
            result = value;
            closeDialog(overlay, value === null ? "cancel" : "close");
        };
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
        mountServerDialog(overlay, {
            initialFocus: inp,
            onCancel: () => { result = null; },
            onClose: () => {
                if (settled) return;
                settled = true;
                resolve(result);
            },
        });
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
    restorePMTabs(); // (122) conversations that survived the last restart
    try {
        onSubscriptions({ channel_ids: await app().Subscriptions() });
    } catch { /* the live authoritative event is still the primary path */ }
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
    if (view.kind === "chan" && view.id === st.myChannelID) {
        setView({ kind: "channel" });
    } else if (view.kind === "channel") renderView();
    else updateHeader();
    renderTabs();
}

// Channel deletion can remove a whole subtree in one event. Purge every
// channel-owned cache immediately instead of waiting for a later subscription
// snapshot, and leave a deleted subscribed-channel view for the joined scope.
export function onChannelsDeleted(channelIDs) {
    const removed = new Set((channelIDs || []).map(Number).filter((id) => id > 0));
    if (removed.size === 0) return;
    subscriptions = subscriptions.filter((id) => !removed.has(id));
    for (const id of removed) {
        chanTabs.delete(id);
        unread.delete(id);
        store.delete("ch:" + id);
    }
    if (removed.has(pendingChannelTab)) pendingChannelTab = 0;
    if (view.kind === "chan" && removed.has(view.id)) {
        $("chat-scope").value = "channel";
        V().setDirectTargetVisible(false);
        setView({ kind: "channel" });
        return;
    }
    renderTabs();
    updateHeader();
}

// resetView clears all per-server chat state (281 tab switch): message
// stores, unread badges, PM tabs, and the rendered panes. The tab journal
// replay rebuilds the view from server frames afterwards.
export function resetView(options = {}) {
    view = { kind: "channel" };
    store.clear();
    unread.clear();
    pmTabs.clear();
    chanTabs.clear();
    subscriptions = [];
    pendingChannelTab = 0;
    replyTo = null;
    pendingFiles = [];
    lastDMTarget = "";
    newCount = 0;
    jl.verb = null;
    jl.names = [];
    jl.el = null;
    customEmoji = [];
    emojiURLs.clear();
    customDirty = true;
    for (const b of offlineBatch.values()) {
        if (b.timer) clearTimeout(b.timer);
    }
    offlineBatch.clear();
    if (!options.preserveReconnectAnnouncements) cancelReconnectAnnouncementBatch();
    linkCards.clear();
    receipts.clear();
    myReactions.clear();
    pinnedIDs.clear();
    threadCache.clear();
    closeThreadPanel();
    closePinsPanel();
    typers.clear();
    resetTypingOut();
    if (typingTimer) {
        clearInterval(typingTimer);
        typingTimer = null;
    }
    renderTyping();
    $("chat-log").innerHTML = "";
    $("pm-tabs").innerHTML = "";
    $("pm-tabs").classList.add("hidden");
    $("reply-bar").classList.add("hidden");
    $("file-preview-row").classList.add("hidden");
    $("chat-head-title").textContent = "Chat";
    $("chat-topic").classList.add("hidden");
    renderTabs();
    // (122) DM logs are sealed to the IDENTITY, not to a server, so the same
    // conversations belong on the tab that was just switched to.
    restorePMTabs();
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
        closeDialog(qsOverlay);
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
    for (const tab of chanTabs.values()) {
        items.push({ label: "# " + channelName(tab.id), hint: "channel tab", action: () => activateChannel(tab.id) });
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
    const mountedOverlay = qsOverlay;
    mountServerDialog(qsOverlay, {
        initialFocus: input,
        onClose: () => {
            if (qsOverlay === mountedOverlay) qsOverlay = null;
        },
    });

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
// @nickname tab-completion in the chat input (105/106)
// ---------------------------------------------------------------------------

let tabCycle = null; // {start, end, base, matches, idx}

function handleTabComplete(e) {
    const input = $("chat-text");
    const pos = input.selectionStart ?? input.value.length;
    if (tabCycle && pos === tabCycle.end) {
        e.preventDefault();
        tabCycle.idx = (tabCycle.idx + 1) % tabCycle.matches.length;
        const pick = tabCycle.matches[tabCycle.idx];
        input.value = input.value.slice(0, tabCycle.start) + "@" + pick + " " + input.value.slice(tabCycle.end);
        tabCycle.end = tabCycle.start + pick.length + 2;
        input.selectionStart = input.selectionEnd = tabCycle.end;
        return;
    }
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
    if (canMentionAll()) {
        for (const f of MENTION_ALL_FORMS) if (f.startsWith(base)) names.push(f);
    }
    if (names.length === 0) {
        tabCycle = null;
        return;
    }
    e.preventDefault();
    const start = pos - m[0].length;
    const pick = names[0];
    const end = start + pick.length + 2;
    tabCycle = { start, end, base, matches: names, idx: 0 };
    input.value = input.value.slice(0, start) + "@" + pick + " " + input.value.slice(pos);
    input.selectionStart = input.selectionEnd = end;
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

export function initChat() {
    window.runtime.EventsOn("subscriptions", onSubscriptions);
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
    $("chat-e2ee-btn").onclick = openE2EEDiagnostics;
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
        if (e.key === "Enter") searchAll(); // Enter escalates the live filter (110)
    });
    $("chat-search-server").onclick = searchAll;

    // Emoji picker (95).
    $("chat-emoji").onclick = (e) => {
        e.stopPropagation();
        toggleEmojiPanel();
    };
    document.addEventListener("click", () => {
        closeEmojiPanel();
        closeReactStrip();
    });

    // File paste + drop + picker (98/100).
    $("chat-text").addEventListener("paste", (e) => {
        const files = [...(e.clipboardData?.files || [])];
        if (files.length === 0) {
            for (const item of [...(e.clipboardData?.items || [])]) {
                if (item.kind !== "file" || !item.type.startsWith("image/")) continue;
                const raw = item.getAsFile();
                if (!raw) continue;
                const ext = item.type.split("/")[1]?.replace("jpeg", "jpg") || "png";
                files.push(raw.name ? raw : new File([raw], `clipboard-${Date.now()}.${ext}`, { type: item.type }));
            }
        }
        if (files.length) {
            e.preventDefault();
            stageFiles(files);
        }
    });
    const wrap = $("chat-wrap");
    let dragDepth = 0; // dragenter/leave fire per child; a counter avoids flicker
    wrap.addEventListener("dragover", (e) => e.preventDefault());
    wrap.addEventListener("dragenter", (e) => {
        e.preventDefault();
        if (++dragDepth === 1) wrap.classList.add("drop-target");
    });
    wrap.addEventListener("dragleave", () => {
        if (--dragDepth <= 0) {
            dragDepth = 0;
            wrap.classList.remove("drop-target");
        }
    });
    wrap.addEventListener("drop", (e) => {
        e.preventDefault();
        dragDepth = 0;
        wrap.classList.remove("drop-target");
        if (e.dataTransfer?.files?.length) stageFiles(e.dataTransfer.files);
    });
    $("chat-attach").onclick = () => $("chat-file").click();
    $("chat-file").addEventListener("change", (e) => {
        if (e.target.files?.length) stageFiles(e.target.files);
        e.target.value = ""; // picking the same file twice must re-stage it
    });

    // @nick completion (106) + typing indicator (120).
    $("chat-text").addEventListener("keydown", (e) => {
        if (e.key === "ArrowUp" || e.key === "ArrowDown") recallSent(e);
        if (e.key === "Tab") handleTabComplete(e);
        else tabCycle = null;
    });
    $("chat-text").addEventListener("input", noteTyping);
    $("chat-text").addEventListener("blur", resetTypingOut);

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
        openChannelTab,
        setChannelSubscription,
        isSubscribed,
    });

    renderTabs();
    renderView();
}

export function unreadFor(chID) {
    return unread.get(chID) || null;
}
