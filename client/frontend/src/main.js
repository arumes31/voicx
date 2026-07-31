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

const $ = (id) => document.getElementById(id);

const state = {
    channels: [],   // flat: {ChannelID, ParentID, Name, HasIcon}
    clients: [],    // flat: {client_id, unique_id, nickname, channel_id, is_speaking}
    myClientID: "",
    myUniqueID: "",
    myNickname: "",
    myChannelID: 0,
    selectedClientID: "",
    pc: null,
    localStream: null,
    screenSharing: false,
    muted: false,
    deafened: false,
    pttActive: false,
    micState: "unknown", // ok | none | denied
    avatars: new Map(),  // unique_id -> data url | null
    avatarPending: new Set(),
    settings: null,
    lastConnect: null,   // {addr, nick, pw, spw} for reconnect-on-loss
    reconnectAttempts: 0,
    reconnectTimer: null,
    vadMonitor: null,
    audioCtx: null,
};

// ---------------------------------------------------------------------------
// Sounds (synthesized, no assets)
// ---------------------------------------------------------------------------

function beep(freq = 660, dur = 0.08) {
    try {
        if (!state.audioCtx) state.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
        const ctx = state.audioCtx;
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.frequency.value = freq;
        gain.gain.setValueAtTime(0.08, ctx.currentTime);
        gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + dur);
        osc.connect(gain).connect(ctx.destination);
        osc.start();
        osc.stop(ctx.currentTime + dur);
    } catch { /* audio unavailable */ }
}

// ---------------------------------------------------------------------------
// Toasts
// ---------------------------------------------------------------------------

