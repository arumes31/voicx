// video.js — video grid (61), focus mode with filmstrip (73), per-subscriber
// quality selector (63), share dialog (69-72), stop-share confirm (85), and
// the low-bandwidth mode (88). Everything here works against the shared
// namespace (window.__voicx) populated by main.js.
const V = () => window.__voicx;

// tiles maps publisher clientID -> {el, video, nameEl, badgeEl, stream}.
const tiles = new Map();
let focusedID = null;

// Connection-wide video quality preference. NOTE: the server's simulcast
// routing keeps ONE layer preference per subscriber (all publishers), so the
// tile context menu sets a shared preference, not a per-tile one — the menu
// says so. "auto" maps: focused tile -> high, grid view -> mid, low-bandwidth
// -> low.
let qualityPref = "auto"; // auto | high | mid | low
let lastSentQuality = "";
let statsTimer = null;

// ---------------------------------------------------------------------------
// Grid (61)
// ---------------------------------------------------------------------------

function gridEl() { return document.getElementById("video-grid"); }

// videoTrackAdded registers (or re-registers) a publisher's video track as a
// grid tile. trackID is the publisher's client ID (per-publisher SFU model).
export function videoTrackAdded(trackID, stream, publisher) {
    const { $ } = V();
    const clid = String(publisher?.client_id || trackID);
    let t = tiles.get(clid);
    if (!t) {
        const el = document.createElement("div");
        el.className = "vtile";
        el.dataset.clid = clid;
        el.innerHTML = `
            <video autoplay playsinline></video>
            <div class="vtile-fallback"><div class="avatar"></div></div>
            <div class="vtile-label">
                <span class="vtile-name"></span>
                <span class="vtile-badge hidden"></span>
            </div>
            <button class="vtile-fullscreen icon-btn" title="fullscreen (Esc exits)">⛶</button>`;
        const video = el.querySelector("video");
        const nameEl = el.querySelector(".vtile-name");
        const badgeEl = el.querySelector(".vtile-badge");
        // (340) fullscreen video tile.
        el.querySelector(".vtile-fullscreen").onclick = (e) => {
            e.stopPropagation();
            if (document.fullscreenElement) document.exitFullscreen();
            else el.requestFullscreen?.().catch(() => {});
        };
        el.onclick = () => toggleFocus(clid);
        el.oncontextmenu = (e) => {
            e.preventDefault();
            openQualityMenu(e.clientX, e.clientY);
        };
        t = { el, video, nameEl, badgeEl, stream: null };
        tiles.set(clid, t);
        gridEl().appendChild(el);
    }
    t.stream = stream;
    t.video.srcObject = stream;
    if (lowBandwidth) t.video.pause();
    applyTileIdentity(t, clid);
    layoutGrid();
    startStatsPoll();
}

// videoTrackRemoved drops a publisher's tile (track ended / publisher left).
export function videoTrackRemoved(trackID) {
    const clid = String(trackID);
    const t = tiles.get(clid);
    if (!t) return;
    t.video.srcObject = null;
    t.el.remove();
    tiles.delete(clid);
    if (focusedID === clid) focusedID = null;
    layoutGrid();
    if (tiles.size === 0) stopStatsPoll();
}

// videoSpeaking toggles the speaking ring on a publisher's tile.
export function videoSpeaking(clientID, speaking) {
    const t = tiles.get(String(clientID));
    if (t) t.el.classList.toggle("speaking", speaking);
}

// videoRefreshNames re-resolves nickname/avatar on all tiles (user joined,
// moved, avatar changed).
export function videoRefreshNames() {
    for (const [clid, t] of tiles) applyTileIdentity(t, clid);
}

// clearVideoGrid removes all tiles (voice teardown).
export function clearVideoGrid() {
    for (const clid of [...tiles.keys()]) videoTrackRemoved(clid);
    focusedID = null;
    layoutGrid();
}

