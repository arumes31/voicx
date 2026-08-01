// voicx client frontend — voice ops console (vanilla JS).
// Wails bridge: window.go.main.App.<Method>(...) calls the Go backend;
// window.runtime.EventsOn(name, cb) receives backend events.

import "@fontsource-variable/sora";
import "@fontsource-variable/outfit";
import "@fontsource-variable/jetbrains-mono";
import { initMenu } from "./menu.js";
import { initSettingsUI } from "./settings-ui.js";
import { initClientInfo } from "./clientinfo.js";
import { initUpdater } from "./updater.js";
import { playEvent, beep, testAll } from "./sounds.js";
import {
    startMicMeter, stopMicMeter, pttRelease, makeLimiter, makeNormalizer,
    getUserVolume, isUserMuted, setUserMuted, registerUserChain, unregisterUserChain,
    setDucking,
} from "./audio.js";
import {
    initVideo, videoTrackAdded, videoTrackRemoved, videoSpeaking,
    videoRefreshNames, clearVideoGrid, shareToggle, setLowBandwidth, isLowBandwidth,
} from "./video.js";
import * as chatUI from "./chat-ui.js";
import { initPermsUI } from "./perms-ui.js";
import { initFilesUI } from "./files-ui.js";
import { initTabs } from "./tabs.js";
import { initSocialUI } from "./social-ui.js";
import { initMetaUI } from "./meta-ui.js";
import { initPolishUI } from "./polish-ui.js";
import { initNotifications } from "./notifications.js";
import { setLanguage, applyStaticLabels } from "./i18n.js";

const P = () => window.__voicxPerms;
window.__voicxChat = chatUI;

const $ = (id) => document.getElementById(id);

const state = {
    channels: [],   // flat: {ChannelID, ParentID, Name, HasIcon, Topic, MaxClients, OpusBitrate, OpusFEC, OpusDTX, OpusStereo}
    clients: [],    // flat: {client_id, unique_id, nickname, channel_id, is_speaking, priority_speaker}
    myClientID: "",
    myUniqueID: "",
    myNickname: "",
    myChannelID: 0,
    isAdmin: false,       // server admin flag from the auth response (6b UI gating)
    myPerms: new Map(),   // own resolved permissions: key -> {value, grant, skip, negate}
    serverGroups: [],     // server group list (6b tree presentation)
    groupByUID: new Map(), // unique_id -> [group entries, sorted by sort_id]
    groupIcons: new Map(), // group_id -> data url | null
    myPriority: false, // own priority-speaker flag
    selectedClientID: "",
    pc: null,
    localStream: null,
    screenSharing: false,
    muted: false,
    deafened: false,
    pttActive: false,
    collapsedChannels: new Set(), // (302) collapsed channel IDs
    expandedVirtual: new Set(),   // (349) user-expanded channels in windowed mode
    treeFilter: "",               // (319) live tree filter
    multiSelect: new Set(),       // (306) ctrl/shift-selected client IDs
    myStatus: "",                 // (307) own presence status
    micState: "unknown", // ok | none | denied
    avatars: new Map(),  // unique_id -> data url | null
    avatarPending: new Set(),
    settings: null,
    lastConnect: null,   // {addr, nick, pw, spw, bookmark} for reconnect-on-loss
    pendingBookmark: null, // (334) {name, addr} of the bookmark loaded into the login dialog
    reconnectAttempts: 0,
    reconnectTimer: null,
    vadMonitor: null,
    audioCtx: null,
    trackUsers: new Map(), // media track ID -> {client_id, unique_id, nickname} (per-publisher tracks; wave-3 video tiles)
    shareStream: null,     // getDisplayMedia result while screen sharing
    shareAudioSender: null, // RTCRtpSender of the optional share system-audio track
};

// ---------------------------------------------------------------------------
// Sounds are synthesized in sounds.js (sound packs); `beep` is the legacy
// helper kept for simple cues.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Toasts
// ---------------------------------------------------------------------------

// (385) "social" means join/leave only: the notify_join_leave toggle is
// labelled "Toasts for join/leave", so untagged callers default to "alert"
// and are never swallowed by it.
function toast(text, kind = "info", category = "alert") {
    const s = state.settings;
    if (s) {
        if (category === "social" && !s.notify_join_leave) return;
        if (category === "conn" && !s.notify_connection) return;
    }
    // (346) record into the notification center; (347/348) DND suppresses
    // visible toasts but still records them silently.
    window.__voicxPolish?.recordNotification(kind, text, {});
    if (window.__voicxPolish?.dndActive?.()) return;
    const el = document.createElement("div");
    el.className = "toast " + kind;
    el.textContent = text;
    $("toasts").appendChild(el);
    setTimeout(() => {
        el.classList.add("out");
        setTimeout(() => el.remove(), 300);
    }, 4000);
}

// ---------------------------------------------------------------------------
// Appearance (294-297): theme, accent, user CSS, UI font, compact mode.
// Applied at startup and after settings changes.
// ---------------------------------------------------------------------------

function applyAppearance() {
    const s = state.settings || {};
    const root = document.documentElement;
    // (336) language applies live (menus + static labels rebuild).
    setLanguage(s.language || "system");
    applyStaticLabels();
    initMenu();
    // (294/295) theme sets the full variable palette via [data-theme].
    root.dataset.theme = s.theme || "dark";
    // (344) reduce motion: toggle + media query both gate animations.
    root.dataset.reduceMotion = s.reduce_motion ? "1" : "";
    // (296) custom accent overrides the theme's accent.
    if (s.accent_color) root.style.setProperty("--accent", s.accent_color);
    else root.style.removeProperty("--accent");
    // (296) scoped user CSS overrides.
    let userStyle = document.getElementById("user-css");
    if (s.user_css) {
        if (!userStyle) {
            userStyle = document.createElement("style");
            userStyle.id = "user-css";
            document.head.appendChild(userStyle);
        }
        userStyle.textContent = s.user_css;
    } else if (userStyle) {
        userStyle.remove();
    }
    // (297) UI font family + base size.
    const fonts = {
        outfit: '"Outfit Variable", sans-serif',
        sora: '"Sora Variable", sans-serif',
        jetbrains: '"JetBrains Mono Variable", monospace',
    };
    root.style.setProperty("--font-body", fonts[s.ui_font] || fonts.outfit);
    root.style.fontSize = (s.ui_font_size || 14) + "px";
    // (293) restore compact mode.
    document.body.classList.toggle("compact", !!s.compact_mode);
}

// ---------------------------------------------------------------------------
// Login / connection
// ---------------------------------------------------------------------------

(async () => {
    try {
        state.settings = await window.go.main.App.GetSettings();
        // (88) reflect the persisted low-bandwidth mode in the voice bar.
        if (state.settings?.low_bandwidth) setLowBandwidth(true, false);
    } catch {
        state.settings = null;
    }
    applyAppearance();
    try {
        const uid = await window.go.main.App.IdentityUID();
        if (uid) $("login-identity").textContent = uid.slice(0, 12) + "…";
    } catch { /* identity display is best-effort */ }
    try {
        const v = await window.go.main.App.ClientVersionShort();
        $("login-version").textContent = "voicx " + v;
        state.clientVersion = v;
    } catch { /* version display is best-effort */ }
    document.querySelector(".login-card").classList.add("in");
})();

function showLogin() {
    // (334) the dialog is retargetable, so a stash from an earlier bookmark
    // must never identify the next login — a same-address login can be a
    // different account (callers loading a bookmark stash after this call).
    state.pendingBookmark = null;
    $("login-overlay").classList.remove("hidden");
    $("app").classList.add("hidden");
    $("conn-lock").classList.add("hidden");
}

$("login-connect").onclick = () => connectFromLogin();

async function connectFromLogin() {
    const addr = $("login-addr").value.trim();
    const nick = $("login-nick").value.trim();
    const pw = $("login-password").value;
    const spw = $("login-serverpw").value;
    $("login-error").textContent = "";
    // (334) a nickname override replaces the login nickname, so addr+nickname
    // no longer identifies the bookmark this login came from: forward the name
    // the bookmark menu stashed, unless the dialog was retargeted since.
    const bookmark = state.pendingBookmark?.addr === addr ? state.pendingBookmark.name : "";
    try {
        const err = await window.go.main.App.ConnectBookmarkTab(bookmark, addr, nick, pw, spw);
        if (err) {
            // (4a) TOFU fingerprint mismatch: prominent warning + explicit
            // trust action — never silently accepted.
            if (err.startsWith("tls fingerprint mismatch")) {
                showFingerprintWarning(addr, err);
                return;
            }
            $("login-error").textContent = err;
            return;
        }
        state.myNickname = nick;
        state.lastConnect = { addr, nick, pw, spw, bookmark };
        // (334) consumed: lastConnect carries the name for reconnects from
        // here on. Clearing only on success keeps the retry after a rejected
        // password identifiable.
        state.pendingBookmark = null;
        state.reconnectAttempts = 0;
        try {
            state.myClientID = await window.go.main.App.ClientID();
        } catch { /* client-id display/ducking degrade gracefully */ }
        try {
            state.isAdmin = await window.go.main.App.IsAdmin();
        } catch { state.isAdmin = false; }
        $("conn-pill").textContent = addr;
        $("conn-pill").classList.add("up");
        $("conn-lock").classList.remove("hidden");
        $("login-overlay").classList.add("hidden");
        $("app").classList.remove("hidden");
        refreshPermissions();
        applyWhisperSettings();
        startupAutoCheck();
        chatUI.onConnect(); // (133) MOTD + myUniqueID for mentions/own-msgs
        // (6b) group memberships drive tree colors/hoisted sections.
        P().refreshGroups().then(() => renderTree());
        window.__voicxFiles.loadServerIcon(); // (270) server icon in the sidebar
        window.__voicxSocial.refreshNews(); // (313) server news pane
        startQualitySampler(); // (333) connection quality pill
        noteActivity(); // (308) auto-away timer starts at connect
        // (4a) surface the connection security as an info line.
        try {
            const sec = await window.go.main.App.ConnectionSecurity();
            sysMsg("connected: " + sec);
        } catch { /* best-effort */ }
    } catch (e) {
        $("login-error").textContent = String(e);
    }
}

