// clientinfo.js — right-click context menu on channel-tree users and the
// TS3-style Client Info dialog (live-refreshing).
import { getUserVolume, isUserMuted, setUserMuted, setUserVolume } from "./audio.js";
import { pickIcon } from "./image-tools.js";

const V = () => window.__voicx;

let menuEl = null;

function closeMenu() {
    if (menuEl) {
        menuEl.remove();
        menuEl = null;
    }
}

function openContextMenu(x, y, client) {
    closeMenu();
    const { $ } = V();
    const P = window.__voicxPerms;
    // (306) multi-select: with several users selected, batch actions apply
    // to all of them.
    const sel = V().state.multiSelect;
    if (sel && sel.size > 1 && sel.has(client.client_id)) {
        return openBatchMenu(x, y, [...sel]);
    }
    const muted = isUserMuted(client.unique_id);
    const volPct = Math.round(getUserVolume(client.unique_id) * 100);
    // (169-171) moderation entries are pre-gated by the caller's resolved
    // powers; the server re-checks and errors still toast.
    const mod = [];
    if (client.client_id !== V().state.myClientID) {
        mod.push(`<a data-act="poke">Poke…</a>`);
        const isContact = (V().state.settings?.contacts || []).some((c) => c.unique_id === client.unique_id);
        mod.push(`<a data-act="contact">${isContact ? "✓ " : ""}Add to contacts</a>`);
        const isBlocked = (V().state.settings?.blocked_users || []).includes(client.unique_id);
        mod.push(`<a data-act="block">${isBlocked ? "✓ " : ""}Block (hide chat + mute)</a>`);
        if (P.canKickChannel()) mod.push(`<a data-act="kick-ch">Kick from channel…</a>`);
        if (P.canKickServer()) mod.push(`<a data-act="kick-srv">Kick from server…</a>`);
        if (P.canBan()) mod.push(`<a data-act="ban">Ban…</a>`);
    }
    menuEl = document.createElement("div");
    menuEl.className = "ctx-menu";
    menuEl.innerHTML = `
        <a data-act="info">Client Info</a>
        <a data-act="pm">Send private message</a>
        <a data-act="copy">Copy unique ID</a>
        <div class="ctx-divider"></div>
        <a data-act="mute">${muted ? "✓ " : ""}Mute locally</a>
        <div class="ctx-volume">
            <span>Volume <span class="mono ctx-vol-pct">${volPct}%</span></span>
            <input type="range" min="0" max="200" value="${volPct}" />
        </div>
        ${mod.length ? `<div class="ctx-divider"></div>${mod.join("")}` : ""}`;
    menuEl.style.left = Math.min(x, window.innerWidth - 240) + "px";
    menuEl.style.top = Math.min(y, window.innerHeight - 260) + "px";
    menuEl.onclick = (e) => e.stopPropagation();

    menuEl.querySelector('[data-act="info"]').onclick = () => {
        closeMenu();
        openClientInfo(client);
    };
    menuEl.querySelector('[data-act="pm"]').onclick = () => {
        closeMenu();
        // wave 5b: open a PM tab (falls back to the plain direct-scope form).
        if (V().openPM) {
            V().openPM(client.unique_id, client.nickname);
            return;
        }
        $("chat-scope").value = "direct";
        V().setDirectTargetVisible(true);
        $("chat-target").value = client.unique_id;
        $("chat-text").focus();
    };
    menuEl.querySelector('[data-act="copy"]').onclick = () => {
        closeMenu();
        navigator.clipboard.writeText(client.unique_id).then(() => {
            V().toast("unique ID copied");
        });
    };
    menuEl.querySelector('[data-act="mute"]').onclick = async () => {
        closeMenu();
        await setUserMuted(client.unique_id, !muted);
        V().toast((muted ? "unmuted " : "muted ") + (client.nickname || client.unique_id) + " locally");
    };
    // (170) kick with reason dialog; (171) ban with duration presets.
    const pokeAct = menuEl.querySelector('[data-act="poke"]');
    if (pokeAct) pokeAct.onclick = () => {
        closeMenu();
        window.__voicxSocial.openPoke(client);
    };
    const contactAct = menuEl.querySelector('[data-act="contact"]');
    if (contactAct) contactAct.onclick = async () => {
        closeMenu();
        const s = V().state.settings;
        if ((s.contacts || []).some((c) => c.unique_id === client.unique_id)) {
            V().toast("already a contact");
            return;
        }
        s.contacts = [...(s.contacts || []), { unique_id: client.unique_id, label: "" }];
        await window.go.main.App.SaveSettings(s);
        V().toast("contact added");
    };
    const blockAct = menuEl.querySelector('[data-act="block"]');
    if (blockAct) blockAct.onclick = async () => {
        closeMenu();
        const s = V().state.settings;
        const blocked = (s.blocked_users || []).includes(client.unique_id);
        if (blocked) s.blocked_users = s.blocked_users.filter((u) => u !== client.unique_id);
        else s.blocked_users = [...(s.blocked_users || []), client.unique_id];
        await window.go.main.App.SaveSettings(s);
        V().toast(blocked ? "unblocked" : "blocked — chat hidden, voice muted locally");
    };
    const kickAct = menuEl.querySelector('[data-act="kick-ch"]');
    if (kickAct) kickAct.onclick = () => {
        closeMenu();
        reasonDialog("Kick from channel", client, (reason) =>
            window.go.main.App.KickClient(client.client_id, false, false, reason, 0));
    };
    const kickSrvAct = menuEl.querySelector('[data-act="kick-srv"]');
    if (kickSrvAct) kickSrvAct.onclick = () => {
        closeMenu();
        reasonDialog("Kick from server", client, (reason) =>
            window.go.main.App.KickClient(client.client_id, true, false, reason, 0));
    };
    const banAct = menuEl.querySelector('[data-act="ban"]');
    if (banAct) banAct.onclick = () => {
        closeMenu();
        banDialog(client);
    };
    const slider = menuEl.querySelector('.ctx-volume input');
    slider.oninput = () => {
        menuEl.querySelector(".ctx-vol-pct").textContent = slider.value + "%";
    };
    slider.onchange = async () => {
        await setUserVolume(client.unique_id, parseInt(slider.value, 10));
    };

    document.body.appendChild(menuEl);
}

