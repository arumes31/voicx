// polish-ui.js — wave-8c polish & accessibility: pane transitions (337),
// resizable panes (338), detachable chat (339), fullscreen video (340),
// zen mode (341), idle video pause (342), screen-reader labels + live
// announcements (343), notification center (346), DND (347/348), and tree
// virtualization (349).
import { setIdleQualityOverride } from "./video.js";

const V = () => window.__voicx;
const App = () => window.go.main.App;

// ---------------------------------------------------------------------------
// Resizable panes (338)
// ---------------------------------------------------------------------------

function initResizablePanes() {
    const sidebar = document.getElementById("sidebar");
    const details = document.getElementById("details");
    const body = document.getElementById("app-body");
    const s = V().state.settings || {};

    const setPaneWidth = (field, width) => {
        const variable = field === "sidebar_width" ? "--sidebar-width" : "--details-width";
        if (width > 120) body.style.setProperty(variable, width + "px");
        else body.style.removeProperty(variable);
    };

    const apply = () => {
        const w = V().state.settings || {};
        setPaneWidth("sidebar_width", w.sidebar_width);
        setPaneWidth("details_width", w.details_width);
    };

    const makeHandle = (el, field, invert) => {
        const h = document.createElement("div");
        h.className = "pane-handle" + (invert ? " right" : "");
        h.title = "drag to resize · double-click to reset";
        let startX = 0, startW = 0;
        h.addEventListener("mousedown", (e) => {
            e.preventDefault();
            startX = e.clientX;
            startW = el.getBoundingClientRect().width;
            const onMove = (ev) => {
                const w = Math.max(160, Math.min(560, startW + (invert ? startX - ev.clientX : ev.clientX - startX)));
                setPaneWidth(field, w);
                (V().state.settings ||= {})[field] = Math.round(w);
            };
            const onUp = () => {
                window.removeEventListener("mousemove", onMove);
                window.removeEventListener("mouseup", onUp);
                App().SaveSettings(V().state.settings);
            };
            window.addEventListener("mousemove", onMove);
            window.addEventListener("mouseup", onUp);
        });
        h.addEventListener("dblclick", () => {
            V().state.settings[field] = 0;
            App().SaveSettings(V().state.settings);
            apply();
        });
        el.appendChild(h);
    };
    makeHandle(sidebar, "sidebar_width", false);
    makeHandle(details, "details_width", true);
    apply();
    void s;
}

// ---------------------------------------------------------------------------
// Detachable chat (339): compact floating chat panel. A true second OS
// window needs Wails v3; this CSS overlay is the documented stand-in.
// ---------------------------------------------------------------------------

let chatPop = null;
let chatHome = null; // {wrapMarker, inputMarker}: where the panes dock back

function toggleChatPopout() {
    if (chatPop) {
        const wrap = document.getElementById("chat-wrap");
        const inputRow = document.getElementById("chat-input-row");
        // The panes live inside the popout: dock them before removing it, or
        // remove() takes the whole chat pane with it.
        if (chatHome?.wrapMarker?.parentNode) {
            chatHome.wrapMarker.parentNode.replaceChild(wrap, chatHome.wrapMarker);
        }
        if (chatHome?.inputMarker?.parentNode) {
            chatHome.inputMarker.parentNode.replaceChild(inputRow, chatHome.inputMarker);
        }
        chatPop.remove();
        chatPop = null;
        chatHome = null;
        wrap.classList.remove("hidden");
        inputRow.classList.remove("hidden");
        return;
    }
    chatPop = document.createElement("div");
    chatPop.className = "chat-popout";
    chatPop.innerHTML = `<div class="chat-pop-head">chat — drag me <button class="icon-btn chat-pop-close">✕</button></div>`;
    const wrap = document.getElementById("chat-wrap");
    const inputRow = document.getElementById("chat-input-row");
    const wrapMarker = document.createComment("chat-wrap-marker");
    const inputMarker = document.createComment("chat-input-row-marker");
    wrap.parentNode.replaceChild(wrapMarker, wrap);
    inputRow.parentNode.replaceChild(inputMarker, inputRow);
    chatHome = { wrapMarker, inputMarker };
    chatPop.appendChild(wrap);
    chatPop.appendChild(inputRow);
    wrap.classList.remove("hidden");
    inputRow.classList.remove("hidden");
    chatPop.querySelector(".chat-pop-close").onclick = toggleChatPopout;
    // Drag by the header.
    const head = chatPop.querySelector(".chat-pop-head");
    head.addEventListener("mousedown", (e) => {
        const startX = e.clientX - chatPop.offsetLeft;
        const startY = e.clientY - chatPop.offsetTop;
        const onMove = (ev) => {
            chatPop.style.left = Math.max(0, ev.clientX - startX) + "px";
            chatPop.style.top = Math.max(0, ev.clientY - startY) + "px";
        };
        const onUp = () => {
            window.removeEventListener("mousemove", onMove);
            window.removeEventListener("mouseup", onUp);
        };
        window.addEventListener("mousemove", onMove);
        window.addEventListener("mouseup", onUp);
    });
    document.body.appendChild(chatPop);
}

