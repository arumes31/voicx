// video.js — video grid (61), focus mode with filmstrip (73), per-subscriber
// quality selector (63), share dialog (69-72), camera on/off + stop-share
// confirm (85), and the low-bandwidth mode (88). Everything here works against
// the shared namespace (window.__voicx) populated by main.js.
import { GridCompositor } from "./grid-compositor.js";

import { isCurrentServerDialog, mountServerDialog } from "./modal.js";
import { setSafeImage } from "./safe-media.js";

const V = () => window.__voicx;

// (70/73) Track-identity contract with the router: a publisher's media arrives
// as one track per SLOT, so camera and screen share are separate tiles and
// shared system audio is a separate source from the microphone.
//   default slots ("mic", "cam")   track id "<clientID>"
//                                  msid stream "voicx-<clientID>"
//   extra slots ("screenaudio", "screen")
//                                  track id "<clientID>|<slot>"
//                                  msid stream "voicx-<clientID>|<slot>"
// The separator is "|" because an msid id is an RFC 4566 token and "/" is not
// a token character. Default slots keep the bare publisher ID, so parsing
// yields slot "" for them and a router that labels nothing still resolves.
const SLOT_SCREEN = "screen";
export const SLOT_SCREEN_AUDIO = "screenaudio"; // main.js routes this slot's audio
const SLOT_SEP = "|";

// parseTrackID splits a media track id into publisher id and slot.
export function parseTrackID(id) {
    const s = String(id ?? "");
    const i = s.indexOf(SLOT_SEP);
    return i < 0 ? { clientID: s, slot: "" } : { clientID: s.slice(0, i), slot: s.slice(i + 1) };
}

// tiles maps media track id -> {el, video, nameEl, kindEl, badgeEl, track,
// stream, clientID, slot, frames, stalls, flowing}. The key is the TRACK id,
// not the publisher, so one publisher can own several tiles (73). track is
// that slot's own video track and stream the per-tile MediaStream wrapping it
// (never the shared stream from ontrack).
const tiles = new Map();
let focusedID = null; // track id of the focused tile

// Connection-wide video quality preference. NOTE: the server's simulcast
// routing keeps ONE layer preference per subscriber (all publishers), so the
// tile context menu sets a shared preference, not a per-tile one — the menu
// says so. "auto" maps: focused tile -> high, grid view -> mid, low-bandwidth
// -> low.
let qualityPref = "auto"; // auto | high | mid | low
let lastSentQuality = "";
let idleOverride = false; // (342) window idle: force low without losing the pref
let cpuPressure = false;
let autoNetworkQuality = "high";
let statsTimer = null;
let compositeTimer = null;
let compositeVideo = null;

// ---------------------------------------------------------------------------
// Grid (61)
// ---------------------------------------------------------------------------

function gridEl() { return document.getElementById("video-grid"); }

// videoTrackAdded registers (or re-registers) one publisher video slot as a
// grid tile. trackID follows the slot contract above, so a publisher sending
// camera and screen at once gets one tile per slot (73).
export function videoTrackAdded(trackID, stream, publisher) {
    const key = String(trackID);
    const parsed = parseTrackID(key);
    const clid = String(publisher?.client_id || parsed.clientID);
    let t = tiles.get(key);
    if (!t) {
        const el = document.createElement("div");
        el.className = "vtile";
        el.dataset.clid = clid;
        el.dataset.slot = parsed.slot;
        el.innerHTML = `
            <video autoplay playsinline></video>
            <div class="vtile-fallback"><div class="avatar"></div></div>
            <div class="vtile-label">
                <span class="vtile-name"></span>
                <span class="vtile-kind hidden"></span>
                <span class="vtile-badge hidden"></span>
            </div>
            <button class="vtile-pip icon-btn" title="floating always-on-top video">▣</button>
            <button class="vtile-fullscreen icon-btn" title="fullscreen (Esc exits)">⛶</button>`;
        const video = el.querySelector("video");
        const nameEl = el.querySelector(".vtile-name");
        const kindEl = el.querySelector(".vtile-kind");
        const badgeEl = el.querySelector(".vtile-badge");
        // The fallback panel is opaque and paints over the <video>, so the
        // has-video class has to follow the element's decode state.
        video.onloadeddata = video.onplaying = video.onemptied = () => {
            updateTileVideo(t);
            // (88) a paused tile must show a still image, not an avatar: the
            // point of the mode is to stop decoding while still showing who is
            // on camera. So pausing waits for the first decoded frame — an
            // element paused before that has nothing to hold and would sit on
            // the avatar for the rest of the call.
            if (lowBandwidth && t.video.readyState >= 2) t.video.pause();
        };
        // (340) fullscreen video tile.
        el.querySelector(".vtile-fullscreen").onclick = (e) => {
            e.stopPropagation();
            if (document.fullscreenElement) document.exitFullscreen();
            else el.requestFullscreen?.().catch(() => {});
        };
        const pip = el.querySelector(".vtile-pip");
        pip.disabled = !document.pictureInPictureEnabled;
        pip.onclick = async (e) => {
            e.stopPropagation();
            try {
                if (document.pictureInPictureElement === video) await document.exitPictureInPicture();
                else {
                    if (document.pictureInPictureElement) await document.exitPictureInPicture();
                    await video.requestPictureInPicture();
                }
            } catch (err) {
                V().sysMsg("floating video unavailable: " + (err.message || err));
            }
        };
        el.onclick = () => toggleFocus(key);
        el.oncontextmenu = (e) => {
            e.preventDefault();
            openTileMenu(e.clientX, e.clientY, t);
        };
        t = {
            el, video, nameEl, kindEl, badgeEl, track: null, stream: null,
            clientID: clid, slot: parsed.slot, frames: 0, stalls: 0, flowing: true,
        };
        tiles.set(key, t);
        gridEl().appendChild(el);
    }
    t.clientID = clid;
    t.el.dataset.clid = clid;
    // (61) select by track id and render from a private stream, so a tile can
    // never follow another slot or publisher even if the ontrack stream ever
    // carries more than one track.
    const vt = stream?.getVideoTracks().find((tr) => tr.id === key) || null;
    t.track = vt;
    t.stream = vt ? new MediaStream([vt]) : null;
    t.video.srcObject = t.stream;
    t.frames = 0;
    t.stalls = 0;
    t.flowing = true;
    // A publisher turning the camera off arrives as a muted receiver track,
    // not as a removed track — that flips the tile back to the avatar.
    if (vt) vt.onmute = vt.onunmute = vt.onended = () => updateTileVideo(t);
    updateTileVideo(t);
    applyTileIdentity(t);
    layoutGrid();
    startStatsPoll();
}