// openBatchMenu is the multi-select context menu (306): actions apply to
// every selected user.
function openBatchMenu(x, y, clientIDs) {
    closeMenu();
    const P = window.__voicxPerms;
    const myID = V().state.myClientID;
    const others = clientIDs.filter((id) => id !== myID);
    menuEl = document.createElement("div");
    menuEl.className = "ctx-menu";
    const entries = [`<a data-act="count" class="ctx-head">${clientIDs.length} selected</a>`];
    entries.push(`<a data-act="mute">Mute all locally</a>`);
    if (P.canKickChannel() && others.length) entries.push(`<a data-act="kick">Kick all from channel</a>`);
    menuEl.innerHTML = entries.join("");
    menuEl.style.left = Math.min(x, window.innerWidth - 240) + "px";
    menuEl.style.top = Math.min(y, window.innerHeight - 200) + "px";
    menuEl.onclick = (e) => e.stopPropagation();

    menuEl.querySelector('[data-act="mute"]').onclick = async () => {
        closeMenu();
        for (const id of clientIDs) {
            const c = V().state.clients.find((x) => x.client_id === id);
            if (c) await setUserMuted(c.unique_id, true);
        }
        V().toast("muted " + clientIDs.length + " users locally");
    };
    const kick = menuEl.querySelector('[data-act="kick"]');
    if (kick) kick.onclick = async () => {
        closeMenu();
        for (const id of others) {
            await window.go.main.App.KickClient(id, false, false, "", 0);
        }
        V().toast("kicked " + others.length + " users");
    };
    document.body.appendChild(menuEl);
}

// inboundAudioByPublisher maps a publisher's client ID -> its inbound-rtp
// audio stat. The SFU gives every publisher its own MediaStream and sets the
// track ID to the publisher's client ID, so a stat is attributable via
// trackIdentifier (or the legacy "track" stat it references) (57).
function inboundAudioByPublisher(stats) {
    const byID = new Map();
    stats.forEach((r) => byID.set(r.id, r));
    const map = new Map();
    stats.forEach((r) => {
        if (r.type !== "inbound-rtp") return;
        if (r.kind !== "audio" && r.mediaType !== "audio") return;
        const tid = r.trackIdentifier || (r.trackId ? byID.get(r.trackId)?.trackIdentifier : "");
        if (tid) map.set(String(tid), r);
    });
    return map;
}

// refreshVoiceStats fills the Voice section from RTCPeerConnection.getStats()
// for the ONE client the dialog was opened for (57): the receive stream
// published by that client, or — when the dialog is our own — the outgoing
// stream as the server's RTCP receiver reports describe it.
async function refreshVoiceStats(overlay, client) {
    const { state } = V();
    const setVal = (f, text, cls) => {
        const el = overlay.querySelector(`[data-f="${f}"]`);
        if (el) {
            el.textContent = text;
            if (cls) el.className = "ci-val " + cls;
        }
    };
    const blank = (loss) => {
        setVal("loss", loss);
        setVal("jitter", "—");
        setVal("jbd", "—");
        setVal("conceal", "—");
        setVal("packets", "—");
        setVal("level", "—");
    };
    if (!state.pc) {
        blank("— (no voice)");
        return;
    }
    try {
        const stats = await state.pc.getStats();
        if (client.client_id === state.myClientID) {
            refreshOwnVoiceStats(stats, setVal, blank);
            return;
        }
        const byPub = inboundAudioByPublisher(stats);
        let inbound = byPub.get(String(client.client_id));
        // fall back to the track registry: a reconnect can leave the dialog's
        // client ID stale while the attributed track is still live.
        if (!inbound && client.unique_id) {
            for (const [trackID, u] of state.trackUsers || []) {
                if (u.unique_id === client.unique_id && byPub.has(String(trackID))) {
                    inbound = byPub.get(String(trackID));
                    break;
                }
            }
        }
        if (!inbound) {
            blank("— no stream from this user");
            return;
        }
        const lost = inbound.packetsLost || 0;
        const recv = inbound.packetsReceived || 0;
        const total = lost + recv;
        setVal("loss", (total > 0 ? (lost / total * 100).toFixed(1) : "0.0") + " %");
        setVal("jitter", ((inbound.jitter || 0) * 1000).toFixed(1) + " ms");
        // (58) jitterBufferDelay/jitterBufferTargetDelay are CUMULATIVE sums of
        // seconds: the average delay is the sum over jitterBufferEmittedCount.
        const emitted = inbound.jitterBufferEmittedCount || 0;
        if (inbound.jitterBufferDelay != null && emitted > 0) {
            let txt = (inbound.jitterBufferDelay / emitted * 1000).toFixed(0) + " ms avg";
            if (inbound.jitterBufferTargetDelay != null) {
                txt += " (target " + (inbound.jitterBufferTargetDelay / emitted * 1000).toFixed(0) + " ms)";
            }
            setVal("jbd", txt);
        } else {
            setVal("jbd", "—");
        }
        // (58) concealment is what the loss actually cost: samples the jitter
        // buffer had to invent.
        const samples = inbound.totalSamplesReceived || 0;
        setVal("conceal", samples > 0
            ? ((inbound.concealedSamples || 0) / samples * 100).toFixed(2) + " % (" + (inbound.concealmentEvents || 0) + " events)"
            : "—");
        setVal("packets", recv + " recv / " + lost + " lost");
        setVal("level", inbound.audioLevel != null ? (inbound.audioLevel * 100).toFixed(0) + " %" : "—");
    } catch { /* stats unavailable */ }
}

