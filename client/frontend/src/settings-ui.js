// settings-ui.js — TS3-style settings dialog with left icon nav.
import { t } from "./i18n.js";
import { SOUND_EVENTS } from "./sounds.js";

const V = () => window.__voicx;

const PAGES = [
    { id: "application", icon: "⚙", label: "settings.application" },
    { id: "capture", icon: "🎙", label: "settings.capture" },
    { id: "playback", icon: "🔊", label: "settings.playback" },
    { id: "hotkeys", icon: "⌨", label: "settings.hotkeys" },
    { id: "whisper", icon: "🤫", label: "settings.whisper" },
    { id: "downloads", icon: "⬇", label: "settings.downloads" },
    { id: "chat", icon: "💬", label: "settings.chat" },
    { id: "security", icon: "🔑", label: "settings.security" },
    { id: "notifications", icon: "🔔", label: "settings.notifications" },
];

let draft = null; // working copy of settings while the dialog is open

function settings() { return draft; }

async function commit() {
    const err = await window.go.main.App.SaveSettings(draft);
    if (err) {
        V().toast("settings not saved: " + err, "warn");
        return false;
    }
    // (282) the draft was cloned when the dialog opened: re-read the merged
    // truth so Go-owned fields (recents) written meanwhile survive.
    V().state.settings = await window.go.main.App.GetSettings();
    // (126-129) chat display prefs apply live (CSS classes on #chat-log).
    if (V().applyChatPrefs) V().applyChatPrefs();
    // (294-297) appearance applies live (theme/accent/user CSS/font/compact).
    if (V().applyAppearance) V().applyAppearance();
    // (291) always-on-top applies immediately.
    window.go.main.App.SetAlwaysOnTop(!!draft.always_on_top);
    return true;
}

function row(label, control) {
    const el = document.createElement("div");
    el.className = "set-row";
    const l = document.createElement("span");
    l.className = "set-label";
    l.textContent = label;
    el.appendChild(l);
    el.appendChild(control);
    return el;
}

function checkbox(checked, onchange) {
    const c = document.createElement("input");
    c.type = "checkbox";
    c.checked = !!checked;
    c.onchange = () => onchange(c.checked);
    return c;
}

function numberInput(value, min, max, onchange) {
    const i = document.createElement("input");
    i.type = "number";
    i.min = min; i.max = max; i.value = value;
    i.onchange = () => onchange(parseInt(i.value, 10));
    return i;
}

function slider(value, min, max, oninput) {
    const wrap = document.createElement("div");
    wrap.className = "set-slider";
    const i = document.createElement("input");
    i.type = "range";
    i.min = min; i.max = max; i.value = value;
    const out = document.createElement("span");
    out.className = "mono";
    out.textContent = value;
    i.oninput = () => { out.textContent = i.value; oninput(parseInt(i.value, 10)); };
    wrap.appendChild(i); wrap.appendChild(out);
    return wrap;
}

function hint(text) {
    const el = document.createElement("div");
    el.className = "set-hint";
    el.textContent = text;
    return el;
}

function disabledRow(label, tooltip) {
    const el = row(label, (() => {
        const b = document.createElement("button");
        b.textContent = "…";
        b.disabled = true;
        b.title = tooltip || "coming soon";
        return b;
    })());
    el.classList.add("set-disabled");
    return el;
}

// --- device enumeration --------------------------------------------------------

async function listDevices(kind) {
    try {
        const devices = await navigator.mediaDevices.enumerateDevices();
        return devices.filter((d) => d.kind === kind);
    } catch {
        return [];
    }
}

function deviceSelect(devices, selectedId, onchange) {
    const sel = document.createElement("select");
    const def = document.createElement("option");
    def.value = "";
    def.textContent = "Default device";
    sel.appendChild(def);
    for (const d of devices) {
        const o = document.createElement("option");
        o.value = d.deviceId;
        o.textContent = d.label || `Device ${d.deviceId.slice(0, 8)}`;
        sel.appendChild(o);
    }
    sel.value = selectedId || "";
    sel.onchange = () => onchange(sel.value);
    return sel;
}

// --- pages ---------------------------------------------------------------------