// videoTrackRemoved drops one slot's tile (track ended / publisher left).
export function videoTrackRemoved(trackID) {
    const key = String(trackID);
    const t = tiles.get(key);
    if (!t) return;
    t.video.srcObject = null;
    t.el.remove();
    tiles.delete(key);
    if (focusedID === key) focusedID = null;
    layoutGrid();
    if (tiles.size === 0) stopStatsPoll();
}

// videoSpeaking toggles the speaking ring on every tile of a publisher: with
// slot tiles (73) a speaker can own a camera tile and a screen tile, and a
// publisher with no video at all still gets the ring around their avatar (61).
export function videoSpeaking(clientID, speaking) {
    const clid = String(clientID);
    for (const t of tiles.values()) {
        if (t.clientID === clid) t.el.classList.toggle("speaking", speaking);
    }
}

// videoRefreshNames re-resolves nickname/avatar on all tiles (user joined,
// moved, avatar changed, started/stopped sharing).
export function videoRefreshNames() {
    for (const t of tiles.values()) applyTileIdentity(t);
}

// clearVideoGrid removes all tiles (voice teardown).
export function clearVideoGrid() {
    for (const key of [...tiles.keys()]) videoTrackRemoved(key);
    focusedID = null;
    // the next session starts from the server's default layer, so a stale
    // lastSentQuality would suppress the first push of this session.
    lastSentQuality = "";
    layoutGrid();
}

// applyTileIdentity fills the nickname label, the "what is this" marker and the
// avatar fallback (61: avatar-with-ring fallback behind the video; visible when
// no frames flow, e.g. camera off or black screen share).
function applyTileIdentity(t) {
    const { state, initials, fetchAvatar } = V();
    const clid = t.clientID;
    const c = state.clients.find((c) => String(c.client_id) === clid);
    const name = c ? (c.nickname || c.unique_id) : clid;
    t.nameEl.textContent = name;
    // (73) several tiles can carry the same nickname, so each one says what it
    // shows.
    const isScreen = tileIsScreen(t);
    t.kindEl.textContent = isScreen ? "🖥 screen" : "";
    t.kindEl.classList.toggle("hidden", !isScreen);
    t.el.classList.toggle("is-screen", isScreen);
    t.el.title = name + (isScreen ? " — screen share" : "");
    const av = t.el.querySelector(".vtile-fallback .avatar");
    const uid = c?.unique_id || "";
    av.dataset.uid = uid;
    const url = uid && state.avatars.get(uid);
    if (url) {
        setSafeImage(av, url);
    } else {
        av.textContent = initials(name || "?");
        if (uid) fetchAvatar(uid);
    }
    t.el.classList.toggle("speaking", !!(c && c.is_speaking));
}

// tileIsScreen reports whether a tile shows a shared surface. A publisher that
// declares the "screen" slot has a dedicated tile for it, so its default tile
// is the camera. One that does not publishes the share on the default slot
// instead, where the screenshare_changed flag is the only marker (73).
function tileIsScreen(t) {
    if (t.slot === SLOT_SCREEN) return true;
    if (t.slot !== "" || !isSharing(t.clientID)) return false;
    for (const other of tiles.values()) {
        if (other.clientID === t.clientID && other.slot === SLOT_SCREEN) return false;
    }
    return true;
}

// updateTileVideo toggles has-video, which hides the avatar fallback. Frames
// have to be decoded (readyState >= HAVE_CURRENT_DATA), the track live, and
// the stream not stalled (61, see refreshBadges): a paused low-bandwidth tile
// still shows its last frame, a camera that was switched off does not.
function updateTileVideo(t) {
    const track = t.track;
    const live = !!track && track.readyState === "live" && track.enabled && !track.muted;
    t.el.classList.toggle("has-video", live && t.flowing && t.video.readyState >= 2);
}

// layoutGrid recomputes the auto-layout class (1→full, 2→half, 3-4→2x2,
// more→scrollable) and hides the grid when empty so chat keeps the space.
function layoutGrid() {
    const grid = gridEl();
    const n = tiles.size;
    grid.classList.toggle("hidden", n === 0);
    grid.dataset.count = n <= 4 ? String(n) : "many";
    grid.classList.toggle("has-focus", !!focusedID && tiles.has(focusedID));
    for (const [key, t] of tiles) t.el.classList.toggle("focused", key === focusedID);
}

// ---------------------------------------------------------------------------
// Focus mode (73): click a tile for the large view + filmstrip; click again
// or Esc to return to the grid. Other tiles keep playing in the strip.
// ---------------------------------------------------------------------------

function toggleFocus(clid) {
    focusedID = focusedID === clid ? null : clid;
    layoutGrid();
    pushQuality(); // auto heuristic: focused = high, grid = mid
}

export function initVideo() {
    syncCameraButton(); // (85) disabled until a voice session captures a camera
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && focusedID) {
            focusedID = null;
            layoutGrid();
            pushQuality();
        }
    });
}

