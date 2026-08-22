import { escapeHTML } from "./markdown.js";
import { isSafeImageDataURL } from "./safe-media.js";

// parseFileRef mirrors client/chat.go. Zero or one separator remains a plain
// legacy reference so historical messages stay readable; only two separators
// carry an attachment storage key and display name.
export function parseFileRef(capture) {
    if (typeof capture !== "string") {
        return { storage: "", key: "", name: "", valid: false, legacy: false };
    }
    const first = capture.indexOf("#");
    if (first < 0) return { storage: capture, key: "", name: capture, valid: true, legacy: true };
    const second = capture.indexOf("#", first + 1);
    if (second < 0) return { storage: capture, key: "", name: capture, valid: true, legacy: true };
    return {
        storage: capture.slice(0, first),
        key: capture.slice(first + 1, second),
        name: capture.slice(second + 1),
        valid: true,
        legacy: false,
    };
}

function emojiURL(cache, name) {
    if (cache instanceof Map) return cache.get(name);
    return cache?.[name];
}

// transformCustomEmoji only changes text nodes in already-safe markdown HTML.
// The URL is independently validated so a compromised emoji response cannot
// turn the renderer into an arbitrary-URL or markup injection sink.
export function transformCustomEmoji(html, emojis, cache) {
    let output = typeof html === "string" ? html : "";
    if (!Array.isArray(emojis)) return output;
    for (const entry of emojis) {
        const name = typeof entry === "string" ? entry : entry?.name;
        if (typeof name !== "string" || name === "") continue;
        const url = emojiURL(cache, name);
        if (!isSafeImageDataURL(url)) continue;
        const token = `:${name}:`;
        const image = `<img class="md-emoji" src="${escapeHTML(url)}" alt=":${escapeHTML(name)}:">`;
        const parts = output.split(/(<[^>]*>)/g);
        let literalDepth = 0;
        for (let index = 0; index < parts.length; index++) {
            const part = parts[index];
            if (part.startsWith("<")) {
                const tag = part.match(/^<\s*(\/?)\s*(code|pre)\b/i);
                if (tag) {
                    if (tag[1]) literalDepth = Math.max(0, literalDepth - 1);
                    else if (!/\/\s*>$/.test(part)) literalDepth++;
                }
            } else if (literalDepth === 0) {
                parts[index] = part.split(token).join(image);
            }
        }
        output = parts.join("");
    }
    return output;
}