function pageApplication() {
    const s = settings();
    const el = document.createElement("div");
    el.appendChild(row("Chat max lines", numberInput(s.chat_max_lines, 10, 5000, (v) => { s.chat_max_lines = v; })));
    el.appendChild(row("Toasts for join/leave", checkbox(s.notify_join_leave, (v) => { s.notify_join_leave = v; })));
    el.appendChild(row("Toasts for connection events", checkbox(s.notify_connection, (v) => { s.notify_connection = v; })));
    el.appendChild(row("Reconnect on connection loss (5 tries)", checkbox(s.reconnect_on_loss, (v) => { s.reconnect_on_loss = v; })));
    el.appendChild(row("Check for updates at startup", checkbox(s.updates_auto_check !== false, (v) => { s.updates_auto_check = v; })));

    // Window / system integration (wave 8a).
    const sep = document.createElement("div");
    sep.className = "set-subhead";
    sep.textContent = "Window & appearance";
    el.appendChild(sep);
    const themeSel = document.createElement("select");
    for (const [v, label] of [["dark", "Dark (default)"], ["light", "Light"], ["hc", "High contrast"]]) {
        const o = document.createElement("option");
        o.value = v;
        o.textContent = label;
        themeSel.appendChild(o);
    }
    themeSel.value = s.theme || "dark";
    themeSel.onchange = () => { s.theme = themeSel.value; };
    el.appendChild(row("Theme (294/295)", themeSel));
    // (336) language selector: system default, English, Deutsch.
    const langSel = document.createElement("select");
    for (const [v, label] of [["system", "System default"], ["en", "English"], ["de", "Deutsch"]]) {
        const o = document.createElement("option");
        o.value = v;
        o.textContent = label;
        langSel.appendChild(o);
    }
    langSel.value = s.language || "system";
    langSel.onchange = () => { s.language = langSel.value; };
    el.appendChild(row(t("settings.language"), langSel));
    const accent = document.createElement("input");
    accent.type = "color";
    accent.value = s.accent_color || "#2ee6a8";
    accent.onchange = () => { s.accent_color = accent.value; };
    el.appendChild(row("Accent color (296)", accent));
    const fontSel = document.createElement("select");
    for (const [v, label] of [["outfit", "Outfit"], ["sora", "Sora"], ["jetbrains", "JetBrains Mono"]]) {
        const o = document.createElement("option");
        o.value = v;
        o.textContent = label;
        fontSel.appendChild(o);
    }
    fontSel.value = s.ui_font || "outfit";
    fontSel.onchange = () => { s.ui_font = fontSel.value; };
    el.appendChild(row("UI font (297)", fontSel));
    el.appendChild(row("UI font size", slider(s.ui_font_size || 14, 10, 20, (v) => { s.ui_font_size = v; })));
    el.appendChild(row("Always on top (291)", checkbox(s.always_on_top, (v) => { s.always_on_top = v; })));
    el.appendChild(row("Compact mode (293)", checkbox(s.compact_mode, (v) => { s.compact_mode = v; })));
    el.appendChild(row("Reduce motion (344)", checkbox(s.reduce_motion, (v) => { s.reduce_motion = v; })));
    el.appendChild(row("Pause video when unfocused (342)", checkbox(s.idle_video_pause !== false, (v) => { s.idle_video_pause = v; })));
    el.appendChild(row("Close to tray (287)", checkbox(s.close_to_tray, (v) => { s.close_to_tray = v; })));
    el.appendChild(disabledRow("Minimize to tray", "Wails v2 exposes no minimize event — only close-to-tray works"));
    const css = document.createElement("textarea");
    css.className = "dlg-input user-css";
    css.rows = 4;
    css.placeholder = "custom CSS overrides (296), e.g. .channel { letter-spacing: 0.5px }";
    css.value = s.user_css || "";
    css.onchange = () => { s.user_css = css.value; };
    el.appendChild(row("User CSS", css));
    return el;
}