// refreshOwnVoiceStats fills the Voice section for our own row: there is no
// inbound stream for oneself, so loss/jitter come from the server's receiver
// reports (remote-inbound-rtp) and the level from the local media source.
function refreshOwnVoiceStats(stats, setVal, blank) {
    let outbound = null, remoteIn = null, source = null;
    stats.forEach((r) => {
        const audio = r.kind === "audio" || r.mediaType === "audio";
        if (r.type === "outbound-rtp" && audio && !outbound) outbound = r;
        if (r.type === "remote-inbound-rtp" && audio && !remoteIn) remoteIn = r;
        if (r.type === "media-source" && audio && !source) source = r;
    });
    if (!outbound && !source) {
        blank("— not publishing");
        return;
    }
    if (remoteIn) {
        const lost = remoteIn.packetsLost || 0;
        const sent = outbound?.packetsSent || 0;
        setVal("loss", sent > 0 ? (lost / sent * 100).toFixed(1) + " % (reported by server)" : "—");
        setVal("jitter", ((remoteIn.jitter || 0) * 1000).toFixed(1) + " ms");
    } else {
        setVal("loss", "— (no receiver report yet)");
        setVal("jitter", "—");
    }
    setVal("jbd", "— (outgoing)");
    setVal("conceal", "— (outgoing)");
    setVal("packets", (outbound?.packetsSent || 0) + " sent");
    const level = source?.audioLevel ?? outbound?.audioLevel;
    setVal("level", level != null ? (level * 100).toFixed(0) + " %" : "—");
}

// --- Client Info dialog --------------------------------------------------------

// humanBytes renders a byte count in B/KiB/MiB (shared with files-ui).
export function humanBytes(n) {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KiB";
    return (n / 1024 / 1024).toFixed(1) + " MiB";
}

function humanDuration(sec) {
    sec = Math.max(0, Math.floor(sec));
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    if (h > 0) return `${h}h ${m}m ${s}s`;
    if (m > 0) return `${m}m ${s}s`;
    return `${s}s`;
}

