// Runtime event payloads cross the Wails bridge as untrusted JSON. Keep the
// boundary small and silent: a malformed event must not prevent the next
// valid event from updating the UI, and arrays/null are never valid payloads.
export function parseRuntimeObject(payload) {
    let value = payload;
    if (typeof value === "string") {
        try {
            value = JSON.parse(value);
        } catch {
            return null;
        }
    }
    if (!value || typeof value !== "object" || Array.isArray(value)) return null;
    return value;
}
