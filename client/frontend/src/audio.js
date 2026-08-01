// audio.js — voice UX helpers: mic level meter, loopback mic test, VAD
// calibration, PTT release delay, remote-chain limiter/normalizer, and the
// per-user volume/mute registry.
const V = () => window.__voicx;

// ---------------------------------------------------------------------------
// Mic level meter (3) — animated level bar driven by an AnalyserNode on the
// local stream. The element #mic-meter exists in the voice bar.
// ---------------------------------------------------------------------------

let meterRaf = null;
let meterAnalyser = null;

export function startMicMeter(stream) {
    stopMicMeter();
    const el = document.getElementById("mic-meter");
    if (!el || !stream || stream.getAudioTracks().length === 0) {
        if (el) el.classList.add("hidden");
        return;
    }
    el.classList.remove("hidden");
    const fill = el.querySelector(".mic-fill");
    try {
        const ctx = new (window.AudioContext || window.webkitAudioContext)();
        const src = ctx.createMediaStreamSource(stream);
        const an = ctx.createAnalyser();
        an.fftSize = 512;
        src.connect(an);
        meterAnalyser = { ctx, an };
        const buf = new Uint8Array(an.frequencyBinCount);
        const tick = () => {
            an.getByteTimeDomainData(buf);
            let sum = 0;
            for (const v of buf) sum += Math.abs(v - 128);
            const level = Math.min(1, (sum / buf.length / 128) * 5);
            fill.style.width = level * 100 + "%";
            fill.className = "mic-fill " + (level > 0.7 ? "hot" : level > 0.35 ? "warm" : "cool");
            meterRaf = requestAnimationFrame(tick);
        };
        tick();
    } catch {
        el.classList.add("hidden");
    }
}

export function stopMicMeter() {
    if (meterRaf) {
        cancelAnimationFrame(meterRaf);
        meterRaf = null;
    }
    if (meterAnalyser) {
        meterAnalyser.ctx.close().catch(() => {});
        meterAnalyser = null;
    }
    const el = document.getElementById("mic-meter");
    if (el) el.classList.add("hidden");
}

// ---------------------------------------------------------------------------
// Loopback mic test (4) — mic -> DelayNode -> speakers, so you hear yourself.
// ---------------------------------------------------------------------------

export async function startLoopback(stream) {
    const ctx = new (window.AudioContext || window.webkitAudioContext)();
    const src = ctx.createMediaStreamSource(stream);
    const delay = ctx.createDelay(0.5);
    delay.delayTime.value = 0.15;
    src.connect(delay).connect(ctx.destination);
    return ctx; // caller closes to stop
}

// ---------------------------------------------------------------------------
// VAD calibration wizard (5) — record ambient audio, compute noise floor,
// return the suggested VAD threshold (floor + margin).
// ---------------------------------------------------------------------------

export async function calibrateMic(deviceId, durationMs = 5000) {
    const stream = await navigator.mediaDevices.getUserMedia({
        audio: deviceId ? { deviceId: { exact: deviceId } } : true,
    });
    try {
        const ctx = new (window.AudioContext || window.webkitAudioContext)();
        const src = ctx.createMediaStreamSource(stream);
        const an = ctx.createAnalyser();
        an.fftSize = 512;
        src.connect(an);
        const buf = new Uint8Array(an.frequencyBinCount);
        let sum = 0, n = 0;
        const start = performance.now();
        while (performance.now() - start < durationMs) {
            an.getByteTimeDomainData(buf);
            let s = 0;
            for (const v of buf) s += Math.abs(v - 128);
            sum += s / buf.length / 128;
            n++;
            await new Promise((r) => setTimeout(r, 50));
        }
        ctx.close().catch(() => {});
        const floor = sum / Math.max(1, n); // average level 0..1
        // Suggested threshold: floor * 1.5 with a small absolute margin,
        // expressed in the slider's 1..100 scale (matching audio.js VAD).
        const suggested = Math.min(100, Math.max(1, Math.round((floor * 1.5 + 0.01) * 100)));
        return { floor, suggested };
    } finally {
        stream.getTracks().forEach((t) => t.stop());
    }
}

// ---------------------------------------------------------------------------
// PTT release delay (6) — defer the PTT-off transition so sentence ends are
// not clipped.
// ---------------------------------------------------------------------------

let releaseTimer = null;

