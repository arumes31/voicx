import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { createMediaDeviceInventory } from "../src/media-devices.js";

describe("media device inventory", () => {
    it("requires an enumeration function", () => {
        assert.throws(() => createMediaDeviceInventory(null), /enumerateDevices must be a function/);
    });

    it("shares concurrent capture, playback, and refresh enumeration", async () => {
        let calls = 0;
        let release;
        const gate = new Promise((resolve) => { release = resolve; });
        const inventory = createMediaDeviceInventory(async () => {
            calls++;
            await gate;
            return [{ kind: "audioinput", deviceId: "mic" }];
        });

        const capture = inventory.load();
        const playback = inventory.load();
        const refresh = inventory.load(true);
        await Promise.resolve();
        assert.equal(calls, 1);

        release();
        const [captureDevices, playbackDevices, refreshedDevices] = await Promise.all([
            capture, playback, refresh,
        ]);
        assert.deepEqual(captureDevices, playbackDevices);
        assert.deepEqual(playbackDevices, refreshedDevices);
        assert.equal(calls, 1);

        await inventory.load();
        assert.equal(calls, 1, "a successful snapshot should be cached");
    });

    it("refreshes once and exposes the new shared snapshot", async () => {
        let calls = 0;
        const inventory = createMediaDeviceInventory(async () => {
            calls++;
            return [{ kind: "audiooutput", deviceId: `speaker-${calls}` }];
        });

        assert.equal((await inventory.load())[0].deviceId, "speaker-1");
        assert.equal((await inventory.load(true))[0].deviceId, "speaker-2");
        assert.equal((await inventory.load())[0].deviceId, "speaker-2");
        assert.equal(calls, 2);
    });

    it("retains the last good snapshot when a refresh fails", async () => {
        let fail = false;
        const inventory = createMediaDeviceInventory(async () => {
            if (fail) throw new Error("permission denied");
            return [{ kind: "audioinput", deviceId: "working-mic" }];
        });

        const initial = await inventory.load();
        fail = true;
        await assert.rejects(inventory.load(true), /permission denied/);
        assert.deepEqual(await inventory.load(), initial);
    });

    it("re-enumerates after invalidation and rejects malformed responses", async () => {
        let calls = 0;
        const inventory = createMediaDeviceInventory(async () => {
            calls++;
            return calls === 1 ? [] : null;
        });

        await inventory.load();
        inventory.invalidate();
        await assert.rejects(inventory.load(), /invalid response/);
        assert.equal(calls, 2);
    });
});
