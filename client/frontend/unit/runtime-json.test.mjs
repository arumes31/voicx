import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { parseRuntimeObject } from "../src/runtime-json.js";

describe("parseRuntimeObject", () => {
    it("silently rejects malformed, null, primitive, and array event payloads", () => {
        for (const payload of ["{", "null", "[]", "42", null, [], true]) {
            assert.equal(parseRuntimeObject(payload), null);
        }
    });

    it("continues after a malformed payload and returns later valid objects", () => {
        assert.equal(parseRuntimeObject("{"), null);
        assert.deepEqual(parseRuntimeObject('{"type":"chat","data":{}}'), { type: "chat", data: {} });
        const object = { channel_ids: [1] };
        assert.equal(parseRuntimeObject(object), object);
    });
});