// applyTileIdentity fills the nickname label and the avatar fallback (61:
// avatar-with-ring fallback behind the video; visible when no frames flow,
// e.g. camera off or black screen share).
function applyTileIdentity(t, clid) {
    const { state, clientName, initials, fetchAvatar } = V();
    const c = state.clients.find((c) => String(c.client_id) === clid);
    const name = c ? (c.nickname || c.unique_id) : clid;
    t.nameEl.textContent = name;
    const av = t.el.querySelector(".vtile-fallback .avatar");
    const uid = c?.unique_id || "";
    av.dataset.uid = uid;
    const url = uid && state.avatars.get(uid);
    if (url) {
        av.innerHTML = `<img src="${url}" alt="">`;
    } else {
        av.textContent = initials(name || "?");
        if (uid) fetchAvatar(uid);
    }
    t.el.classList.toggle("speaking", !!(c && c.is_speaking));
}

// layoutGrid recomputes the auto-layout class (1→full, 2→half, 3-4→2x2,
// more→scrollable) and hides the grid when empty so chat keeps the space.
function layoutGrid() {
    const grid = gridEl();
    const n = tiles.size;
    grid.classList.toggle("hidden", n === 0);
    grid.dataset.count = n <= 4 ? String(n) : "many";
    grid.classList.toggle("has-focus", !!focusedID && tiles.has(focusedID));
    for (const [clid, t] of tiles) t.el.classList.toggle("focused", clid === focusedID);
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
    if (lowBandwidth) return "low";
    if (qualityPref !== "auto") return qualityPref;
    return focusedID ? "high" : "mid";
}

// pushQuality sends MsgVideoQuality when the effective layer changed.
async function pushQuality() {
    const q = effectiveQuality();
    if (q === lastSentQuality) return;
    lastSentQuality = q;
    const err = await window.go.main.App.SetVideoQuality(q);
    if (err) V().sysMsg("video quality failed: " + err);
}

let qMenuEl = null;

function openQualityMenu(x, y) {
    if (qMenuEl) qMenuEl.remove();
    qMenuEl = document.createElement("div");
    qMenuEl.className = "ctx-menu";
    const cur = qualityPref;
    qMenuEl.innerHTML = `
        <a data-q="auto">${cur === "auto" ? "✓ " : ""}Auto (follows view)</a>
        <a data-q="high">${cur === "high" ? "✓ " : ""}High</a>
        <a data-q="mid">${cur === "mid" ? "✓ " : ""}Mid</a>
        <a data-q="low">${cur === "low" ? "✓ " : ""}Low</a>
        <div class="ctx-divider"></div>
        <a class="ctx-note">applies to all incoming video</a>`;
    qMenuEl.style.left = Math.min(x, window.innerWidth - 260) + "px";
    qMenuEl.style.top = Math.min(y, window.innerHeight - 200) + "px";
    qMenuEl.onclick = (e) => e.stopPropagation();
    for (const a of qMenuEl.querySelectorAll("a[data-q]")) {
        a.onclick = () => {
            qualityPref = a.dataset.q;
            pushQuality();
            qMenuEl.remove();
            qMenuEl = null;
        };
    }
    document.body.appendChild(qMenuEl);
    const close = () => { if (qMenuEl) { qMenuEl.remove(); qMenuEl = null; } };
    setTimeout(() => {
        document.addEventListener("click", close, { once: true });
        document.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); }, { once: true });
    });
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

async function refreshBadges() {
    const { state } = V();
    if (!state.pc || tiles.size === 0) return;
    try {
        const stats = await state.pc.getStats();
        const byTrack = new Map(); // trackIdentifier -> frameWidth
        stats.forEach((r) => {
            if (r.type === "inbound-rtp" && (r.kind === "video" || r.mediaType === "video")) {
                byTrack.set(r.trackIdentifier, r.frameWidth || 0);
            }
        });
        for (const [clid, t] of tiles) {
            let w = 0;
            const track = t.stream?.getVideoTracks()[0];
            if (track) w = byTrack.get(track.id) || 0;
            if (!w) {
                t.badgeEl.classList.add("hidden");
                continue;
            }
            t.badgeEl.classList.remove("hidden");
            t.badgeEl.textContent = w >= 1280 ? "HD" : w >= 640 ? "MD" : "LD";
        }
    } catch { /* stats unavailable */ }
}

// ---------------------------------------------------------------------------
// Low-bandwidth mode (88): cap own video send to 150 kbps (single layer),
// request the low simulcast layer, pause tile rendering, persist the toggle.
// ---------------------------------------------------------------------------

let lowBandwidth = false;