// ---------------------------------------------------------------------------
// Quality selector (63)
// ---------------------------------------------------------------------------

// effectiveQuality maps the preference to a concrete layer. Auto heuristic:
// focused view -> high, grid view -> mid, low-bandwidth mode -> low.
function effectiveQuality() {
    if (lowBandwidth || idleOverride || cpuPressure) return "low";
    if (qualityPref !== "auto") return qualityPref;
    const viewQuality = focusedID ? "high" : "mid";
    const rank = { low: 0, mid: 1, high: 2 };
    return rank[autoNetworkQuality] < rank[viewQuality] ? autoNetworkQuality : viewQuality;
}

// setIdleQualityOverride is the idle-pause hook (342). It goes through the
// state machine so lastSentQuality stays in sync with the server and the
// user's preference comes back on resume.
export function setIdleQualityOverride(on) {
    if (idleOverride === on) return;
    idleOverride = on;
    pushQuality();
}

// pushQuality sends MsgVideoQuality when the effective layer changed.
async function pushQuality() {
    const q = effectiveQuality();
    // (88) the persisted restore and the voice-bar toggle both run while
    // disconnected, where there is no connection to send the preference on.
    // Clearing the last-sent value keeps the first push after joining honest.
    if (!V().state.pc) {
        lastSentQuality = "";
        return;
    }
    if (q === lastSentQuality) return;
    lastSentQuality = q;
    const err = await window.go.main.App.SetVideoQuality(q);
    if (err) V().sysMsg("video quality failed: " + err);
}

let qMenuEl = null;

// openTileMenu is the tile right-click menu: the shared receive-quality
// preference (63) plus, when the tile's publisher is sharing system audio,
// that share's own volume/mute (70).
function openTileMenu(x, y, t) {
    if (qMenuEl) qMenuEl.remove();
    qMenuEl = document.createElement("div");
    qMenuEl.className = "ctx-menu";
    const cur = qualityPref;
    const sa = V().shareAudioCtl?.get(t.clientID) || null;
    qMenuEl.innerHTML = `
        <a data-q="auto">${cur === "auto" ? "✓ " : ""}Auto (follows view)</a>
        <a data-q="high">${cur === "high" ? "✓ " : ""}High</a>
        <a data-q="mid">${cur === "mid" ? "✓ " : ""}Mid</a>
        <a data-q="low">${cur === "low" ? "✓ " : ""}Low</a>
        <div class="ctx-divider"></div>
        <a class="ctx-note">applies to all incoming video</a>
        <a data-grid-pip="1">Float whole grid</a>` + (sa ? `
        <div class="ctx-divider"></div>
        <a data-sa="mute">${sa.muted ? "Unmute" : "Mute"} shared system audio</a>
        <div class="ctx-volume">
            <span>Shared audio <span class="mono ctx-vol-pct">${sa.volume}%</span></span>
            <input type="range" min="0" max="200" value="${sa.volume}" />
        </div>
        <a class="ctx-note">separate from this user's voice volume/mute</a>` : "");
    qMenuEl.style.left = Math.min(x, window.innerWidth - 260) + "px";
    qMenuEl.style.top = Math.min(y, window.innerHeight - (sa ? 320 : 200)) + "px";
    qMenuEl.onclick = (e) => e.stopPropagation();
    for (const a of qMenuEl.querySelectorAll("a[data-q]")) {
        a.onclick = () => {
            qualityPref = a.dataset.q;
            pushQuality();
            qMenuEl.remove();
            qMenuEl = null;
        };
    }
    qMenuEl.querySelector("[data-grid-pip]").onclick = () => openGridOverlay();
    if (sa) {
        qMenuEl.querySelector('a[data-sa="mute"]').onclick = () => {
            V().shareAudioCtl.setMuted(t.clientID, !sa.muted);
            qMenuEl.remove();
            qMenuEl = null;
        };
        const slider = qMenuEl.querySelector(".ctx-volume input");
        slider.oninput = () => {
            qMenuEl.querySelector(".ctx-vol-pct").textContent = slider.value + "%";
            V().shareAudioCtl.setVolume(t.clientID, parseInt(slider.value, 10));
        };
    }
    document.body.appendChild(qMenuEl);
    const close = () => { if (qMenuEl) { qMenuEl.remove(); qMenuEl = null; } };
    setTimeout(() => {
        document.addEventListener("click", close, { once: true });
        document.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); }, { once: true });
    });
}

async function openGridOverlay() {
    if (!document.pictureInPictureEnabled || tiles.size === 0) {
        V().sysMsg("floating grid is unavailable");
        return;
    }
    stopGridOverlay();
    try {
        const compositor = new GridCompositor();
        const canvas = document.createElement("canvas");
        canvas.width = 1280;
        canvas.height = 720;
        canvas.className = "grid-composite-canvas";
        document.body.appendChild(canvas);
        const output = document.createElement("video");
        output.muted = true;
        output.playsInline = true;
        output.srcObject = canvas.captureStream(15);
        output.className = "grid-composite-video";
        document.body.appendChild(output);
        await output.play();
        const target = canvas.getContext("bitmaprenderer");
        let drawing = false;
        const draw = async () => {
            if (drawing) return;
            drawing = true;
            try {
                target.transferFromImageBitmap(await compositor.compose([...tiles.values()].map((tile) => tile.video)));
            } finally { drawing = false; }
        };
        await draw();
        compositeTimer = setInterval(draw, 1000 / 15);
        compositeVideo = output;
        output.onleavepictureinpicture = stopGridOverlay;
        await output.requestPictureInPicture();
    } catch (err) {
        stopGridOverlay();
        V().sysMsg("floating grid failed: " + (err.message || err));
    }
}

function stopGridOverlay() {
    if (compositeTimer) clearInterval(compositeTimer);
    compositeTimer = null;
    if (compositeVideo) {
        compositeVideo.srcObject?.getTracks().forEach((track) => track.stop());
        compositeVideo.remove();
    }
    compositeVideo = null;
    document.querySelector(".grid-composite-canvas")?.remove();
}