function pageCapture() {
    const s = settings();
    const el = document.createElement("div");
    const deviceRow = row("Capture device", document.createElement("span"));
    el.appendChild(deviceRow);
    listDevices("audioinput").then((devs) => {
        deviceRow.replaceChild(deviceSelect(devs, s.capture_device_id, (v) => { s.capture_device_id = v; }), deviceRow.lastChild);
    });

    // Activation mode.
    const modeWrap = document.createElement("div");
    modeWrap.className = "set-modes";
    for (const [id, label] of [["ptt", "Push-to-Talk"], ["vad", "Voice Activity Detection"], ["continuous", "Continuous Transmission"]]) {
        const l = document.createElement("label");
        const r = document.createElement("input");
        r.type = "radio";
        r.name = "actmode";
        r.checked = (s.activation_mode || "ptt") === id;
        r.onchange = () => {
            s.activation_mode = id;
            vadRow.style.display = id === "vad" ? "" : "none";
        };
        l.appendChild(r);
        l.appendChild(document.createTextNode(" " + label));
        modeWrap.appendChild(l);
    }
    el.appendChild(modeWrap);

    const vadRow = row("VAD threshold", slider(s.vad_threshold, 1, 100, (v) => { s.vad_threshold = v; }));
    vadRow.style.display = (s.activation_mode || "ptt") === "vad" ? "" : "none";
    el.appendChild(vadRow);

    el.appendChild(row("Echo cancellation", checkbox(s.echo_cancellation !== false, (v) => { s.echo_cancellation = v; })));
    el.appendChild(row("Noise suppression", checkbox(s.noise_suppression !== false, (v) => { s.noise_suppression = v; })));

    // (78) Camera frame rate (applies on the next Join Voice).
    const fpsSelect = document.createElement("select");
    for (const fps of [15, 30, 60]) {
        const o = document.createElement("option");
        o.value = String(fps);
        o.textContent = fps + " fps";
        fpsSelect.appendChild(o);
    }
    fpsSelect.value = String(s.camera_fps || 30);
    fpsSelect.onchange = () => { s.camera_fps = parseInt(fpsSelect.value, 10); };
    el.appendChild(row("Camera frame rate", fpsSelect));

    el.appendChild(row("PTT release delay (ms)", slider(s.ptt_release_delay_ms || 0, 0, 2000, (v) => { s.ptt_release_delay_ms = v; })));
    el.appendChild(hint("Capture changes apply on the next Join Voice."));
    // (25) the music profile overrides these two, so say so where they live.
    el.appendChild(hint("Music channels (stereo, 96 kbit/s or more) capture in stereo with echo cancellation, "
        + "noise suppression and auto gain off; your settings above apply everywhere else."));

    // Mic test meter (3) + loopback playback (4).
    const testWrap = document.createElement("div");
    testWrap.className = "mic-test";
    const startBtn = document.createElement("button");
    startBtn.textContent = "Begin Test";
    let loopbackCtx = null;
    const loopChk = checkbox(false, async (v) => {
        if (loopbackCtx) {
            loopbackCtx.close().catch(() => {});
            loopbackCtx = null;
        }
        if (v && testWrap._stream) {
            const { startLoopback } = await import("./audio.js");
            loopbackCtx = await startLoopback(testWrap._stream);
        }
    });
    startBtn.onclick = async () => {
        startBtn.disabled = true;
        const stopBtn = document.createElement("button");
        stopBtn.textContent = "Stop";
        const bar = document.createElement("div");
        bar.className = "mic-bar";
        bar.innerHTML = `<div class="mic-fill"></div>`;
        const fill = bar.querySelector(".mic-fill");
        const err = document.createElement("div");
        err.className = "set-hint warn";
        testWrap.appendChild(stopBtn);
        testWrap.appendChild(bar);
        testWrap.appendChild(err);
        let stream, raf, ctx;
        try {
            stream = await navigator.mediaDevices.getUserMedia({
                audio: s.capture_device_id ? { deviceId: { exact: s.capture_device_id } } : true,
            });
            testWrap._stream = stream;
            ctx = new (window.AudioContext || window.webkitAudioContext)();
            const src = ctx.createMediaStreamSource(stream);
            const an = ctx.createAnalyser();
            an.fftSize = 512;
            src.connect(an);
            const buf = new Uint8Array(an.frequencyBinCount);
            const tick = () => {
                an.getByteTimeDomainData(buf);
                let sum = 0;
                for (const v of buf) sum += Math.abs(v - 128);
                fill.style.width = Math.min(100, (sum / buf.length / 128) * 500) + "%";
                raf = requestAnimationFrame(tick);
            };
            tick();
        } catch (e) {
            err.textContent = "mic test failed: " + (e.message || e.name);
        }
        stopBtn.onclick = () => {
            if (raf) cancelAnimationFrame(raf);
            if (stream) stream.getTracks().forEach((t) => t.stop());
            if (ctx) ctx.close();
            testWrap._stream = null;
            if (loopbackCtx) {
                loopbackCtx.close().catch(() => {});
                loopbackCtx = null;
                loopChk.checked = false;
            }
            stopBtn.remove(); bar.remove();
            startBtn.disabled = false;
        };
    };
    testWrap.appendChild(startBtn);
    el.appendChild(testWrap);
    el.appendChild(row("Loopback test (hear yourself — use headphones!)", loopChk));

    // (5) VAD auto-calibrate.
    const calBtn = document.createElement("button");
    calBtn.textContent = "Auto-calibrate (5s ambient)";
    const calOut = document.createElement("span");
    calOut.className = "set-hint";
    calBtn.onclick = async () => {
        calBtn.disabled = true;
        calOut.textContent = " recording ambient…";
        try {
            const { calibrateMic } = await import("./audio.js");
            const r = await calibrateMic(s.capture_device_id || undefined, 5000);
            s.vad_threshold = r.suggested;
            calOut.textContent = ` noise floor ${(r.floor * 100).toFixed(1)}% → threshold set to ${r.suggested}`;
            if ((s.activation_mode || "ptt") === "vad") renderPage("capture");
        } catch (e) {
            calOut.textContent = " calibration failed: " + (e.message || e.name);
        }
        calBtn.disabled = false;
    };
    const calWrap = document.createElement("div");
    calWrap.className = "mic-test";
    calWrap.appendChild(calBtn);
    calWrap.appendChild(calOut);
    el.appendChild(calWrap);
    return el;
}

function pagePlayback() {
    const s = settings();
    const el = document.createElement("div");
    const deviceRow = row("Output device", document.createElement("span"));
    el.appendChild(deviceRow);
    listDevices("audiooutput").then((devs) => {
        deviceRow.replaceChild(deviceSelect(devs, s.playback_device_id, (v) => { s.playback_device_id = v; }), deviceRow.lastChild);
    });
    el.appendChild(row("Voice volume", slider(s.volume, 0, 200, (v) => {
        s.volume = v;
        const rv = document.getElementById("remote-video");
        if (rv) V().applyOutputSettings(rv);
    })));
    el.appendChild(row("Voice limiter (compressor)", checkbox(s.voice_limiter !== false, (v) => { s.voice_limiter = v; })));
    el.appendChild(row("Per-user gain normalization (cap 4x)", checkbox(s.gain_normalize, (v) => { s.gain_normalize = v; })));
    // (53) each publisher is levelled on its own chain, so a loud speaker no
    // longer sets the gain for a quiet one.
    el.appendChild(hint("Normalization levels every speaker separately. Limiter and normalizer apply on the next Join Voice."));
    const testBtn = document.createElement("button");
    testBtn.textContent = "Play Test Sound";
    testBtn.onclick = () => V().beep(660, 0.25);
    el.appendChild(row("Test", testBtn));
    return el;
}

