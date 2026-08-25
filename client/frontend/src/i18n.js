// i18n.js — wave-8c lightweight internationalization (336).
//
// USAGE PATTERN (how to add strings):
//   1. Add the key with BOTH translations to the catalogs below
//      (key = dotted path, e.g. "menu.connections"). en and de must carry
//      the same keys — unit tests enforce parity without putting work on the
//      language-switch hot path.
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
    "settings.server": "Server",
    "settings.notifications": "Notifications",
    "settings.language": "Language",
    "settings.searchPlaceholder": "search settings…",
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
    "settings.server": "Server",
    "settings.notifications": "Benachrichtigungen",
    "settings.language": "Sprache",
    "settings.searchPlaceholder": "Einstellungen suchen…",
    "chat.connectedAs": "Verbunden als {nick}",
    "status.offline": "offline",
    "status.retry": "Versuch {n}/{max} in {s}s…",
    "notif.title": "Benachrichtigungen",
    "notif.clearAll": "alle löschen",
};

const catalogs = Object.freeze({
    en: Object.freeze(en),
    de: Object.freeze(de),
});

// catalogParity is a pure, immutable diagnostic for unit tests and release
// checks. Language changes stay allocation-free apart from their own setting.
export function catalogParity() {
    const enKeys = new Set(Object.keys(catalogs.en));
    const deKeys = new Set(Object.keys(catalogs.de));
    return Object.freeze({
        missingFromEnglish: Object.freeze([...deKeys].filter((key) => !enKeys.has(key)).sort()),
        missingFromGerman: Object.freeze([...enKeys].filter((key) => !deKeys.has(key)).sort()),
    });
}

let lang = "en";

// setLanguage applies a language setting ("system" | "en" | "de").
export function setLanguage(l) {
    if (!l || l === "system") {
        l = (navigator.language || "en").toLowerCase().startsWith("de") ? "de" : "en";
    }
    lang = catalogs[l] ? l : "en";
}

export function currentLanguage() {
    return lang;
}

// t translates a key, substituting {placeholders} from vars. Missing keys
// warn once in the debug console and fall back to the key itself.
const warned = new Set();

// interpolate replaces every literal placeholder occurrence without treating
// the key as a regular expression. Translation keys may contain punctuation.
export function interpolate(template, vars) {
    let text = String(template);
    for (const [key, value] of Object.entries(vars || {})) {
        text = text.split("{" + key + "}").join(String(value));
    }
    return text;
}

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
    return vars ? interpolate(s, vars) : s;
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
