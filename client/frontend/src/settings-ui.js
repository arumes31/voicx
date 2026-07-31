// settings-ui.js — TS3-style settings dialog with left icon nav.
const V = () => window.__voicx;

const PAGES = [
    { id: "application", icon: "⚙", label: "Application" },
    { id: "capture", icon: "🎙", label: "Capture" },
    { id: "playback", icon: "🔊", label: "Playback" },
    { id: "hotkeys", icon: "⌨", label: "Hotkeys" },
    { id: "whisper", icon: "🤫", label: "Whisper" },
    { id: "downloads", icon: "⬇", label: "Downloads" },
    { id: "chat", icon: "💬", label: "Chat" },
    { id: "security", icon: "🔑", label: "Security" },
    { id: "notifications", icon: "🔔", label: "Notifications" },
];

let draft = null; // working copy of settings while the dialog is open

function settings() { return draft; }

async function commit() {
    const err = await window.go.main.App.SaveSettings(draft);
    if (err) {
        V().toast("settings not saved: " + err, "warn");
        return false;
    }
    V().state.settings = draft;
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
    el.appendChild(hint("Capture changes apply on the next Join Voice."));

    // Mic test meter.
    const testWrap = document.createElement("div");
    testWrap.className = "mic-test";
    const startBtn = document.createElement("button");
    startBtn.textContent = "Begin Test";
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
            stopBtn.remove(); bar.remove();
            startBtn.disabled = false;
        };
    };
    testWrap.appendChild(startBtn);
    el.appendChild(testWrap);
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
    const testBtn = document.createElement("button");
    testBtn.textContent = "Play Test Sound";
    testBtn.onclick = () => V().beep(660, 0.25);
    el.appendChild(row("Test", testBtn));
    return el;
}

function pageHotkeys() {
    const s = settings();
    const el = document.createElement("div");

    const capture = (initial, oncapture) => {
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
    };

    el.appendChild(row("Push-to-talk key", capture(s.hotkey_ptt, (v) => {
        s.hotkey_ptt = v;
        if (/^[A-Z]$/.test(v)) V().toast("warning: bare letter hotkeys fire while typing", "warn");
    })));
    el.appendChild(row("Mute toggle key", capture(s.hotkey_mute, (v) => {
        s.hotkey_mute = v;
        if (/^[A-Z]$/.test(v)) V().toast("warning: bare letter hotkeys fire while typing", "warn");
    })));
    const reset = document.createElement("button");
    reset.textContent = "Reset to defaults (Space / Ctrl+M)";
    reset.onclick = () => {
        s.hotkey_ptt = "Space"; s.hotkey_mute = "Ctrl+M";
        renderPage("hotkeys");
    };
    el.appendChild(reset);
    el.appendChild(hint("Hotkeys are re-registered when you Apply or OK."));
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
    el.appendChild(row("Chat max lines", numberInput(s.chat_max_lines, 10, 5000, (v) => { s.chat_max_lines = v; })));
    el.appendChild(row("Log channel chats to file", checkbox(s.log_channel_chat, (v) => { s.log_channel_chat = v; })));
    el.appendChild(row("Log private chats to file", checkbox(s.log_private_chat, (v) => { s.log_private_chat = v; })));
    el.appendChild(row("Log server/global chats to file", checkbox(s.log_server_chat, (v) => { s.log_server_chat = v; })));
    el.appendChild(hint("Chat log: <config>/voicx/chat.log (Help → Open log folder)."));
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
    el.appendChild(row("Toasts for join/leave", checkbox(s.notify_join_leave, (v) => { s.notify_join_leave = v; })));
    el.appendChild(row("Toasts for connection events", checkbox(s.notify_connection, (v) => { s.notify_connection = v; })));
    el.appendChild(row("Play sounds (join/leave/mention)", checkbox(s.play_sounds, (v) => { s.play_sounds = v; })));
    el.appendChild(row("Whisper/direct message sound", checkbox(s.whisper_sound !== false, (v) => { s.whisper_sound = v; })));
    const testBtn = document.createElement("button");
    testBtn.textContent = "Test sounds";
    testBtn.onclick = () => V().beep(660);
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
                <div id="settings-content"></div>
                <div class="settings-footer">
                    <button id="set-ok">OK</button>
                    <button id="set-cancel">Cancel</button>
                    <button id="set-apply">Apply</button>
                </div>
            </div>
        </div>`;

    const nav = overlay.querySelector(".settings-nav");
    for (const p of PAGES) {
        const item = document.createElement("div");
        item.className = "settings-nav-item";
        item.dataset.page = p.id;
        item.innerHTML = `<span class="nav-icon">${p.icon}</span><span>${p.label}</span>`;
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
}