function openClientInfo(client) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg client-info">
            <div class="ci-title">
                <span>Connection Info</span>
                <span class="ci-nick"></span>
            </div>
            <div class="ci-grid">
                <div class="ci-label">Client name</div><div class="ci-val" data-f="nick"></div>
                <div class="ci-label">Unique ID</div>
                <div class="ci-val">
                    <span class="mono" data-f="uid"></span>
                    <button class="ci-copy" title="copy">⧉</button>
                </div>
                <div class="ci-label">Connection time</div><div class="ci-val" data-f="conn"></div>
                <div class="ci-label">Idle time</div><div class="ci-val" data-f="idle"></div>
                <div class="ci-label">Ping</div><div class="ci-val" data-f="ping"></div>
                <div class="ci-label">Client address</div><div class="ci-val" data-f="addr"></div>
                <div class="ci-label">Transfer in</div><div class="ci-val" data-f="bin"></div>
                <div class="ci-label">Transfer out</div><div class="ci-val" data-f="bout"></div>
            </div>
            <div class="ci-voice-head">Voice</div>
            <div class="ci-grid">
                <div class="ci-label">Packet loss</div><div class="ci-val" data-f="loss"></div>
                <div class="ci-label">Jitter</div><div class="ci-val" data-f="jitter"></div>
                <div class="ci-label">Jitter buffer</div><div class="ci-val" data-f="jbd"></div>
                <div class="ci-label">Concealment</div><div class="ci-val" data-f="conceal"></div>
                <div class="ci-label">Packets</div><div class="ci-val" data-f="packets"></div>
                <div class="ci-label">Audio level</div><div class="ci-val" data-f="level"></div>
            </div>
            <div class="dlg-buttons"><button class="dlg-ok">Close</button></div>
        </div>`;

    overlay.querySelector(".ci-nick").textContent = client.nickname || client.unique_id;
    overlay.querySelector(".ci-copy").onclick = () => {
        navigator.clipboard.writeText(client.unique_id).then(() => V().toast("unique ID copied"));
    };
    // (314) avatar full view on click; (325) click-to-copy chips.
    const card = document.querySelector(`#client-card .card-avatar img`);
    if (card) {
        card.style.cursor = "zoom-in";
        card.onclick = () => window.__voicxSocial.avatarLightbox(card.src);
    }
    // (315) local per-user note editor.
    const noteRow = document.createElement("div");
    noteRow.className = "ci-note";
    noteRow.innerHTML = `
        <div class="ci-label">Local note</div>
        <div class="ci-val"><input class="dlg-input ci-note-input" placeholder="only you see this…" /></div>`;
    const noteInput = noteRow.querySelector(".ci-note-input");
    noteInput.value = window.__voicxSocial.userNote(client.unique_id);
    noteInput.onchange = () => {
        window.__voicxSocial.saveUserNote(client.unique_id, noteInput.value.trim())
            .then(() => V().toast("note saved"));
    };
    overlay.querySelector(".ci-grid").appendChild(noteRow);
    // (315) server groups of this user (from wave-6b membership data).
    const groups = (V().state.groupByUID?.get(client.unique_id) || []).map((g) => g.name).join(", ");
    if (groups) {
        const gRow = document.createElement("div");
        gRow.className = "ci-note";
        gRow.innerHTML = `<div class="ci-label">Groups</div><div class="ci-val">${window.__voicxSocial.esc(groups)}</div>`;
        overlay.querySelector(".ci-grid").appendChild(gRow);
    }

    const setVal = (f, text, cls) => {
        const el = overlay.querySelector(`[data-f="${f}"]`);
        el.textContent = text;
        if (cls) el.className = "ci-val " + cls;
    };

    const refresh = async () => {
        let info;
        try {
            info = await window.go.main.App.GetClientInfo(client.client_id);
        } catch {
            return; // transient; try again next tick
        }
        setVal("nick", info.nickname);
        overlay.querySelector('[data-f="uid"]').textContent = info.unique_id;
        setVal("conn", humanDuration(Date.now() / 1000 - info.connected_at));
        setVal("idle", humanDuration(info.idle_seconds));
        setVal("ping", info.ping_ms >= 0 ? info.ping_ms + " ms" : "unknown");
        if (info.ip) {
            setVal("addr", info.ip + ":" + info.port);
        } else {
            setVal("addr", "hidden — requires b_client_remoteaddress_view", "ci-muted");
        }
        setVal("bin", humanBytes(info.bytes_in));
        setVal("bout", humanBytes(info.bytes_out));

        // (57/58) Voice stats from getStats(), scoped to this dialog's client.
        refreshVoiceStats(overlay, client);
    };

    let refreshTimer = null;
    const close = () => {
        if (refreshTimer) {
            clearInterval(refreshTimer);
            refreshTimer = null;
        }
        overlay.remove();
    };
    overlay.querySelector(".dlg-ok").onclick = close;
    overlay.onclick = (e) => { if (e.target === overlay) close(); };

    document.body.appendChild(overlay);
    refresh();
    refreshTimer = setInterval(refresh, 2000);
}

// --- moderation dialogs (170/171) -------------------------------------------

// reasonDialog prompts for a kick reason and invokes cb(reason).
function reasonDialog(title, client, cb) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3></h3>
            <div class="dlg-text kick-target"></div>
            <input type="text" class="dlg-input reason" placeholder="reason (optional)" />
            <div class="dlg-buttons">
                <button class="dlg-ok">Kick</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    overlay.querySelector("h3").textContent = title;
    overlay.querySelector(".kick-target").textContent = client.nickname || client.unique_id;
    overlay.querySelector(".dlg-ok").onclick = async () => {
        const reason = overlay.querySelector(".reason").value.trim();
        overlay.remove();
        const err = await cb(reason);
        if (err) V().toast("kick failed: " + err, "warn");
    };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    overlay.querySelector(".reason").focus();
}

// banDialog prompts for reason + duration preset and bans the client (171).
const BAN_DURATIONS = [
    { label: "5 minutes", seconds: 300 },
    { label: "1 hour", seconds: 3600 },
    { label: "1 day", seconds: 86400 },
    { label: "permanent", seconds: 0 },
];

function banDialog(client) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3>Ban client</h3>
            <div class="dlg-text kick-target"></div>
            <input type="text" class="dlg-input reason" placeholder="reason (optional)" />
            <label class="dlg-label">Duration</label>
            <select class="dlg-input duration">
                ${BAN_DURATIONS.map((d, i) => `<option value="${i}">${d.label}</option>`).join("")}
            </select>
            <div class="dlg-buttons">
                <button class="dlg-ok danger-btn">Ban</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    overlay.querySelector(".kick-target").textContent = client.nickname || client.unique_id;
    overlay.querySelector(".dlg-ok").onclick = async () => {
        const reason = overlay.querySelector(".reason").value.trim();
        const dur = BAN_DURATIONS[parseInt(overlay.querySelector(".duration").value, 10) || 0];
        overlay.remove();
        const err = await window.go.main.App.KickClient(client.client_id, true, true, reason, dur.seconds);
        if (err) V().toast("ban failed: " + err, "warn");
        else V().toast("banned " + (client.nickname || client.unique_id) + " (" + dur.label + ")");
    };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    overlay.querySelector(".reason").focus();
}

