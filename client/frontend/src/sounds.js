// sounds.js — sound pack system: synthesized per-event sounds via WebAudio
// plus dedicated media cues, with pack selection, per-event enable, a master
// volume slider and the master play_sounds gate.
import channelJoinURL from "./assets/channel_join.mp3?url";

const V = () => window.__voicx;

// Sound events are grouped for the settings UI as well as exported flat for
// the player and migration tests.  The notification group deliberately keeps
// the matrix event names: notify() controls when those sounds may fire.
export const SOUND_EVENT_GROUPS = [
    { label: "Connection", events: [
        ["connection_connected", "Connected"],
        ["connection_reconnected", "Reconnected"],
        ["connection_disconnected", "Disconnected"],
        ["connection_lost", "Connection lost"],
        ["connection_reconnecting", "Reconnecting"],
        ["connection_failed", "Connection failed"],
        ["server_error", "Server action error"],
    ] },
    { label: "Your channel", events: [
        ["own_channel_join", "Joined channel"],
        ["own_channel_switch", "Switched channel"],
        ["own_channel_leave", "Left channel"],
    ] },
    { label: "Other users", events: [
        ["user_join", "User joined"],
        ["user_leave", "User left"],
        ["user_move_in", "User moved in"],
        ["user_move_out", "User moved out"],
    ] },
    { label: "Voice controls", events: [
        ["mic_on", "Microphone on"],
        ["mic_off", "Microphone off"],
        ["deafen_on", "Deafened"],
        ["deafen_off", "Undeafened"],
        ["ptt_on", "Push-to-talk on"],
        ["ptt_off", "Push-to-talk off"],
    ] },
    { label: "Notifications", events: [
        ["mention", "Mention"],
        ["keyword", "Keyword highlight"],
        ["dm", "Direct message"],
        ["channel_message", "Channel message"],
        ["whisper", "Voice whisper"],
        ["poke", "Poke"],
        ["join_leave", "Join/leave (your channel)"],
        ["buddy_online", "Watched contact online"],
        ["kick", "Kick/ban"],
        ["announcement", "Announcement"],
        ["channel_watch", "Channel watch"],
    ] },
];

export const SOUND_EVENTS = SOUND_EVENT_GROUPS.flatMap((group) => group.events.map(([id]) => id));

// Every cue is a recognisable contour rather than a single beep:
// [frequency Hz, duration seconds, start offset seconds]. Packs vary the
// instrument and register while preserving each event's melodic identity.
const CUES = {
    connection_connected: [[523, .07, 0], [659, .08, .08], [784, .14, .18]],
    connection_reconnected: [[392, .07, 0], [523, .08, .08], [659, .14, .18]],
    connection_disconnected: [[659, .07, 0], [523, .08, .08], [392, .14, .18]],
    connection_lost: [[440, .11, 0], [330, .13, .12], [220, .22, .27]],
    connection_reconnecting: [[330, .06, 0], [392, .07, .07], [494, .10, .15]],
    connection_failed: [[247, .10, 0], [196, .13, .12], [165, .19, .26]],
    server_error: [[220, .11, 0], [196, .14, .13], [220, .11, .30]],
    own_channel_switch: [[659, .07, 0], [784, .10, .08], [988, .13, .19]],
    own_channel_leave: [[587, .08, 0], [440, .10, .09], [349, .14, .20]],
    user_join: [[659, .06, 0], [784, .10, .07]],
    user_leave: [[523, .07, 0], [392, .12, .08]],
    user_move_in: [[494, .06, 0], [659, .06, .07], [784, .11, .14]],
    user_move_out: [[784, .06, 0], [659, .06, .07], [494, .11, .14]],
    mic_on: [[659, .06, 0], [784, .08, .07]],
    mic_off: [[523, .06, 0], [392, .08, .07]],
    deafen_on: [[392, .08, 0], [294, .10, .09], [196, .13, .20]],
    deafen_off: [[262, .07, 0], [392, .08, .08], [523, .12, .18]],
    ptt_on: [[740, .05, 0], [880, .07, .06]],
    ptt_off: [[660, .05, 0], [494, .07, .06]],
    mention: [[784, .07, 0], [1047, .11, .08]],
    keyword: [[659, .06, 0], [784, .06, .07], [988, .10, .14]],
    dm: [[523, .07, 0], [784, .11, .08]],
    channel_message: [[587, .06, 0], [659, .08, .07]],
    whisper: [[880, .05, 0], [1175, .07, .06], [880, .11, .14]],
    poke: [[988, .05, 0], [1175, .05, .06], [988, .08, .12]],
    join_leave: [[554, .06, 0], [659, .09, .07]],
    buddy_online: [[523, .06, 0], [659, .06, .07], [784, .11, .14]],
    kick: [[330, .09, 0], [247, .12, .10], [196, .16, .23]],
    announcement: [[784, .07, 0], [988, .07, .08], [1175, .13, .17]],
    channel_watch: [[494, .06, 0], [659, .06, .07], [880, .10, .14]],
};

