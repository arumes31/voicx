import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { extractPresentedFingerprint } from "../src/security.js";

const fingerprint = Array.from({ length: 32 }, (_, index) => index.toString(16).padStart(2, "0")).join(":");

describe("certificate mismatch details", () => {
    it("extracts and normalizes the presented SHA-256 fingerprint", () => {
        const detail = `tls fingerprint mismatch (presented: ${fingerprint.toUpperCase()})`;
        assert.equal(extractPresentedFingerprint(detail), fingerprint);
    });

    it("fails closed for missing, truncated, or malformed fingerprints", () => {
        assert.equal(extractPresentedFingerprint("tls fingerprint mismatch"), "");
        assert.equal(extractPresentedFingerprint("presented: aa:bb"), "");
        assert.equal(extractPresentedFingerprint(`presented: ${"gg:".repeat(31)}gg`), "");
        assert.equal(extractPresentedFingerprint(null), "");
    });
});
