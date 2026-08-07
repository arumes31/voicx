import test from "node:test";
import assert from "node:assert/strict";

import {
    LIVE_ANNOUNCEMENT_MAX_PENDING,
    createLiveAnnouncementQueue,
} from "../src/live-announcer.js";

function scheduler() {
    const tasks = [];
    return {
        schedule(callback) {
            const task = { callback, cancelled: false };
            tasks.push(task);
            return task;
        },
        cancel(task) {
            task.cancelled = true;
        },
        runAll() {
            let guard = 100;
            while (tasks.length && guard-- > 0) {
                const task = tasks.shift();
                if (!task.cancelled) task.callback();
            }
            assert.ok(guard > 0, "announcement scheduler drained");
        },
        tasks,
    };
}

function region(log, priority) {
    let value = "";
    return Object.defineProperty({}, "textContent", {
        get: () => value,
        set: (next) => {
            value = next;
            log.push([priority, next]);
        },
    });
}

test("serializes bursts, prioritizes alerts, and preserves identical repeats", () => {
    const clock = scheduler();
    const writes = [];
    const regions = {
        polite: region(writes, "polite"),
        assertive: region(writes, "assertive"),
    };
    const queue = createLiveAnnouncementQueue({
        resolveRegion: (priority) => regions[priority],
        schedule: clock.schedule,
        cancel: clock.cancel,
        gapMs: 1,
        holdMs: 1,
    });

    assert.equal(queue.announce("same"), true);
    assert.equal(queue.announce("same"), true);
    assert.equal(queue.announce("urgent", "assertive"), true);
    assert.equal(queue.isActive(), true);
    assert.equal(queue.pendingCount(), 2);
    clock.runAll();

    assert.deepEqual(writes.filter(([, text]) => text).map((entry) => entry[1]), ["same", "urgent", "same"]);
    assert.ok(writes.filter(([, text]) => text === "").length >= 3);
    assert.equal(queue.pendingCount(), 0);
    assert.equal(queue.isActive(), false);
});

test("bounds pending work without sacrificing an assertive-only backlog", () => {
    const clock = scheduler();
    const spoken = [];
    const queue = createLiveAnnouncementQueue({
        resolveRegion: (priority) => region(spoken, priority),
        schedule: clock.schedule,
        cancel: clock.cancel,
        maxPending: 2,
    });

    queue.announce("active");
    queue.announce("old polite");
    queue.announce("new polite");
    queue.announce("urgent", "assertive");
    queue.announce("newest polite");
    assert.equal(queue.pendingCount(), 2);
    clock.runAll();
    assert.deepEqual(spoken.filter(([, text]) => text).map((entry) => entry[1]), ["active", "urgent", "newest polite"]);

    const alertClock = scheduler();
    const alerts = [];
    const alertQueue = createLiveAnnouncementQueue({
        resolveRegion: (priority) => region(alerts, priority),
        schedule: alertClock.schedule,
        cancel: alertClock.cancel,
        maxPending: 2,
    });
    alertQueue.announce("active alert", "assertive");
    alertQueue.announce("old alert", "assertive");
    alertQueue.announce("new alert", "assertive");
    assert.equal(alertQueue.announce("polite dropped"), false);
    assert.equal(alertQueue.announce("newest alert", "assertive"), true);
    alertClock.runAll();
    assert.deepEqual(alerts.filter(([, text]) => text).map((entry) => entry[1]),
        ["active alert", "new alert", "newest alert"]);
    assert.equal(LIVE_ANNOUNCEMENT_MAX_PENDING, 16);
});

test("handles unavailable regions, invalid input, and disposal", () => {
    const clock = scheduler();
    const queue = createLiveAnnouncementQueue({
        resolveRegion: () => null,
        schedule: clock.schedule,
        cancel: clock.cancel,
        maxPending: 0,
    });
    assert.equal(queue.announce(null), false);
    assert.equal(queue.announce(undefined), false);
    assert.equal(queue.announce(""), false);
    assert.equal(queue.announce("no region"), true);
    assert.equal(queue.isActive(), false);

    const active = createLiveAnnouncementQueue({
        resolveRegion: () => ({ textContent: "" }),
        schedule: clock.schedule,
        cancel: clock.cancel,
    });
    active.announce("pending");
    assert.equal(clock.tasks.length, 1);
    active.dispose();
    assert.equal(active.announce("after dispose"), false);
    assert.equal(active.pendingCount(), 0);
    assert.equal(active.isActive(), false);
    clock.runAll();

    assert.throws(() => createLiveAnnouncementQueue(), /resolveRegion/);
});
