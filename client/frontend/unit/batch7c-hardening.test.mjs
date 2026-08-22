import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { describe, it } from "node:test";

async function source(path) {
    return readFile(new URL(path, import.meta.url), "utf8");
}

describe("Batch 7C frontend hardening contracts", () => {
    it("opens About links externally and makes late version updates harmless", async () => {
        const menu = await source("../src/menu.js");
        assert.match(menu, /https:\/\/github\.com\/arumes31\/voicx/);
        assert.match(menu, /https:\/\/github\.com\/arumes31\/voicx\/issues/);
        assert.match(menu, /rel="noopener noreferrer"/);
        assert.match(menu, /overlay\.isConnected\s*&&\s*versionEl\.isConnected/);
        assert.match(menu, /BrowserOpenURL\(link\.href\)/);
    });

    it("keeps chat retries scoped and attachment previews safe and lazy", async () => {
        const chat = await source("../src/chat-ui.js");
        assert.match(chat, /const previewScope = captureScope\(\(\) => readInlineAttachmentScope\(chID\)\)/);
        assert.match(chat, /if \(!previewIsCurrent\(\)\) return;[\s\S]{0,120}wrap\.textContent = ""/);
        assert.match(chat, /el\.preload = "none"/);
        assert.match(chat, /el\.loading = "lazy"/);
        assert.match(chat, /el\.decoding = "async"/);
        assert.match(chat, /const retryFiles = \[\]/);
        assert.match(chat, /pendingFiles = \[\.\.\.retryFiles, \.\.\.pendingFiles\]/);
        assert.match(chat, /if \(err\) \{[\s\S]{0,120}return;/);
    });

    it("contains control-bridge errors and emits one ICE warning per outage", async () => {
        const main = await source("../src/main.js");
        assert.match(main, /await window\.go\.main\.App\.Disconnect\(\);[\s\S]{0,600}catch \{[\s\S]{0,600}toast\("disconnect failed", "warn", "conn"\)/);
        assert.match(main, /void window\.go\.main\.App\.SendICECandidate\([\s\S]{0,180}\)\.catch\(\(\) => \{\}\)/);
        assert.match(main, /const ICE_BACKOFF_MS = \[1000, 2000, 5000, 15000\]/);
        assert.match(main, /let iceFailureNotified = false/);
        assert.match(main, /if \(!iceFailureNotified\) \{[\s\S]{0,180}Voice connection unstable/);
        assert.doesNotMatch(main, /sysMsg\("ice restart failed/);
    });

    it("prevents a stale checksum restoration timer from changing an old row", async () => {
        const files = await source("../src/files-ui.js");
        assert.match(files, /const current = \(\) => generation === serverViewGeneration/);
        assert.match(files, /setTimeout\(\(\) => \{\s*if \(!current\(\)\) return;/);
    });
});
