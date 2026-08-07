// polish-ui.js — wave-8c polish & accessibility: pane transitions (337),
// resizable panes (338), detachable chat (339), fullscreen video (340),
// zen mode (341), idle video pause (342), screen-reader labels + live
// announcements (343), notification center (346), DND (347/348), and tree
// virtualization (349).
import { setIdleQualityOverride } from "./video.js";
import { isActivationKey } from "./a11y.js";
import { mountDialog } from "./modal.js";

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
    const handles = new Map();
    let keyboardSaveTimer = null;

    const setPaneWidth = (field, width) => {
        const variable = field === "sidebar_width" ? "--sidebar-width" : "--details-width";
        if (width > 120) body.style.setProperty(variable, width + "px");
        else body.style.removeProperty(variable);
    };

    const apply = () => {
        const w = V().state.settings || {};
        setPaneWidth("sidebar_width", w.sidebar_width);
        setPaneWidth("details_width", w.details_width);
        requestAnimationFrame(() => {
            for (const { element, handle } of handles.values()) {
                handle.setAttribute("aria-valuenow", String(Math.round(element.getBoundingClientRect().width)));
            }
        });
    };

    const scheduleKeyboardSave = () => {
        clearTimeout(keyboardSaveTimer);
        keyboardSaveTimer = setTimeout(() => App().SaveSettings(V().state.settings), 250);
    };

    const makeHandle = (el, field, invert) => {
        const h = document.createElement("div");
        h.className = "pane-handle" + (invert ? " right" : "");
        h.title = "drag to resize · double-click to reset";
        h.tabIndex = 0;
        h.setAttribute("role", "separator");
        h.setAttribute("aria-orientation", "vertical");
        h.setAttribute("aria-label", `Resize ${el.id === "sidebar" ? "channels" : "details"} pane`);
        h.setAttribute("aria-valuemin", "160");
        h.setAttribute("aria-valuemax", "560");
        const updateValue = (width) => h.setAttribute("aria-valuenow", String(Math.round(width)));
        handles.set(field, { element: el, handle: h });
        updateValue(el.getBoundingClientRect().width);
        let startX = 0, startW = 0;
        h.addEventListener("mousedown", (e) => {
            e.preventDefault();
            startX = e.clientX;
            startW = el.getBoundingClientRect().width;
            const onMove = (ev) => {
                const w = Math.max(160, Math.min(560, startW + (invert ? startX - ev.clientX : ev.clientX - startX)));
                setPaneWidth(field, w);
                updateValue(w);
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
            scheduleKeyboardSave();
            apply();
            updateValue(el.getBoundingClientRect().width);
        });
        h.addEventListener("keydown", (event) => {
            if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
            event.preventDefault();
            const direction = event.key === "ArrowRight" ? 1 : -1;
            const current = el.getBoundingClientRect().width;
            const width = Math.max(160, Math.min(560, current + direction * (invert ? -16 : 16)));
            setPaneWidth(field, width);
            updateValue(width);
            (V().state.settings ||= {})[field] = Math.round(width);
            scheduleKeyboardSave();
        });
        el.appendChild(h);
    };
    makeHandle(sidebar, "sidebar_width", false);
    makeHandle(details, "details_width", true);
    apply();
    window.runtime.EventsOn("settings_update", apply);
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
    chatPop.innerHTML = `<div class="chat-pop-head">chat — drag me <button class="icon-btn chat-pop-close" aria-label="Close chat popout">✕</button></div>`;
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

function initA11y() {
    const tree = document.getElementById("channel-tree");
    tree.setAttribute("role", "tree");
    tree.setAttribute("aria-label", "channels and users");
    const chatLog = document.getElementById("chat-log");
    chatLog.setAttribute("role", "log");
    // History is rerendered for filters, pagination and tab switches. Keep it
    // silent and announce only messages that arrive live via #chat-announcer.
    chatLog.setAttribute("aria-live", "off");
    const login = document.querySelector(".login-card");
    login.setAttribute("role", "dialog");
    login.setAttribute("aria-modal", "true");
    if (!document.getElementById("login-overlay").classList.contains("hidden")) {
        requestAnimationFrame(() => document.getElementById("login-addr")?.focus({ preventScroll: true }));
    }
}

// Speaking events share the application's single polite live region (343).
function announce(text) {
    V().announceLive(text, "polite");
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
    document.getElementById("notif-bell")?.setAttribute(
        "aria-label", n > 0 ? `Notifications, ${n} unread` : "Notifications");
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
                row.tabIndex = 0;
                row.setAttribute("role", "button");
                row.onclick = () => {
                    overlay.remove();
                    window.__voicx.openPM(n.uid, "");
                };
            } else if (n.channelID) {
                row.classList.add("clickable");
                row.tabIndex = 0;
                row.setAttribute("role", "button");
                row.onclick = () => {
                    overlay.remove();
                    App().JoinChannel(n.channelID);
                };
            }
            if (row.classList.contains("clickable")) {
                row.addEventListener("keydown", (event) => {
                    if (!isActivationKey(event.key)) return;
                    event.preventDefault();
                    row.click();
                });
            }
            list.appendChild(row);
        }
    };
    overlay.innerHTML = `
        <div class="dlg notif-center">
            <div class="pm-head">
                <h3>${"Notifications"}</h3>
                <button class="icon-btn nc-clear" title="Clear all" aria-label="Clear all notifications">🗑</button>
                <button class="icon-btn nc-close" title="Close" aria-label="Close notifications">✕</button>
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
    mountDialog(overlay);
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