// pttRelease returns the effective PTT state: on instantly, off after the
// configured release delay (re-arm cancels a pending release).
export function pttRelease(wantActive, apply) {
    const delay = V().state.settings?.ptt_release_delay_ms || 0;
    if (wantActive) {
        if (releaseTimer) {
            clearTimeout(releaseTimer);
            releaseTimer = null;
        }
        apply(true);
        return;
    }
    if (delay <= 0) {
        apply(false);
        return;
    }
    if (releaseTimer) clearTimeout(releaseTimer);
    releaseTimer = setTimeout(() => {
        releaseTimer = null;
        apply(false);
    }, delay);
}

// ---------------------------------------------------------------------------
// Per-user volume (1) & local mute (2) — registry keyed by unique ID,
// persisted in settings. Applied to per-user gain nodes in remote chains.
// The server emits one audio track per publisher (track ID = publisher client
// ID), and main.js registers a gain+mute node pair per track via
// registerUserChain, so per-sender volume/mute is audible.
// ---------------------------------------------------------------------------

export function getUserVolume(uid) {
    const s = V().state.settings;
    return (s?.user_volumes?.[uid] ?? 100) / 100;
}

export function isUserMuted(uid) {
    const s = V().state.settings;
    return (s?.muted_users || []).includes(uid);
}

export async function setUserVolume(uid, pct) {
    const s = Object.assign({}, V().state.settings);
    s.user_volumes = Object.assign({}, s.user_volumes, { [uid]: pct });
    await saveAll(s);
    applyUserAudio(uid);
}

export async function setUserMuted(uid, muted) {
    const s = Object.assign({}, V().state.settings);
    const set = new Set(s.muted_users || []);
    if (muted) set.add(uid);
    else set.delete(uid);
    s.muted_users = [...set];
    await saveAll(s);
    applyUserAudio(uid);
}

async function saveAll(s) {
    const err = await window.go.main.App.SaveSettings(s);
    // (282) re-read rather than caching the copy we sent: the Go side owns
    // fields the frontend never has (recents, what's-new marker).
    if (!err) V().state.settings = await window.go.main.App.GetSettings();
}

// userNodes maps uniqueID -> {gain: GainNode, mute: GainNode}.
const userNodes = new Map();

// registerUserChain creates the per-user gain nodes for a remote audio
// chain (used when a track can be attributed to a user).
export function registerUserChain(uid, gainNode, muteNode) {
    userNodes.set(uid, { gain: gainNode, mute: muteNode });
    applyUserAudio(uid);
}

export function unregisterUserChain(uid) {
    userNodes.delete(uid);
}

// ---------------------------------------------------------------------------
// Priority-speaker ducking (13/14) — while a priority speaker in my channel
// talks, all non-priority publishers are ducked to 25% (-12 dB). Priority
// speakers themselves are exempt. main.js decides when ducking applies.
// ---------------------------------------------------------------------------

const DUCK_FACTOR = 0.25;

let duckActive = false;
let duckExempt = new Set();

// setDucking toggles ducking; exceptUIDs lists the unique IDs exempt from it.
export function setDucking(active, exceptUIDs) {
    duckActive = active;
    duckExempt = new Set(exceptUIDs || []);
    for (const uid of userNodes.keys()) applyUserAudio(uid);
}

function duckMultiplier(uid) {
    return duckActive && !duckExempt.has(uid) ? DUCK_FACTOR : 1;
}

export function applyUserAudio(uid) {
    const n = userNodes.get(uid);
    if (!n) return;
    const duckMult = duckMultiplier(uid);
    n.gain.gain.value = (isUserMuted(uid) ? 0 : getUserVolume(uid)) * duckMult;
    n.mute.gain.value = (isUserMuted(uid) ? 0 : 1) * duckMult;
}

// ---------------------------------------------------------------------------
// Voice limiter (52) & gain normalization (53) — remote chain processors.
// ---------------------------------------------------------------------------

export function makeLimiter(ctx) {
    const comp = ctx.createDynamicsCompressor();
    comp.threshold.value = -12;
    comp.knee.value = 20;
    comp.ratio.value = 8;
    comp.attack.value = 0.003;
    comp.release.value = 0.2;
    return comp;
}

// makeNormalizer returns a GainNode plus a tick() that adapts gain toward a
// target RMS level (cap 4x, smooth attack/release).
export function makeNormalizer(ctx, analyser) {
    const gain = ctx.createGain();
    const buf = new Uint8Array(analyser.frequencyBinCount);
    const target = 0.1; // target RMS level
    const tick = () => {
        analyser.getByteTimeDomainData(buf);
        let sum = 0;
        for (const v of buf) sum += (v - 128) * (v - 128);
        const rms = Math.sqrt(sum / buf.length) / 128;
        if (rms > 0.001) {
            const want = Math.min(4, target / rms);
            gain.gain.value += (want - gain.gain.value) * 0.05;
        }
    };
    return { gain, tick };
}