// --- Channel context menu & edit dialog (24) ---------------------------------

// Quality presets for the channel edit dialog. bitrate is in bits/s;
// "music channel" = stereo && bitrate >= 96000 (server-side semantics).
const QUALITY_PRESETS = {
    voice: { label: "Voice (32 kbps)", bitrate: 32000, fec: false, dtx: false, stereo: false },
    hq: { label: "HQ Voice (64 kbps)", bitrate: 64000, fec: true, dtx: false, stereo: false },
    music: { label: "Music (128 kbps stereo)", bitrate: 128000, fec: true, dtx: false, stereo: true },
};

function openChannelMenu(x, y, channel) {
    closeMenu();
    const isCurrent = channel.ChannelID === V().state.myChannelID;
    const isSubscribed = !!window.__voicxChat?.isSubscribed?.(channel.ChannelID);
    // (320) recent channels for quick rejoin.
    const recent = (V().recentChannels ? V().recentChannels() : [])
        .map((id) => V().state.channels.find((c) => c.ChannelID === id))
        .filter(Boolean)
        .filter((c) => c.ChannelID !== channel.ChannelID)
        .map((c) => `<a data-act="recent-${c.ChannelID}">↩ # ${c.Name}</a>`);
    menuEl = document.createElement("div");
    menuEl.className = "ctx-menu";
    menuEl.innerHTML = `
        <a data-act="open-chat">Open chat tab</a>
        <a data-act="subscription" class="${isCurrent ? "disabled" : ""}">${isCurrent ? "✓ Joined (always subscribed)" : isSubscribed ? "✓ Unsubscribe" : "Subscribe"}</a>
        <div class="ctx-divider"></div>
        <a data-act="edit">Edit channel</a>
        <a data-act="notify">Notifications…</a>
        <a data-act="create-sub">Create sub-channel…</a>
        <a data-act="copy-id">Copy channel ID</a>
        <a data-act="copy-addr">Copy server address</a>
        ${recent.length ? `<div class="ctx-divider"></div>${recent.join("")}` : ""}
        <div class="ctx-divider"></div>
        <a data-act="delete" class="ctx-danger">Delete channel…</a>`;
    menuEl.style.left = Math.min(x, window.innerWidth - 240) + "px";
    menuEl.style.top = Math.min(y, window.innerHeight - 260) + "px";
    menuEl.onclick = (e) => e.stopPropagation();
    menuEl.querySelector('[data-act="open-chat"]').onclick = () => {
        closeMenu();
        window.__voicxChat?.openChannelTab?.(channel.ChannelID);
    };
    menuEl.querySelector('[data-act="subscription"]').onclick = () => {
        closeMenu();
        if (!isCurrent) window.__voicxChat?.setChannelSubscription?.(channel.ChannelID, !isSubscribed);
    };
    menuEl.querySelector('[data-act="edit"]').onclick = () => {
        closeMenu();
        openChannelEdit(channel);
    };
    menuEl.querySelector('[data-act="create-sub"]').onclick = () => {
        closeMenu();
        openChannelCreate(channel);
    };
    menuEl.querySelector('[data-act="notify"]').onclick = () => {
        closeMenu();
        openChannelNotify(channel);
    };
    menuEl.querySelector('[data-act="copy-id"]').onclick = () => {
        closeMenu();
        navigator.clipboard.writeText(String(channel.ChannelID)).then(() => V().toast("channel ID copied"));
    };
    menuEl.querySelector('[data-act="copy-addr"]').onclick = () => {
        closeMenu();
        navigator.clipboard.writeText(V().state.lastConnect?.addr || "").then(() => V().toast("server address copied"));
    };
    for (const a of menuEl.querySelectorAll('[data-act^="recent-"]')) {
        a.onclick = () => {
            closeMenu();
            window.go.main.App.JoinChannel(Number(a.dataset.act.slice(7)));
        };
    }
    menuEl.querySelector('[data-act="delete"]').onclick = () => {
        closeMenu();
        confirmChannelDelete(channel);
    };
    document.body.appendChild(menuEl);
}

// (320) recentChannels returns the current server's recent channel IDs.
function recentChannels() {
    if (!V().state.lastConnect || !V().state.settings?.recent_channels) return [];
    return V().state.settings.recent_channels[V().state.lastConnect.addr] || [];
}

