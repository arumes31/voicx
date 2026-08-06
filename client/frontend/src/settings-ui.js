// settings-ui.js — TS3-style settings dialog with left icon nav.
import { t } from "./i18n.js";
import { calibrateMic, startLoopback } from "./audio.js";
import { play, SOUND_EVENTS, testAll } from "./sounds.js";
import { MATRIX_EVENTS, defaultMatrixRow } from "./notifications.js";

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
    { id: "server", icon: "🖥", label: "settings.server" },
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
    // (291) always-on-top and (292) opacity apply immediately.
    window.go.main.App.SetAlwaysOnTop(!!draft.always_on_top);
    window.go.main.App.SetWindowOpacity(draft.window_opacity || 100);
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

// revertLivePreview puts the saved appearance back after a cancelled dialog:
// window opacity (292) and the theme swatches (295) both preview live, so
// discarding the draft has to discard what is on screen too.
function revertLivePreview() {
    const saved = V().state.settings || {};
    V().applyAppearance();
    window.go.main.App.SetWindowOpacity(saved.window_opacity || 100);
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

    // Presence (308/390): the idle timer and the status line it publishes.
    const psep = document.createElement("div");
    psep.className = "set-subhead";
    psep.textContent = "Presence";
    el.appendChild(psep);
    el.appendChild(row("Auto-away after (minutes, 0 = off)", numberInput(s.auto_away_minutes ?? 15, 0, 240, (v) => { s.auto_away_minutes = v; })));
    const awayMsg = document.createElement("input");
    awayMsg.className = "dlg-input";
    awayMsg.maxLength = 200;
    awayMsg.placeholder = "auto-away";
    awayMsg.value = s.auto_away_message ?? "";
    awayMsg.onchange = () => { s.auto_away_message = awayMsg.value; };
    el.appendChild(row("Auto-away status message (390)", awayMsg));
    el.appendChild(hint("Other clients see this text next to your 🕐 away icon while you are idle."));

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
    el.appendChild(row("Minimize to tray (288)", checkbox(s.minimize_to_tray, (v) => { s.minimize_to_tray = v; })));
    // (292) the floor keeps the window clickable. Applied on release, not per
    // drag frame: the binding persists the settings file on every call.
    const opacity = slider(s.window_opacity || 100, 20, 100, (v) => { s.window_opacity = v; });
    opacity.querySelector("input").addEventListener("change",
        () => window.go.main.App.SetWindowOpacity(s.window_opacity || 100));
    el.appendChild(row("Window opacity (292)", opacity));
    el.appendChild(themeEditor(s)); // (295)
    const css = document.createElement("textarea");
    css.className = "dlg-input user-css";
    css.rows = 4;
    css.placeholder = "custom CSS overrides (296), e.g. .channel { letter-spacing: 0.5px }";
    css.value = s.user_css || "";
    css.onchange = () => { s.user_css = css.value; };
    el.appendChild(row("User CSS", css));
    return el;
}

function pageServer() {
    const el = document.createElement("div");
    const status = hint("Loading effective server configuration…");
    el.appendChild(status);
    if (!V().state.isAdmin) {
        status.textContent = "Server configuration is available to administrators only.";
        return el;
    }

    const form = document.createElement("div");
    form.hidden = true;
    el.appendChild(form);
    const values = {};
    const addNumber = (label, key, min, max) => {
        const input = numberInput(0, min, max, (v) => { values[key] = v; });
        form.appendChild(row(label, input));
        values[key] = 0;
        return input;
    };
    const maxClients = addNumber("Maximum clients (0 = unlimited)", "max_clients", 0, 100000);
    const timeout = addNumber("Connection timeout (seconds)", "client_timeout_seconds", 30, 86400);
    const bitrate = addNumber("Default Opus bitrate (bit/s)", "opus_bitrate", 6000, 510000);
    const fec = checkbox(false, (v) => { values.opus_fec = v; });
    const dtx = checkbox(false, (v) => { values.opus_dtx = v; });
    const stereo = checkbox(false, (v) => { values.opus_stereo = v; });
    form.appendChild(row("Default Opus in-band FEC", fec));
    form.appendChild(row("Default Opus DTX", dtx));
    form.appendChild(row("Default Opus stereo", stereo));
    form.appendChild(hint("Codec defaults apply to newly created channels. Existing channels keep their explicit settings."));
    const applyToForm = (cfg) => {
        Object.assign(values, cfg);
        maxClients.value = cfg.max_clients;
        timeout.value = cfg.client_timeout_seconds;
        bitrate.value = cfg.opus_bitrate;
        fec.checked = !!cfg.opus_fec;
        dtx.checked = !!cfg.opus_dtx;
        stereo.checked = !!cfg.opus_stereo;
    };
    const apply = document.createElement("button");
    apply.textContent = "Apply server configuration";
    apply.onclick = async () => {
        apply.disabled = true;
        try {
            const result = await window.go.main.App.SetServerConfig(values);
            applyToForm(result);
            status.textContent = "Server configuration saved and active.";
            V().toast("server configuration updated");
        } catch (err) {
            status.textContent = "Could not save server configuration: " + err;
            V().toast(status.textContent, "warn");
        } finally {
            apply.disabled = false;
        }
    };
    form.appendChild(row("Runtime settings", apply));

    window.go.main.App.GetServerConfig().then((cfg) => {
        applyToForm(cfg);
        status.textContent = "Changes take effect immediately and are restored after restart.";
        form.hidden = false;
    }).catch((err) => {
        status.textContent = "Could not load server configuration: " + err;
    });
    return el;
}