// hkErrors tracks the last registration error per action (301 conflict
// detection), fed by hotkey_status events.
const hkErrors = new Map();

// hotkeyCapture builds a click-to-rebind button (shared by the map rows).
function hotkeyCapture(initial, oncapture) {
    const b = document.createElement("button");
    b.className = "hotkey-capture";
    b.textContent = initial || "Click and press a key";
    b.onclick = () => {
        b.textContent = "press keys…";
        b.classList.add("capturing");
        const onKey = (e) => {
            e.preventDefault();
            const parts = [];
            if (e.ctrlKey) parts.push("Ctrl");
            if (e.altKey) parts.push("Alt");
            if (e.shiftKey) parts.push("Shift");
            if (e.metaKey) parts.push("Win");
            let key = e.key;
            if (key === " ") key = "Space";
            if (key.length === 1) key = key.toUpperCase();
            if (!["Control", "Alt", "Shift", "Meta"].includes(e.key)) {
                parts.push(key);
                b.textContent = parts.join("+");
                b.classList.remove("capturing");
                document.removeEventListener("keydown", onKey, true);
                oncapture(parts.join("+"));
            }
        };
        document.addEventListener("keydown", onKey, true);
    };
    return b;
}

function pageHotkeys() {
    const s = settings();
    const el = document.createElement("div");

    // (299) the full bindable action map: profile selector + one row per
    // action with rebind/unbind. Overrides land in the selected profile;
    // "default" edits the base bindings.
    const ACTIONS = [
        ["ptt", "Push-to-talk", "hotkey_ptt", "ptt"],
        ["mute_toggle", "Mute toggle", "hotkey_mute", "mute"],
        ["deafen_toggle", "Deafen toggle", "hotkey_deafen", "deafen"],
        ["whisper_reply", "Whisper reply", "whisper_reply_hotkey", "whisper_reply"],
        ["quick_connect", "Quick connect (new tab)", "hotkey_quick_connect", "quick_connect"],
        ["compact_toggle", "Compact mode toggle", "hotkey_compact", "compact"],
        ["zen_toggle", "Zen mode toggle", "hotkey_zen", "zen"],
    ];
    const profiles = Object.keys(s.hotkey_profiles || {}).sort();
    let curProfile = "default";

    const head = document.createElement("div");
    head.className = "set-row";
    const sel = document.createElement("select");
    for (const p of ["default", ...profiles]) {
        const o = document.createElement("option");
        o.value = p;
        o.textContent = p === "default" ? "default profile" : "profile: " + p;
        sel.appendChild(o);
    }
    sel.onchange = () => { curProfile = sel.value; renderRows(); };
    const saveAs = document.createElement("button");
    saveAs.textContent = "Save as new profile…";
    saveAs.onclick = () => {
        const name = prompt("Profile name (assign it to a bookmark in the bookmark manager):");
        if (!name) return;
        s.hotkey_profiles = s.hotkey_profiles || {};
        s.hotkey_profiles[name] = {
            ptt: s.hotkey_ptt, mute: s.hotkey_mute, deafen: s.hotkey_deafen,
            whisper_reply: s.whisper_reply_hotkey, quick_connect: s.hotkey_quick_connect,
            compact: s.hotkey_compact,
        };
        renderPage("hotkeys");
    };
    head.appendChild(sel);
    head.appendChild(saveAs);
    el.appendChild(head);

    const rowsEl = document.createElement("div");
    el.appendChild(rowsEl);

    const renderRows = () => {
        rowsEl.innerHTML = "";
        const prof = (s.hotkey_profiles || {})[curProfile];
        for (const [action, label, field, profField] of ACTIONS) {
            const override = curProfile !== "default" && prof && prof[profField];
            const spec = curProfile === "default" ? (s[field] || "") : (override || "(default: " + (s[field] || "unbound") + ")");
            const wrap = document.createElement("div");
            wrap.className = "hk-map-row";
            const cap = hotkeyCapture(override || s[field] || "", (v) => {
                if (curProfile === "default") {
                    s[field] = v;
                } else {
                    s.hotkey_profiles = s.hotkey_profiles || {};
                    s.hotkey_profiles[curProfile] = s.hotkey_profiles[curProfile] || {};
                    s.hotkey_profiles[curProfile][profField] = v;
                }
                if (/^[A-Z]$/.test(v)) V().toast("warning: bare letter hotkeys fire while typing", "warn");
            });
            const unbind = document.createElement("button");
            unbind.textContent = "✕";
            unbind.title = curProfile === "default" ? "unbind" : "clear override (fall back to default)";
            unbind.onclick = () => {
                if (curProfile === "default") s[field] = "";
                else if (prof) prof[profField] = "";
                renderRows();
            };
            const errEl = document.createElement("span");
            errEl.className = "hk-err warn";
            if (hkErrors.has(action)) errEl.textContent = hkErrors.get(action);
            wrap.appendChild(row(label, cap));
            wrap.appendChild(unbind);
            wrap.appendChild(errEl);
            if (override) wrap.classList.add("hk-override");
            rowsEl.appendChild(wrap);
        }
    };
    renderRows();

    const reset = document.createElement("button");
    reset.textContent = "Reset to defaults (Space / Ctrl+M)";
    reset.onclick = () => {
        s.hotkey_ptt = "Space"; s.hotkey_mute = "Ctrl+M";
        renderPage("hotkeys");
    };
    el.appendChild(reset);
    el.appendChild(hint("Hotkeys are re-registered when you Apply or OK. Profiles apply on connect via the bookmark's profile field (300)."));
    return el;
}

