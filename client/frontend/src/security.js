const fingerprintPattern = /presented:\s*((?:[0-9a-f]{2}:){31}[0-9a-f]{2})/i;

export function extractPresentedFingerprint(detail) {
    if (typeof detail !== "string") return "";
    return detail.match(fingerprintPattern)?.[1]?.toLowerCase() || "";
}
