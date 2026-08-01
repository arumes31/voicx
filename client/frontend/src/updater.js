// updater.js — Check for updates modal + startup auto-check.
const V = () => window.__voicx;

let modal = null;

function closeModal() {
    if (modal) {
        modal.remove();
        modal = null;
    }
}

function showUpdateModal() {
    closeModal();
    modal = document.createElement("div");
    modal.className = "dlg-overlay";
    modal.innerHTML = `
        <div class="dlg dlg-wide update-dlg">
            <h3>Check for updates</h3>
            <div class="upd-current mono"></div>
            <div class="upd-status"></div>
            <div class="upd-progress hidden">
                <div class="upd-bar"><div class="upd-fill"></div></div>
                <div class="upd-pct mono"></div>
            </div>
            <div class="dlg-buttons">
                <button class="upd-update hidden">Update now</button>
                <button class="upd-close">Close</button>
            </div>
        </div>`;
    modal.querySelector(".upd-close").onclick = closeModal;
    modal.onclick = (e) => { if (e.target === modal) closeModal(); };
    document.body.appendChild(modal);
    return modal;
}

async function runCheck(m) {
    const status = m.querySelector(".upd-status");
    const cur = m.querySelector(".upd-current");
    try {
        const info = await window.go.main.App.IdentityInfo();
        cur.textContent = "current build: " + (window.__voicx.state.clientVersion || "dev");
        void info;
    } catch { /* best effort */ }
    status.textContent = "checking for updates…";

    let info;
    try {
        info = await window.go.main.App.CheckForUpdate();
    } catch (e) {
        status.textContent = "check failed: " + e;
        status.classList.add("warn");
        return;
    }
    if (!info.available) {
        status.textContent = "✓ up to date (" + (info.version || "dev") + ")";
        return;
    }

    status.innerHTML = `update available: <b>${info.version}</b> (${(info.size / 1024 / 1024).toFixed(1)} MiB)`;
    const btn = m.querySelector(".upd-update");
    btn.classList.remove("hidden");
    btn.onclick = () => startDownload(m, info);
}

async function startDownload(m, info) {
    const status = m.querySelector(".upd-status");
    const btn = m.querySelector(".upd-update");
    const prog = m.querySelector(".upd-progress");
    btn.disabled = true;
    prog.classList.remove("hidden");
    status.textContent = "downloading…";

    const fill = m.querySelector(".upd-fill");
    const pct = m.querySelector(".upd-pct");
    const onProgress = (p) => {
        if (p >= 0) {
            fill.style.width = p + "%";
            pct.textContent = p + "%";
        }
    };
    const unsub = window.runtime.EventsOn("update_progress", onProgress);

    try {
        const err = await window.go.main.App.DownloadAndApply(info);
        unsub();
        if (err) {
            status.textContent = err;
            status.classList.add("warn");
            prog.classList.add("hidden");
            btn.disabled = false;
            return;
        }
        status.textContent = "update applied — restart required";
        btn.textContent = "Restart now";
        btn.disabled = false;
        btn.onclick = () => window.go.main.App.ApplyAndRestart();
    } catch (e) {
        unsub();
        status.textContent = "update failed: " + e;
        status.classList.add("warn");
        prog.classList.add("hidden");
        btn.disabled = false;
    }
}

async function checkForUpdatesInteractive() {
    const m = showUpdateModal();
    await runCheck(m);
}

// startupAutoCheck runs once after login (Application setting:
// updates.auto_check, default on).
export function startupAutoCheck() {
    const s = V().state.settings;
    if (s && s.updates_auto_check === false) return;
    window.go.main.App.CheckForUpdate().then((info) => {
        if (info.available) {
            V().toast(`Update ${info.version} available — Help → Check for updates`, "info", "conn");
        }
    }).catch(() => { /* offline or no update source: stay quiet */ });
}

export function initUpdater() {
    window.__voicx.checkForUpdatesInteractive = checkForUpdatesInteractive;
    window.__voicx.startupAutoCheck = startupAutoCheck;
}
