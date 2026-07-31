// clientinfo.js — right-click context menu on channel-tree users and the
// TS3-style Client Info dialog (live-refreshing).
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
    menuEl = document.createElement("div");
    menuEl.className = "ctx-menu";
    menuEl.innerHTML = `
        <a data-act="info">Client Info</a>
        <a data-act="pm">Send private message</a>
        <a data-act="copy">Copy unique ID</a>`;
    menuEl.style.left = Math.min(x, window.innerWidth - 220) + "px";
    menuEl.style.top = Math.min(y, window.innerHeight - 140) + "px";
    menuEl.onclick = (e) => e.stopPropagation();

    menuEl.querySelector('[data-act="info"]').onclick = () => {
        closeMenu();
        openClientInfo(client);
    };
    menuEl.querySelector('[data-act="pm"]').onclick = () => {
        closeMenu();
        $("chat-scope").value = "direct";
        $("chat-target").classList.remove("hidden");
        $("chat-target").value = client.unique_id;
        $("chat-text").focus();
    };
    menuEl.querySelector('[data-act="copy"]').onclick = () => {
        closeMenu();
        navigator.clipboard.writeText(client.unique_id).then(() => {
            V().toast("unique ID copied");
        });
    };

    document.body.appendChild(menuEl);
}

// --- Client Info dialog --------------------------------------------------------

function humanBytes(n) {
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

let refreshTimer = null;

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
            <div class="dlg-buttons"><button class="dlg-ok">Close</button></div>
        </div>`;

    overlay.querySelector(".ci-nick").textContent = client.nickname || client.unique_id;
    overlay.querySelector(".ci-copy").onclick = () => {
        navigator.clipboard.writeText(client.unique_id).then(() => V().toast("unique ID copied"));
    };

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
    };

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

// --- wiring -------------------------------------------------------------------

export function initClientInfo() {
    const tree = document.getElementById("channel-tree");
    tree.addEventListener("contextmenu", (e) => {
        e.preventDefault();
        const row = e.target.closest(".client");
        if (!row || !row.dataset.clid) return;
        const client = V().state.clients.find((c) => c.client_id === row.dataset.clid);
        if (!client) return;
        openContextMenu(e.clientX, e.clientY, client);
    });
    document.addEventListener("click", closeMenu);
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") closeMenu();
    });
}