function pageWhisper() {
    const s = settings();
    const st = V().state;
    const el = document.createElement("div");
    el.appendChild(row("Activate whisper", checkbox(s.whisper_active, (v) => { s.whisper_active = v; })));
    el.appendChild(hint("While active, your voice goes to the checked targets instead of your channel."));

    const clientsWrap = document.createElement("div");
    clientsWrap.className = "set-subhead";
    clientsWrap.textContent = "Clients (online now)";
    el.appendChild(clientsWrap);
    const clients = st.clients.filter((c) => c.client_id !== st.myClientID);
    if (clients.length === 0) el.appendChild(hint("No other clients online."));
    for (const c of clients) {
        el.appendChild(row(c.nickname || c.unique_id, checkbox(
            (s.whisper_clients || []).includes(c.unique_id),
            (v) => {
                s.whisper_clients = s.whisper_clients || [];
                if (v && !s.whisper_clients.includes(c.unique_id)) s.whisper_clients.push(c.unique_id);
                if (!v) s.whisper_clients = s.whisper_clients.filter((u) => u !== c.unique_id);
            })));
    }

    const chWrap = document.createElement("div");
    chWrap.className = "set-subhead";
    chWrap.textContent = "Channels";
    el.appendChild(chWrap);
    for (const ch of st.channels) {
        el.appendChild(row(ch.Name, checkbox(
            (s.whisper_channels || []).includes(ch.ChannelID),
            (v) => {
                s.whisper_channels = s.whisper_channels || [];
                if (v && !s.whisper_channels.includes(ch.ChannelID)) s.whisper_channels.push(ch.ChannelID);
                if (!v) s.whisper_channels = s.whisper_channels.filter((id) => id !== ch.ChannelID);
            })));
    }

    el.appendChild(hint("Applying also sends the whisper list to the server now."));
    return el;
}

function pageDownloads() {
    const s = settings();
    const el = document.createElement("div");
    const folder = document.createElement("span");
    folder.className = "mono";
    folder.textContent = s.download_folder || "(not set)";
    const change = document.createElement("button");
    change.textContent = "Change…";
    change.onclick = async () => {
        const dir = await window.go.main.App.PickDownloadFolder();
        if (dir) {
            s.download_folder = dir;
            folder.textContent = dir;
        }
    };
    const wrap = document.createElement("div");
    wrap.className = "set-folder";
    wrap.appendChild(folder); wrap.appendChild(change);
    el.appendChild(row("Download folder", wrap));
    el.appendChild(hint("Client-side file download UI is future work; the folder is stored for it."));
    return el;
}