// startStatsPoll refreshes the per-tile layer badge from getStats (63,
// best-effort): inbound-rtp frameWidth maps f/h/q to HD/MD/LD.
function startStatsPoll() {
    if (statsTimer) return;
    statsTimer = setInterval(refreshBadges, 3000);
}

function stopStatsPoll() {
    if (statsTimer) {
        clearInterval(statsTimer);
        statsTimer = null;
    }
}

// STALL_POLLS is how many consecutive polls without a newly decoded frame
// count as "the camera is off" (61). Two polls ≈ 6 s, long enough to survive a
// freeze/keyframe gap and short enough that a camera-off does not linger.
const STALL_POLLS = 2;

async function refreshBadges() {
    const { state } = V();
    if (!state.pc || tiles.size === 0) return;
    try {
        const stats = await state.pc.getStats();
        const byTrack = new Map(); // trackIdentifier -> {w, frames}
        let availableIncomingBitrate = 0;
        stats.forEach((r) => {
            if (r.type === "inbound-rtp" && (r.kind === "video" || r.mediaType === "video")) {
                byTrack.set(r.trackIdentifier, { w: r.frameWidth || 0, frames: r.framesDecoded || 0 });
            }
            if (r.type === "candidate-pair" && (r.nominated || r.selected) && r.state === "succeeded") {
                availableIncomingBitrate = Math.max(availableIncomingBitrate, r.availableIncomingBitrate || 0);
            }
        });
        if (availableIncomingBitrate > 0 && qualityPref === "auto") {
            const next = availableIncomingBitrate < 700000 ? "low" : availableIncomingBitrate < 2500000 ? "mid" : "high";
            if (next !== autoNetworkQuality) {
                autoNetworkQuality = next;
                pushQuality();
            }
        }
        const cpu = await window.go.main.App.SystemCPUPercent();
        const nextPressure = cpu >= 85;
        if (nextPressure !== cpuPressure) {
            cpuPressure = nextPressure;
            V().sysMsg(nextPressure ? `CPU ${cpu.toFixed(0)}% — reducing video resolution` : "CPU recovered — restoring video resolution");
            lastSentQuality = "";
            pushQuality();
            applySendCaps();
        }
        for (const t of tiles.values()) {
            const s = (t.track && byTrack.get(t.track.id)) || null;
            // (61) a camera switched off mid-call keeps its receiver track
            // "live" and unmuted in WebView2 — onmute never fires — so without
            // this the tile would freeze on a stale frame for the rest of the
            // call. Screen shares are exempt: a still desktop legitimately
            // encodes nothing, and so does a tile paused by low-bandwidth
            // mode (88), which is meant to hold its last frame.
            const exempt = tileIsScreen(t) || t.video.paused;
            if (!s || exempt) {
                t.stalls = 0;
                t.flowing = true;
            } else if (s.frames > t.frames) {
                t.frames = s.frames;
                t.stalls = 0;
                t.flowing = true;
            } else if (++t.stalls >= STALL_POLLS) {
                t.flowing = false;
            }
            updateTileVideo(t); // WebView2 does not always fire track mute/unmute
            const w = s ? s.w : 0;
            if (!w || !t.flowing) {
                t.badgeEl.classList.add("hidden");
                continue;
            }
            t.badgeEl.classList.remove("hidden");
            t.badgeEl.textContent = w >= 1280 ? "HD" : w >= 640 ? "MD" : "LD";
        }
    } catch { /* stats unavailable */ }
}

// isSharing reports the publisher's screen-share flag from screenshare_changed.
function isSharing(clientID) {
    const c = V().state.clients.find((c) => String(c.client_id) === String(clientID));
    return !!c?.sharing;
}

// ---------------------------------------------------------------------------
// Low-bandwidth mode (88): cap own video send to 150 kbps (single layer),
// request the low simulcast layer, pause tile rendering, persist the toggle.
// ---------------------------------------------------------------------------

let lowBandwidth = false;

// LOW_BW_BITRATE is the send ceiling of the mode; a screen share must not lift
// it (88), so the share preset caps are clamped to it while the mode is on.
const LOW_BW_BITRATE = 150000;

export function isLowBandwidth() { return lowBandwidth; }

// setLowBandwidth toggles the mode; persist=true saves it into settings.
export async function setLowBandwidth(on, persist) {
    lowBandwidth = on;
    const { $ } = V();
    $("voice-lowbw").classList.toggle("active", on);
    $("voice-lowbw").setAttribute("aria-pressed", String(on));
    gridEl().classList.toggle("lowbw", on);
    lastSentQuality = ""; // force re-push with the new effective quality
    pushQuality();
    for (const t of tiles.values()) {
        // (88) same rule as the decode handler: a tile with nothing decoded yet
        // keeps playing until its first frame lands and pauses on it there, so
        // every tile ends up frozen on a picture rather than on an avatar.
        if (on) { if (t.video.readyState >= 2) t.video.pause(); }
        else t.video.play().catch(() => {});
    }
    applySendCaps();
    if (persist) {
        const s = Object.assign({}, V().state.settings, { low_bandwidth: on });
        const err = await window.go.main.App.SaveSettings(s);
        // (282) re-read rather than caching the copy we sent: the Go side owns
        // fields the frontend never has (recents, what's-new marker).
        if (!err) V().state.settings = await window.go.main.App.GetSettings();
    }
}

// videoSender returns the sender of a video transceiver this client can send
// on, or null. Matches by transceiver so it never confuses the share-audio
// sender, and skips recvonly ones: with no camera there is no send transceiver
// at all and replaceTrack on a server-side recvonly would publish nothing.
function videoSenderFor(pc) {
    if (!pc) return null;
    const t = pc.getTransceivers().find((t) =>
        (t.receiver.track?.kind === "video" || t.sender.track?.kind === "video") &&
        (t.direction === "sendrecv" || t.direction === "sendonly"));
    return t ? t.sender : null;
}

