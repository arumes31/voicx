// file-links.js validates the server-issued capability-link fragments before
// combining them with the control address recorded by the desktop client.
// The health listener is deliberately separate from the TLS control socket;
// never derive its scheme from the control connection.

const CONTROL_SCHEMES = new Set(["http:", "https:", "tcp:", "tls:", "voicx:"]);
const LINK_SCHEMES = new Set(["http", "https"]);
const LINK_PATH = /^\/dl\/([0-9a-f]{32})$/;

function controlAddressURL(address) {
    if (typeof address !== "string" || address.trim() === "") return null;
    const value = address.trim();
    const hasScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(value);
    try {
        const parsed = new URL(hasScheme ? value : `voicx://${value}`);
        if (!CONTROL_SCHEMES.has(parsed.protocol) || !parsed.hostname ||
            parsed.username || parsed.password || parsed.search || parsed.hash ||
            (parsed.pathname !== "/" && parsed.pathname !== "")) return null;
        return parsed;
    } catch {
        return null;
    }
}

function linkScheme(value) {
    if (value === undefined || value === null || value === "") return "http"; // old servers
    if (typeof value !== "string") return null;
    const scheme = value.toLowerCase();
    return LINK_SCHEMES.has(scheme) ? scheme : null;
}

function linkPort(value) {
    return Number.isInteger(value) && value > 0 && value <= 65535 ? value : null;
}

function linkPath(value) {
    return typeof value === "string" && LINK_PATH.test(value) ? value : null;
}

// buildFileLink returns a normalized URL or null. It accepts DNS hosts, IPv4,
// and bracketed IPv6 through URL parsing, but refuses credentials, paths,
// query strings, malformed protocol fields, and all non-download paths.
export function buildFileLink(controlAddress, response) {
    const control = controlAddressURL(controlAddress);
    const scheme = linkScheme(response?.scheme);
    const port = linkPort(response?.health_port);
    const path = linkPath(response?.path);
    if (!control || !scheme || !port || !path) return null;

    try {
        const url = new URL(`${scheme}://placeholder.invalid`);
        url.hostname = control.hostname;
        url.port = String(port);
        url.pathname = path;
        return url.href;
    } catch {
        return null;
    }
}