// openChannelNotify edits the per-channel notification overrides
// (386/387/389): inherit/on/off per event class, mute, and watch threshold.
function openChannelNotify(channel) {
    const N = window.__voicxNotify;
    const ov = N.channelOverride(channel.ChannelID) || {};
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    const sel = (cls, val) => `
        <select class="dlg-input ${cls}">
            ${["inherit", "on", "off"].map((v) => `<option value="${v}" ${v === (val || "inherit") ? "selected" : ""}>${v}</option>`).join("")}
        </select>`;
    overlay.innerHTML = `
        <div class="dlg">
            <h3>Notifications: #${window.__voicxSocial.esc(channel.Name)}</h3>
            <label class="dlg-label">Messages (386)</label>${sel("cn-messages", ov.messages)}
            <label class="dlg-label">Mentions & keywords</label>${sel("cn-mentions", ov.mentions)}
            <label class="dlg-label">Joins & leaves</label>${sel("cn-joins", ov.joins)}
            <label class="dlg-label"><input type="checkbox" class="cn-muted" ${ov.muted ? "checked" : ""} /> Mute channel entirely (387)</label>
            <label class="dlg-label">Watch: toast when user count reaches (0 = off) (389)</label>
            <input type="number" class="dlg-input cn-watch" min="0" value="${ov.watch_threshold || 0}" />
            <div class="dlg-buttons">
                <button class="dlg-ok">Save</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    const q = (s) => overlay.querySelector(s);
    q(".dlg-ok").onclick = async () => {
        await N.saveChannelOverride(channel.ChannelID, {
            messages: q(".cn-messages").value === "inherit" ? "" : q(".cn-messages").value,
            mentions: q(".cn-mentions").value === "inherit" ? "" : q(".cn-mentions").value,
            joins: q(".cn-joins").value === "inherit" ? "" : q(".cn-joins").value,
            muted: q(".cn-muted").checked,
            watch_threshold: parseInt(q(".cn-watch").value, 10) || 0,
        });
        overlay.remove();
        V().toast("channel notifications saved");
    };
    q(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
}

// countSubtree returns how many descendants a channel has (167 warning).
function countSubtree(channelID) {
    const byParent = new Map();
    for (const ch of V().state.channels) {
        const p = ch.ParentID || 0;
        if (!byParent.has(p)) byParent.set(p, []);
        byParent.get(p).push(ch.ChannelID);
    }
    let n = 0;
    const walk = (id) => {
        for (const child of byParent.get(id) || []) {
            n++;
            walk(child);
        }
    };
    walk(channelID);
    return n;
}

// confirmChannelDelete warns about the subtree before deleting (167).
function confirmChannelDelete(channel) {
    const children = countSubtree(channel.ChannelID);
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg">
            <h3>Delete channel</h3>
            <div class="dlg-text">
                <p>Delete <b class="ch-del-name"></b>?</p>
                ${children ? `<p class="warn">⚠ This channel has ${children} sub-channel(s) — they are deleted too.</p>` : ""}
            </div>
            <div class="dlg-buttons">
                <button class="dlg-ok danger-btn">Delete</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    overlay.querySelector(".ch-del-name").textContent = channel.Name;
    overlay.querySelector(".dlg-ok").onclick = async () => {
        overlay.remove();
        const err = await window.go.main.App.DeleteChannel(channel.ChannelID);
        if (err) V().toast("delete failed: " + err, "warn");
    };
    overlay.querySelector(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
}

// openChannelCreate is the full create dialog (164): name, parent, type,
// topic, max clients, password, needed join power, Opus preset.
function openChannelCreate(parent) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    overlay.innerHTML = `
        <div class="dlg channel-edit">
            <h3>Create channel</h3>
            <label class="dlg-label">Name</label>
            <input type="text" class="dlg-input cc-name" />
            <label class="dlg-label">Parent</label>
            <div class="dlg-text cc-parent pm-dim"></div>
            <label class="dlg-label">Type</label>
            <select class="dlg-input cc-type">
                <option value="0">temporary (auto-deleted when empty)</option>
                <option value="1">semi-permanent</option>
                <option value="2" selected>permanent</option>
            </select>
            <label class="dlg-label">Topic</label>
            <input type="text" class="dlg-input cc-topic" />
            <label class="dlg-label">Max clients (0 = unlimited)</label>
            <input type="number" class="dlg-input cc-maxclients" min="0" value="0" />
            <label class="dlg-label">Password (empty = none)</label>
            <input type="password" class="dlg-input cc-password" />
            <label class="dlg-label" title="joiners need at least this i_channel_join_power">Needed join power</label>
            <input type="number" class="dlg-input cc-joinpower" min="0" value="0" />
            <label class="dlg-label">Quality preset</label>
            <select class="dlg-input cc-preset">
                <option value="voice">${QUALITY_PRESETS.voice.label}</option>
                <option value="hq">${QUALITY_PRESETS.hq.label}</option>
                <option value="music">${QUALITY_PRESETS.music.label}</option>
            </select>
            <div class="dlg-buttons">
                <button class="dlg-ok">Create</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;
    const q = (sel) => overlay.querySelector(sel);
    q(".cc-parent").textContent = parent ? "under " + parent.Name : "(root)";
    q(".dlg-ok").onclick = async () => {
        const name = q(".cc-name").value.trim();
        if (!name) {
            V().toast("channel name is required", "warn");
            return;
        }
        const p = QUALITY_PRESETS[q(".cc-preset").value];
        const err = await window.go.main.App.CreateChannel(
            name,
            q(".cc-topic").value,
            parent ? parent.ChannelID : 0,
            parseInt(q(".cc-type").value, 10),
            parseInt(q(".cc-maxclients").value, 10) || 0,
            q(".cc-password").value,
            parseInt(q(".cc-joinpower").value, 10) || 0,
            p.bitrate, p.fec, p.dtx, p.stereo);
        overlay.remove();
        if (err) V().toast("create failed: " + err, "warn");
        // Permission denials arrive as servererror toasts; on success the
        // server broadcasts channel_created and the tree refreshes.
    };
    q(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    q(".cc-name").focus();
}

function matchPreset(bitrate, fec, dtx, stereo) {
    for (const [key, p] of Object.entries(QUALITY_PRESETS)) {
        if (p.bitrate === bitrate && p.fec === fec && p.dtx === dtx && p.stereo === stereo) return key;
    }
    return "custom";
}

// subtreeOf returns a channel and every descendant of it (168): a channel
// cannot become its own ancestor.
function subtreeOf(channelID) {
    const out = new Set([channelID]);
    let grew = true;
    while (grew) {
        grew = false;
        for (const c of V().state.channels) {
            if (!out.has(c.ChannelID) && out.has(c.ParentID || 0)) {
                out.add(c.ChannelID);
                grew = true;
            }
        }
    }
    return out;
}

function openChannelEdit(channel) {
    const overlay = document.createElement("div");
    overlay.className = "dlg-overlay";
    // (156 honest UI): the server does not auto-assign a channel-creator
    // group; show the chip when the caller holds channel-modify rights.
    const adminChip = window.__voicxPerms.canPermManage() || V().state.isAdmin ||
        (V().state.myPerms?.get("b_channel_modify")?.value > 0)
        ? `<span class="group-chip ce-admin-chip" title="you hold b_channel_modify here">channel admin</span>`
        : "";
    overlay.innerHTML = `
        <div class="dlg channel-edit">
            <h3>Edit channel</h3>
            <div class="ce-name"></div>
            ${adminChip}
            <label class="dlg-label">Topic</label>
            <input type="text" class="dlg-input ce-topic" />
            <label class="dlg-label">Description</label>
            <textarea class="dlg-input ce-desc" rows="2"></textarea>
            <label class="dlg-label">Max clients (0 = unlimited)</label>
            <input type="number" class="dlg-input ce-maxclients" min="0" />
            <label class="dlg-label">Slow mode seconds (0 = off)</label>
            <input type="number" class="dlg-input ce-slowmode" min="0" title="minimum seconds between messages; holders of b_chat_slowmode_bypass are exempt" />
            <label class="dlg-label">Quality preset</label>
            <select class="dlg-input ce-preset">
                <option value="voice">${QUALITY_PRESETS.voice.label}</option>
                <option value="hq">${QUALITY_PRESETS.hq.label}</option>
                <option value="music">${QUALITY_PRESETS.music.label}</option>
                <option value="custom">Custom</option>
            </select>
            <label class="dlg-label">Opus bitrate (bits/s, 0 = default 32000)</label>
            <input type="number" class="dlg-input ce-bitrate" min="0" step="1000" />
            <div class="ce-flags">
                <label><input type="checkbox" class="ce-fec" /> FEC</label>
                <label><input type="checkbox" class="ce-dtx" /> DTX</label>
                <label><input type="checkbox" class="ce-stereo" /> Stereo</label>
            </div>
            <label class="dlg-label">Needed join power (0 = open to everyone)</label>
            <input type="number" class="dlg-input ce-joinpower" min="0" title="a client needs i_channel_join_power at or above this to join; you cannot raise it above your own join power (160)" />
            <label class="dlg-label">Parent channel (168)</label>
            <select class="dlg-input ce-parent">
                <option value="0">— top level —</option>
            </select>
            <label class="dlg-label">Sort index (163; lower sorts first among siblings)</label>
            <input type="number" class="dlg-input ce-order" />
            <label class="dlg-label"><input type="checkbox" class="ce-inherit" /> inherit the parent's channel permissions and join power (157)</label>
            <label class="dlg-label">Channel icon</label>
            <div class="ce-icon-row">
                <button class="icon-btn ce-icon-upload" title="Upload icon (compressed, max 1024px)">⬆ Upload…</button>
                <select class="dlg-input ce-icon-copy">
                    <option value="0">or reuse an existing channel icon…</option>
                </select>
            </div>
            <div class="dlg-buttons">
                <button class="dlg-ok">Save</button>
                <button class="dlg-cancel">Cancel</button>
            </div>
        </div>`;

    const q = (sel) => overlay.querySelector(sel);
    q(".ce-name").textContent = channel.Name;
    q(".ce-topic").value = channel.Topic || "";
    q(".ce-desc").value = channel.Description || "";
    q(".ce-maxclients").value = channel.MaxClients || 0;
    q(".ce-slowmode").value = channel.SlowModeSeconds || 0;
    // OpusBitrate 0 means the server default (32000); show the effective value.
    q(".ce-bitrate").value = channel.OpusBitrate || 32000;
    q(".ce-fec").checked = !!channel.OpusFEC;
    q(".ce-dtx").checked = !!channel.OpusDTX;
    q(".ce-stereo").checked = !!channel.OpusStereo;
    q(".ce-preset").value = matchPreset(
        channel.OpusBitrate || 32000, !!channel.OpusFEC, !!channel.OpusDTX, !!channel.OpusStereo);

    // (160/163/168/157) tree fields. The parent list omits the channel itself
    // and its subtree: the server refuses a cycle, and offering one is a
    // guaranteed error dialog.
    q(".ce-joinpower").value = channel.NeededJoinPower || 0;
    q(".ce-order").value = channel.OrderIndex || 0;
    q(".ce-inherit").checked = !!channel.InheritPermissions;
    const banned = subtreeOf(channel.ChannelID);
    const parentSel = q(".ce-parent");
    for (const c of V().state.channels) {
        if (banned.has(c.ChannelID)) continue;
        const opt = document.createElement("option");
        opt.value = c.ChannelID;
        opt.textContent = "# " + c.Name;
        parentSel.appendChild(opt);
    }
    parentSel.value = String(channel.ParentID || 0);

    q(".ce-preset").onchange = () => {
        const p = QUALITY_PRESETS[q(".ce-preset").value];
        if (!p) return; // custom: leave the fields editable as-is
        q(".ce-bitrate").value = p.bitrate;
        q(".ce-fec").checked = p.fec;
        q(".ce-dtx").checked = p.dtx;
        q(".ce-stereo").checked = p.stereo;
    };
    const markCustom = () => { q(".ce-preset").value = "custom"; };
    q(".ce-bitrate").oninput = markCustom;
    q(".ce-fec").onchange = markCustom;
    q(".ce-dtx").onchange = markCustom;
    q(".ce-stereo").onchange = markCustom;

    // (271) channel icon: upload (compressed) or reuse from another channel.
    const copySel = q(".ce-icon-copy");
    for (const c of V().state.channels) {
        if (c.HasIcon && c.ChannelID !== channel.ChannelID) {
            const opt = document.createElement("option");
            opt.value = c.ChannelID;
            opt.textContent = "# " + c.Name;
            copySel.appendChild(opt);
        }
    }
    q(".ce-icon-upload").onclick = async () => {
        const img = await pickIcon();
        if (!img) return;
        const err = await window.go.main.App.ChannelIconSet(channel.ChannelID, img.dataBase64, 0);
        if (err) V().toast("icon failed: " + err, "warn");
        else V().toast("channel icon updated");
    };
    copySel.onchange = async () => {
        const fromID = parseInt(copySel.value, 10);
        copySel.value = "0";
        if (!fromID) return;
        const err = await window.go.main.App.ChannelIconSet(channel.ChannelID, "", fromID);
        if (err) V().toast("icon copy failed: " + err, "warn");
        else V().toast("channel icon reused");
    };

    q(".dlg-ok").onclick = async () => {
        const err = await window.go.main.App.ChannelEdit(
            channel.ChannelID,
            q(".ce-topic").value,
            parseInt(q(".ce-maxclients").value, 10) || 0,
            parseInt(q(".ce-bitrate").value, 10) || 0,
            q(".ce-fec").checked,
            q(".ce-dtx").checked,
            q(".ce-stereo").checked,
            q(".ce-desc").value,
            parseInt(q(".ce-slowmode").value, 10) || 0);

        // Only the tree fields the user actually moved are sent: a parent_id
        // on the wire drops the server's whole permission cache, so an
        // untouched re-parent must not ride along with a topic edit.
        const joinPower = parseInt(q(".ce-joinpower").value, 10) || 0;
        const orderIndex = parseInt(q(".ce-order").value, 10) || 0;
        const parentID = parseInt(parentSel.value, 10) || 0;
        const inherit = q(".ce-inherit").checked;
        const fields = [];
        if (joinPower !== (channel.NeededJoinPower || 0)) fields.push("join_power");
        if (orderIndex !== (channel.OrderIndex || 0)) fields.push("order");
        if (parentID !== (channel.ParentID || 0)) fields.push("parent");
        if (inherit !== !!channel.InheritPermissions) fields.push("inherit");
        let treeErr = "";
        if (fields.length) {
            treeErr = await window.go.main.App.ChannelEditTree(
                channel.ChannelID, fields.join(","), joinPower, orderIndex, parentID, inherit);
        }
        overlay.remove();
        if (err) V().sysMsg("channel edit failed: " + err);
        if (treeErr) V().sysMsg("channel edit failed: " + treeErr);
        // On success the server broadcasts channel_updated, which refreshes
        // the tree (main.js).
    };
    q(".dlg-cancel").onclick = () => overlay.remove();
    overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
    document.body.appendChild(overlay);
    q(".ce-topic").focus();
}

// --- wiring -------------------------------------------------------------------

export function initClientInfo() {
    const tree = document.getElementById("channel-tree");
    // (164) root channel creation via the sidebar + button.
    document.getElementById("channel-create-btn").onclick = (e) => {
        e.stopPropagation();
        openChannelCreate(null);
    };
    tree.addEventListener("contextmenu", (e) => {
        e.preventDefault();
        const row = e.target.closest(".client");
        if (row && row.dataset.clid) {
            const client = V().state.clients.find((c) => c.client_id === row.dataset.clid);
            if (client) openContextMenu(e.clientX, e.clientY, client);
            return;
        }
        const chRow = e.target.closest(".channel");
        if (!chRow || !chRow.dataset.chid) return;
        const channel = V().state.channels.find((c) => c.ChannelID === Number(chRow.dataset.chid));
        if (channel) openChannelMenu(e.clientX, e.clientY, channel);
    });
    document.addEventListener("click", closeMenu);
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") closeMenu();
    });
}