function videoSender() {
    return videoSenderFor(V().state.pc);
}

// applySendCaps caps (or restores) the outgoing video bitrate: 150 kbps on a
// single active layer in low-bandwidth mode, uncapped simulcast otherwise.
function applySendCaps() {
    const sender = videoSender();
    if (!sender) return;
    const p = sender.getParameters();
    if (!p.encodings?.length) return;
    p.encodings.forEach((e, i) => {
        if (lowBandwidth || cpuPressure) {
            e.active = i === 0;
            e.maxBitrate = lowBandwidth ? LOW_BW_BITRATE : 500000;
            e.scaleResolutionDownBy = Math.max(Number(e.scaleResolutionDownBy) || 1, 2);
        } else {
            e.active = true;
            e.scaleResolutionDownBy = 1;
            // a share sets a preset ceiling on the same sender; restore it
            // (undefined = codec default) so stopping the share uncaps again.
            e.maxBitrate = sharePresetBitrate || undefined;
        }
    });
    sender.setParameters(p).catch(() => {});
}

// sharePresetBitrate is the active share's preset ceiling (72), 0 when the
// camera is on the sender. applySendCaps needs it so leaving low-bandwidth
// mode mid-share restores the preset instead of uncapping the share.
let sharePresetBitrate = 0;

// ---------------------------------------------------------------------------
// Screen share (69-72, 85)
// ---------------------------------------------------------------------------

// Share quality presets (72): applied via getDisplayMedia constraints plus a
// sender maxBitrate cap.
const SHARE_PRESETS = {
    text: { label: "Text (1080p15, sharp)", width: 1920, height: 1080, fps: 15, bitrate: 2500000 },
    balanced: { label: "Balanced (720p30)", width: 1280, height: 720, fps: 30, bitrate: 1500000 },
    motion: { label: "Motion (720p60)", width: 1280, height: 720, fps: 60, bitrate: 2500000 },
};

// (71) Chromium puts cropTo on BrowserCaptureMediaStreamTrack, a SUBCLASS of
// MediaStreamTrack — probing the base prototype reports "unsupported" on every
// browser that actually has the API. The base check stays as a fallback for
// builds that shipped it there.
const regionSupported = typeof CropTarget !== "undefined" && (
    (typeof BrowserCaptureMediaStreamTrack !== "undefined" &&
        "cropTo" in BrowserCaptureMediaStreamTrack.prototype) ||
    (typeof MediaStreamTrack !== "undefined" && "cropTo" in MediaStreamTrack.prototype));

// shareToggle is the voice-screen button handler: stop when sharing
// (confirming first, 85), otherwise open the share dialog.
export async function shareToggle() {
    const { state } = V();
    if (state.screenSharing) {
        await confirmStopShare();
        return;
    }
    openShareDialog();
}

function openShareDialog() {
    const { $ } = V();
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg share-dlg">
            <h3>Share screen</h3>
            <label class="dlg-label">Source</label>
            <div class="share-sources">
                <label><input type="radio" name="shsrc" value="monitor" checked /> Screen</label>
                <label><input type="radio" name="shsrc" value="window" /> Window</label>
                <label title="${regionSupported ? "Crop the share to a region of this app" : "Region Capture not available in this WebView2"}">
                    <input type="radio" name="shsrc" value="region" ${regionSupported ? "" : "disabled"} /> Region of this app
                </label>
            </div>
            <label class="dlg-label">Quality preset</label>
            <select class="dlg-input sh-preset">
                <option value="balanced">${SHARE_PRESETS.balanced.label}</option>
                <option value="text">${SHARE_PRESETS.text.label}</option>
                <option value="motion">${SHARE_PRESETS.motion.label}</option>
            </select>
            <label class="share-audio"><input type="checkbox" class="sh-audio" /> Include system audio</label>
            <div class="dlg-buttons">
                <button class="dlg-ok">Start sharing</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    overlay.querySelector(".dlg-ok").onclick = async () => {
        if (!isCurrentServerDialog(overlay)) return;
        const surface = overlay.querySelector('input[name="shsrc"]:checked').value;
        const preset = overlay.querySelector(".sh-preset").value;
        const withAudio = overlay.querySelector(".sh-audio").checked;
        overlay.remove();
        await startShare({ surface, preset, withAudio });
    };
    mountServerDialog(overlay);
    if (!regionSupported) {
        // (71) the radio is disabled, so the dialog has to say why and what to
        // do instead — a disabled control with no explanation reads as a bug.
        const note = document.createElement("div");
        note.className = "set-hint warn";
        note.textContent = "Region of this app needs the Region Capture API (CropTarget), " +
            "which this WebView2 runtime does not have — update WebView2, or share the whole window.";
        overlay.querySelector(".share-sources").appendChild(note);
    }
}

