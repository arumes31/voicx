import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { describe, it } from "node:test";

describe("workspace dialog actions", () => {
    it("uses shared modal prompts instead of native blocking dialogs", async () => {
        for (const path of ["../src/files-ui.js", "../src/chat-ui.js"]) {
            const source = await readFile(new URL(path, import.meta.url), "utf8");
            assert.doesNotMatch(source, /\b(?:prompt|confirm)\s*\(/);
        }
    });
});