// --- theme editor (295) --------------------------------------------------------

// THEME_VARS are the palette variables the editor exposes. Only the opaque
// ones: a color input cannot express the rgba() borders and shadows, and
// --accent already has its own control (296) whose inline style would win.
const THEME_VARS = [
    ["--bg", "Background"],
    ["--bg-raised", "Raised surface"],
    ["--bg-panel", "Panel"],
    ["--bg-hover", "Hover"],
    ["--text", "Text"],
    ["--text-dim", "Text (dim)"],
    ["--text-faint", "Text (faint)"],
    ["--warn", "Warning"],
    ["--danger", "Danger"],
];

// The editor owns the block between these markers inside user_css; anything
// the user typed by hand around it survives an edit.
const THEME_START = "/* voicx-theme-start */";
const THEME_END = "/* voicx-theme-end */";

// themeBlockBody returns the CSS between the markers ("" when absent).
function themeBlockBody(css) {
    const a = (css || "").indexOf(THEME_START);
    const b = (css || "").indexOf(THEME_END);
    return a >= 0 && b > a ? css.slice(a + THEME_START.length, b) : "";
}

// parseThemeOverrides reads the editor's own block back into a map.
function parseThemeOverrides(css) {
    const out = {};
    for (const m of themeBlockBody(css).matchAll(/(--[a-z-]+)\s*:\s*([^;]+);/g)) {
        out[m[1]] = m[2].trim();
    }
    return out;
}

// writeThemeOverrides splices the block back into user_css. The selector
// carries an attribute so it outranks the :root[data-theme="…"] palettes,
// which a bare :root block would lose to.
function writeThemeOverrides(css, overrides) {
    const keys = Object.keys(overrides);
    const block = keys.length === 0 ? "" : THEME_START + "\n:root, :root[data-theme] {\n" +
        keys.map((k) => `    ${k}: ${overrides[k]};`).join("\n") + "\n}\n" + THEME_END;
    const a = (css || "").indexOf(THEME_START);
    const b = (css || "").indexOf(THEME_END);
    if (a >= 0 && b > a) {
        return (css.slice(0, a) + block + css.slice(b + THEME_END.length)).trim();
    }
    return (block + "\n" + (css || "")).trim();
}