// startShare captures the display (69: displaySurface preference — Chromium's
// picker ultimately decides what is shareable), replaces the camera slot with
// the screen track (one video track per peer), optionally merges display
// audio (70), and applies the quality preset (72).
async function startShare({ surface, preset, withAudio }) {
    const { state } = V();
    const generation = state.serverGeneration;
    const peerConnection = state.pc;
    const current = () => state.serverGeneration === generation && state.pc === peerConnection;
    const discardDisplay = (stream) => stream?.getTracks().forEach((track) => track.stop());
    const p = SHARE_PRESETS[preset] || SHARE_PRESETS.balanced;
    const video = {
        width: { ideal: p.width },
        height: { ideal: p.height },
        frameRate: { ideal: p.fps },
    };
    if (surface !== "region") video.displaySurface = surface; // 69: "monitor" | "window"
    const gdm = { video, audio: !!withAudio };
    if (surface === "region") {
        // (71) Region Capture only applies to a self-capture of this app's own
        // surface. Without these the picker hands back a monitor/window track
        // and cropTo() rejects it, so the region path could never succeed.
        gdm.preferCurrentTab = true;
        gdm.selfBrowserSurface = "include";
    }
    let display;
    try {
        display = await navigator.mediaDevices.getDisplayMedia(gdm);
    } catch (e) {
        if (!current()) return;
        if (withAudio) {
            // (70) WebView2 may refuse display audio (works on Windows for
            // screen/tab shares) — retry video-only and say so.
            V().sysMsg("system audio not available for this share (" + (e.message || e.name) + "); sharing video only");
            try {
                display = await navigator.mediaDevices.getDisplayMedia(Object.assign({}, gdm, { audio: false }));
                withAudio = false;
            } catch (e2) {
                if (!current()) return;
                V().sysMsg("screen capture failed: " + (e2.message || e2.name));
                return;
            }
        } else {
            V().sysMsg("screen capture failed: " + (e.message || e.name));
            return;
        }
    }
    if (!current()) {
        discardDisplay(display);
        return;
    }
    const screenTrack = display.getVideoTracks()[0];
    if (!screenTrack) {
        discardDisplay(display);
        V().sysMsg("screen capture produced no video track");
        return;
    }
    screenTrack.contentHint = preset === "text" ? "detail" : "motion";

    if (surface === "region") {
        const ok = await pickRegionAndCrop(screenTrack, current);
        if (!ok || !current()) {
            discardDisplay(display);
            return; // cancelled
        }
    }

    state.shareStream = display;
    let vs = videoSenderFor(peerConnection);
    if (!vs && peerConnection) {
        // Cameraless machine: startVoice never added a send transceiver, so
        // the share needs its own one plus a renegotiation. addTrack would
        // recycle one of the server's recvonly m-lines and publish on a
        // subscriber slot, so take a dedicated sendonly transceiver.
        let tr = null;
        try {
            tr = peerConnection.addTransceiver(screenTrack, { direction: "sendonly", streams: [display] });
            vs = tr.sender;
            await renegotiate(peerConnection, generation);
            if (!current()) {
                discardDisplay(display);
                return;
            }
        } catch (e) {
            if (!current()) {
                discardDisplay(display);
                return;
            }
            V().sysMsg("publishing screen share failed: " + (e.message || e.name));
            // a rollback only drops transceivers created by applying a remote
            // description, so this one survives with its direction flip and
            // would be re-offered — stop it to keep the dead m-line out of
            // videoSender()'s reach.
            try { tr?.stop(); } catch { /* nothing left to stop */ }
            display.getTracks().forEach((t) => t.stop());
            state.shareStream = null;
            clearRegionBox(); // (71) nothing is being cropped after this
            return;
        }
    } else if (vs) {
        try {
            await vs.replaceTrack(screenTrack);
            if (!current()) {
                discardDisplay(display);
                return;
            }
        } catch (e) {
            if (!current()) {
                discardDisplay(display);
                return;
            }
            V().sysMsg("publishing screen share failed: " + (e.message || e.name));
            display.getTracks().forEach((t) => t.stop());
            state.shareStream = null;
            clearRegionBox(); // (71)
            return;
        }
    }
    // (72) bitrate cap per preset — remembered so applySendCaps can restore it
    // when low-bandwidth mode is switched off while the share runs.
    sharePresetBitrate = p.bitrate;
    if (vs) {
        const params = vs.getParameters();
        if (params.encodings?.length) {
            // (88) the mode's 150 kbps ceiling outranks the preset: without the
            // clamp, starting a share silently uncapped low-bandwidth mode
            // until the share ended.
            params.encodings.forEach((e, i) => {
                e.active = lowBandwidth ? i === 0 : true;
                e.maxBitrate = lowBandwidth ? LOW_BW_BITRATE : p.bitrate;
            });
            vs.setParameters(params).catch(() => {});
        }
    }
    screenTrack.onended = () => doStopShare(); // browser "stop sharing" UI

    // (70) merge display audio as a second published audio track (the server
    // fans out every publisher audio track) + renegotiate.
    const displayAudio = withAudio ? display.getAudioTracks()[0] : null;
    if (displayAudio && peerConnection) {
        if (!state.shareAudioTransceiver) {
            let tr = null;
            try {
                // addTrack would recycle one of the server's recvonly audio
                // m-lines and publish the share on a subscriber slot, so take a
                // dedicated sendonly transceiver.
                tr = peerConnection.addTransceiver(displayAudio, { direction: "sendonly", streams: [display] });
                state.shareAudioTransceiver = tr;
                state.shareAudioSender = tr.sender;
                await renegotiate(peerConnection, generation);
                if (!current()) {
                    discardDisplay(display);
                    return;
                }
            } catch (e) {
                if (!current()) {
                    discardDisplay(display);
                    return;
                }
                V().sysMsg("publishing share audio failed: " + (e.message || e.name));
                // a rollback keeps this transceiver, so without the stop() the
                // dead track is re-offered on the next renegotiation.
                try { tr?.stop(); } catch { /* nothing left to stop */ }
                displayAudio.stop();
                state.shareAudioTransceiver = null;
                state.shareAudioSender = null;
            }
        } else {
            try {
                state.shareAudioSender = state.shareAudioTransceiver.sender;
                await state.shareAudioSender.replaceTrack(displayAudio);
                if (!current()) {
                    discardDisplay(display);
                    return;
                }
            } catch (e) {
                if (!current()) {
                    discardDisplay(display);
                    return;
                }
                V().sysMsg("publishing share audio failed: " + (e.message || e.name));
                displayAudio.stop();
            }
        }
    }

    if (!current()) {
        discardDisplay(display);
        return;
    }
    state.screenSharing = true;
    const shareErr = await window.go.main.App.SetScreenShareQuality(true, p.height);
    if (!current()) {
        discardDisplay(display);
        return;
    }
    if (shareErr) {
        V().sysMsg("screen share permission failed: " + shareErr);
        await doStopShare();
        return;
    }
    document.getElementById("voice-screen").classList.add("active");
    document.getElementById("voice-screen").setAttribute("aria-pressed", "true");
}