// ---------------------------------------------------------------------------
// Zen mode (341)
// ---------------------------------------------------------------------------

function toggleZen() {
    const on = !document.body.classList.contains("zen");
    document.body.classList.toggle("zen", on);
    let ind = document.getElementById("zen-indicator");
    if (on && !ind) {
        ind = document.createElement("button");
        ind.id = "zen-indicator";
        ind.textContent = "zen — click or hotkey to exit";
        ind.onclick = toggleZen;
        document.body.appendChild(ind);
    } else if (!on && ind) {
        ind.remove();
    }
}

// ---------------------------------------------------------------------------
// Idle video pause (342)
// ---------------------------------------------------------------------------

let idleVideoTimer = null;
let idleVideoPaused = false;

function initIdleVideoPause() {
    window.addEventListener("blur", () => {
        if (!(V().state.settings?.idle_video_pause !== false)) return;
        idleVideoTimer = setTimeout(() => {
            idleVideoPaused = true;
            document.querySelectorAll("#video-grid video, #remote-video").forEach((v) => v.pause());
            if (V().state.pc) setIdleQualityOverride(true);
        }, 60000);
    });
    window.addEventListener("focus", () => {
        if (idleVideoTimer) {
            clearTimeout(idleVideoTimer);
            idleVideoTimer = null;
        }
        if (idleVideoPaused) {
            idleVideoPaused = false;
            document.querySelectorAll("#video-grid video, #remote-video").forEach((v) => v.play().catch(() => {}));
            setIdleQualityOverride(false); // video.js re-applies the user's pref
        }
    });
}

// ---------------------------------------------------------------------------
// Screen reader (343)
// ---------------------------------------------------------------------------