export function isLowBandwidth() { return lowBandwidth; }

// setLowBandwidth toggles the mode; persist=true saves it into settings.
export async function setLowBandwidth(on, persist) {
    lowBandwidth = on;
    const { $ } = V();
    $("voice-lowbw").classList.toggle("active", on);
    gridEl().classList.toggle("lowbw", on);
    lastSentQuality = ""; // force re-push with the new effective quality
    pushQuality();
    for (const t of tiles.values()) {
        if (on) t.video.pause();
        else t.video.play().catch(() => {});
    }
    applySendCaps();
    if (persist) {
        const s = Object.assign({}, V().state.settings, { low_bandwidth: on });
        const err = await window.go.main.App.SaveSettings(s);
        if (!err) V().state.settings = s;
    }
}

// videoSender returns the sender of the video transceiver, or null. Matches
// by transceiver so it never confuses the share-audio sender.
function videoSender() {
    const { state } = V();
    if (!state.pc) return null;
    const t = state.pc.getTransceivers().find((t) =>
        t.receiver.track.kind === "video" || t.sender.track?.kind === "video");
    return t ? t.sender : null;
}

// applySendCaps caps (or restores) the outgoing video bitrate: 150 kbps on a
// single active layer in low-bandwidth mode, uncapped simulcast otherwise.
function applySendCaps() {
    const sender = videoSender();
    if (!sender) return;
    const p = sender.getParameters();
    if (!p.encodings?.length) return;
    p.encodings.forEach((e, i) => {
        if (lowBandwidth) {
            e.active = i === 0;
            e.maxBitrate = 150000;
        } else {
            e.active = true;
            e.maxBitrate = undefined;
        }
    });
    sender.setParameters(p).catch(() => {});
}

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

const regionSupported = typeof CropTarget !== "undefined" &&
    typeof MediaStreamTrack !== "undefined" && "cropTo" in MediaStreamTrack.prototype;

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
        const surface = overlay.querySelector('input[name="shsrc"]:checked').value;
        const preset = overlay.querySelector(".sh-preset").value;
        const withAudio = overlay.querySelector(".sh-audio").checked;
        overlay.remove();
        await startShare({ surface, preset, withAudio });
    };
    document.body.appendChild(overlay);
    if (!regionSupported) {
        // (71) honest limitation note.
        const note = document.createElement("div");
        note.className = "set-hint";
        note.textContent = "Region capture requires a newer WebView2 (CropTarget API missing).";
        overlay.querySelector(".share-sources").appendChild(note);
    }
}

// startShare captures the display (69: displaySurface preference — Chromium's
// picker ultimately decides what is shareable), replaces the camera slot with
// the screen track (one video track per peer), optionally merges display
// audio (70), and applies the quality preset (72).
async function startShare({ surface, preset, withAudio }) {
    const { state } = V();
    const p = SHARE_PRESETS[preset] || SHARE_PRESETS.balanced;
    const video = {
        width: { ideal: p.width },
        height: { ideal: p.height },
        frameRate: { ideal: p.fps },
    };
    if (surface !== "region") video.displaySurface = surface; // 69: "monitor" | "window"
    let display;
    try {
        display = await navigator.mediaDevices.getDisplayMedia({ video, audio: !!withAudio });
    } catch (e) {
        if (withAudio) {
            // (70) WebView2 may refuse display audio (works on Windows for
            // screen/tab shares) — retry video-only and say so.
            V().sysMsg("system audio not available for this share (" + (e.message || e.name) + "); sharing video only");
            try {
                display = await navigator.mediaDevices.getDisplayMedia({ video, audio: false });
                withAudio = false;
            } catch (e2) {
                V().sysMsg("screen capture failed: " + (e2.message || e2.name));
                return;
            }
        } else {
            V().sysMsg("screen capture failed: " + (e.message || e.name));
            return;
        }
    }
    const screenTrack = display.getVideoTracks()[0];
    if (!screenTrack) {
        V().sysMsg("screen capture produced no video track");
        return;
    }
    screenTrack.contentHint = preset === "text" ? "detail" : "motion";

    if (surface === "region") {
        const ok = await pickRegionAndCrop(screenTrack);
        if (!ok) {
            display.getTracks().forEach((t) => t.stop());
            return; // cancelled
        }
    }

    state.shareStream = display;
    const vs = videoSender();
    if (vs) {
        await vs.replaceTrack(screenTrack).catch(() => {});
        // (72) bitrate cap per preset.
        const params = vs.getParameters();
        if (params.encodings?.length) {
            params.encodings.forEach((e) => { e.maxBitrate = p.bitrate; });
            vs.setParameters(params).catch(() => {});
        }
    }
    screenTrack.onended = () => doStopShare(); // browser "stop sharing" UI

    // (70) merge display audio as a second published audio track (the server
    // fans out every publisher audio track) + renegotiate.
    const displayAudio = withAudio ? display.getAudioTracks()[0] : null;
    if (displayAudio && state.pc) {
        try {
            state.shareAudioSender = state.pc.addTrack(displayAudio, display);
            await renegotiate();
        } catch (e) {
            V().sysMsg("publishing share audio failed: " + (e.message || e.name));
            displayAudio.stop();
            state.shareAudioSender = null;
        }
    }

    state.screenSharing = true;
    await window.go.main.App.SetScreenShare(true);
    document.getElementById("voice-screen").classList.add("active");
}