const PACKS = {
    soft: { type: "sine", pitch: 1 },
    bright: { type: "triangle", pitch: 1.12 },
    retro: { type: "square", pitch: .84 },
};

function audioCtx() {
    const st = V().state;
    if (!st.audioCtx) st.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    return st.audioCtx;
}

// soundsEnabled is the master play_sounds gate (28). It was written by the
// settings UI and read nowhere; explicit previews and the test buttons pass
// force so a silenced master does not make them look broken.
function soundsEnabled() {
    return V().state.settings?.play_sounds !== false;
}

function eventEnabled(name, force = false) {
    const s = V().state.settings;
    if (!s) return false;
    // Journal frames rebuild an inactive tab's UI after tab_reset. They are
    // history, not a new user action; force remains available for previews.
    if (!force && V().state.replayingTabID) return false;
    if (!force && !soundsEnabled()) return false;
    if (s.event_sounds && s.event_sounds[name] === false) return false;
    // (347/348) DND silences sounds (mentions still badge silently).
    if (window.__voicxPolish?.dndActive?.()) return false;
    return true;
}

// playEvent plays the sound for an event if enabled in settings.
export function playEvent(name, force = false) {
    const s = V().state.settings;
    if (!eventEnabled(name, force)) return;
    const pack = PACKS[s.sound_pack] || PACKS.soft;
    const cue = CUES[name];
    if (!cue) return;
    const volume = (s.sound_volume ?? 100) / 100;
    for (const [freq, dur, offset] of cue) {
        playTone(pack.type, freq * pack.pitch, dur, volume, offset);
    }
}

// playChannelJoin keeps the bundled cue for this client's own first join.
// Channel switches use their separate synthesized motif, so the two actions
// remain recognisable without replacing the shipped media asset.
const activeMedia = new Set();

export function playChannelJoin(force = false) {
    if (!eventEnabled("own_channel_join", force)) return;
    try {
        const audio = new Audio(channelJoinURL);
        audio.volume = Math.min(1, Math.max(0, (V().state.settings?.sound_volume ?? 100) / 100));
        activeMedia.add(audio);
        const release = () => activeMedia.delete(audio);
        audio.addEventListener("ended", release, { once: true });
        audio.addEventListener("error", release, { once: true });
        audio.play().catch(release);
    } catch { /* media playback unavailable */ }
}

// play sounds an oscillator note.
export function play(type, freq, dur, vol = 1, force = false) {
    if (!force && !soundsEnabled()) return;
    playTone(type, freq, dur, vol);
}

function playTone(type, freq, dur, vol = 1, offset = 0) {
    try {
        const ctx = audioCtx();
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.type = type;
        osc.frequency.value = freq;
        const peak = 0.08 * Math.min(2, Math.max(0, vol));
        const start = ctx.currentTime + offset;
        gain.gain.setValueAtTime(peak, start);
        gain.gain.exponentialRampToValueAtTime(0.001, start + dur);
        osc.connect(gain).connect(ctx.destination);
        osc.start(start);
        osc.stop(start + dur);
    } catch { /* audio unavailable */ }
}

// testAll plays every enabled event sound in sequence with labels. It is an
// explicit action, so it plays through the master gate.
export function testAll() {
    let delay = 0;
    for (const name of SOUND_EVENTS) {
        const s = V().state.settings;
        if (s.event_sounds && s.event_sounds[name] === false) continue;
        setTimeout(() => {
            V().toast("sound: " + name);
            if (name === "own_channel_join") playChannelJoin(true);
            else playEvent(name, true);
        }, delay);
        delay += 450;
    }
}

// beep is the legacy simple beep used by older call sites (the settings test
// button): an explicit action, so it bypasses the master gate.
export function beep(freq = 660, dur = 0.08) {
    play("sine", freq, dur, 1, true);
}