// trapFocus keeps Tab inside a modal (298). Dialogs are appended to <body>
// over the app rather than replacing it, so without a trap the second Tab
// walks out of the dialog into the tree and chat behind it.
function trapFocus(overlay) {
    const focusables = () => [...overlay.querySelectorAll(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')]
        .filter((el) => !el.disabled && el.offsetParent !== null);
    // Only claim focus when the dialog did not already place it itself.
    if (!overlay.contains(document.activeElement)) focusables()[0]?.focus();
    overlay.addEventListener("keydown", (e) => {
        if (e.key !== "Tab") return;
        const items = focusables();
        if (items.length === 0) return;
        e.preventDefault();
        const idx = items.indexOf(document.activeElement);
        const next = e.shiftKey
            ? (idx <= 0 ? items.length - 1 : idx - 1)
            : (idx < 0 || idx === items.length - 1 ? 0 : idx + 1);
        items[next].focus();
    });
}

function initA11y() {
    const tree = document.getElementById("channel-tree");
    tree.setAttribute("role", "tree");
    tree.setAttribute("aria-label", "channels and users");
    const chatLog = document.getElementById("chat-log");
    chatLog.setAttribute("role", "log");
    chatLog.setAttribute("aria-live", "polite");
    // Dialogs announce themselves as dialogs (343).
    new MutationObserver((mutations) => {
        for (const m of mutations) {
            for (const node of m.addedNodes) {
                if (node.nodeType === 1 && node.classList?.contains("dlg-overlay")) {
                    const dlg = node.querySelector(".dlg");
                    if (dlg) {
                        dlg.setAttribute("role", "dialog");
                        dlg.setAttribute("aria-modal", "true");
                    }
                    trapFocus(node); // (298)
                }
            }
        }
    }).observe(document.body, { childList: true });
    // Visually-hidden announcement region (speaking events).
    if (!document.getElementById("sr-announcer")) {
        const sr = document.createElement("div");
        sr.id = "sr-announcer";
        sr.className = "sr-only";
        sr.setAttribute("aria-live", "polite");
        document.body.appendChild(sr);
    }
}

// announce posts a message to the screen-reader live region (343).
function announce(text) {
    const sr = document.getElementById("sr-announcer");
    if (sr) sr.textContent = text;
}

// ---------------------------------------------------------------------------
// Notification center (346) + DND (347/348)
// ---------------------------------------------------------------------------

const notifHistory = []; // {kind, text, at, channelID, uid}

// recordNotification appends to the bell history (session-persisted, 50).
export function recordNotification(kind, text, ctx = {}) {
    notifHistory.unshift({ kind, text, at: Date.now(), ...ctx });
    if (notifHistory.length > 50) notifHistory.pop();
    updateBellBadge();
}

function updateBellBadge() {
    const badge = document.getElementById("notif-badge");
    if (!badge) return;
    const n = notifHistory.filter((x) => !x.read).length;
    badge.textContent = n > 0 ? (n > 9 ? "9+" : n) : "";
    badge.classList.toggle("hidden", n === 0);
}

// dndActive reports whether DND is on (toggle or quiet hours, 347/348).
export function dndActive() {
    const s = V().state.settings || {};
    if (s.dnd_enabled) return true;
    if (!s.dnd_from || !s.dnd_to) return false;
    const now = new Date();
    const cur = now.getHours() * 60 + now.getMinutes();
    const parse = (x) => {
        const [h, m] = x.split(":").map(Number);
        return (h || 0) * 60 + (m || 0);
    };
    const from = parse(s.dnd_from);
    const to = parse(s.dnd_to);
    if (from <= to) return cur >= from && cur < to;
    return cur >= from || cur < to; // overnight window
}

function openNotifCenter() {
    notifHistory.forEach((x) => { x.read = true; });
    updateBellBadge();
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const render = () => {
        const list = overlay.querySelector(".nc-list");
        list.innerHTML = notifHistory.length ? "" : `<div class="empty-state">no notifications</div>`;
        for (const n of notifHistory) {
            const row = document.createElement("div");
            row.className = "nc-row";
            row.innerHTML = `<span class="nc-kind mono"></span><span class="nc-text"></span><span class="nc-time mono"></span>`;
            row.querySelector(".nc-kind").textContent = n.kind;
            row.querySelector(".nc-text").textContent = n.text;
            row.querySelector(".nc-time").textContent = new Date(n.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
            // Click-through: jump to the channel or PM tab (346).
            if (n.uid && window.__voicx.openPM) {
                row.classList.add("clickable");
                row.onclick = () => {
                    overlay.remove();
                    window.__voicx.openPM(n.uid, "");
                };
            } else if (n.channelID) {
                row.classList.add("clickable");
                row.onclick = () => {
                    overlay.remove();
                    App().JoinChannel(n.channelID);
                };
            }
            list.appendChild(row);
        }
    };
    overlay.innerHTML = `
        <div class="dlg notif-center">
            <div class="pm-head">
                <h3>${"Notifications"}</h3>
                <button class="icon-btn nc-clear" title="clear all">🗑</button>
                <button class="icon-btn nc-close" title="Close">✕</button>
            </div>
            <div class="nc-list"></div>
        </div>`;
    overlay.querySelector(".nc-close").onclick = () => overlay.remove();
    overlay.querySelector(".nc-clear").onclick = () => {
        notifHistory.length = 0;
        updateBellBadge();
        render();
    };
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    render();
}

// ---------------------------------------------------------------------------
// Tree virtualization (349)
// ---------------------------------------------------------------------------

// When the total row count (channels + users) exceeds 500 the tree goes
// windowed: only channels on the path to my channel (and channels the user
// explicitly expanded via double-click) render their member rows; every
// other channel renders as a collapsed header with its [n] count, so DOM
// size stays O(channels) instead of O(channels + users). Verified with
// window.__voicxFakeTree(n), which injects n synthetic channels of 3 users.
const VIRTUAL_THRESHOLD = 500;

// virtualizeEnabled reports whether the tree is in windowed mode.
export function virtualizeEnabled() {
    const { state } = V();
    return state.channels.length + state.clients.length > VIRTUAL_THRESHOLD;
}

// myBranchIDs returns the set of channel IDs on the path to my channel
// (itself + ancestors), which always renders fully when virtualized.
export function myBranchIDs() {
    const { state } = V();
    const out = new Set();
    let id = state.myChannelID;
    let guard = 0;
    while (id && guard++ < 1000) {
        out.add(id);
        const ch = state.channels.find((c) => c.ChannelID === id);
        id = ch ? ch.ParentID : 0;
    }
    return out;
}

// injectFakeTree adds n synthetic channels of 3 users each and measures the
// render time (349 verification).
function injectFakeTree(n) {
    const { state } = V();
    for (let i = 0; i < n; i++) {
        const cid = 900000 + i;
        state.channels.push({ ChannelID: cid, ParentID: 0, Name: "stress-" + i, ClientCount: 3 });
        for (let u = 0; u < 3; u++) {
            state.clients.push({
                client_id: "fake-" + cid + "-" + u,
                unique_id: "fake-uid-" + cid + "-" + u,
                nickname: "stress-user-" + i + "-" + u,
                channel_id: cid,
            });
        }
    }
    const t0 = performance.now();
    V().renderTree();
    const ms = performance.now() - t0;
    const domRows = document.querySelectorAll("#channel-tree .channel, #channel-tree .client").length;
    const total = state.channels.length + state.clients.length;
    if (import.meta.env.DEV) {
        console.log(`[virtualization] ${total} logical rows, ${domRows} DOM rows, rendered in ${ms.toFixed(1)}ms (windowed=${virtualizeEnabled()})`);
    }
    return { ms, domRows, total, windowed: virtualizeEnabled() };
}

// ---------------------------------------------------------------------------

export function initPolishUI() {
    initResizablePanes();
    initIdleVideoPause();
    initA11y();
    document.getElementById("notif-bell").onclick = openNotifCenter;
    window.__voicxPolish = { toggleChatPopout, toggleZen, openNotifCenter, announce, recordNotification, dndActive, virtualizeEnabled, myBranchIDs };
    window.__voicxFakeTree = injectFakeTree;
}