// pickRegionAndCrop shows a draggable/resizable box over the app; on confirm
// the track is cropped to it via the Region Capture API (71). The box then
// STAYS in the document: a crop target with no layout box produces no frames,
// so removing it would blank the share. It is torn down in doStopShare.
// The crop itself lives on the MediaStreamTrack, so it survives the
// renegotiation that publishes the share (and any later one).
let cancelPendingRegion = null;

function pickRegionAndCrop(track, isCurrent = () => true) {
    return new Promise((resolve) => {
        cancelPendingRegion?.();
        const box = document.createElement("div");
        box.className = "region-box";
        box.innerHTML = `
            <div class="region-title">drag to move · drag corner to resize</div>
            <button class="region-ok">Crop &amp; share</button>
            <button class="region-cancel">Cancel</button>`;
        document.body.appendChild(box);
        let settled = false;
        const finish = (value) => {
            if (settled) return;
            settled = true;
            if (cancelPendingRegion === cancel) cancelPendingRegion = null;
            resolve(value);
        };
        const cancel = () => {
            box.remove();
            finish(false);
        };
        cancelPendingRegion = cancel;

        // Drag by the title bar; resize via CSS resize handle.
        const title = box.querySelector(".region-title");
        title.onpointerdown = (e) => {
            const startX = e.clientX - box.offsetLeft;
            const startY = e.clientY - box.offsetTop;
            const move = (ev) => {
                box.style.left = Math.max(0, ev.clientX - startX) + "px";
                box.style.top = Math.max(0, ev.clientY - startY) + "px";
            };
            const up = () => {
                document.removeEventListener("pointermove", move);
                document.removeEventListener("pointerup", up);
            };
            document.addEventListener("pointermove", move);
            document.addEventListener("pointerup", up);
        };

        box.querySelector(".region-cancel").onclick = cancel;
        box.querySelector(".region-ok").onclick = async () => {
            if (settled || !isCurrent()) {
                cancel();
                return;
            }
            try {
                const target = await CropTarget.fromElement(box);
                await track.cropTo(target);
                if (settled || !isCurrent()) {
                    cancel();
                    return;
                }
                // the box is inside its own crop rect from now on, so it has to
                // stop painting anything: .cropping leaves a transparent hole
                // and marks the region with an outline, which is drawn outside
                // the border box and therefore outside the captured rect.
                box.innerHTML = "";
                box.classList.add("cropping");
                V().state.regionBox = box;
                finish(true);
            } catch (e) {
                if (!settled && isCurrent()) V().sysMsg("region capture failed: " + (e.message || e.name));
                cancel();
            }
        };
    });
}

// clearRegionBox drops the persistent crop target (71).
export function clearRegionBox() {
    const cancel = cancelPendingRegion;
    cancelPendingRegion = null;
    cancel?.();
    const { state } = V();
    if (state.regionBox) {
        state.regionBox.remove();
        state.regionBox = null;
    }
}

// receiverCount is the set of users the SFU could be forwarding my video to:
// it only fans a publisher out to peers in the publisher's own channel.
function receiverCount() {
    const { state } = V();
    if (!state.myChannelID) return 0;
    return state.clients.filter((c) =>
        c.channel_id === state.myChannelID && c.client_id !== state.myClientID).length;
}

// videoIsBeingDecoded turns "may be watching" into evidence (85). The router
// relays subscriber PLI/FIR/NACK back to the publisher, so a non-zero count on
// my video sender means a subscriber's decoder really is consuming this
// stream; zero only means nobody had to ask, hence the softer wording.
async function videoIsBeingDecoded() {
    const sender = videoSender();
    if (!sender || !V().state.pc) return false;
    try {
        const stats = await sender.getStats();
        let n = 0;
        stats.forEach((r) => {
            if (r.type === "outbound-rtp" && (r.kind === "video" || r.mediaType === "video")) {
                n += (r.pliCount || 0) + (r.firCount || 0) + (r.nackCount || 0);
            }
        });
        return n > 0;
    } catch {
        return false;
    }
}