function pageChat() {
    const s = settings();
    const el = document.createElement("div");

    // (126-129) display prefs — applied live via CSS classes on #chat-log.
    const sel = (options, value, onchange) => {
        const i = document.createElement("select");
        for (const [v, label] of options) {
            const o = document.createElement("option");
            o.value = v;
            o.textContent = label;
            i.appendChild(o);
        }
        i.value = value;
        i.onchange = () => onchange(i.value);
        return i;
    };
    el.appendChild(row("Timestamps", sel(
        [["absolute", "Absolute"], ["relative", "Relative"], ["off", "Off"]],
        s.chat_timestamps || "absolute", (v) => { s.chat_timestamps = v; })));
    el.appendChild(row("Density", sel(
        [["comfortable", "Comfortable"], ["compact", "Compact"]],
        s.chat_density || "comfortable", (v) => { s.chat_density = v; })));
    el.appendChild(row("Layout", sel(
        [["irc", "IRC lines"], ["bubbles", "Bubbles"]],
        s.chat_layout || "irc", (v) => { s.chat_layout = v; })));
    el.appendChild(row("Font size", slider(s.chat_font_size || 14, 12, 18, (v) => { s.chat_font_size = v; })));
    // (130) system-message category filters.
    el.appendChild(row("Show join/leave system lines", checkbox(s.sys_join_leave !== false, (v) => { s.sys_join_leave = v; })));
    el.appendChild(row("Show kick system lines", checkbox(s.sys_kick !== false, (v) => { s.sys_kick = v; })));

    el.appendChild(row("Chat max lines", numberInput(s.chat_max_lines, 10, 5000, (v) => { s.chat_max_lines = v; })));
    el.appendChild(row("Log channel chats to file", checkbox(s.log_channel_chat, (v) => { s.log_channel_chat = v; })));
    el.appendChild(row("Log private chats to file", checkbox(s.log_private_chat, (v) => { s.log_private_chat = v; })));
    el.appendChild(row("Log server/global chats to file", checkbox(s.log_server_chat, (v) => { s.log_server_chat = v; })));

    // (388) keyword highlights for the current server (one per line).
    const kwAddr = V().state.lastConnect?.addr || "";
    const kw = document.createElement("textarea");
    kw.className = "dlg-input user-css";
    kw.rows = 3;
    kw.placeholder = "keyword highlights, one per line (current server)";
    kw.value = ((s.keywords || {})[kwAddr] || []).join("\n");
    kw.onchange = () => {
        s.keywords = s.keywords || {};
        s.keywords[kwAddr] = kw.value.split("\n").map((x) => x.trim()).filter(Boolean);
    };
    el.appendChild(row("Keywords (388)", kw));

    el.appendChild(hint("Chat log: <config>/voicx/chat.log (Help → Open log folder)."));
    // (4b) encryption note.
    const enc = document.createElement("div");
    enc.className = "set-hint";
    enc.textContent = "Encryption: direct messages are end-to-end encrypted (🔒 — the server cannot read them). Channel and global chat are encrypted with server-held keys (🛡) so history and moderation keep working. Key rotation happens when a member leaves; they can still read history from their membership period.";
    el.appendChild(enc);
    return el;
}

function pageSecurity() {
    const el = document.createElement("div");
    const uidLine = document.createElement("div");
    uidLine.className = "set-uid mono";
    uidLine.textContent = "…";
    const created = document.createElement("div");
    created.className = "set-hint";
    const copyBtn = document.createElement("button");
    copyBtn.textContent = "Copy";
    copyBtn.onclick = () => {
        navigator.clipboard.writeText(uidLine.dataset.uid || "").then(() => V().toast("unique ID copied"));
    };
    el.appendChild(row("Unique ID", (() => {
        const w = document.createElement("div");
        w.className = "set-folder";
        w.appendChild(uidLine); w.appendChild(copyBtn);
        return w;
    })()));
    el.appendChild(created);

    // (4a) Transport security: TLS is the default; plaintext is an explicit
    // dev opt-in.
    const s = settings();
    el.appendChild(row("Allow plaintext connections (dev servers)", checkbox(!!s.allow_plaintext, (v) => { s.allow_plaintext = v; })));
    el.appendChild(hint("Server connections use TLS with trust-on-first-use fingerprint pinning (known_servers.json). Enable plaintext only for local dev servers."));

    window.go.main.App.IdentityInfo().then((info) => {
        uidLine.textContent = info.unique_id || "(none)";
        uidLine.dataset.uid = info.unique_id || "";
        created.textContent = info.created_at ? `created ${info.created_at} · ${info.path}` : info.path;
    });

    el.appendChild(row("Export identity", (() => {
        const b = document.createElement("button");
        b.textContent = "Export…";
        b.onclick = async () => {
            const err = await window.go.main.App.ExportIdentity();
            if (err) V().toast("export failed: " + err, "warn");
            else V().toast("identity exported");
        };
        return b;
    })()));
    el.appendChild(row("Import identity", (() => {
        const b = document.createElement("button");
        b.textContent = "Import…";
        b.onclick = async () => {
            const err = await window.go.main.App.ImportIdentity();
            if (err) V().toast("import failed: " + err, "warn");
            else V().toast("identity imported — reconnect to use it", "warn");
        };
        return b;
    })()));

    const danger = document.createElement("div");
    danger.className = "set-danger";
    const regen = document.createElement("button");
    regen.className = "danger-btn";
    regen.textContent = "Regenerate identity";
    regen.onclick = () => {
        if (!confirm("Regenerating replaces your identity key. Servers will see you as a NEW user (registered accounts keep working via password). Continue?")) return;
        window.go.main.App.RegenerateIdentity().then(() => {
            V().toast("identity regenerated — reconnect to use it", "warn");
            window.go.main.App.IdentityInfo().then((info) => {
                uidLine.textContent = info.unique_id;
                uidLine.dataset.uid = info.unique_id;
            });
        });
    };
    danger.appendChild(regen);
    el.appendChild(danger);
    return el;
}

