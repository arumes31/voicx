export const LIVE_ANNOUNCEMENT_MAX_PENDING = 16;

// Serialize updates across both live regions. Assertive messages move ahead of
// pending polite messages, while the currently speaking item is never cut off.
// Clearing before every write makes identical consecutive messages observable
// to assistive technology instead of being coalesced as an unchanged value.
export function createLiveAnnouncementQueue({
    resolveRegion,
    schedule = (callback, delay) => setTimeout(callback, delay),
    cancel = (timer) => clearTimeout(timer),
    gapMs = 50,
    holdMs = 250,
    maxPending = LIVE_ANNOUNCEMENT_MAX_PENDING,
} = {}) {
    if (typeof resolveRegion !== "function") throw new TypeError("resolveRegion must be a function");

    const queues = { polite: [], assertive: [] };
    const limit = Math.max(1, Number(maxPending) || LIVE_ANNOUNCEMENT_MAX_PENDING);
    let active = false;
    let disposed = false;
    let timer = null;
    let lastRegion = null;

    const pendingCount = () => queues.polite.length + queues.assertive.length;

    const nextItem = () => queues.assertive.shift() || queues.polite.shift() || null;

    const processNext = () => {
        if (disposed || active) return;
        const item = nextItem();
        if (!item) return;
        const region = resolveRegion(item.priority);
        if (!region) {
            processNext();
            return;
        }
        active = true;
        if (lastRegion && lastRegion !== region) lastRegion.textContent = "";
        region.textContent = "";
        timer = schedule(() => {
            timer = null;
            if (disposed) return;
            region.textContent = item.text;
            lastRegion = region;
            timer = schedule(() => {
                timer = null;
                active = false;
                processNext();
            }, holdMs);
        }, gapMs);
    };

    const announce = (text, priority = "polite") => {
        if (disposed || text === null || text === undefined || String(text) === "") return false;
        const normalizedPriority = priority === "assertive" ? "assertive" : "polite";
        if (pendingCount() >= limit) {
            // Prefer dropping an older polite update. A new polite update never
            // evicts an assertive-only backlog.
            if (queues.polite.length > 0) queues.polite.shift();
            else if (normalizedPriority === "polite") return false;
            else queues.assertive.shift();
        }
        queues[normalizedPriority].push({ text: String(text), priority: normalizedPriority });
        processNext();
        return true;
    };

    const dispose = () => {
        disposed = true;
        if (timer !== null) cancel(timer);
        timer = null;
        active = false;
        queues.polite.length = 0;
        queues.assertive.length = 0;
    };

    return {
        announce,
        dispose,
        pendingCount,
        isActive: () => active,
    };
}