// confirmStopPublish (85) asks before cutting a stream others receive; it
// stops straight away when nobody in the channel can be receiving it.
async function confirmStopPublish({ heading, what, stopLabel, keepLabel, onStop }) {
    const { state } = V();
    const generation = state.serverGeneration;
    const peerConnection = state.pc;
    const n = receiverCount();
    if (n === 0) {
        onStop();
        return;
    }
    const decoded = await videoIsBeingDecoded();
    if (state.serverGeneration !== generation || state.pc !== peerConnection) return;
    const users = n + " user" + (n === 1 ? "" : "s");
    const text = decoded
        ? `${users} in your channel — at least one is decoding your ${what} right now.`
        : `${users} in your channel can receive your ${what}.`;
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg share-dlg">
            <h3>${heading}</h3>
            <p class="dlg-text">${text}</p>
            <div class="dlg-buttons">
                <button class="dlg-ok">${stopLabel}</button>
                <button class="dlg-cancel">${keepLabel}</button>
            </div>
        </div>`;
    overlay.querySelector(".dlg-ok").onclick = () => {
        overlay.remove();
        onStop();
    };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    mountServerDialog(overlay);
}

function confirmStopShare() {
    return confirmStopPublish({
        heading: "Stop sharing?",
        what: "screen",
        stopLabel: "Stop sharing",
        keepLabel: "Keep sharing",
        onStop: () => doStopShare(),
    });
}

// doStopShare restores the camera slot and stops the display tracks.
async function doStopShare() {
    const { state } = V();
    if (!state.screenSharing) return;
    state.screenSharing = false;
    document.getElementById("voice-screen").classList.remove("active");
    document.getElementById("voice-screen").setAttribute("aria-pressed", "false");
    await window.go.main.App.SetScreenShare(false);

    if (state.shareAudioSender && state.pc) {
        // The transceiver stays (stopping the track silences it); removing it
        // would need another renegotiation.
        state.shareAudioSender.track?.stop();
        state.shareAudioSender.replaceTrack(null).catch(() => {});
        state.shareAudioSender = null;
    }
    if (state.shareStream) {
        state.shareStream.getTracks().forEach((t) => t.stop());
        state.shareStream = null;
    }
    clearRegionBox(); // (71)
    sharePresetBitrate = 0;
    // (85) the camera slot only comes back when the camera is actually on:
    // restoring a track the user switched off would republish it silently.
    const camera = cameraOff ? null : (state.localStream?.getVideoTracks()[0] || null);
    const vs = videoSender();
    if (vs) {
        await vs.replaceTrack(camera).catch(() => {});
        applySendCaps(); // restore low-bandwidth/uncapped state
    }
    syncCameraButton();
}

// ---------------------------------------------------------------------------
// Camera on/off (85): the video half of the voice-bar quick button. It only
// gates a camera captured at join — a session that started without one has no
// send transceiver, and adding one would need a renegotiation with nothing to
// send on it.
// ---------------------------------------------------------------------------

let cameraOff = false;

// syncCameraButton reflects camera availability and state in the voice bar.
function syncCameraButton() {
    const { state, $ } = V();
    const cam = state.localStream?.getVideoTracks()[0] || null;
    const btn = $("voice-video");
    if (!btn) return;
    btn.disabled = !cam;
    btn.classList.toggle("active", !!cam && !cameraOff);
    btn.setAttribute("aria-pressed", String(!!cam && !cameraOff));
    btn.title = !cam ? "No camera in this session"
        : cameraOff ? "Camera off — click to turn on" : "Camera on — click to turn off";
    $("local-video").classList.toggle("hidden", !cam || cameraOff);
}

// resetCameraState clears the toggle for a fresh (or ended) voice session.
export function resetCameraState() {
    cameraOff = false;
    sharePresetBitrate = 0;
    syncCameraButton();
}

// cameraToggle is the voice-video button handler.
export async function cameraToggle() {
    const { state } = V();
    const cam = state.localStream?.getVideoTracks()[0] || null;
    if (!cam) {
        V().sysMsg("no camera in this voice session");
        return;
    }
    if (cameraOff) {
        await setCameraOff(false);
        return;
    }
    // While sharing, the camera is not on the wire at all — the share owns the
    // send slot — so there is nothing for anyone to be watching yet.
    if (state.screenSharing) {
        await setCameraOff(true);
        return;
    }
    await confirmStopPublish({
        heading: "Turn camera off?",
        what: "camera",
        stopLabel: "Turn off",
        keepLabel: "Keep on",
        onStop: () => setCameraOff(true),
    });
}

async function setCameraOff(off) {
    const { state } = V();
    const cam = state.localStream?.getVideoTracks()[0] || null;
    cameraOff = off;
    if (cam) cam.enabled = !off;
    // (85) replaceTrack(null) as well: a disabled track still occupies the
    // encoder and keeps sending black frames to every subscriber. While a
    // share owns the sender the toggle only parks the capture.
    if (!state.screenSharing) {
        const vs = videoSender();
        if (vs) await vs.replaceTrack(off ? null : cam).catch(() => {});
        if (!off) applySendCaps();
    }
    syncCameraButton();
}

// trackSlots declares which slot each outbound track occupies (70). The router
// carries one source per slot, so the share's system audio has to be named or
// it contends with the microphone for the default audio slot and one of the
// two is dropped for the rest of the session. The declaration REPLACES the
// previous one, so it must be complete on EVERY offer — including the ICE
// restart, which would otherwise tear the share's output track off every
// subscriber exactly when the network is already unstable.
export function trackSlots() {
    const { state } = V();
    if (!state.pc) return [];
    const shareAudio = state.shareAudioSender?.track || null;
    const out = [];
    for (const s of state.pc.getSenders()) {
        const t = s.track;
        if (!t) continue;
        // The screen currently rides the camera sender (one video slot per
        // publisher), so only the audio side needs distinguishing.
        out.push({ track_id: t.id, slot: t === shareAudio ? SLOT_SCREEN_AUDIO : t.kind === "audio" ? "mic" : "cam" });
    }
    return out;
}

// renegotiate runs a client-initiated offer/answer round (used after adding a
// share track); the server treats re-offers idempotently. A failed round is
// rolled back to "stable": without that the pc stays in have-local-offer and
// every later renegotiation dies with InvalidStateError.
async function renegotiate(peerConnection = V().state.pc, generation = V().state.serverGeneration) {
    const current = () => V().state.pc === peerConnection && V().state.serverGeneration === generation;
    const ensureCurrent = () => {
        if (!current()) throw new DOMException("server session changed", "AbortError");
    };
    try {
        ensureCurrent();
        const offer = await peerConnection.createOffer();
        ensureCurrent();
        await peerConnection.setLocalDescription(offer);
        ensureCurrent();
        const answerSDP = await window.go.main.App.WebRTCOffer(offer.sdp, trackSlots());
        ensureCurrent();
        await peerConnection.setRemoteDescription({ type: "answer", sdp: answerSDP });
        ensureCurrent();
    } catch (e) {
        await peerConnection?.setLocalDescription({ type: "rollback" }).catch(() => {});
        throw e;
    }
}