function pageNotifications() {
    const s = settings();
    const el = document.createElement("div");
    // (385) notification matrix: rows = events, columns = outputs.
    const EVENTS = [
        ["mention", "mention"], ["keyword", "keyword highlight"], ["dm", "direct message"],
        ["poke", "poke"], ["join_leave", "join/leave (your channel)"],
        ["buddy_online", "watched contact online"], ["kick", "kick/ban"],
        ["announcement", "announcement"], ["channel_watch", "channel watch"],
    ];
    const matrix = document.createElement("table");
    matrix.className = "perm-grid notify-matrix";
    matrix.innerHTML = `<thead><tr><th>event</th><th>toast</th><th>sound</th><th>flash</th><th>native</th><th>custom beep (384)</th></tr></thead><tbody></tbody>`;
    const tbody = matrix.querySelector("tbody");
    s.notify_matrix = s.notify_matrix || {};
    s.custom_sounds = s.custom_sounds || {};
    for (const [event, label] of EVENTS) {
        const rowData = s.notify_matrix[event] || { toast: true, sound: true, flash: true, native: true };
        s.notify_matrix[event] = rowData;
        const tr = document.createElement("tr");
        tr.innerHTML = `<td class="mono">${label}</td>`;
        for (const col of ["toast", "sound", "flash", "native"]) {
            const td = document.createElement("td");
            td.appendChild(checkbox(rowData[col], (v) => { rowData[col] = v; }));
            tr.appendChild(td);
        }
        // (384) custom beep: freq + duration + preview.
        const td = document.createElement("td");
        td.className = "beep-cell";
        const spec = s.custom_sounds[event] || { freq: 0, duration_ms: 200 };
        s.custom_sounds[event] = spec;
        const freq = document.createElement("input");
        freq.type = "number";
        freq.min = 100; freq.max = 2000; freq.value = spec.freq || "";
        freq.placeholder = "Hz";
        freq.className = "beep-freq";
        freq.onchange = () => { spec.freq = parseInt(freq.value, 10) || 0; };
        const dur = document.createElement("input");
        dur.type = "number";
        dur.min = 30; dur.max = 2000; dur.value = spec.duration_ms || 200;
        dur.className = "beep-dur";
        dur.onchange = () => { spec.duration_ms = parseInt(dur.value, 10) || 200; };
        const prev = document.createElement("button");
        prev.textContent = "▶";
        prev.title = "preview";
        prev.onclick = async () => {
            const { play } = await import("./sounds.js");
            // force: a preview must be audible even with the master toggle off.
            if (spec.freq > 0) play("sine", spec.freq, (spec.duration_ms || 200) / 1000, 1, true);
        };
        td.append(freq, dur, prev);
        tr.appendChild(td);
        tbody.appendChild(tr);
    }
    el.appendChild(matrix);
    el.appendChild(hint("Empty freq = sound-pack default. DND overrides everything (mentions still badge)."));
    // (347/348) do-not-disturb: toggle + quiet hours schedule.
    el.appendChild(row("Do not disturb (347)", checkbox(s.dnd_enabled, (v) => { s.dnd_enabled = v; })));
    const from = document.createElement("input");
    from.type = "time";
    from.value = s.dnd_from || "";
    from.onchange = () => { s.dnd_from = from.value; };
    const to = document.createElement("input");
    to.type = "time";
    to.value = s.dnd_to || "";
    to.onchange = () => { s.dnd_to = to.value; };
    const hours = document.createElement("div");
    hours.className = "dnd-hours";
    hours.append(from, document.createTextNode(" – "), to);
    el.appendChild(row("Quiet hours (348, empty = off)", hours));
    el.appendChild(hint("DND suppresses toasts, sounds, and taskbar flashes; mentions still badge silently."));
    el.appendChild(row("Toasts for join/leave", checkbox(s.notify_join_leave, (v) => { s.notify_join_leave = v; })));
    el.appendChild(row("Toasts for connection events", checkbox(s.notify_connection, (v) => { s.notify_connection = v; })));
    el.appendChild(row("Warn when talking while muted", checkbox(s.warn_muted_talking !== false, (v) => { s.warn_muted_talking = v; })));
    el.appendChild(row("Hint when talking to an empty channel", checkbox(s.warn_empty_channel !== false, (v) => { s.warn_empty_channel = v; })));

    // (28) master gate, now actually read by sounds.js: only an explicit
    // false silences playback, so a settings blob without the field is on.
    el.appendChild(row("Play sounds (master)", checkbox(s.play_sounds !== false, (v) => { s.play_sounds = v; })));

    // (28) Sound pack system.
    const packSel = document.createElement("select");
    for (const p of ["soft", "bright", "retro"]) {
        const o = document.createElement("option");
        o.value = p;
        o.textContent = p[0].toUpperCase() + p.slice(1);
        packSel.appendChild(o);
    }
    packSel.value = s.sound_pack || "soft";
    packSel.onchange = () => { s.sound_pack = packSel.value; };
    el.appendChild(row("Sound pack", packSel));
    el.appendChild(row("Sound volume", slider(s.sound_volume ?? 100, 0, 200, (v) => { s.sound_volume = v; })));

    const sub = document.createElement("div");
    sub.className = "set-subhead";
    sub.textContent = "Event sounds";
    el.appendChild(sub);
    // (28) driven by the pack's event list so every matrix event is togglable.
    for (const ev of SOUND_EVENTS) {
        const enabled = !s.event_sounds || s.event_sounds[ev] !== false;
        el.appendChild(row(ev.replace(/_/g, " "), checkbox(enabled, (v) => {
            s.event_sounds = s.event_sounds || {};
            s.event_sounds[ev] = v;
        })));
    }

    const testBtn = document.createElement("button");
    testBtn.textContent = "Test all sounds";
    testBtn.onclick = async () => {
        const { testAll } = await import("./sounds.js");
        testAll();
    };
    el.appendChild(row("Test", testBtn));
    return el;
}

