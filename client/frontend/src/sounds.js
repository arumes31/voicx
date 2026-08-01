// sounds.js — sound pack system: synthesized per-event sounds via WebAudio
// oscillators (no asset files), with pack selection, per-event enable, and
// a master volume slider.
const V = () => window.__voicx;

// Sound events: join, leave, mention, dm, whisper, poke (reserved), mic_on, mic_off.
export const SOUND_EVENTS = ["join", "leave", "mention", "dm", "whisper", "poke", "mic_on", "mic_off"];

// Presets: each event = [type, freqHz, durationSec]. Packs tune the timbre.
const PACKS = {
    soft: {
        join: ["sine", 520, 0.12], leave: ["sine", 380, 0.12], mention: ["sine", 740, 0.10],
        dm: ["sine", 660, 0.10], whisper: ["sine", 880, 0.14], poke: ["sine", 990, 0.08],
        mic_on: ["sine", 620, 0.07], mic_off: ["sine", 420, 0.07],
    },
    bright: {
        join: ["triangle", 660, 0.10], leave: ["triangle", 440, 0.10], mention: ["triangle", 880, 0.09],
        dm: ["triangle", 760, 0.09], whisper: ["triangle", 1040, 0.12], poke: ["triangle", 1200, 0.07],
        mic_on: ["triangle", 760, 0.06], mic_off: ["triangle", 500, 0.06],
    },
    retro: {
        join: ["square", 440, 0.08], leave: ["square", 330, 0.08], mention: ["square", 660, 0.07],
        dm: ["square", 550, 0.07], whisper: ["square", 770, 0.10], poke: ["square", 880, 0.06],
        mic_on: ["square", 550, 0.05], mic_off: ["square", 380, 0.05],
    },
};

function audioCtx() {
    const st = V().state;
    if (!st.audioCtx) st.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    return st.audioCtx;
}

// playEvent plays the sound for an event if enabled in settings.
export function playEvent(name) {
    const s = V().state.settings;
    if (!s) return;
    if (s.event_sounds && s.event_sounds[name] === false) return;
    // (347/348) DND silences sounds (mentions still badge silently).
    if (window.__voicxPolish?.dndActive?.()) return;
    const pack = PACKS[s.sound_pack] || PACKS.soft;
    const preset = pack[name];
    if (!preset) return;
    play(preset[0], preset[1], preset[2], (s.sound_volume ?? 100) / 100);
}

// play sounds an oscillator note.
export function play(type, freq, dur, vol = 1) {
    try {
        const ctx = audioCtx();
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.type = type;
        osc.frequency.value = freq;
        const peak = 0.08 * Math.min(2, Math.max(0, vol));
        gain.gain.setValueAtTime(peak, ctx.currentTime);
        gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + dur);
        osc.connect(gain).connect(ctx.destination);
        osc.start();
        osc.stop(ctx.currentTime + dur);
    } catch { /* audio unavailable */ }
}

// testAll plays every enabled event sound in sequence with labels.
export function testAll() {
    let delay = 0;
    for (const name of SOUND_EVENTS) {
        const s = V().state.settings;
        if (s.event_sounds && s.event_sounds[name] === false) continue;
        setTimeout(() => {
            V().toast("sound: " + name);
            playEvent(name);
        }, delay);
        delay += 450;
    }
}

// beep is the legacy simple beep used by older call sites.
export function beep(freq = 660, dur = 0.08) {
    play("sine", freq, dur, 1);
}
