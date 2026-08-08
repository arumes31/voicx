// A settings session needs one hardware snapshot for both capture and
// playback. The loader caches successful results, shares an in-flight request,
// and keeps the last good snapshot when a manual refresh fails.
export function createMediaDeviceInventory(enumerateDevices) {
    if (typeof enumerateDevices !== "function") {
        throw new TypeError("enumerateDevices must be a function");
    }

    let cached = null;
    let pending = null;

    return {
        invalidate() {
            cached = null;
        },
        load(refresh = false) {
            if (!refresh && cached !== null) return Promise.resolve(cached);
            if (pending) return pending;
            pending = Promise.resolve()
                .then(() => enumerateDevices())
                .then((devices) => {
                    if (!Array.isArray(devices)) {
                        throw new Error("media device discovery returned an invalid response");
                    }
                    cached = devices.slice();
                    return cached;
                })
                .finally(() => {
                    pending = null;
                });
            return pending;
        },
    };
}