const PAGE_BUILDERS = {
    application: pageApplication,
    capture: pageCapture,
    playback: pagePlayback,
    hotkeys: pageHotkeys,
    whisper: pageWhisper,
    downloads: pageDownloads,
    chat: pageChat,
    security: pageSecurity,
    notifications: pageNotifications,
};

function renderPage(id) {
    document.querySelectorAll(".settings-nav-item").forEach((n) => {
        n.classList.toggle("active", n.dataset.page === id);
    });
    const container = document.getElementById("settings-content");
    container.innerHTML = "";
    container.appendChild(PAGE_BUILDERS[id]());
}

function openSettings(pageId = "application") {
    draft = JSON.parse(JSON.stringify(V().state.settings || {}));

    let overlay = document.getElementById("settings-overlay");
    if (overlay) overlay.remove();

    overlay = document.createElement("div");
    overlay.id = "settings-overlay";
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="settings-dialog">
            <div class="settings-nav"></div>
            <div class="settings-main">
                <input id="settings-search" class="dlg-input" placeholder="" />
                <div id="settings-content"></div>
                <div class="settings-footer">
                    <button id="set-ok">OK</button>
                    <button id="set-cancel">Cancel</button>
                    <button id="set-apply">Apply</button>
                </div>
            </div>
        </div>`;

    // (350) settings search: filters rows by label text across all pages,
    // jumping to the matching page and highlighting hits.
    const search = overlay.querySelector("#settings-search");
    search.placeholder = t("settings.searchPlaceholder");
    const content = overlay.querySelector("#settings-content");
    search.oninput = () => {
        const q = search.value.trim().toLowerCase();
        if (!q) {
            renderPage(document.querySelector(".settings-nav-item.active")?.dataset.page || "application");
            return;
        }
        // Search all pages; collect matches as (page, label).
        const hits = [];
        for (const p of PAGES) {
            const pageEl = PAGE_BUILDERS[p.id]();
            pageEl.querySelectorAll(".set-row, .set-subhead, .set-hint, button").forEach((r) => {
                const label = (r.querySelector(".set-label")?.textContent || r.textContent || "").toLowerCase();
                if (label.includes(q)) hits.push({ page: p.id, label: label.trim() });
            });
        }
        content.innerHTML = "";
        if (hits.length === 0) {
            content.innerHTML = `<div class="empty-state">no matching settings</div>`;
            return;
        }
        for (const h of hits.slice(0, 40)) {
            const row = document.createElement("div");
            row.className = "set-search-hit";
            row.innerHTML = `<span class="mono set-search-page"></span><span class="set-search-label"></span>`;
            row.querySelector(".set-search-page").textContent = h.page;
            row.querySelector(".set-search-label").textContent = h.label;
            row.onclick = () => {
                search.value = "";
                renderPage(h.page);
                content.querySelectorAll(".set-row").forEach((r) => {
                    if ((r.querySelector(".set-label")?.textContent || r.textContent || "").toLowerCase().includes(h.label.slice(0, 20))) {
                        r.classList.add("set-hit");
                        r.scrollIntoView({ block: "center" });
                    }
                });
            };
            content.appendChild(row);
        }
    };

    const nav = overlay.querySelector(".settings-nav");
    for (const p of PAGES) {
        const item = document.createElement("div");
        item.className = "settings-nav-item";
        item.dataset.page = p.id;
        item.innerHTML = `<span class="nav-icon">${p.icon}</span><span>${t(p.label)}</span>`;
        item.onclick = () => renderPage(p.id);
        nav.appendChild(item);
    }

    const applyAll = async () => {
        if (!(await commit())) return false;
        if (draft.whisper_active) {
            window.go.main.App.WhisperSet(draft.whisper_clients || [], draft.whisper_channels || [], true);
        } else {
            window.go.main.App.WhisperSet([], [], false);
        }
        return true;
    };

    overlay.querySelector("#set-ok").onclick = async () => {
        if (await applyAll()) overlay.remove();
    };
    overlay.querySelector("#set-cancel").onclick = () => overlay.remove();
    overlay.querySelector("#set-apply").onclick = applyAll;
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };

    document.body.appendChild(overlay);
    renderPage(pageId);
}

export function initSettingsUI() {
    window.__voicx.openSettings = openSettings;
    // (301) track registration errors for the hotkey map rows.
    window.runtime.EventsOn("hotkey_status", (st) => {
        if (st.error) hkErrors.set(st.action, st.error);
        else if (st.registered) hkErrors.delete(st.action);
    });
}
