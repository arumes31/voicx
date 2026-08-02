// markdown.js — (91/92/93) XSS-safe mini-markdown renderer for chat.
// Discipline: user text is HTML-escaped FIRST; every transform below only
// injects markup built from already-escaped content. Code spans/blocks are
// stashed away before escaping so their contents are never interpreted as
// markdown or HTML.

// ~50 common shortcode -> unicode emoji mappings (95).
export const EMOJI = {
    smile: "😄", smiley: "😃", grin: "😁", laughing: "😆", sweat_smile: "😅",
    joy: "😂", rofl: "🤣", wink: "😉", blush: "😊", yum: "😋",
    sunglasses: "😎", heart_eyes: "😍", kissing_heart: "😘", thinking: "🤔", neutral_face: "😐",
    expressionless: "😑", rolling_eyes: "🙄", smirk: "😏", worried: "😟", cry: "😢",
    sob: "😭", angry: "😠", rage: "🤬", scream: "😱", sleeping: "😴",
    mask: "😷", nerd: "🤓", innocent: "😇", upside_down: "🙃", salute: "🫡",
    thumbsup: "👍", "+1": "👍", thumbsdown: "👎", "-1": "👎", ok_hand: "👌",
    clap: "👏", wave: "👋", muscle: "💪", pray: "🙏", eyes: "👀",
    eye: "👁️", fire: "🔥", sparkles: "✨", star: "⭐", tada: "🎉",
    heart: "❤️", broken_heart: "💔", hundred: "💯", "100": "💯", boom: "💥",
    zap: "⚡", rocket: "🚀", check: "✅", x: "❌", warning: "⚠️",
    question: "❓", exclamation: "❗", bulb: "💡", lock: "🔒", unlock: "🔓",
    microphone: "🎙️", speaker: "🔊", mute: "🔇", computer: "💻", bug: "🐛",
    poop: "💩", skull: "💀", ghost: "👻", robot: "🤖", pizza: "🍕",
};

const KEYWORDS = /\b(func|package|import|return|if|else|for|range|go|defer|var|const|let|function|class|new|typeof|instanceof|await|async|yield|type|struct|interface|map|chan|nil|true|false|null|undefined|switch|case|default|break|continue|fallthrough|select|this|export|from|extends|try|catch|finally|throw|in|of|do|while)\b/g;

// escapeHTML escapes the five HTML-significant characters.
export function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({
        "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    }[c]));
}

// highlight applies light syntax colouring to ALREADY-escaped code text:
// strings, // and /* */ comments, and a shared js/go keyword set (92).
// Keywords run FIRST so they can't match inside the span markup injected by
// the string/comment passes; nested spans (keyword inside a string) are fine.
function highlight(escaped) {
    return escaped
        .replace(KEYWORDS, `<span class="tk-kw">$1</span>`)
        .replace(/(&quot;[^\n]*?&quot;|&#39;[^\n]*?&#39;)/g, `<span class="tk-str">$1</span>`)
        .replace(/(\/\/[^\n]*|\/\*[\s\S]*?\*\/)/g, `<span class="tk-com">$1</span>`);
}

function detectLanguage(code) {
    if (/^\s*(package\s+\w+|func\s+\w+|go\s+func|type\s+\w+\s+struct)\b/m.test(code)) return "go";
    if (/\b(const|let|var)\s+\w+\s*=|=>|\b(document|window)\./.test(code)) return "javascript";
    if (/^\s*(def|class|from\s+\w+\s+import|import\s+\w+)\b/m.test(code)) return "python";
    if (/^\s*(SELECT|INSERT|UPDATE|DELETE|CREATE\s+TABLE)\b/im.test(code)) return "sql";
    if (/^\s*[.#]?[\w-]+\s*\{[^}]*:[^}]*\}/m.test(code)) return "css";
    if (/^\s*[<{][\s\S]*[>}]\s*$/.test(code)) return "markup";
    return "plain";
}

function tableCells(line) {
    return line.trim().replace(/^\||\|$/g, "").split("|").map((cell) => cell.trim());
}

function renderTables(text) {
    const lines = text.split(/\r?\n/);
    const out = [];
    for (let i = 0; i < lines.length; i++) {
        const header = tableCells(lines[i]);
        const separator = i + 1 < lines.length ? tableCells(lines[i + 1]) : [];
        const isTable = lines[i].includes("|") && header.length > 1 && separator.length === header.length &&
            separator.every((cell) => /^:?-{3,}:?$/.test(cell));
        if (!isTable) {
            out.push(lines[i]);
            continue;
        }
        let html = `<div class="md-table-wrap"><table class="md-table"><thead><tr>${header.map((cell) => `<th>${cell}</th>`).join("")}</tr></thead><tbody>`;
        i += 2;
        while (i < lines.length && lines[i].includes("|")) {
            const cells = tableCells(lines[i]);
            if (cells.length !== header.length) break;
            html += `<tr>${cells.map((cell) => `<td>${cell}</td>`).join("")}</tr>`;
            i++;
        }
        html += "</tbody></table></div>";
        out.push(html);
        i--;
    }
    return out.join("\n");
}

// renderMarkdown converts user text to a safe HTML string. Links are
// rendered as <a class="md-link" data-url="...">; opening happens via click
// delegation (runtime.BrowserOpenURL) wired up in chat-ui.js — never via a
// raw href navigation inside the webview.
export function renderMarkdown(text) {
    if (!text) return "";
    const stash = [];
    const push = (html) => {
        stash.push(html);
        return "\u0001" + (stash.length - 1) + "\u0002";
    };

    let t = String(text);
    // Fenced code blocks (```lang\n...```) first — their content is opaque.
    t = t.replace(/```(\w*)\r?\n?([\s\S]*?)(?:```|$)/g, (_m, lang, code) => {
        const language = (lang || detectLanguage(code)).toLowerCase();
        return push(`<pre class="md-pre language-${escapeHTML(language)}"><code>${highlight(escapeHTML(code.replace(/\n$/, "")))}</code></pre>`);
    });
    // Inline code spans.
    t = t.replace(/`([^`\n]+)`/g, (_m, code) =>
        push(`<code class="md-code">${escapeHTML(code)}</code>`));

    // Everything else gets escaped, then inline markup is applied.
    t = escapeHTML(t);
    t = t.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
    t = t.replace(/__([^_\n]+)__/g, "<u>$1</u>");
    t = t.replace(/~~([^~\n]+)~~/g, "<del>$1</del>");
    t = t.replace(/(^|[\s(])\*([^*\n]+)\*(?=[\s).,;!?]|$)/g, "$1<em>$2</em>");
    // Images (channel descriptions; 113). http(s) only — no javascript:.
    t = t.replace(/!\[([^\]\n]*)\]\((https?:\/\/[^\s)]+)\)/g,
        `<img class="md-img" alt="$1" src="$2">`);
    // Autolink URLs (93): tooltip carries the full URL.
    t = t.replace(/(?<![="'\w])(https?:\/\/[^\s<>"'\])}]+)/g,
        (u) => `<a href="#" class="md-link" data-url="${u}" title="${u}">${u}</a>`);
    // :shortcode: emoji (95).
    t = t.replace(/:([a-z0-9_+-]+):/gi, (m, name) => EMOJI[name.toLowerCase()] || m);
    t = renderTables(t);
    t = t.replace(/\n/g, "<br>");
    // Restore stashed code.
    t = t.replace(/\u0001(\d+)\u0002/g, (_m, i) => stash[Number(i)]);
    return t;
}
