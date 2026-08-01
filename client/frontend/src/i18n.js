// i18n.js — wave-8c lightweight internationalization (336).
//
// USAGE PATTERN (how to add strings):
//   1. Add the key with BOTH translations to the catalogs below
//      (key = dotted path, e.g. "menu.connections"). en and de must carry
//      the same keys — the debug console warns about mismatches at startup
//      and there is a Go-side parity test reading this file.
//   2. In code use t("menu.connections"); with placeholders:
//      t("chat.connectedAs", { nick }) → "Connected as {nick}".
//   3. Settings → Application → Language applies it live (menus and the
//      settings dialog rebuild; static index.html labels are set once at
//      startup — full coverage of every string is intentionally out of
//      scope this wave).

const en = {
    "menu.connections": "Connections",
    "menu.bookmarks": "Bookmarks",
    "menu.self": "Self",
    "menu.view": "View",
    "menu.permissions": "Permissions",
    "menu.tools": "Tools",
    "menu.help": "Help",
    "menu.connect": "Connect…",
    "menu.disconnect": "Disconnect",
    "menu.quit": "Quit",
    "menu.settings": "Settings…",
    "menu.compact": "Compact mode",
    "menu.zen": "Zen mode",
    "menu.setStatus": "Set status…",
    "menu.contacts": "Contacts…",
    "menu.setAvatar": "Set avatar…",
    "menu.setServerIcon": "Set server icon…",
    "menu.auditLog": "Audit Log…",
    "menu.bans": "Bans…",
    "menu.debugConsole": "Debug console…",
    "menu.connStats": "Connection stats…",
    "menu.exportLogs": "Export logs…",
    "menu.about": "About voicx",
    "menu.checkUpdates": "Check for updates…",
    "menu.permManager": "Permission Manager…",
    "menu.viewMyPerms": "View my permissions",
    "menu.whisperLists": "Whisper lists…",
    "common.ok": "OK",
    "common.cancel": "Cancel",
    "common.close": "Close",
    "common.save": "Save",
    "common.delete": "Delete",
    "common.create": "Create",
    "common.apply": "Apply",
    "login.server": "SERVER",
    "login.nickname": "NICKNAME",
    "login.password": "PASSWORD",
    "login.serverPassword": "SERVER PASSWORD",
    "login.connect": "CONNECT",
    "login.recentServers": "RECENT SERVERS",
    "settings.application": "Application",
    "settings.capture": "Capture",
    "settings.playback": "Playback",
    "settings.hotkeys": "Hotkeys",
    "settings.whisper": "Whisper",
    "settings.downloads": "Downloads",
    "settings.chat": "Chat",
    "settings.security": "Security",
    "settings.notifications": "Notifications",
    "settings.language": "Language (336)",
    "settings.searchPlaceholder": "search settings… (350)",
    "chat.connectedAs": "Connected as {nick}",
    "status.offline": "offline",
    "status.retry": "retry {n}/{max} in {s}s…",
    "notif.title": "Notifications",
    "notif.clearAll": "clear all",
};

