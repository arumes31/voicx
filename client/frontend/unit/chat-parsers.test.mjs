import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { parseFileRef, transformCustomEmoji } from "../src/chat-parsers.js";

describe("parseFileRef", () => {
    it("matches the Go parser for legacy, partial, encrypted, and empty captures", () => {
        assert.deepEqual(parseFileRef("photo.png"), {
            storage: "photo.png", key: "", name: "photo.png", valid: true, legacy: true,
        });
        assert.deepEqual(parseFileRef("legacy#name"), {
            storage: "legacy#name", key: "", name: "legacy#name", valid: true, legacy: true,
        });
        assert.deepEqual(parseFileRef("blob#key#photo.png"), {
            storage: "blob", key: "key", name: "photo.png", valid: true, legacy: false,
        });
        assert.deepEqual(parseFileRef("#key#"), {
            storage: "", key: "key", name: "", valid: true, legacy: false,
        });
        assert.deepEqual(parseFileRef("blob#key#name#with#hash"), {
            storage: "blob", key: "key", name: "name#with#hash", valid: true, legacy: false,
        });
        assert.deepEqual(parseFileRef(null), {
            storage: "", key: "", name: "", valid: false, legacy: false,
        });
    });
});

describe("transformCustomEmoji", () => {
    const url = "data:image/png;base64,aGVsbG8=";

    it("replaces every text token while preserving markup and attribute values", () => {
        const output = transformCustomEmoji(
            '<a data-note=":wave:">:wave:</a> :wave: <code>:wave:</code>',
            [{ name: "wave" }], new Map([["wave", url]]),
        );
        assert.equal((output.match(/class="md-emoji"/g) || []).length, 2);
        assert.match(output, /data-note=":wave:"/);
        assert.match(output, /<code>:wave:<\/code>/);
    });

    it("rejects unsafe URLs and escapes hostile names without producing markup", () => {
        assert.equal(
            transformCustomEmoji(":wave:", [{ name: "wave" }], new Map([["wave", "javascript:alert(1)"]])),
            ":wave:",
        );
        const output = transformCustomEmoji(
            ':x" onerror="boom:', [{ name: 'x" onerror="boom' }], { 'x" onerror="boom': url },
        );
        assert.match(output, /alt=":x&quot; onerror=&quot;boom:"/);
        assert.doesNotMatch(output, / onerror="/);
    });

    it("handles absent/invalid inputs without changing the source", () => {
        assert.equal(transformCustomEmoji(":wave:", null, new Map()), ":wave:");
        assert.equal(transformCustomEmoji(null, [{ name: "wave" }], new Map([["wave", url]])), "");
        assert.equal(transformCustomEmoji(":wave:", [{}, ""], new Map()), ":wave:");
    });
});