// pickRegionAndCrop shows a draggable/resizable box over the app; on confirm
// the track is cropped to it via the Region Capture API (71).
function pickRegionAndCrop(track) {
    return new Promise((resolve) => {
        const box = document.createElement("div");
        box.className = "region-box";
        box.innerHTML = `
            <div class="region-title">drag to move · drag corner to resize</div>
            <button class="region-ok">Crop &amp; share</button>
            <button class="region-cancel">Cancel</button>`;
        document.body.appendChild(box);

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

        box.querySelector(".region-cancel").onclick = () => {
            box.remove();
            resolve(false);
        };
        box.querySelector(".region-ok").onclick = async () => {
            try {
                const target = await CropTarget.fromElement(box);
                await track.cropTo(target);
                box.remove();
                resolve(true);
            } catch (e) {
                V().sysMsg("region capture failed: " + (e.message || e.name));
                box.remove();
                resolve(false);
            }
        };
    });
}

// confirmStopShare (85): warn when others may be watching.
async function confirmStopShare() {
    const { state } = V();
    const watchers = state.clients.filter((c) =>
        c.channel_id === state.myChannelID && c.channel_id !== 0 && c.client_id !== state.myClientID).length;
    if (watchers === 0) {
        doStopShare();
        return;
    }
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg share-dlg">
            <h3>Stop sharing?</h3>
            <p class="dlg-text">${watchers} user${watchers === 1 ? "" : "s"} may be watching your screen.</p>
            <div class="dlg-buttons">
                <button class="dlg-ok">Stop sharing</button>
                <button class="dlg-cancel">Keep sharing</button>
            </div>
        </div>`;
    overlay.querySelector(".dlg-ok").onclick = () => {
        overlay.remove();
        doStopShare();
    };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
}

// doStopShare restores the camera slot and stops the display tracks.
async function doStopShare() {
    const { state } = V();
    if (!state.screenSharing) return;
    state.screenSharing = false;
    document.getElementById("voice-screen").classList.remove("active");
    await window.go.main.App.SetScreenShare(false);

    if (state.shareAudioSender && state.pc) {
        // The transceiver stays (stopping the track silences it); removing it
        // would need another renegotiation.
        state.shareAudioSender.track?.stop();
        state.shareAudioSender = null;
    }
    if (state.shareStream) {
        state.shareStream.getTracks().forEach((t) => t.stop());
        state.shareStream = null;
    }
    const camera = state.localStream?.getVideoTracks()[0] || null;
    const vs = videoSender();
    if (vs) {
        await vs.replaceTrack(camera).catch(() => {});
        applySendCaps(); // restore low-bandwidth/uncapped state
    }
}

// renegotiate runs a client-initiated offer/answer round (used after adding
// the share-audio track); the server treats re-offers idempotently.
async function renegotiate() {
    const { state } = V();
    const offer = await state.pc.createOffer();
    await state.pc.setLocalDescription(offer);
    const answerSDP = await window.go.main.App.WebRTCOffer(offer.sdp);
    await state.pc.setRemoteDescription({ type: "answer", sdp: answerSDP });
}