// showFingerprintWarning displays the TOFU mismatch dialog (4a): the server
// certificate changed since the pinned fingerprint — possible MITM. The user
// may explicitly trust the new fingerprint, which updates known_servers.json
// and retries the connect.
async function showFingerprintWarning(addr, detail) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg share-dlg fp-warn">
            <h3>⚠ Server certificate changed</h3>
            <p class="dlg-text">The TLS fingerprint of <span class="mono"></span> does not match the pinned one. This could be a man-in-the-middle attack — or a legitimate server reinstall.</p>
            <p class="dlg-text mono fp-detail"></p>
            <div class="dlg-buttons">
                <button class="dlg-cancel">Abort</button>
                <button class="dlg-ok danger-btn">Trust new fingerprint</button>
            </div>
        </div>`;
    overlay.querySelector("span.mono").textContent = addr;
    overlay.querySelector(".fp-detail").textContent = detail;
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.querySelector(".dlg-ok").onclick = async () => {
        const fp = await window.go.main.App.ServerFingerprint();
        const err = await window.go.main.App.TrustServerFingerprint(addr, fp);
        overlay.remove();
        if (err) {
            $("login-error").textContent = "trust failed: " + err;
            return;
        }
        connectFromLogin();
    };
    document.body.appendChild(overlay);
}

async function disconnect() {
    state.lastConnect = null; // intentional disconnect: no reconnect
    if (state.reconnectTimer) {
        clearTimeout(state.reconnectTimer);
        state.reconnectTimer = null;
    }
    await window.go.main.App.Disconnect();
}

window.runtime.EventsOn("disconnected", () => {
    if (state.settings?.notify_connection !== false) toast("Connection lost", "warn", "conn");
    sysMsg("disconnected from server");
    teardownVoice();
    resetVoiceUI();
    stopQualitySampler();
    $("conn-pill").textContent = "offline";
    $("conn-pill").classList.remove("up");

    // Reconnect on connection loss (Application setting): 5 tries, 5s apart.
    if (state.settings?.reconnect_on_loss && state.lastConnect && state.reconnectAttempts < 5) {
        state.reconnectAttempts++;
        sysMsg(`reconnecting in 5s (attempt ${state.reconnectAttempts}/5)…`);
        // (332) reconnect timeline in the status pill.
        $("conn-pill").textContent = `retry ${state.reconnectAttempts}/5 in 5s…`;
        let countdown = 4;
        const tick = setInterval(() => {
            if (countdown <= 0 || !state.reconnectTimer) {
                clearInterval(tick);
                return;
            }
            $("conn-pill").textContent = `retry ${state.reconnectAttempts}/5 in ${countdown--}s…`;
        }, 1000);
        state.reconnectTimer = setTimeout(async () => {
            clearInterval(tick);
            const c = state.lastConnect;
            if (!c) return;
            const err = await window.go.main.App.ConnectBookmarkTab(c.bookmark || "", c.addr, c.nick, c.pw, c.spw);
            if (err === "") {
                try {
                    state.myClientID = await window.go.main.App.ClientID();
                } catch { /* best-effort */ }
                try {
                    state.isAdmin = await window.go.main.App.IsAdmin();
                } catch { state.isAdmin = false; }
                $("conn-pill").textContent = c.addr;
                $("conn-pill").classList.add("up");
                $("login-overlay").classList.add("hidden");
                $("app").classList.remove("hidden");
                refreshPermissions();
                applyWhisperSettings();
                chatUI.onConnect();
                P().refreshGroups().then(() => renderTree());
            }
        }, 5000);
        return;
    }
    showLogin();
});

window.runtime.EventsOn("servererror", (msg) => sysMsg("server: " + msg));

// (282) the Go side maintains settings of its own (recents on every connect),
// so the merged blob it pushes is the authoritative cache — without this the
// recents list stays frozen at the value read once at startup.
window.runtime.EventsOn("settings_update", (s) => { state.settings = s; });

// (320) recordRecentChannel tracks the last 5 joined channels per server
// address in settings (debounced persist).
function recordRecentChannel(channelID) {
    const s = state.settings;
    if (!s || !channelID || !state.lastConnect) return;
    s.recent_channels = s.recent_channels || {};
    const key = state.lastConnect.addr;
    const list = [channelID, ...(s.recent_channels[key] || []).filter((x) => x !== channelID)];
    s.recent_channels[key] = list.slice(0, 5);
    debouncedSaveSettings();
}

// (320) recentChannels returns the current server's recent channel IDs.
function recentChannels() {
    if (!state.lastConnect || !state.settings?.recent_channels) return [];
    return state.settings.recent_channels[state.lastConnect.addr] || [];
}

// ---------------------------------------------------------------------------
// Presence: auto-away (308)
// ---------------------------------------------------------------------------

let idleTimer = null;

// noteActivity resets the idle timer; after auto_away_minutes without input
// the client sets itself away, restoring on the next activity.
function noteActivity() {
    if (state.myStatus === "away") {
        state.myStatus = "";
        window.go.main.App.SetStatus("online", "");
    }
    if (idleTimer) clearTimeout(idleTimer);
    const minutes = state.settings?.auto_away_minutes ?? 15;
    if (minutes <= 0) return;
    idleTimer = setTimeout(() => {
        if (!state.myClientID) return;
        state.myStatus = "away";
        window.go.main.App.SetStatus("away", "auto-away");
        sysMsg("auto-away after " + minutes + " min idle");
    }, minutes * 60000);
}

["mousemove", "keydown", "mousedown"].forEach((ev) =>
    document.addEventListener(ev, noteActivity, { passive: true }));

// ---------------------------------------------------------------------------
// Connection quality pill (333) + reconnect timeline (332)
// ---------------------------------------------------------------------------

// qualityFromPing classifies the connection from the smoothed RTT: good
// <100ms, fair <250ms, poor above (no RTT yet = unknown).
function qualityFromPing(pingMs, known) {
    if (!known) return "";
    if (pingMs < 100) return "good";
    if (pingMs < 250) return "fair";
    return "poor";
}

let qualityTimer = null;

function startQualitySampler() {
    stopQualitySampler();
    qualityTimer = setInterval(async () => {
        if (!state.myClientID) return;
        try {
            const info = await window.go.main.App.GetClientInfo(state.myClientID);
            const q = qualityFromPing(info.ping_ms, info.ping_ms >= 0);
            const pill = $("conn-pill");
            pill.dataset.quality = q;
            if (q) pill.title = `connection quality: ${q} (RTT ${info.ping_ms} ms)`;
        } catch { /* transient */ }
    }, 5000);
}

function stopQualitySampler() {
    if (qualityTimer) {
        clearInterval(qualityTimer);
        qualityTimer = null;
    }
}

// ---------------------------------------------------------------------------
// Snapshot & events
// ---------------------------------------------------------------------------

// (307) client_id -> channel_id at the time of user_left: going invisible and
// coming back is announced as leave+join, and user_joined has no channel.
// Only a returning client consumes its entry and the snapshot arrives once at
// login, so the map is capped: clients that leave for good would otherwise
// accumulate for the whole session.
const lastKnownChannel = new Map();
const LAST_CHANNEL_MAX = 200;

window.runtime.EventsOn("snapshot", (json) => {
    const snap = JSON.parse(json);
    // broadcast.ClientInfo does not serialize priority_speaker, so carry the
    // flags over from the previous state (they arrive via
    // priority_speaker_changed events).
    const prioByID = new Map(state.clients.filter((c) => c.priority_speaker).map((c) => [c.client_id, true]));
    state.channels = [];
    state.clients = [];
    lastKnownChannel.clear(); // the snapshot is authoritative
    for (const root of snap.root_channels || []) flattenChannel(root);
    for (const c of state.clients) {
        if (prioByID.has(c.client_id)) c.priority_speaker = true;
    }
    // (317) blocked users are locally muted on sight; (318) contact nickname
    // history updates from presence; (383) buddy alerts; (389) channel watch.
    applyBlockAndContacts();
    resolveTrackUsers();
    window.__voicxNotify?.checkBuddyOnline();
    window.__voicxNotify?.checkChannelWatch();
    renderTree();
});

// applyBlockAndContacts mutes blocked users locally and records contact
// nickname history from the fresh snapshot (317/318). Settings persist is
// debounced.
function applyBlockAndContacts() {
    const s = state.settings;
    if (!s) return;
    for (const uid of s.blocked_users || []) {
        setUserMuted(uid, true);
    }
    let dirty = false;
    for (const contact of s.contacts || []) {
        const online = state.clients.find((c) => c.unique_id === contact.unique_id);
        if (!online || !online.nickname) continue;
        contact.nick_history = contact.nick_history || [];
        if (contact.nick_history[contact.nick_history.length - 1] !== online.nickname) {
            contact.nick_history.push(online.nickname);
            if (contact.nick_history.length > 8) contact.nick_history.shift();
            dirty = true;
        }
    }
    if (dirty) debouncedSaveSettings();
}

let saveSettingsTimer = null;

// debouncedSaveSettings persists settings at most once per 2s (318/320).
function debouncedSaveSettings() {
    if (saveSettingsTimer) clearTimeout(saveSettingsTimer);
    saveSettingsTimer = setTimeout(() => {
        if (state.settings) window.go.main.App.SaveSettings(state.settings);
    }, 2000);
}

function flattenChannel(node) {
    state.channels.push({
        ChannelID: node.ChannelID,
        ParentID: node.ParentID,
        Name: node.Name,
        HasIcon: !!node.HasIcon,
        // (304) lock icon — the field is read under both spellings so a json
        // tag on the server's Channel struct cannot silently drop the lock.
        HasPassword: !!(node.has_password ?? node.HasPassword),
        ClientCount: node.ClientCount || 0, // (303) [n/max]
        Topic: node.Topic || "",
        Description: node.Description || "",
        MaxClients: node.MaxClients || 0,
        OpusBitrate: node.OpusBitrate || 0,
        OpusFEC: !!node.OpusFEC,
        OpusDTX: !!node.OpusDTX,
        OpusStereo: !!node.OpusStereo,
    });
    for (const c of node.clients || []) state.clients.push(c);
    for (const child of node.children || []) flattenChannel(child);
}

window.runtime.EventsOn("channellist", (json) => {
    const list = JSON.parse(json);
    for (const ch of list.channels || []) {
        if (!state.channels.find((c) => c.ChannelID === Number(ch.id))) {
            state.channels.push({ ChannelID: Number(ch.id), ParentID: 0, Name: ch.name, HasIcon: false });
        }
    }
    renderTree();
});

window.runtime.EventsOn("event", (json) => {
    const env = JSON.parse(json);
    const d = env.data || {};
    switch (env.type) {
        case "user_joined": {
            // (307) the server omits channel_id on user_joined, and an
            // invisible→visible return is announced as leave+join: restore the
            // channel remembered from the leave so the user is not stranded in
            // channel 0 ("no channel") until the next user_moved.
            const joinedChannel = d.channel_id ?? lastKnownChannel.get(d.client_id) ?? 0;
            lastKnownChannel.delete(d.client_id);
            state.clients.push({ client_id: d.client_id, unique_id: d.unique_id, nickname: d.nickname, channel_id: joinedChannel, is_speaking: false });
            if (d.client_id !== state.myClientID) {
                chatUI.sysJoinLeave(d.nickname || d.unique_id || "someone", "joined"); // (130/131)
                if (joinedChannel === state.myChannelID && state.myChannelID !== 0) {
                    // (385) joins in my channel dispatch through the matrix.
                    window.__voicxNotify?.notify("join_leave", (d.nickname || "someone") + " joined your channel",
                        { channelID: state.myChannelID, className: "joins", kind: "info" });
                }
            }
            break;
        }
        case "user_left": {
            const was = state.clients.find((c) => c.client_id === d.client_id);
            state.clients = state.clients.filter((c) => c.client_id !== d.client_id);
            if (was) {
                if (was.channel_id) {
                    lastKnownChannel.set(d.client_id, was.channel_id);
                    // insertion order: drop the oldest unclaimed entry first.
                    while (lastKnownChannel.size > LAST_CHANNEL_MAX) {
                        lastKnownChannel.delete(lastKnownChannel.keys().next().value);
                    }
                }
                chatUI.sysJoinLeave(was.nickname || was.unique_id || "someone", "left"); // (130/131)
                if (was.channel_id === state.myChannelID && state.myChannelID !== 0) {
                    window.__voicxNotify?.notify("join_leave", (was.nickname || "someone") + " left your channel",
                        { channelID: state.myChannelID, className: "joins", kind: "info" });
                }
            }
            videoTrackRemoved(d.client_id);
            recomputeDucking();
            break;
        }
        case "user_moved": {
            const c = state.clients.find((c) => c.client_id === d.client_id);
            if (c) c.channel_id = d.channel_id;
            if (d.client_id === state.myClientID) {
                state.myChannelID = d.channel_id;
                applyChannelAudio();
                chatUI.onMyChannelChanged(); // (103/111) load history + header for the new channel
                window.__voicxFiles?.onChannelChanged?.(); // (256) file browser follows the channel
                recordRecentChannel(d.channel_id); // (320) recent channels
            } else if (d.channel_id === state.myChannelID && state.myChannelID !== 0 && c) {
                window.__voicxNotify?.notify("join_leave", (c.nickname || "someone") + " joined your channel",
                    { channelID: state.myChannelID, className: "joins", kind: "info" });
            }
            recomputeDucking();
            break;
        }
        case "channel_created":
            state.channels.push({ ChannelID: d.channel_id, ParentID: d.parent_id || 0, Name: d.name, HasIcon: false });
            break;
        case "channel_deleted":
            state.channels = state.channels.filter((c) => c.ChannelID !== d.channel_id);
            break;
        case "channel_updated": {
            const ch = state.channels.find((c) => c.ChannelID === d.channel_id);
            if (ch) {
                ch.Topic = d.topic || "";
                ch.Description = d.description || "";
                ch.MaxClients = d.max_clients || 0;
                ch.OpusBitrate = d.opus_bitrate || 0;
                ch.OpusFEC = !!d.opus_fec;
                ch.OpusDTX = !!d.opus_dtx;
                ch.OpusStereo = !!d.opus_stereo;
                if (d.channel_id === state.myChannelID) applyChannelAudio();
            }
            break;
        }
        case "priority_speaker_changed": {
            const c = state.clients.find((c) => c.client_id === d.client_id);
            if (c) c.priority_speaker = d.active;
            if (d.client_id === state.myClientID) {
                state.myPriority = d.active;
                $("voice-prio").classList.toggle("active", d.active);
            }
            sysMsg(clientName(d.client_id) + (d.active ? " is now a priority speaker" : " is no longer a priority speaker"));
            recomputeDucking();
            break;
        }
        // (307-309) presence status updates.
        case "status_changed": {
            const c = state.clients.find((c) => c.client_id === d.client_id);
            if (c) {
                c.status = d.status || "";
                c.status_message = d.message || "";
            }
            break;
        }
        // (321/322) poke: toast + sound + taskbar flash.
        case "poke":
            // (385) pokes dispatch through the notification matrix.
            window.__voicxNotify?.notify("poke",
                `poke from ${d.from_nickname || "someone"}${d.message ? ": " + d.message : ""}`,
                { className: "messages", kind: "warn" });
            break;
        case "speaking_changed": {
            const c = state.clients.find((c) => c.client_id === d.client_id);
            if (c) c.is_speaking = d.speaking;
            // (343) announce speaking events to the screen-reader region.
            if (d.speaking) window.__voicxPolish?.announce((c ? c.nickname || c.unique_id : "someone") + " started speaking");
            updateTalkBanner();
            videoSpeaking(d.client_id, d.speaking);
            recomputeDucking();
            break;
        }
        case "chat":
            addChat(d);
            return;
        // wave-5b chat events — handled by chat-ui.js; no tree refresh needed
        // (chat-ui triggers renderTree itself when unread badges change).
        case "chat_edited":
            chatUI.onChatEdited(d);
            return;
        case "chat_deleted":
            chatUI.onChatDeleted(d);
            return;
        case "chat_pinned":
            chatUI.onChatPinned(d, true);
            return;
        case "chat_unpinned":
            chatUI.onChatPinned(d, false);
            return;
        case "chat_reaction":
            chatUI.onChatReaction(d);
            return;
        case "dm_delivered":
            chatUI.onDelivered(d);
            return;
        case "dm_read":
            chatUI.onRead(d);
            return;
        case "emoji_added":
            chatUI.onEmojiAdded();
            return;
        case "announcement":
            chatUI.onAnnouncement(d);
            return;
        case "kicked":
            // (385) kicks dispatch through the notification matrix.
            window.__voicxNotify?.notify("kick", "client kicked" + (d.reason ? ": " + d.reason : ""),
                { className: "messages", kind: "warn" });
            if (state.settings?.sys_kick !== false) { // (130) category filter
                sysMsg("client " + d.client_id + " was kicked" + (d.reason ? " (" + d.reason + ")" : ""));
            }
            break;
        case "screenshare_changed":
            sysMsg(clientName(d.client_id) + (d.active ? " started" : " stopped") + " screen sharing");
            break;
        case "avatar_changed":
            state.avatars.delete(d.unique_id);
            fetchAvatar(d.unique_id);
            videoRefreshNames();
            break;
        // wave 6b group events: memberships drive tree colors/hoisted
        // sections and the details-pane group chips.
        case "group_assigned":
            if (d.group_name) {
                toast((d.by ? "you were " : "") + "added to group " + d.group_name, "info", "alert");
            }
            P().refreshGroups().then(() => renderTree());
            break;
        case "group_unassigned":
            if (d.group_name) {
                toast("removed from group " + d.group_name, "info", "alert");
            }
            P().refreshGroups().then(() => renderTree());
            break;
        case "group_expired":
            toast("a timed group membership expired", "info", "alert");
            P().refreshGroups().then(() => renderTree());
            break;
    }
    resolveTrackUsers();
    // (389) the snapshot arrives once at login, so only the live join/leave/
    // move events can ever show a channel crossing its watch threshold.
    window.__voicxNotify?.checkChannelWatch();
    renderTree();
    videoRefreshNames();
});

// ---------------------------------------------------------------------------
// Channel tree
// ---------------------------------------------------------------------------

// (303) recomputeClientCounts derives the [n/max] badge counts from the live
// client list: the snapshot's ClientCount is a point-in-time value that
// user_joined/left/moved never refresh. The hover card (8b) and windowed mode
// (349) read the same field.
function recomputeClientCounts() {
    const counts = new Map();
    for (const c of state.clients) counts.set(c.channel_id, (counts.get(c.channel_id) || 0) + 1);
    for (const ch of state.channels) ch.ClientCount = counts.get(ch.ChannelID) || 0;
}

function renderTree() {
    recomputeClientCounts();
    const root = $("channel-tree");
    root.innerHTML = "";
    // (179) Hoisted server groups render as their own sections above the
    // channels; (177) section headers carry the group icon; (178) nickname
    // colors come from the first applicable group (clientRow).
    for (const h of P().hoistedGroups()) {
        const sec = document.createElement("div");
        sec.className = "hoist-section";
        const head = document.createElement("div");
        head.className = "hoist-head";
        head.innerHTML = `<span class="hoist-icon"></span><span class="hoist-name"></span>`;
        head.querySelector(".hoist-name").textContent = h.group.name;
        if (h.group.color) head.style.color = h.group.color;
        if (h.group.icon) {
            P().groupIconURL(h.group.id).then((url) => {
                if (url) head.querySelector(".hoist-icon").innerHTML = `<img src="${url}" alt="">`;
            });
        }
        sec.appendChild(head);
        for (const c of h.members) sec.appendChild(clientRow(c));
        root.appendChild(sec);
    }
    if (state.channels.length === 0) {
        const empty = document.createElement("div");
        empty.className = "empty-state";
        empty.textContent = "No channels yet";
        root.appendChild(empty);
    }
    const byParent = new Map();
    for (const ch of state.channels) {
        const key = ch.ParentID || 0;
        if (!byParent.has(key)) byParent.set(key, []);
        byParent.get(key).push(ch);
    }
    // (319) live tree filter: matching channels/users stay; a channel stays
    // when it or a descendant or one of its users matches.
    if (state.treeFilter) {
        const q = state.treeFilter.toLowerCase();
        const keep = new Set();
        const matchCh = (ch) => {
            let ok = ch.Name.toLowerCase().includes(q) ||
                state.clients.some((c) => c.channel_id === ch.ChannelID &&
                    (c.nickname || c.unique_id || "").toLowerCase().includes(q));
            for (const child of byParent.get(ch.ChannelID) || []) ok = matchCh(child) || ok;
            if (ok) keep.add(ch.ChannelID);
            return ok;
        };
        for (const ch of byParent.get(0) || []) matchCh(ch);
        for (const [p, list] of byParent) {
            byParent.set(p, list.filter((ch) => keep.has(ch.ChannelID)));
        }
    }
    for (const ch of byParent.get(0) || []) renderChannel(root, ch, byParent, 0);
    renderClientCard();
    chatUI.refreshHeader(); // (111) topic/title follows tree + channel updates
}

function renderChannel(parentEl, ch, byParent, depth) {
    const el = document.createElement("div");
    el.className = "channel" + (ch.ChannelID === state.myChannelID ? " mine" : "");
    el.style.setProperty("--depth", depth);
    el.dataset.chid = ch.ChannelID;
    el.tabIndex = 0; // (298) keyboard navigation
    el.setAttribute("role", "treeitem"); // (343)
    el.setAttribute("aria-label", "channel " + ch.Name + (ch.HasPassword ? ", password protected" : ""));
    el.innerHTML = `<span class="ch-icon">${ch.HasIcon ? "◈" : "#"}</span><span class="ch-name"></span>`;
    el.querySelector(".ch-name").textContent = ch.Name;
    // (387) muted channel icon.
    if (window.__voicxNotify?.channelOverride?.(ch.ChannelID)?.muted) {
        const mute = document.createElement("span");
        mute.className = "ch-lock";
        mute.textContent = " 🔕";
        mute.title = "channel muted (notifications off)";
        el.querySelector(".ch-name").appendChild(mute);
    }
    // (304) password lock; (303) client count [n/max].
    if (ch.HasPassword) {
        const lock = document.createElement("span");
        lock.className = "ch-lock";
        lock.textContent = " 🔒";
        lock.title = "password-protected channel";
        el.querySelector(".ch-name").appendChild(lock);
    }
    if (ch.ClientCount > 0 || ch.MaxClients > 0) {
        const count = document.createElement("span");
        count.className = "ch-count mono";
        count.textContent = `[${ch.ClientCount}${ch.MaxClients > 0 ? "/" + ch.MaxClients : ""}]`;
        el.appendChild(count);
    }
    // (104) unread badge; accent variant when it includes a mention of me.
    const ub = chatUI.unreadFor(ch.ChannelID);
    if (ub && ub.n > 0) {
        const badge = document.createElement("span");
        badge.className = "ch-unread" + (ub.mention ? " mention" : "");
        badge.textContent = ub.n > 99 ? "99+" : ub.n;
        el.appendChild(badge);
    }
    el.onclick = () => window.go.main.App.JoinChannel(ch.ChannelID);
    // (305) drag users onto a channel to move them there.
    el.addEventListener("dragover", (e) => {
        if (e.dataTransfer.types.includes("text/voicx-uid")) {
            e.preventDefault();
            el.classList.add("drop-active");
        }
    });
    el.addEventListener("dragleave", () => el.classList.remove("drop-active"));
    el.addEventListener("drop", (e) => {
        e.preventDefault();
        el.classList.remove("drop-active");
        const uid = e.dataTransfer.getData("text/voicx-uid");
        const target = state.clients.find((c) => c.unique_id === uid);
        if (!target) return;
        if (target.client_id === state.myClientID) {
            window.go.main.App.JoinChannel(ch.ChannelID);
        } else {
            window.go.main.App.MoveClient(target.client_id, ch.ChannelID);
        }
    });
    parentEl.appendChild(el);

    // (302) collapsed channels hide their members and children; (349) in
    // windowed mode every channel off my branch counts as collapsed unless
    // the user explicitly expanded it (double-click).
    let collapsed = state.collapsedChannels.has(ch.ChannelID);
    if (window.__voicxPolish?.virtualizeEnabled?.()) {
        collapsed = !window.__voicxPolish.myBranchIDs().has(ch.ChannelID) &&
            !state.expandedVirtual.has(ch.ChannelID);
    }
    if (!collapsed) {
        for (const c of state.clients.filter((c) => c.channel_id === ch.ChannelID)) {
            el.appendChild(clientRow(c));
        }
        for (const child of byParent.get(ch.ChannelID) || []) renderChannel(el, child, byParent, depth + 1);
    }
}

function clientRow(c) {
    const row = document.createElement("div");
    row.className = "client" + (c.is_speaking ? " speaking" : "") +
        (state.multiSelect.has(c.client_id) ? " selected" : "") +
        (c.status === "away" || c.status === "busy" || c.status === "invisible" ? " " + c.status : "");
    row.dataset.clid = c.client_id;
    row.tabIndex = 0; // (298) keyboard navigation
    row.setAttribute("role", "treeitem"); // (343)
    row.setAttribute("aria-label", (c.nickname || c.unique_id || "user") +
        (c.status ? ", " + c.status : "") + (c.is_speaking ? ", speaking" : ""));
    // (140/305) users are draggable (group assign in the manager; move by
    // dropping onto a channel).
    if (c.unique_id) {
        row.draggable = true;
        row.addEventListener("dragstart", (e) => {
            e.dataTransfer.setData("text/voicx-uid", c.unique_id);
            e.dataTransfer.effectAllowed = "copy";
        });
    }
    const av = document.createElement("span");
    av.className = "avatar";
    av.dataset.uid = c.unique_id;
    const dataUrl = state.avatars.get(c.unique_id);
    if (dataUrl) {
        av.innerHTML = `<img src="${dataUrl}" alt="">`;
    } else {
        av.textContent = initials(c.nickname || c.unique_id || "?");
        fetchAvatar(c.unique_id);
    }
    const name = document.createElement("span");
    name.className = "client-name";
    name.textContent = (c.nickname || c.unique_id) + (c.client_id === state.myClientID ? " (you)" : "");
    // (178) nickname color from the first applicable server group.
    const gColor = P().groupColorFor(c.unique_id);
    if (gColor) name.style.color = gColor;
    const g = P().primaryGroup(c.unique_id);
    if (g) name.title = "group: " + g.name;
    row.appendChild(av);
    row.appendChild(name);
    // (310) group icon next to the name when the primary group has one.
    if (g && g.icon) {
        const gi = document.createElement("span");
        gi.className = "group-icon";
        P().groupIconURL(g.id).then((url) => {
            if (url) gi.innerHTML = `<img src="${url}" alt="" title="${g.name}">`;
        });
        row.appendChild(gi);
    }
    // (307-309) presence status icons; (381) invisible marker (admin view).
    if (c.status === "away" || c.status === "busy" || c.status === "invisible") {
        const st = document.createElement("span");
        st.className = "status-icons";
        st.textContent = c.status === "away" ? " 🕐" : c.status === "busy" ? " ⛔" : " 👻";
        st.title = c.status + (c.status_message ? ": " + c.status_message : "");
        row.appendChild(st);
    }
    // (10) Own status icons: muted / deafened / screen sharing.
    if (c.client_id === state.myClientID) {
        const icons = document.createElement("span");
        icons.className = "status-icons";
        let txt = "";
        if (state.muted) txt += " 🔇";
        if (state.deafened) txt += " 🙉";
        if (state.screenSharing) txt += " 🖥";
        icons.textContent = txt;
        row.appendChild(icons);
        // (347) DND shows on own status icons.
        if (window.__voicxPolish?.dndActive?.()) {
            const dnd = document.createElement("span");
            dnd.className = "status-icons";
            dnd.textContent = " 🌙";
            dnd.title = "do not disturb active";
            row.appendChild(dnd);
        }
    } else if (isUserMuted(c.unique_id)) {
        // (2) Local-mute icon for users muted locally by me.
        const icons = document.createElement("span");
        icons.className = "status-icons";
        icons.textContent = " 🔕";
        icons.title = "muted locally";
        row.appendChild(icons);
    }
    row.onclick = (e) => {
        e.stopPropagation();
        // (306) ctrl/shift multi-select; plain click selects just this user.
        if (e.ctrlKey || e.metaKey) {
            if (state.multiSelect.has(c.client_id)) state.multiSelect.delete(c.client_id);
            else state.multiSelect.add(c.client_id);
        } else if (e.shiftKey && state.selectedClientID) {
            const rows = state.clients.filter((x) => x.channel_id === c.channel_id);
            const ids = rows.map((x) => x.client_id);
            const a = ids.indexOf(state.selectedClientID);
            const b = ids.indexOf(c.client_id);
            if (a >= 0 && b >= 0) {
                for (const id of ids.slice(Math.min(a, b), Math.max(a, b) + 1)) state.multiSelect.add(id);
            }
        } else {
            state.multiSelect = new Set([c.client_id]);
        }
        state.selectedClientID = c.client_id;
        renderTree();
    };
    return row;
}

function initials(name) {
    const parts = name.trim().split(/\s+/);
    return (parts[0][0] + (parts.length > 1 ? parts[1][0] : "")).toUpperCase();
}

function clientName(clientID) {
    const c = state.clients.find((c) => c.client_id === clientID);
    return c ? (c.nickname || c.unique_id) : clientID;
}

// ---------------------------------------------------------------------------
// Avatars
// ---------------------------------------------------------------------------

async function fetchAvatar(uniqueID) {
    if (!uniqueID || state.avatars.has(uniqueID) || state.avatarPending.has(uniqueID)) return;
    state.avatarPending.add(uniqueID);
    try {
        const data = await window.go.main.App.GetAvatar(uniqueID);
        if (data && data.data_base64) {
            state.avatars.set(uniqueID, `data:${data.content_type};base64,${data.data_base64}`);
        } else {
            state.avatars.set(uniqueID, null);
        }
    } catch {
        state.avatars.set(uniqueID, null);
    }
    state.avatarPending.delete(uniqueID);
    document.querySelectorAll(`.avatar[data-uid="${CSS.escape(uniqueID)}"]`).forEach((el) => {
        const url = state.avatars.get(uniqueID);
        if (url) el.innerHTML = `<img src="${url}" alt="">`;
    });
    renderClientCard();
}

// ---------------------------------------------------------------------------
// Client card (details pane)
// ---------------------------------------------------------------------------

function renderClientCard() {
    const el = $("client-card");
    const c = state.clients.find((c) => c.client_id === state.selectedClientID);
    if (!c) {
        el.innerHTML = `<div class="empty-state">Select a user</div>`;
        return;
    }
    const ch = state.channels.find((x) => x.ChannelID === c.channel_id);
    const dataUrl = state.avatars.get(c.unique_id);
    el.innerHTML = `
        <div class="card-avatar ${c.is_speaking ? "speaking" : ""}">
            ${dataUrl ? `<img src="${dataUrl}" alt="">` : `<span>${initials(c.nickname || c.unique_id || "?")}</span>`}
        </div>
        <div class="card-nick"></div>
        <div class="card-uid mono"></div>
        <div class="card-channel"></div>
        <div class="card-groups"></div>`;
    el.querySelector(".card-nick").textContent = c.nickname || c.unique_id;
    el.querySelector(".card-uid").textContent = (c.unique_id || "").slice(0, 16) + "…";
    el.querySelector(".card-channel").textContent = ch ? "in " + ch.Name : "no channel";
    // (143-145) the user's server groups (from wave-6b membership data).
    const chips = el.querySelector(".card-groups");
    for (const g of state.groupByUID?.get(c.unique_id) || []) {
        const chip = document.createElement("span");
        chip.className = "group-chip";
        chip.textContent = g.name;
        if (g.color) {
            chip.style.borderColor = g.color;
            chip.style.color = g.color;
        }
        chips.appendChild(chip);
    }
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

function addChat(d) {
    // (317) blocked users' messages are hidden.
    if (d.from_unique_id && (state.settings?.blocked_users || []).includes(d.from_unique_id)) {
        return;
    }
    // File logging per scope (Chat setting) — kept from the pre-5b renderer.
    const scope = d.channel_id ? "channel" : (d.offline ? "dm · offline" : d.e2e ? "dm" : "chat");
    const self = d.from === state.myNickname;
    const s = state.settings;
    if (s) {
        const line = `[${scope}] ${d.from}: ${d.text}`;
        if ((scope === "channel" && s.log_channel_chat) ||
            (scope.startsWith("dm") && s.log_private_chat) ||
            (scope === "chat" && s.log_server_chat)) {
            window.go.main.App.LogChat(line);
        }
    }
    // Whisper/mention sound for direct messages, plus taskbar flash (32).
    // Track the last whisperer for the Ctrl+R whisper-reply hotkey (33).
    if (!d.channel_id && !self) {
        if (d.from_client_id) {
            const sender = state.clients.find((c) => c.client_id === d.from_client_id);
            if (sender) state.lastWhispererUID = sender.unique_id;
        }
        // (385) DMs dispatch through the notification matrix.
        window.__voicxNotify?.notify("dm", (d.from || "someone") + ": " + (d.text || "").slice(0, 80),
            { uid: d.from_unique_id, className: "messages", kind: "warn", noSound: !state.settings?.whisper_sound });
    }
    // Rendering, tabs, badges, receipts etc. live in chat-ui.js (wave 5b).
    chatUI.addChat(d);
}

function sysMsg(text) {
    const log = $("chat-log");
    const el = document.createElement("div");
    el.className = "msg sys";
    el.textContent = "— " + text + " —";
    log.appendChild(el);
    log.scrollTop = log.scrollHeight;
}

$("chat-scope").onchange = () => {
    $("chat-target").classList.toggle("hidden", $("chat-scope").value !== "direct");
};

async function sendChat() {
    // Rich send flow (reply prefix, staged file uploads, DM tabs) is in
    // chat-ui.js (wave 5b); this wrapper keeps the __voicx export stable.
    await chatUI.sendMessage();
}

$("chat-send").onclick = sendChat;
$("chat-text").addEventListener("keydown", (e) => { if (e.key === "Enter") sendChat(); });

// ---------------------------------------------------------------------------
// Voice
// ---------------------------------------------------------------------------

$("voice-join").onclick = async () => {
    if (state.pc) {
        teardownVoice();
        resetVoiceUI();
        return;
    }
    try {
        await startVoice();
    } catch (e) {
        sysMsg("voice failed: " + e);
        teardownVoice();
        resetVoiceUI();
    }
};

function audioConstraints() {
    const s = state.settings || {};
    return {
        echoCancellation: s.echo_cancellation !== false,
        noiseSuppression: s.noise_suppression !== false,
        ...(s.capture_device_id ? { deviceId: { exact: s.capture_device_id } } : {}),
    };
}

// (78) Camera frame rate from settings (15/30/60, default 30).
function videoConstraints() {
    const fps = state.settings?.camera_fps || 30;
    return { width: 640, height: 360, frameRate: { ideal: fps } };
}

async function startVoice() {
    // Capture degrades in steps — mic+camera, then mic only, then camera only.
    // Either device may be absent, and a machine without a webcam must still
    // get a full audio session; micErr stays null while the mic works.
    let micErr = null;
    try {
        state.localStream = await navigator.mediaDevices.getUserMedia({
            audio: audioConstraints(),
            video: videoConstraints(),
        });
    } catch {
        try {
            state.localStream = await navigator.mediaDevices.getUserMedia({
                audio: audioConstraints(),
            });
        } catch (e) {
            micErr = e;
            try {
                state.localStream = await navigator.mediaDevices.getUserMedia({
                    video: videoConstraints(),
                });
            } catch (ve) {
                throw new Error("no microphone or camera available (mic: " +
                    (micErr.name || micErr) + ", camera: " + (ve.name || ve) + ")");
            }
        }
    }
    setMicState(micErr === null ? "ok" : micErr.name === "NotAllowedError" ? "denied" : "none");
    if (state.localStream.getVideoTracks().length === 0) {
        sysMsg("no camera found; joined voice audio-only");
    }
    // (78) contentHint tracks the configured frame rate.
    const camTrack = state.localStream.getVideoTracks()[0];
    if (camTrack) camTrack.contentHint = (state.settings?.camera_fps || 30) >= 60 ? "motion" : "detail";
    // the self-view is a fixed floating panel with a border: without a camera
    // track it would just be an empty box over the UI (audio-only fallback).
    $("local-video").srcObject = camTrack ? state.localStream : null;
    $("local-video").classList.toggle("hidden", !camTrack);

    // ICE servers delivered by the server at connect (STUN/TURN); an empty
    // list means "client defaults" (plain RTCPeerConnection).
    let iceServers = null;
    try {
        iceServers = await window.go.main.App.GetICEServers();
    } catch { /* fall back to client defaults */ }
    const pc = iceServers && iceServers.length
        ? new RTCPeerConnection({ iceServers })
        : new RTCPeerConnection();
    state.pc = pc;

    const audioTrack = state.localStream.getAudioTracks()[0];
    if (audioTrack) {
        pc.addTransceiver(audioTrack, { direction: "sendrecv", streams: [state.localStream] });
    }

    const videoTrack = state.localStream.getVideoTracks()[0];
    if (videoTrack) {
        try {
            pc.addTransceiver(videoTrack, {
                direction: "sendrecv",
                streams: [state.localStream],
                sendEncodings: [
                    { rid: "f" },
                    { rid: "h", scaleResolutionDownBy: 2 },
                    { rid: "q", scaleResolutionDownBy: 4 },
                ],
            });
        } catch {
            pc.addTransceiver(videoTrack, { direction: "sendrecv", streams: [state.localStream] });
        }
    }

    pc.onicecandidate = (e) => {
        if (e.candidate) {
            window.go.main.App.SendICECandidate(
                e.candidate.candidate, e.candidate.sdpMid || "", e.candidate.sdpMLineIndex || 0);
        }
    };
    // (59) ICE restart: re-offer with iceRestart on failed/disconnected,
    // backing off 1s, 2s, 5s, 15s before giving up with a warning toast.
    pc.oniceconnectionstatechange = () => onICEStateChange(pc);
    pc.ontrack = (e) => {
        // The server assigns each per-publisher track an ID equal to the
        // publisher's client ID (via the MSID track ID), so tracks can be
        // attributed without SSRC mapping.
        const publisher = state.clients.find((c) => String(c.client_id) === String(e.track.id)) || null;
        state.trackUsers.set(e.track.id, publisher
            ? { client_id: publisher.client_id, unique_id: publisher.unique_id, nickname: publisher.nickname }
            : { client_id: e.track.id, unique_id: "", nickname: "" });
        e.track.addEventListener("ended", () => state.trackUsers.delete(e.track.id));
        if (e.track.kind === "video") {
            // (61) one grid tile per publisher video track; removed on ended.
            videoTrackAdded(e.track.id, e.streams[0], publisher);
            e.track.addEventListener("ended", () => videoTrackRemoved(e.track.id));
            return;
        }
        // Audio: route through the WebAudio chain (per-user gain hook,
        // limiter, normalizer) instead of the video element.
        attachRemoteAudio(e.track, publisher);
    };

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    const answerSDP = await window.go.main.App.WebRTCOffer(offer.sdp);
    await pc.setRemoteDescription({ type: "answer", sdp: answerSDP });

    applyChannelAudio();
    // (88) re-apply the persisted low-bandwidth mode to the fresh session.
    if (state.settings?.low_bandwidth) setLowBandwidth(true, false);
    startVoiceMonitor();
    startMicMeter(state.localStream);
    applyVoiceState();
    $("voice-join").classList.add("active");
    $("voice-status").textContent = "voice on";
}

// (24) applyChannelAudio applies the current channel's Opus settings to the
// outgoing audio: a maxBitrate cap on the sender encoding (0 = server
// default 32000 bits/s, i.e. no explicit cap) and a contentHint on the local
// track ("music" for stereo channels, "speech" otherwise).
async function applyChannelAudio() {
    if (!state.pc) return;
    const ch = state.channels.find((c) => c.ChannelID === state.myChannelID);
    if (!ch) return;
    const bitrate = ch.OpusBitrate || 0;
    const sender = state.pc.getSenders().find((s) => s.track && s.track.kind === "audio");
    if (sender) {
        const p = sender.getParameters();
        if (p.encodings?.length) {
            p.encodings[0].maxBitrate = bitrate > 0 ? bitrate : undefined;
            await sender.setParameters(p).catch(() => {});
        }
    }
    const track = state.localStream?.getAudioTracks()[0];
    if (track) track.contentHint = ch.OpusStereo ? "music" : "speech";
}

// (13/14) Priority-speaker ducking: while another priority speaker in my
// channel is talking, everyone else is ducked (setDucking in audio.js);
// priority speakers themselves are exempt. Un-ducking is delayed 500 ms so
// sentence gaps don't pump the gain; the timer is cancelled when a priority
// speaker starts again.
let unduckTimer = null;

function recomputeDucking() {
    const inMyChannel = state.clients.filter((c) => c.channel_id === state.myChannelID && c.channel_id !== 0);
    const prioAll = inMyChannel.filter((c) => c.priority_speaker);
    const anyOtherTalking = prioAll.some((c) => c.client_id !== state.myClientID && c.is_speaking);
    if (anyOtherTalking) {
        if (unduckTimer) {
            clearTimeout(unduckTimer);
            unduckTimer = null;
        }
        setDucking(true, prioAll.map((c) => c.unique_id));
        return;
    }
    if (unduckTimer) clearTimeout(unduckTimer);
    unduckTimer = setTimeout(() => {
        unduckTimer = null;
        const stillTalking = state.clients.some((c) =>
            c.channel_id === state.myChannelID && c.channel_id !== 0 &&
            c.priority_speaker && c.client_id !== state.myClientID && c.is_speaking);
        if (!stillTalking) setDucking(false, []);
    }, 500);
}

// (13) Priority-speaker self toggle in the voice bar.
$("voice-prio").onclick = async () => {
    const err = await window.go.main.App.SetPrioritySpeaker(!state.myPriority);
    if (err) {
        sysMsg("priority speaker failed: " + err);
        return;
    }
    state.myPriority = !state.myPriority;
    $("voice-prio").classList.toggle("active", state.myPriority);
};

// (59) ICE restart ladder: on failed/disconnected, re-offer with iceRestart
// after 1s, 2s, 5s, then 15s; a connected/completed state resets the ladder.
// The server treats a re-offer as a full session replacement (idempotent).
const ICE_BACKOFF_MS = [1000, 2000, 5000, 15000];
let iceFailures = 0;
let iceTimer = null;

function onICEStateChange(pc) {
    const s = pc.iceConnectionState;
    if (s === "connected" || s === "completed") {
        resetICERestart();
        return;
    }
    if ((s === "failed" || s === "disconnected") && iceTimer === null) {
        scheduleICERestart();
    }
}

function resetICERestart() {
    iceFailures = 0;
    if (iceTimer !== null) {
        clearTimeout(iceTimer);
        iceTimer = null;
    }
}

function scheduleICERestart() {
    if (!state.pc) return;
    if (iceFailures >= ICE_BACKOFF_MS.length) {
        toast("Voice connection unstable — rejoin voice if it does not recover", "warn", "conn");
        return;
    }
    const delay = ICE_BACKOFF_MS[iceFailures++];
    iceTimer = setTimeout(async () => {
        iceTimer = null;
        const pc = state.pc;
        if (!pc) return;
        const s = pc.iceConnectionState;
        if (s === "connected" || s === "completed" || s === "closed") {
            resetICERestart();
            return;
        }
        try {
            const offer = await pc.createOffer({ iceRestart: true });
            await pc.setLocalDescription(offer);
            const answerSDP = await window.go.main.App.WebRTCOffer(offer.sdp);
            await pc.setRemoteDescription({ type: "answer", sdp: answerSDP });
        } catch (e) {
            sysMsg("ice restart failed: " + e);
        }
        // Still not connected: oniceconnectionstatechange may not fire again
        // for a persistent failure, so chain the next backoff step explicitly.
        if (state.pc === pc && !["connected", "completed", "closed"].includes(pc.iceConnectionState)) {
            scheduleICERestart();
        }
    }, delay);
}

function teardownVoice() {
    stopVoiceMonitor();
    stopMicMeter();
    resetICERestart();
    if (unduckTimer) {
        clearTimeout(unduckTimer);
        unduckTimer = null;
    }
    setDucking(false, []);
    clearVideoGrid();
    if (state.shareStream) {
        state.shareStream.getTracks().forEach((t) => t.stop());
        state.shareStream = null;
        state.shareAudioSender = null;
        state.screenSharing = false;
        window.go.main.App.SetScreenShare(false);
        $("voice-screen").classList.remove("active");
    }
    detachRemoteAudio();
    if (state.pc) { state.pc.close(); state.pc = null; }
    if (state.localStream) {
        for (const t of state.localStream.getTracks()) t.stop();
        state.localStream = null;
    }
    $("local-video").classList.add("hidden");
    $("remote-video").classList.add("hidden");
}

function resetVoiceUI() {
    $("voice-join").classList.remove("active");
    $("voice-status").textContent = "voice off";
    state.pttActive = false;
    $("ptt-btn").classList.remove("live");
    state.myPriority = false;
    $("voice-prio").classList.remove("active");
    updateTalkBanner();
}

function setMicState(s) {
    state.micState = s;
    const el = $("mic-status");
    if (s === "ok") {
        el.textContent = "";
        $("ptt-btn").disabled = false;
    } else if (s === "none") {
        el.textContent = "No microphone found — video only";
        $("ptt-btn").disabled = true;
        sysMsg("no microphone found; joined voice video-only");
    } else if (s === "denied") {
        el.textContent = "Mic access denied — video only";
        $("ptt-btn").disabled = true;
        sysMsg("microphone access denied; joined voice video-only");
    }
}

// Server->client ICE and renegotiation.
window.runtime.EventsOn("ice", (json) => {
    if (!state.pc) return;
    const c = JSON.parse(json);
    state.pc.addIceCandidate({
        candidate: c.candidate,
        sdpMid: c.sdp_mid || null,
        sdpMLineIndex: c.sdp_mline_index ?? null,
    }).catch(() => {});
});

window.runtime.EventsOn("offer", async (json) => {
    if (!state.pc) return;
    const o = JSON.parse(json);
    await state.pc.setRemoteDescription({ type: "offer", sdp: o.sdp });
    const answer = await state.pc.createAnswer();
    await state.pc.setLocalDescription(answer);
    window.go.main.App.WebRTCAnswer(answer.sdp);
});

// Mute / deafen / PTT ---------------------------------------------------------

function applyVoiceState() {
    if (!state.localStream) return;
    const mode = state.settings?.activation_mode || "ptt";
    let audible = !state.muted;
    // ptt and vad both gate transmission on the (hotkey- or VAD-driven)
    // pttActive flag; continuous always transmits.
    if (mode !== "continuous") audible = audible && state.pttActive;
    for (const t of state.localStream.getAudioTracks()) t.enabled = audible;
}

// Voice monitor: one interval driving VAD transmission, the mic meter's
// sibling level feed, the talking-while-muted warning (26), and the
// talking-to-empty-channel hint (27).
let mutedTalkStreak = 0, emptyStreak = 0, lastMutedWarn = 0, lastEmptyWarn = 0;

function startVoiceMonitor() {
    stopVoiceMonitor();
    const audioTrack = state.localStream?.getAudioTracks()[0];
    if (!audioTrack) return;
    try {
        if (!state.audioCtx) state.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
        const ctx = state.audioCtx;
        const src = ctx.createMediaStreamSource(state.localStream);
        const analyser = ctx.createAnalyser();
        analyser.fftSize = 512;
        src.connect(analyser);
        const buf = new Uint8Array(analyser.frequencyBinCount);
        let lastVoice = 0;
        state.vadMonitor = setInterval(() => {
            analyser.getByteTimeDomainData(buf);
            let sum = 0;
            for (const v of buf) sum += Math.abs(v - 128);
            const level = sum / buf.length / 128; // 0..1
            const mode = state.settings?.activation_mode || "ptt";
            const threshold = (state.settings?.vad_threshold ?? 50) / 100 * 0.2;

            // VAD transmission.
            if (mode === "vad") {
                if (level > threshold) {
                    lastVoice = Date.now();
                    if (!state.pttActive) setPTT(true);
                } else if (state.pttActive && Date.now() - lastVoice > 300) {
                    setPTT(false);
                }
            }

            // (26) talking while muted / PTT off.
            const blocked = state.muted || (mode !== "continuous" && !state.pttActive);
            if (blocked && level > threshold) {
                mutedTalkStreak++;
            } else {
                mutedTalkStreak = 0;
            }
            if (mutedTalkStreak > 10 && (state.settings?.warn_muted_talking !== false)) {
                warnMutedTalking();
            }

            // (27) talking to an empty channel.
            const others = state.clients.filter((c) => c.channel_id === state.myChannelID && c.client_id !== state.myClientID);
            if (others.length === 0 && state.myChannelID !== 0 && level > threshold) {
                emptyStreak++;
            } else {
                emptyStreak = 0;
            }
            if (emptyStreak > 10 && (state.settings?.warn_empty_channel !== false)) {
                warnEmptyChannel();
            }
        }, 100);
    } catch { /* monitor unavailable */ }
}

function stopVoiceMonitor() {
    if (state.vadMonitor) {
        clearInterval(state.vadMonitor);
        state.vadMonitor = null;
    }
    mutedTalkStreak = 0;
    emptyStreak = 0;
}

// (26) Talking-while-muted warning: amber banner, rate-limited, dismissible.
function warnMutedTalking() {
    if (Date.now() - lastMutedWarn < 10000) return;
    lastMutedWarn = Date.now();
    const b = $("talk-banner");
    b.textContent = "You're muted!";
    b.classList.remove("hidden");
    b.classList.add("warn-banner");
    b.onclick = () => {
        b.classList.add("hidden");
        b.classList.remove("warn-banner");
        lastMutedWarn = Date.now();
    };
    setTimeout(() => {
        b.classList.add("hidden");
        b.classList.remove("warn-banner");
        b.textContent = "● TALKING";
        b.onclick = null;
    }, 2500);
}

// (27) Talking-to-empty-channel hint.
function warnEmptyChannel() {
    if (Date.now() - lastEmptyWarn < 30000) return;
    lastEmptyWarn = Date.now();
    toast("Nobody is in your channel", "info", "alert");
}

// Output settings: volume + sink for remote media elements.
function applyOutputSettings(el) {
    const s = state.settings || {};
    el.volume = Math.min(1, (s.volume ?? 100) / 100);
    if (s.playback_device_id && el.setSinkId) {
        el.setSinkId(s.playback_device_id).catch(() => {});
    }
    el.muted = state.deafened;
    if (remoteChain.master) {
        remoteChain.master.gain.value = Math.min(2, (s.volume ?? 100) / 100);
    }
}

// Remote audio WebAudio chain. One shared context carries every publisher:
// per-track source -> per-user gain -> per-user mute -> master gain (volume)
// -> [normalizer] -> [limiter] -> destination. Per-publisher tracks (track ID
// = publisher client ID) make per-user volume/mute audible; the registry
// itself lives in audio.js.
const remoteChain = { ctx: null, master: null, interval: null };
// remoteTracks maps media track ID -> {src, gain, mute, uid} for per-track
// teardown when a publisher leaves or voice is stopped.
const remoteTracks = new Map();

// ensureRemoteChain builds the shared processing tail once per voice session.
function ensureRemoteChain() {
    if (remoteChain.ctx) return true;
    try {
        const ctx = new (window.AudioContext || window.webkitAudioContext)();
        remoteChain.ctx = ctx;
        const master = ctx.createGain();
        remoteChain.master = master;
        master.gain.value = Math.min(2, (state.settings?.volume ?? 100) / 100);

        let node = master;

        // (53) gain normalization (optional).
        if (state.settings?.gain_normalize) {
            const analyser = ctx.createAnalyser();
            analyser.fftSize = 512;
            master.connect(analyser);
            const norm = makeNormalizer(ctx, analyser);
            master.disconnect();
            master.connect(norm.gain);
            node = norm.gain;
            remoteChain.interval = setInterval(norm.tick, 100);
        }

        // (52) voice limiter/compressor (default on).
        let out = node;
        if (state.settings?.voice_limiter !== false) {
            const comp = makeLimiter(ctx);
            node.connect(comp);
            out = comp;
        }
        out.connect(ctx.destination);

        // Output device selection where supported (Chrome 110+).
        const s = state.settings || {};
        if (s.playback_device_id && typeof ctx.setSinkId === "function") {
            ctx.setSinkId(s.playback_device_id).catch(() => {});
        }
        return true;
    } catch (e) {
        sysMsg("remote audio chain failed: " + e);
        return false;
    }
}

// attachRemoteAudio adds one publisher's audio track to the shared chain.
// publisher is the resolved state.clients entry (or null when unknown); its
// unique ID keys the per-user volume/mute registry from audio.js.
function attachRemoteAudio(track, publisher) {
    detachRemoteTrack(track.id); // re-attach after an ICE restart replaces the track
    if (!ensureRemoteChain()) return;
    try {
        const ctx = remoteChain.ctx;
        // a MediaStreamAudioSourceNode taps only the first audio track of the
        // stream it is built from, so the per-user volume (1) and local mute
        // (2) chains would all follow one publisher if several tracks share a
        // stream: wrap this track alone.
        const src = ctx.createMediaStreamSource(new MediaStream([track]));
        const gain = ctx.createGain();
        const mute = ctx.createGain();
        src.connect(gain);
        gain.connect(mute);
        mute.connect(remoteChain.master);

        const uid = publisher?.unique_id || "";
        remoteTracks.set(track.id, { src, gain, mute, uid });
        if (uid) registerUserChain(uid, gain, mute);

        track.addEventListener("ended", () => detachRemoteTrack(track.id));
    } catch (e) {
        sysMsg("remote audio chain failed: " + e);
    }
}

// resolveTrackUsers re-attributes tracks whose renegotiation arrived before
// the publisher's user_joined event: at ontrack time the client list has no
// entry yet, and without a second pass the per-user volume (1) and local mute
// (2) chains are never registered for that publisher for the whole session.
function resolveTrackUsers() {
    for (const [trackID, u] of state.trackUsers) {
        if (u.unique_id) continue;
        const publisher = state.clients.find((c) => String(c.client_id) === String(trackID));
        if (!publisher) continue;
        state.trackUsers.set(trackID, {
            client_id: publisher.client_id, unique_id: publisher.unique_id, nickname: publisher.nickname,
        });
        const t = remoteTracks.get(trackID);
        if (t && !t.uid && publisher.unique_id) {
            t.uid = publisher.unique_id;
            registerUserChain(publisher.unique_id, t.gain, t.mute);
        }
    }
}

// detachRemoteTrack removes one publisher's nodes from the shared chain.
function detachRemoteTrack(trackID) {
    const t = remoteTracks.get(trackID);
    if (!t) return;
    remoteTracks.delete(trackID);
    if (t.uid) unregisterUserChain(t.uid);
    try {
        t.mute.disconnect();
        t.gain.disconnect();
        t.src.disconnect();
    } catch { /* already disconnected */ }
}

function detachRemoteAudio() {
    for (const trackID of [...remoteTracks.keys()]) detachRemoteTrack(trackID);
    if (remoteChain.interval) {
        clearInterval(remoteChain.interval);
        remoteChain.interval = null;
    }
    if (remoteChain.ctx) {
        remoteChain.ctx.close().catch(() => {});
        remoteChain.ctx = null;
        remoteChain.master = null;
    }
    state.trackUsers.clear();
}

function setPTT(active) {
    // (6) PTT release delay: PTT-up is deferred by the configured delay so
    // sentence ends aren't clipped. pttRelease handles cancel-on-repress.
    pttRelease(active, (effective) => {
        if (state.pttActive === effective) return;
        state.pttActive = effective;
        $("ptt-btn").classList.toggle("live", effective);
        window.go.main.App.SetPTT(effective);
        applyVoiceState();
        updateTalkBanner();
    });
}

$("ptt-btn").addEventListener("mousedown", (e) => { e.preventDefault(); setPTT(true); });
["mouseup", "mouseleave"].forEach((ev) => $("ptt-btn").addEventListener(ev, () => setPTT(false)));

$("voice-mute").onclick = () => {
    state.muted = !state.muted;
    $("voice-mute").classList.toggle("active", state.muted);
    window.go.main.App.SetMuted(state.muted);
    if (state.muted) playEvent("mic_off");
    else playEvent("mic_on");
    applyVoiceState();
    renderTree();
};

function setDeafened(on) {
    state.deafened = on;
    $("remote-video").muted = on;
    if (remoteChain.master) remoteChain.master.gain.value = on ? 0 : Math.min(2, (state.settings?.volume ?? 100) / 100);
    renderTree();
}

window.runtime.EventsOn("hotkey", (action) => {
    if (action === "mute_toggle") {
        $("voice-mute").click();
        return;
    }
    if (action === "deafen_toggle") {
        const { state, setDeafened, sysMsg } = window.__voicx;
        setDeafened(!state.deafened);
        sysMsg(state.deafened ? "deafened (incoming audio off)" : "undeafened");
        return;
    }
    // (285) quick connect: last-used bookmark/recent in a new tab.
    if (action === "quick_connect") {
        window.__voicxTabs?.quickConnectLast();
        return;
    }
    // (293) compact mode toggle.
    if (action === "compact_toggle") {
        toggleCompact();
        return;
    }
    // (341) zen mode toggle.
    if (action === "zen_toggle") {
        window.__voicxPolish?.toggleZen();
        return;
    }
    // (33) Whisper reply: activate whisper targeting the last whisperer.
    if (action === "whisper_reply") {
        if (state.lastWhispererUID) {
            window.go.main.App.WhisperSet([state.lastWhispererUID], [], true);
            sysMsg("whisper reply armed → " + clientName(state.lastWhispererUID) || state.lastWhispererUID);
        } else {
            toast("no whisper to reply to", "info", "alert");
        }
        return;
    }
    if (document.hasFocus() && document.activeElement === $("chat-text")) return;
    if (state.micState === "none" || state.micState === "denied") return;
    if ((state.settings?.activation_mode || "ptt") !== "ptt") return;
    setPTT(action === "ptt_down");
});

// (293) compact mode: mini window with the voice bar and current speaker
// only. Toggled from the View menu or the compact hotkey.
function toggleCompact() {
    const on = !document.body.classList.contains("compact");
    document.body.classList.toggle("compact", on);
    if (state.settings) {
        state.settings.compact_mode = on;
        window.go.main.App.SaveSettings(state.settings);
    }
}

// (298) keyboard navigation pass: Esc closes the topmost dialog/menu;
// arrow keys move through the channel tree; Enter joins the focused channel.
document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
        const dlg = [...document.querySelectorAll(".dlg-overlay")].pop();
        if (dlg) {
            dlg.remove();
            e.stopPropagation();
            return;
        }
        const ctx = document.querySelector(".ctx-menu");
        if (ctx) {
            ctx.remove();
            e.stopPropagation();
            return;
        }
        return;
    }
    // Arrow navigation in the channel tree (when it or a row has focus).
    if (!["ArrowUp", "ArrowDown", "Enter"].includes(e.key)) return;
    const tree = $("channel-tree");
    const rows = [...tree.querySelectorAll(".channel, .client")];
    if (rows.length === 0) return;
    const activeEl = document.activeElement;
    const idx = rows.indexOf(activeEl);
    if (e.key === "Enter") {
        if (idx >= 0 && activeEl.classList.contains("channel")) activeEl.click();
        return;
    }
    if (!tree.contains(activeEl) && idx < 0) return;
    e.preventDefault();
    const next = e.key === "ArrowDown" ? Math.min(rows.length - 1, idx + 1) : Math.max(0, idx - 1);
    rows[next < 0 ? 0 : next].focus();
});

window.runtime.EventsOn("hotkey_status", (st) => {
    const el = $("hotkey-status");
    if (st.registered) {
        el.textContent = "⌨ " + st.action;
        el.classList.remove("err");
        el.title = st.action + " hotkey registered";
    } else {
        el.textContent = "⌨ off";
        el.classList.add("err");
        el.title = st.action + " hotkey failed: " + (st.error || "");
        toast("hotkey failed: " + st.action + " — " + (st.error || "registration failed"), "warn", "conn");
    }
});

// TALK banner: visible whenever PTT is live or we are speaking.
function updateTalkBanner() {
    const me = state.clients.find((c) => c.client_id === state.myClientID);
    const talking = state.pttActive || !!(me && me.is_speaking);
    $("talk-banner").classList.toggle("hidden", !talking);
}

// Screen share (69-72, 85) + low-bandwidth mode (88) — implemented in video.js.

$("voice-screen").onclick = () => shareToggle();

$("voice-lowbw").onclick = () => setLowBandwidth(!isLowBandwidth(), true);

// Whisper (re-applied from settings after connect) -----------------------------

function applyWhisperSettings() {
    const s = state.settings;
    if (!s || !s.whisper_active) return;
    if ((s.whisper_clients?.length || 0) === 0 && (s.whisper_channels?.length || 0) === 0) return;
    window.go.main.App.WhisperSet(s.whisper_clients || [], s.whisper_channels || [], true);
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

async function refreshPermissions() {
    const area = $("perm-area");
    try {
        const entries = await window.go.main.App.GetPermissions();
        // (6b) keep the resolved set for UI gating (kick/ban/group menus).
        state.myPerms = new Map((entries || []).map((e) => [e.key, e]));
        if (!entries || entries.length === 0) {
            area.innerHTML = `<div class="empty-state">No permissions — guest default</div>`;
            return;
        }
        let html = `<table class="perm-grid"><thead><tr><th>key</th><th>value</th><th>flags</th></tr></thead><tbody>`;
        for (const e of entries) {
            const flags = [e.skip ? "skip" : "", e.negate ? "negate" : ""].filter(Boolean).join(",");
            html += `<tr><td class="mono">${e.key}</td><td><span class="pill-val">${e.value}</span></td><td>${flags}</td></tr>`;
        }
        area.innerHTML = html + "</tbody></table>";
    } catch {
        area.innerHTML = `<div class="empty-state">Permissions unavailable</div>`;
    }
}

// ---------------------------------------------------------------------------
// Shared namespace for menu.js and settings-ui.js
// ---------------------------------------------------------------------------

window.__voicx = {
    state, $, toast, sysMsg, beep, showLogin, disconnect, sendChat, setPTT,
    setDeafened, refreshPermissions, applyVoiceState, applyOutputSettings,
    startVADMonitor: startVoiceMonitor, stopVADMonitor: stopVoiceMonitor,
    connectFromLogin, renderTree,
    clientName, initials, fetchAvatar,
    applyAppearance, toggleCompact, recentChannels,
};

initMenu();
initSettingsUI();
initClientInfo();
initVideo();
initUpdater();
initPermsUI(); // wave-6b permission/group UI (registers window.__voicxPerms)
initFilesUI(); // wave-7 file browser/transfers (registers window.__voicxFiles)
initTabs();    // wave-8a server tabs (registers window.__voicxTabs)
initSocialUI(); // wave-8b presence/contacts/hover (registers window.__voicxSocial)
initMetaUI();   // wave-8b debug/stats/onboarding (registers window.__voicxMeta)
initPolishUI(); // wave-8c polish/a11y (registers window.__voicxPolish)
initNotifications(); // wave-9 notification matrix (registers window.__voicxNotify)
chatUI.initChat(); // wave-5b chat UI (must run after __voicx exists)
