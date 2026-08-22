import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { buildFileLink } from "../src/file-links.js";

const response = {
    health_port: 12337,
    path: "/dl/0123456789abcdef0123456789abcdef",
};

describe("buildFileLink", () => {
    it("normalizes DNS, IPv4, IPv6, full control URLs, and old-server schemes", () => {
        assert.equal(buildFileLink("voice.example:12333", response),
            "http://voice.example:12337/dl/0123456789abcdef0123456789abcdef");
        assert.equal(buildFileLink("127.0.0.1:12333", { ...response, scheme: "HTTPS" }),
            "https://127.0.0.1:12337/dl/0123456789abcdef0123456789abcdef");
        assert.equal(buildFileLink("[2001:db8::7]:12333", response),
            "http://[2001:db8::7]:12337/dl/0123456789abcdef0123456789abcdef");
        assert.equal(buildFileLink("tls://Voice.Example:12333", response),
            "http://voice.example:12337/dl/0123456789abcdef0123456789abcdef");
    });

    it("fails closed for malformed addresses and hostile protocol fields", () => {
        const invalidAddresses = ["", "2001:db8::7", "https://user@voice.example", "voice.example/path", "voice.example?x=1"];
        for (const address of invalidAddresses) {
            assert.equal(buildFileLink(address, response), null, address);
        }
        for (const invalid of [
            { ...response, scheme: "javascript" },
            { ...response, scheme: "http://evil.example" },
            { ...response, health_port: "12337" },
            { ...response, health_port: 0 },
            { ...response, path: "//evil.example" },
            { ...response, path: "/dl/0123456789abcdef0123456789abcdeg" },
            { ...response, path: "/dl/0123456789abcdef0123456789abcdef?x=1" },
        ]) {
            assert.equal(buildFileLink("voice.example:12333", invalid), null, JSON.stringify(invalid));
        }
    });
});
