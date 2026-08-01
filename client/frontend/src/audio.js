// audio.js — voice UX helpers: mic level meter, loopback mic test, VAD
// calibration, PTT release delay, the channel capture profile, remote-chain
// limiter/per-user normalizer, and the per-user volume/mute registry.
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
// Channel capture profile (25) — a music channel is published as high-bitrate
// stereo Opus, so capturing it mono through the browser's speech DSP
// (echo cancellation / noise suppression / AGC) destroys exactly what the
// mode exists for. Ordinary channels keep the user's own capture settings.
// ---------------------------------------------------------------------------

// MUSIC_MIN_BITRATE mirrors the server's musicMinBitrate: stereo at or above
// it is a music channel (the same rule that bypasses the talk gate).
export const MUSIC_MIN_BITRATE = 96000;

// isMusicChannel reads the audio profile off a channel snapshot entry. An
// explicit profile published by the server wins; otherwise the server's own
// stereo+bitrate rule is re-derived. Both spellings are accepted so a json
// tag on the server's channel struct cannot silently drop the profile.
export function isMusicChannel(ch) {
    if (!ch) return false;
    const profile = ch.AudioProfile ?? ch.audio_profile;
    if (typeof profile === "string" && profile !== "") {
        return profile === "music" || profile === "broadcast";
    }
    const flag = ch.IsMusic ?? ch.is_music;
    if (typeof flag === "boolean") return flag;
    const stereo = ch.OpusStereo ?? ch.opus_stereo;
    const bitrate = ch.OpusBitrate ?? ch.opus_bitrate ?? 0;
    return !!stereo && bitrate >= MUSIC_MIN_BITRATE;
}

// captureConstraints builds the getUserMedia audio constraints for the
// channel: music channels force stereo with the browser's processing off,
// every other channel uses the user's settings unchanged.
export function captureConstraints(ch) {
    const s = V().state.settings || {};
    const device = s.capture_device_id ? { deviceId: { exact: s.capture_device_id } } : {};
    if (isMusicChannel(ch)) {
        return {
            ...device,
            channelCount: 2,
            echoCancellation: false,
            noiseSuppression: false,
            autoGainControl: false,
        };
    }
    return {
        ...device,
        echoCancellation: s.echo_cancellation !== false,
        noiseSuppression: s.noise_suppression !== false,
    };
}

// trackProfiles remembers which profile a live capture track was taken with;
// a track we never saw captured counts as "voice" (the settings profile).
const trackProfiles = new WeakMap();
let recapturing = false; // one getUserMedia swap at a time

function profileOf(ch) {
    return isMusicChannel(ch) ? "music" : "voice";
}

// markCaptureProfile records the profile a caller captured a track with, so
// applyCaptureProfile does not re-capture a track that already matches.
export function markCaptureProfile(track, ch) {
    if (track) trackProfiles.set(track, profileOf(ch));
}

// applyCaptureProfile re-captures the microphone and swaps the sender's track
// when the joined channel's profile differs from the live track's — entering a
// music channel switches to stereo/no-DSP, leaving one restores the user's
// settings. Returns {track, changed}; on any failure the working track is
// kept. The caller must restart anything holding the old track (mic meter,
// VAD monitor) and re-apply mute/PTT state when changed is true.
export async function applyCaptureProfile(pc, stream, ch) {
    const cur = stream?.getAudioTracks()[0] || null;
    if (!cur) return { track: null, changed: false };
    const want = profileOf(ch);
    if ((trackProfiles.get(cur) || "voice") === want) return { track: cur, changed: false };
    // a move and a channel_updated can land together: a second capture while
    // the first is still in flight would swap the sender's track twice.
    if (recapturing) return { track: cur, changed: false };
    recapturing = true;
    let fresh = null;
    try {
        fresh = await navigator.mediaDevices.getUserMedia({ audio: captureConstraints(ch) });
    } catch {
        recapturing = false;
        return { track: cur, changed: false };
    }
    recapturing = false;
    const next = fresh.getAudioTracks()[0];
    if (!next) {
        fresh.getTracks().forEach((t) => t.stop());
        return { track: cur, changed: false };
    }
    trackProfiles.set(next, want);
    // mute/PTT gate the track with .enabled: a fresh track starts enabled and
    // would un-mute the user behind their back.
    next.enabled = cur.enabled;
    next.contentHint = want === "music" ? "music" : "speech";
    const sender = pc?.getSenders().find((s) => s.track && s.track.kind === "audio") || null;
    if (sender) {
        try {
            await sender.replaceTrack(next);
        } catch {
            next.stop();
            return { track: cur, changed: false };
        }
    }
    cur.stop();
    stream.removeTrack(cur);
    stream.addTrack(next);
    return { track: next, changed: true };
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
    // gain and mute are in series, so the duck factor belongs on exactly one
    // of them — applying it to both squares it (14: -24 dB, not -12 dB).
    n.gain.gain.value = (isUserMuted(uid) ? 0 : getUserVolume(uid)) * duckMultiplier(uid);
    n.mute.gain.value = isUserMuted(uid) ? 0 : 1;
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

// userNorms maps a chain key (the remote track ID) -> {analyser, gain, tick}.
// One 100 ms ticker drives every publisher's normalizer.
const userNorms = new Map();
let normTimer = null;

// attachUserNormalizer inserts one publisher's normalizer directly behind its
// source and returns the node to continue the chain from. It sits before the
// per-user volume/mute nodes: normalizing after them would fight the volume
// slider back up to the target. Returns src unchanged when the optional
// gain_normalize setting is off.
export function attachUserNormalizer(ctx, key, src) {
    detachUserNormalizer(key);
    if (!V().state.settings?.gain_normalize) return src;
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 512;
    src.connect(analyser); // tap only: the analyser feeds nothing onward
    const { gain, tick } = makeNormalizer(ctx, analyser);
    src.connect(gain);
    userNorms.set(key, { analyser, gain, tick });
    if (!normTimer) normTimer = setInterval(tickUserNormalizers, 100);
    return gain;
}

function tickUserNormalizers() {
    for (const n of userNorms.values()) n.tick();
}

export function detachUserNormalizer(key) {
    const n = userNorms.get(key);
    if (!n) return;
    userNorms.delete(key);
    try {
        n.gain.disconnect();
        n.analyser.disconnect();
    } catch { /* already disconnected */ }
    if (userNorms.size === 0 && normTimer) {
        clearInterval(normTimer);
        normTimer = null;
    }
}

// detachAllUserNormalizers stops the ticker when voice ends.
export function detachAllUserNormalizers() {
    for (const key of [...userNorms.keys()]) detachUserNormalizer(key);
}