// currentVar resolves a variable to a #rrggbb value for the color input.
function currentVar(name, overrides) {
    const raw = overrides[name] || getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    const rgb = raw.match(/^rgba?\((\d+)[,\s]+(\d+)[,\s]+(\d+)/);
    if (rgb) {
        return "#" + [1, 2, 3].map((i) => Number(rgb[i]).toString(16).padStart(2, "0")).join("");
    }
    // #abc shorthand: the color input only accepts the six-digit form.
    if (/^#[0-9a-f]{3}$/i.test(raw)) return "#" + raw.slice(1).split("").map((c) => c + c).join("");
    return /^#[0-9a-f]{6}$/i.test(raw) ? raw : "#000000";
}

// themeEditor builds the CSS-variable editor (295): every swatch rewrites the
// managed block in user_css, which applyAppearance injects as a stylesheet.
function themeEditor(s) {
    const wrap = document.createElement("div");
    const head = document.createElement("div");
    head.className = "set-subhead";
    head.textContent = "Theme colors (295)";
    wrap.appendChild(head);
    const grid = document.createElement("div");
    grid.className = "theme-grid";
    const overrides = parseThemeOverrides(s.user_css);
    const apply = () => {
        s.user_css = writeThemeOverrides(s.user_css, overrides);
        // Preview by writing the same style element applyAppearance owns. A
        // full applyAppearance would rebuild the menu bar on every frame of a
        // swatch drag; cancelling the dialog calls it and restores the saved
        // stylesheet.
        let st = document.getElementById("user-css");
        if (!st) {
            st = document.createElement("style");
            st.id = "user-css";
            document.head.appendChild(st);
        }
        st.textContent = s.user_css;
    };
    for (const [name, label] of THEME_VARS) {
        const cell = document.createElement("label");
        cell.className = "theme-cell";
        const inp = document.createElement("input");
        inp.type = "color";
        inp.value = currentVar(name, overrides);
        inp.oninput = () => {
            overrides[name] = inp.value;
            apply();
        };
        const txt = document.createElement("span");
        txt.textContent = label;
        txt.title = name;
        cell.appendChild(inp);
        cell.appendChild(txt);
        grid.appendChild(cell);
    }
    wrap.appendChild(grid);
    const reset = document.createElement("button");
    reset.textContent = "Reset theme colors";
    reset.onclick = () => {
        for (const k of Object.keys(overrides)) delete overrides[k];
        apply();
        for (const [i, [name]] of THEME_VARS.entries()) {
            grid.children[i].querySelector("input").value = currentVar(name, overrides);
        }
    };
    wrap.appendChild(reset);
    return wrap;
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

    // (78) Camera frame rate applies when the automatic voice session next starts.
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
    el.appendChild(hint("Capture changes apply when voice next reconnects."));
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
    el.appendChild(hint("Normalization levels every speaker separately. Limiter and normalizer apply when voice next reconnects."));
    const testBtn = document.createElement("button");
    testBtn.textContent = "Play Test Sound";
    testBtn.onclick = () => V().beep(660, 0.25);
    el.appendChild(row("Test", testBtn));
    return el;
}

// hkErrors tracks the last activation error per action (301 conflict
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
    reset.textContent = "Reset to defaults (PTT unbound / Ctrl+M)";
    reset.onclick = () => {
        s.hotkey_ptt = ""; s.hotkey_mute = "Ctrl+M";
        renderPage("hotkeys");
    };
    el.appendChild(reset);
    el.appendChild(hint("Hotkeys are applied when you Apply or OK. On Windows they do not reserve or consume the configured keys. Profiles apply on connect via the bookmark's profile field (300)."));
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
    el.appendChild(hint("Downloads from the file browser land here without asking. Leave it unset to be prompted for a location each time."));
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

// refreshIdentities repaints the identity manager table (351) from the Go
// side, which owns the store.
async function refreshIdentities(tbody) {
    let list = [];
    try {
        list = await window.go.main.App.ListIdentities();
    } catch (e) {
        tbody.innerHTML = "";
        const tr = document.createElement("tr");
        tr.innerHTML = `<td colspan="6" class="warn"></td>`;
        tr.querySelector("td").textContent = "identities unavailable: " + e;
        tbody.appendChild(tr);
        return;
    }
    // (351) the draft is a clone taken when the dialog opened; switching an
    // identity writes settings behind its back, so mirror it or OK reverts it.
    const active = list.find((x) => x.active);
    if (active && settings()) settings().active_identity = active.id;
    tbody.innerHTML = "";
    for (const e of list) {
        const tr = document.createElement("tr");
        if (e.active) tr.classList.add("id-active");

        const name = document.createElement("td");
        name.textContent = (e.active ? "● " : "") + e.name;
        name.title = e.path;
        tr.appendChild(name);

        const uid = document.createElement("td");
        uid.className = "mono id-uid";
        uid.textContent = e.unique_id ? e.unique_id.slice(0, 16) + "…" : "(unreadable)";
        uid.title = e.unique_id || "";
        uid.onclick = () => {
            if (!e.unique_id) return;
            navigator.clipboard.writeText(e.unique_id).then(() => V().toast("unique ID copied"));
        };
        tr.appendChild(uid);

        // (352) proof-of-work level on the unique ID.
        const lvl = document.createElement("td");
        lvl.className = "mono";
        lvl.textContent = String(e.security_level ?? 0);
        tr.appendChild(lvl);

        // (354) which storage mode the key is actually in.
        const prot = document.createElement("td");
        prot.textContent = e.protection === "dpapi" ? "🔒 DPAPI" : "📄 plaintext";
        prot.title = e.protection === "dpapi"
            ? "private key sealed to this Windows account — a copy of this file will not open elsewhere"
            : "private key stored in the clear (no OS key store in use)";
        tr.appendChild(prot);

        // (353) backup state.
        const backup = document.createElement("td");
        backup.textContent = e.exported_at ? "✓ " + e.exported_at : "⚠ never";
        if (!e.exported_at) backup.className = "warn";
        tr.appendChild(backup);

        const actions = document.createElement("td");
        actions.className = "id-actions";
        const mk = (label, title, fn) => {
            const b = document.createElement("button");
            b.textContent = label;
            b.title = title;
            b.onclick = fn;
            actions.appendChild(b);
            return b;
        };
        mk("Use", "make this the identity used on the next connect", async () => {
            const err = await window.go.main.App.SwitchIdentity(e.id);
            if (err) V().toast("switch failed: " + err, "warn");
            else V().toast("active identity: " + e.name + " — reconnect to use it", "warn");
            refreshIdentities(tbody);
        }).disabled = e.active;
        mk("Rename", "change the display label", async () => {
            const name = prompt("Identity name:", e.name);
            if (!name) return;
            const err = await window.go.main.App.RenameIdentity(e.id, name);
            if (err) V().toast("rename failed: " + err, "warn");
            refreshIdentities(tbody);
        });
        mk("Export", "save a portable backup of this identity", async () => {
            const err = await window.go.main.App.ExportIdentity(e.id);
            if (err) V().toast("export failed: " + err, "warn");
            else V().toast("identity exported — keep the file safe");
            refreshIdentities(tbody);
        });
        mk("Level…", "raise the proof-of-work security level (352)", async () => {
            const target = parseInt(prompt("Target security level (leading zero bits, 1-40):", String((e.security_level || 0) + 4)), 10);
            if (!target) return;
            V().toast("computing security level (up to 30s)…");
            const res = await window.go.main.App.ImproveIdentityLevel(e.id, target, 30);
            if (res.error) V().toast("level failed: " + res.error, "warn");
            else V().toast(`security level ${res.level} (counter ${res.counter})`);
            refreshIdentities(tbody);
        });
        mk("Delete", "remove this identity from this machine", async () => {
            const warn = e.exported_at
                ? `Delete identity "${e.name}"?`
                : `"${e.name}" has NEVER been exported. Deleting it loses that account on every server forever. Delete anyway?`;
            if (!confirm(warn)) return;
            const err = await window.go.main.App.DeleteIdentity(e.id, true);
            if (err) V().toast("delete failed: " + err, "warn");
            refreshIdentities(tbody);
        }).classList.add("danger-btn");
        tr.appendChild(actions);
        tbody.appendChild(tr);
    }
}

function pageSecurity() {
    const s = settings();
    const el = document.createElement("div");

    // (351) multiple identities: the identity IS the account, so the manager
    // is the primary control here.
    const sub = document.createElement("div");
    sub.className = "set-subhead";
    sub.textContent = "Identities (351)";
    el.appendChild(sub);

    const table = document.createElement("table");
    table.className = "perm-grid identity-grid";
    table.innerHTML = `<thead><tr><th>name</th><th>unique ID</th><th>level</th><th>key storage</th><th>backup</th><th></th></tr></thead><tbody></tbody>`;
    const tbody = table.querySelector("tbody");
    el.appendChild(table);
    // (350) the settings search rebuilds every page on each keystroke; only
    // the page that is really on screen may hit the identity store, which
    // unseals a protected key per row.
    setTimeout(() => { if (tbody.isConnected) refreshIdentities(tbody); }, 0);

    const bar = document.createElement("div");
    bar.className = "set-folder";
    const newBtn = document.createElement("button");
    newBtn.textContent = "New identity…";
    newBtn.onclick = async () => {
        const name = prompt("Name for the new identity (e.g. \"gaming\"):");
        if (!name) return;
        const err = await window.go.main.App.CreateIdentity(name);
        if (err) V().toast("create failed: " + err, "warn");
        refreshIdentities(tbody);
    };
    const importBtn = document.createElement("button");
    importBtn.textContent = "Import…";
    importBtn.onclick = async () => {
        const err = await window.go.main.App.ImportIdentity();
        if (err) V().toast("import failed: " + err, "warn");
        else V().toast("identity imported and selected — reconnect to use it", "warn");
        refreshIdentities(tbody);
    };
    const regen = document.createElement("button");
    regen.className = "danger-btn";
    regen.textContent = "Regenerate active";
    regen.onclick = async () => {
        if (!confirm("Regenerating replaces the ACTIVE identity's key. Servers will see you as a NEW user (registered accounts keep working via password). Continue?")) return;
        const uid = await window.go.main.App.RegenerateIdentity();
        V().toast("identity regenerated (" + uid.slice(0, 16) + "…) — reconnect to use it", "warn");
        refreshIdentities(tbody);
    };
    bar.append(newBtn, importBtn, regen);
    el.appendChild(bar);
    el.appendChild(hint("The active identity (●) is used on the next connect. Click a unique ID to copy it. "
        + "An export is a portable plaintext copy — it is the only way to recover an identity if this machine dies (353)."));

    // (354) key storage at rest, with its fallback stated plainly.
    const protSel = document.createElement("select");
    for (const [v, label] of [["auto", "Use the OS key store when available (default)"], ["off", "Always store in plaintext"]]) {
        const o = document.createElement("option");
        o.value = v;
        o.textContent = label;
        protSel.appendChild(o);
    }
    protSel.value = s.identity_key_protection === "off" ? "off" : "auto";
    protSel.onchange = () => { s.identity_key_protection = protSel.value; };
    el.appendChild(row("Private key storage (354)", protSel));
    el.appendChild(hint("On Windows the private key is sealed with DPAPI to your user account, so a stolen copy of the file "
        + "is useless elsewhere. Where DPAPI is unavailable the client falls back to the plaintext file instead of refusing to "
        + "start. Applies the next time an identity file is written (switch, rename, level, regenerate)."));

    // (4a) Transport security: TLS is the default; plaintext is an explicit
    // dev opt-in.
    const tsub = document.createElement("div");
    tsub.className = "set-subhead";
    tsub.textContent = "Transport";
    el.appendChild(tsub);
    el.appendChild(row("Allow plaintext connections (dev servers)", checkbox(!!s.allow_plaintext, (v) => { s.allow_plaintext = v; })));
    el.appendChild(hint("Server connections use TLS with trust-on-first-use fingerprint pinning (known_servers.json). Enable plaintext only for local dev servers."));
    return el;
}

function pageNotifications() {
    const s = settings();
    const el = document.createElement("div");
    const chatLevel = document.createElement("select");
    chatLevel.className = "dlg-input";
    for (const [value, label] of [
        ["direct", "Direct messages only"],
        ["channel_mentions", "DMs + channel mentions"],
        ["role_mentions", "DMs + role mentions"],
        ["all", "All messages"],
    ]) {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = label;
        chatLevel.appendChild(option);
    }
    chatLevel.value = s.chat_notification_level || "all";
    chatLevel.onchange = () => { s.chat_notification_level = chatLevel.value; };
    el.appendChild(row("Chat notification category", chatLevel));
    el.appendChild(hint("Per-channel mute and notification matrix outputs still apply after this category filter."));
    // (385) notification matrix: rows = events, columns = outputs. The rows
    // are the dispatcher's own event list, so every event it can fire has
    // reachable toggles here and the two cannot drift apart.
    const EVENTS = MATRIX_EVENTS;
    const matrix = document.createElement("table");
    matrix.className = "perm-grid notify-matrix";
    matrix.innerHTML = `<thead><tr><th>event</th><th>toast</th><th>sound</th><th>flash</th><th>native</th><th>custom beep (384)</th></tr></thead><tbody></tbody>`;
    const tbody = matrix.querySelector("tbody");
    s.notify_matrix = s.notify_matrix || {};
    s.custom_sounds = s.custom_sounds || {};
    for (const [event, label] of EVENTS) {
        // Unset rows seed from the dispatcher's default: this grid writes what
        // it shows, so a local guess would change behaviour on first visit.
        const rowData = s.notify_matrix[event] || defaultMatrixRow(event);
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
    server: pageServer,
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
    const cancel = () => {
        overlay.remove();
        revertLivePreview();
    };
    overlay.querySelector("#set-cancel").onclick = cancel;
    overlay.querySelector("#set-apply").onclick = applyAll;
    overlay.onclick = (e) => { if (e.target === overlay) cancel(); };

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