function toast(text, kind = "info", category = "social") {
    const s = state.settings;
    if (s) {
        if (category === "social" && !s.notify_join_leave) return;
        if (category === "conn" && !s.notify_connection) return;
    }
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
// Login / connection
// ---------------------------------------------------------------------------

(async () => {
    try {
        state.settings = await window.go.main.App.GetSettings();
    } catch {
        state.settings = null;
    }
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
    $("login-overlay").classList.remove("hidden");
    $("app").classList.add("hidden");
}

$("login-connect").onclick = () => connectFromLogin();

async function connectFromLogin() {
    const addr = $("login-addr").value.trim();
    const nick = $("login-nick").value.trim();
    const pw = $("login-password").value;
    const spw = $("login-serverpw").value;
    $("login-error").textContent = "";
    try {
        const err = await window.go.main.App.Connect(addr, nick, pw, spw);
        if (err) {
            $("login-error").textContent = err;
            return;
        }
        state.myNickname = nick;
        state.lastConnect = { addr, nick, pw, spw };
        state.reconnectAttempts = 0;
        $("conn-pill").textContent = addr;
        $("conn-pill").classList.add("up");
        $("login-overlay").classList.add("hidden");
        $("app").classList.remove("hidden");
        refreshPermissions();
        applyWhisperSettings();
        startupAutoCheck();
    } catch (e) {
        $("login-error").textContent = String(e);
    }
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
    $("conn-pill").textContent = "offline";
    $("conn-pill").classList.remove("up");

    // Reconnect on connection loss (Application setting): 5 tries, 5s apart.
    if (state.settings?.reconnect_on_loss && state.lastConnect && state.reconnectAttempts < 5) {
        state.reconnectAttempts++;
        sysMsg(`reconnecting in 5s (attempt ${state.reconnectAttempts}/5)…`);
        state.reconnectTimer = setTimeout(async () => {
            const c = state.lastConnect;
            if (!c) return;
            const err = await window.go.main.App.Connect(c.addr, c.nick, c.pw, c.spw);
            if (err === "") {
                $("conn-pill").textContent = c.addr;
                $("conn-pill").classList.add("up");
                $("login-overlay").classList.add("hidden");
                $("app").classList.remove("hidden");
                refreshPermissions();
                applyWhisperSettings();
            }
        }, 5000);
        return;
    }
    showLogin();
});

window.runtime.EventsOn("servererror", (msg) => sysMsg("server: " + msg));

// ---------------------------------------------------------------------------
// Snapshot & events
// ---------------------------------------------------------------------------

window.runtime.EventsOn("snapshot", (json) => {
    const snap = JSON.parse(json);
    state.channels = [];
    state.clients = [];
    for (const root of snap.root_channels || []) flattenChannel(root);
    renderTree();
});

function flattenChannel(node) {
    state.channels.push({
        ChannelID: node.ChannelID,
        ParentID: node.ParentID,
        Name: node.Name,
        HasIcon: !!node.HasIcon,
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
        case "user_joined":
            state.clients.push({ client_id: d.client_id, unique_id: d.unique_id, nickname: d.nickname, channel_id: 0, is_speaking: false });
            if (d.client_id !== state.myClientID && d.channel_id === state.myChannelID && state.myChannelID !== 0) {
                toast((d.nickname || "someone") + " joined your channel", "info", "social");
                if (state.settings?.play_sounds) beep(660);
            }
            break;
        case "user_left": {
            const was = state.clients.find((c) => c.client_id === d.client_id);
            state.clients = state.clients.filter((c) => c.client_id !== d.client_id);
            if (was && was.channel_id === state.myChannelID && state.myChannelID !== 0) {
                toast((was.nickname || "someone") + " left your channel", "info", "social");
                if (state.settings?.play_sounds) beep(440);
            }
            break;
        }
        case "user_moved": {
            const c = state.clients.find((c) => c.client_id === d.client_id);
            if (c) c.channel_id = d.channel_id;
            if (d.client_id === state.myClientID) state.myChannelID = d.channel_id;
            else if (d.channel_id === state.myChannelID && state.myChannelID !== 0 && c) {
                toast((c.nickname || "someone") + " joined your channel", "info", "social");
                if (state.settings?.play_sounds) beep(660);
            }
            break;
        }
        case "channel_created":
            state.channels.push({ ChannelID: d.channel_id, ParentID: d.parent_id || 0, Name: d.name, HasIcon: false });
            break;
        case "channel_deleted":
            state.channels = state.channels.filter((c) => c.ChannelID !== d.channel_id);
            break;
        case "speaking_changed": {
            const c = state.clients.find((c) => c.client_id === d.client_id);
            if (c) c.is_speaking = d.speaking;
            updateTalkBanner();
            break;
        }
        case "chat":
            addChat(d);
            return;
        case "kicked":
            toast("client kicked: " + (d.reason || ""), "warn", "social");
            sysMsg("client " + d.client_id + " was kicked" + (d.reason ? " (" + d.reason + ")" : ""));
            break;
        case "screenshare_changed":
            sysMsg(clientName(d.client_id) + (d.active ? " started" : " stopped") + " screen sharing");
            break;
        case "avatar_changed":
            state.avatars.delete(d.unique_id);
            fetchAvatar(d.unique_id);
            break;
    }
    renderTree();
});

// ---------------------------------------------------------------------------
// Channel tree
// ---------------------------------------------------------------------------

function renderTree() {
    const root = $("channel-tree");
    root.innerHTML = "";
    if (state.channels.length === 0) {
        root.innerHTML = `<div class="empty-state">No channels yet</div>`;
        return;
    }
    const byParent = new Map();
    for (const ch of state.channels) {
        const key = ch.ParentID || 0;
        if (!byParent.has(key)) byParent.set(key, []);
        byParent.get(key).push(ch);
    }
    for (const ch of byParent.get(0) || []) renderChannel(root, ch, byParent, 0);
    renderClientCard();
}

function renderChannel(parentEl, ch, byParent, depth) {
    const el = document.createElement("div");
    el.className = "channel" + (ch.ChannelID === state.myChannelID ? " mine" : "");
    el.style.setProperty("--depth", depth);
    el.innerHTML = `<span class="ch-icon">${ch.HasIcon ? "◈" : "#"}</span><span class="ch-name"></span>`;
    el.querySelector(".ch-name").textContent = ch.Name;
    el.onclick = () => window.go.main.App.JoinChannel(ch.ChannelID);
    parentEl.appendChild(el);

    for (const c of state.clients.filter((c) => c.channel_id === ch.ChannelID)) {
        el.appendChild(clientRow(c));
    }

    for (const child of byParent.get(ch.ChannelID) || []) renderChannel(el, child, byParent, depth + 1);
}

function clientRow(c) {
    const row = document.createElement("div");
    row.className = "client" + (c.is_speaking ? " speaking" : "") + (c.client_id === state.selectedClientID ? " selected" : "");
    row.dataset.clid = c.client_id;
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
    row.appendChild(av);
    row.appendChild(name);
    row.onclick = (e) => {
        e.stopPropagation();
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
        <div class="card-channel"></div>`;
    el.querySelector(".card-nick").textContent = c.nickname || c.unique_id;
    el.querySelector(".card-uid").textContent = (c.unique_id || "").slice(0, 16) + "…";
    el.querySelector(".card-channel").textContent = ch ? "in " + ch.Name : "no channel";
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

function addChat(d) {
    const log = $("chat-log");
    const el = document.createElement("div");
    el.className = "msg";
    const scope = d.channel_id ? "channel" : (d.offline ? "dm · offline" : "chat");
    const self = d.from === state.myNickname;
    el.innerHTML = `
        <span class="msg-time mono">${timeNow()}</span>
        <span class="msg-from${self ? " self" : ""}"></span>
        <span class="msg-tag">${scope}</span>
        <span class="msg-text"></span>`;
    el.querySelector(".msg-from").textContent = d.from + ":";
    el.querySelector(".msg-text").textContent = d.text;
    log.appendChild(el);

    // Trim to the configured max lines (Chat setting).
    const max = state.settings?.chat_max_lines || 200;
    while (log.children.length > max) log.firstChild.remove();

    // Chat file logging per scope (Chat setting).
    const s = state.settings;
    if (s) {
        const line = `[${scope}] ${d.from}: ${d.text}`;
        if ((scope === "channel" && s.log_channel_chat) ||
            (scope.startsWith("dm") && s.log_private_chat) ||
            (scope === "chat" && s.log_server_chat)) {
            window.go.main.App.LogChat(line);
        }
    }
    // Whisper/mention sound for direct messages.
    if (!d.channel_id && !self && state.settings?.whisper_sound) beep(880);

    log.scrollTop = log.scrollHeight;
}

function sysMsg(text) {
    const log = $("chat-log");
    const el = document.createElement("div");
    el.className = "msg sys";
    el.textContent = "— " + text + " —";
    log.appendChild(el);
    log.scrollTop = log.scrollHeight;
}

function timeNow() {
    const d = new Date();
    return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
}

$("chat-scope").onchange = () => {
    $("chat-target").classList.toggle("hidden", $("chat-scope").value !== "direct");
};

async function sendChat() {
    const text = $("chat-text").value.trim();
    if (!text) return;
    const scope = $("chat-scope").value;
    let target = "";
    if (scope === "channel") target = String(state.myChannelID || "");
    if (scope === "direct") target = $("chat-target").value.trim();
    const err = await window.go.main.App.SendChat(scope, target, text);
    if (err) sysMsg("chat failed: " + err);
    $("chat-text").value = "";
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

async function startVoice() {
    let audioOK = true;
    try {
        state.localStream = await navigator.mediaDevices.getUserMedia({
            audio: audioConstraints(),
            video: { width: 640, height: 360 },
        });
    } catch (e) {
        if (e.name === "NotFoundError" || e.name === "NotAllowedError") {
            audioOK = false;
            setMicState(e.name === "NotFoundError" ? "none" : "denied");
            state.localStream = await navigator.mediaDevices.getUserMedia({
                video: { width: 640, height: 360 },
            });
        } else {
            throw e;
        }
    }
    if (audioOK) setMicState("ok");
    $("local-video").srcObject = state.localStream;
    $("local-video").classList.remove("hidden");

    const pc = new RTCPeerConnection();
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
    pc.ontrack = (e) => {
        const rv = $("remote-video");
        rv.srcObject = e.streams[0];
        rv.classList.remove("hidden");
        applyOutputSettings(rv);
    };

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    const answerSDP = await window.go.main.App.WebRTCOffer(offer.sdp);
    await pc.setRemoteDescription({ type: "answer", sdp: answerSDP });

    startVADMonitor();
    applyVoiceState();
    $("voice-join").classList.add("active");
    $("voice-status").textContent = "voice on";
}

function teardownVoice() {
    stopVADMonitor();
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

// VAD monitor: client-side voice activity detection driving transmission.
function startVADMonitor() {
    stopVADMonitor();
    if ((state.settings?.activation_mode || "ptt") !== "vad") return;
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
            const threshold = (state.settings?.vad_threshold ?? 50) / 100 * 0.2;
            if (level > threshold) {
                lastVoice = Date.now();
                if (!state.pttActive) setPTT(true);
            } else if (state.pttActive && Date.now() - lastVoice > 300) {
                setPTT(false);
            }
        }, 50);
    } catch { /* VAD unavailable: stay in ptt behavior */ }
}

function stopVADMonitor() {
    if (state.vadMonitor) {
        clearInterval(state.vadMonitor);
        state.vadMonitor = null;
    }
}

// Output settings: volume + sink for remote media elements.
function applyOutputSettings(el) {
    const s = state.settings || {};
    el.volume = Math.min(1, (s.volume ?? 100) / 100);
    if (s.playback_device_id && el.setSinkId) {
        el.setSinkId(s.playback_device_id).catch(() => {});
    }
    el.muted = state.deafened;
}

function setPTT(active) {
    if (state.pttActive === active) return;
    state.pttActive = active;
    $("ptt-btn").classList.toggle("live", active);
    window.go.main.App.SetPTT(active);
    applyVoiceState();
    updateTalkBanner();
}

$("ptt-btn").addEventListener("mousedown", (e) => { e.preventDefault(); setPTT(true); });
["mouseup", "mouseleave"].forEach((ev) => $("ptt-btn").addEventListener(ev, () => setPTT(false)));

$("voice-mute").onclick = () => {
    state.muted = !state.muted;
    $("voice-mute").classList.toggle("active", state.muted);
    window.go.main.App.SetMuted(state.muted);
    applyVoiceState();
};

function setDeafened(on) {
    state.deafened = on;
    $("remote-video").muted = on;
}

window.runtime.EventsOn("hotkey", (action) => {
    if (action === "mute_toggle") {
        $("voice-mute").click();
        return;
    }
    if (document.hasFocus() && document.activeElement === $("chat-text")) return;
    if (state.micState === "none" || state.micState === "denied") return;
    if ((state.settings?.activation_mode || "ptt") !== "ptt") return;
    setPTT(action === "ptt_down");
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

// Screen share ---------------------------------------------------------------

$("voice-screen").onclick = async () => {
    state.screenSharing = !state.screenSharing;
    await window.go.main.App.SetScreenShare(state.screenSharing);
    $("voice-screen").classList.toggle("active", state.screenSharing);

    if (state.screenSharing && state.pc) {
        try {
            const display = await navigator.mediaDevices.getDisplayMedia({ video: true });
            const screenTrack = display.getVideoTracks()[0];
            for (const sender of state.pc.getSenders()) {
                if (sender.track && sender.track.kind === "video") sender.replaceTrack(screenTrack);
            }
            screenTrack.onended = () => $("voice-screen").click();
        } catch (e) {
            sysMsg("screen capture failed: " + e);
        }
    }
};

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
    startVADMonitor, stopVADMonitor, connectFromLogin,
};

initMenu();
initSettingsUI();
initClientInfo();
initUpdater();