const de = {
    "menu.connections": "Verbindungen",
    "menu.bookmarks": "Lesezeichen",
    "menu.self": "Selbst",
    "menu.view": "Ansicht",
    "menu.permissions": "Rechte",
    "menu.tools": "Werkzeuge",
    "menu.help": "Hilfe",
    "menu.connect": "Verbinden…",
    "menu.disconnect": "Trennen",
    "menu.quit": "Beenden",
    "menu.settings": "Einstellungen…",
    "menu.compact": "Kompaktmodus",
    "menu.zen": "Zen-Modus",
    "menu.setStatus": "Status setzen…",
    "menu.contacts": "Kontakte…",
    "menu.setAvatar": "Avatar setzen…",
    "menu.setServerIcon": "Server-Icon setzen…",
    "menu.auditLog": "Audit-Log…",
    "menu.bans": "Bans…",
    "menu.debugConsole": "Debug-Konsole…",
    "menu.connStats": "Verbindungsstatistik…",
    "menu.exportLogs": "Logs exportieren…",
    "menu.about": "Über voicx",
    "menu.checkUpdates": "Nach Updates suchen…",
    "menu.permManager": "Rechte-Manager…",
    "menu.viewMyPerms": "Meine Rechte anzeigen",
    "menu.whisperLists": "Flüsterlisten…",
    "common.ok": "OK",
    "common.cancel": "Abbrechen",
    "common.close": "Schließen",
    "common.save": "Speichern",
    "common.delete": "Löschen",
    "common.create": "Erstellen",
    "common.apply": "Anwenden",
    "login.server": "SERVER",
    "login.nickname": "SPITZNAME",
    "login.password": "PASSWORT",
    "login.serverPassword": "SERVER-PASSWORT",
    "login.connect": "VERBINDEN",
    "login.recentServers": "LETZTE SERVER",
    "settings.application": "Anwendung",
    "settings.capture": "Aufnahme",
    "settings.playback": "Wiedergabe",
    "settings.hotkeys": "Tastenkürzel",
    "settings.whisper": "Flüstern",
    "settings.downloads": "Downloads",
    "settings.chat": "Chat",
    "settings.security": "Sicherheit",
    "settings.notifications": "Benachrichtigungen",
    "settings.language": "Sprache (336)",
    "settings.searchPlaceholder": "Einstellungen suchen… (350)",
    "chat.connectedAs": "Verbunden als {nick}",
    "status.offline": "offline",
    "status.retry": "Versuch {n}/{max} in {s}s…",
    "notif.title": "Benachrichtigungen",
    "notif.clearAll": "alle löschen",
};

const catalogs = { en, de };

let lang = "en";

// setLanguage applies a language setting ("system" | "en" | "de").
export function setLanguage(l) {
    if (!l || l === "system") {
        l = (navigator.language || "en").toLowerCase().startsWith("de") ? "de" : "en";
    }
    lang = catalogs[l] ? l : "en";
    // Startup self-check: warn about key mismatches (336 debug aid).
    const enKeys = Object.keys(en).sort().join(",");
    const deKeys = Object.keys(de).sort().join(",");
    if (enKeys !== deKeys) {
        console.warn("[i18n] catalog key mismatch between en and de");
    }
}

export function currentLanguage() {
    return lang;
}

// t translates a key, substituting {placeholders} from vars. Missing keys
// warn once in the debug console and fall back to the key itself.
const warned = new Set();

export function t(key, vars) {
    let s = catalogs[lang][key];
    if (s === undefined) {
        s = catalogs.en[key];
        if (s === undefined) {
            if (!warned.has(key)) {
                warned.add(key);
                console.warn("[i18n] missing key:", key);
            }
            s = key;
        } else if (!warned.has(lang + ":" + key)) {
            warned.add(lang + ":" + key);
            console.warn("[i18n] missing translation", lang, key, "— using en");
        }
    }
    if (vars) {
        for (const [k, v] of Object.entries(vars)) {
            s = s.replace("{" + k + "}", String(v));
        }
    }
    return s;
}

// applyStaticLabels re-labels the static index.html surfaces (login card).
export function applyStaticLabels() {
    const set = (id, key) => {
        const el = document.getElementById(id);
        if (!el) return;
        if (el.tagName === "INPUT" || el.tagName === "BUTTON") {
            if (el.tagName === "BUTTON") el.textContent = t(key);
            else if (el.previousSibling?.textContent !== undefined) { /* labels are text nodes */ }
        } else {
            el.textContent = t(key);
        }
    };
    const loginLabels = document.querySelectorAll(".login-card label");
    const keys = ["login.server", "login.nickname", "login.password", "login.serverPassword"];
    loginLabels.forEach((l, i) => {
        if (keys[i] && l.firstChild) l.firstChild.textContent = t(keys[i]) + " ";
    });
    const btn = document.getElementById("login-connect");
    if (btn) btn.textContent = t("login.connect");
    const recentsHead = [...document.querySelectorAll(".login-card .pane-head")].pop();
    if (recentsHead) recentsHead.textContent = t("login.recentServers");
    void set;
}
