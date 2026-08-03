import { expect, test } from "@playwright/test";

const settings = {
    settings_version: 4,
    capture_device_id: "",
    playback_device_id: "",
    activation_mode: "ptt",
    vad_threshold: 25,
    volume: 100,
    chat_max_lines: 200,
    window_opacity: 100,
    camera_fps: 30,
    sound_volume: 100,
    auto_away_minutes: 0,
    notification_matrix: {},
    bookmarks: [],
};

test.beforeEach(async ({ page }) => {
    await page.addInitScript(({ initialSettings }) => {
        window.__events = {};
        window.__calls = {};
        window.__savedSettings = null;
        window.__tabs = [];
        window.runtime = {
            EventsOn(name, callback) { (window.__events[name] ||= []).push(callback); },
            EventsEmit() {},
            WindowIsFullscreen: async () => false,
        };
        const app = new Proxy({}, {
            get(_target, method) {
                return async (...args) => {
                    window.__calls[method] = (window.__calls[method] || 0) + 1;
                    if (method === "GetSettings") return structuredClone(initialSettings);
                    if (method === "SaveSettings") { window.__savedSettings = structuredClone(args[0]); return ""; }
                    if (method === "ListTabs") return structuredClone(window.__tabs);
                    if (method === "ClientID") {
                        if (window.__clientIDGate) await window.__clientIDGate;
                        return window.__activeClient || "client-a";
                    }
                    if (method === "IsAdmin") return true;
                    if (method === "IsGuest") return false;
                    if (method === "IdentityUID") return "playwright-identity";
                    if (method === "ClientVersionShort") return "test";
                    if (method === "GetPermissions") return [];
                    if (method === "SetActiveTab") {
                        window.__tabs = window.__tabs.map((tab) => ({ ...tab, active: tab.id === args[0] }));
                        window.__activeClient = args[0] === "tab-b" ? "client-b" : "client-a";
                        for (const cb of window.__events.tab_update || []) cb(structuredClone(window.__tabs));
                        for (const cb of window.__events.tab_reset || []) cb(args[0]);
                        return "";
                    }
                    return "";
                };
            },
        });
        window.go = { main: { App: app } };
        Object.defineProperty(navigator, "mediaDevices", { configurable: true, value: {
            enumerateDevices: async () => [
                { kind: "audioinput", deviceId: "mic-built-in", label: "Built-in Mic" },
                { kind: "audioinput", deviceId: "mic-usb", label: "USB Mic" },
                { kind: "audiooutput", deviceId: "speaker-usb", label: "USB Speakers" },
            ],
            getUserMedia: async () => { throw new Error("not needed by this workflow"); },
        }});
    }, { initialSettings: settings });
    await page.goto("/");
    await page.waitForFunction(() => !!window.__voicx?.openSettings);
});

test("switches the capture device and persists it", async ({ page }) => {
    await page.evaluate(() => window.__voicx.openSettings("capture"));
    const select = page.locator("#settings-content .set-row").filter({ hasText: "Capture device" }).locator("select");
    await expect(select).toHaveValue("");
    await select.selectOption("mic-usb");
    await page.getByRole("button", { name: "Apply", exact: true }).click();
    await expect.poll(() => page.evaluate(() => window.__savedSettings?.capture_device_id)).toBe("mic-usb");
});

test("switches active server tabs without retaining stale identity", async ({ page }) => {
    await page.evaluate(() => {
        window.__tabs = [
            { id: "tab-a", addr: "a.example:12333", nickname: "alice", active: true, connected: true, unread: 0, mentions: 0 },
            { id: "tab-b", addr: "b.example:12333", nickname: "bob", active: false, connected: true, unread: 2, mentions: 1 },
        ];
        for (const cb of window.__events.tab_update || []) cb(structuredClone(window.__tabs));
        for (const cb of window.__events.tab_reset || []) cb("tab-a");
    });
    await expect(page.locator('.srv-tab[data-tab-id="tab-a"]')).toHaveClass(/active/);
    await page.locator('.srv-tab[data-tab-id="tab-b"]').click();
    await expect(page.locator('.srv-tab[data-tab-id="tab-b"]')).toHaveClass(/active/);
    await expect.poll(() => page.evaluate(() => window.__voicx.state.myClientID)).toBe("client-b");
});

test("recognises an immediate channel join while switched-tab identity is loading", async ({ page }) => {
    await page.evaluate(() => {
        window.__tabs = [
            { id: "tab-a", addr: "a.example:12333", nickname: "alice", active: true, connected: true, unread: 0, mentions: 0 },
            { id: "tab-b", addr: "b.example:12333", nickname: "bob", active: false, connected: true, unread: 0, mentions: 0 },
        ];
        window.__activeClient = "client-a";
        window.__voicx.state.myClientID = "client-a";
        let release;
        window.__clientIDGate = new Promise((resolve) => { release = resolve; });
        window.__releaseClientID = release;
        for (const cb of window.__events.tab_update || []) cb(structuredClone(window.__tabs));
    });

    await page.locator('.srv-tab[data-tab-id="tab-b"]').click();
    await page.evaluate(() => {
        const snapshot = JSON.stringify({
            root_channels: [{
                ChannelID: 42, ParentID: 0, Name: "Lobby",
                clients: [{ client_id: "client-b", unique_id: "user-b", nickname: "Bob", channel_id: 0, is_speaking: false }],
                children: [],
            }],
        });
        const moved = JSON.stringify({ type: "user_moved", data: { client_id: "client-b", channel_id: 42 } });
        for (const cb of window.__events.snapshot || []) cb(snapshot);
        for (const cb of window.__events.event || []) cb(moved);
        window.__releaseClientID();
        window.__clientIDGate = null;
    });

    await expect.poll(() => page.evaluate(() => window.__voicx.state.myClientID)).toBe("client-b");
    await expect.poll(() => page.evaluate(() => window.__voicx.state.myChannelID)).toBe(42);
});

test("starts voice and plays the local cue when joining or switching channels", async ({ page }) => {
    await page.evaluate(() => {
        window.__getUserMediaCalls = 0;
        window.__playedMedia = [];
        HTMLMediaElement.prototype.play = function play() {
            window.__playedMedia.push(this.currentSrc || this.src);
            return Promise.resolve();
        };
        navigator.mediaDevices.getUserMedia = async () => {
            window.__getUserMediaCalls++;
            throw new DOMException("test permission denial", "NotAllowedError");
        };
        window.__voicx.state.myClientID = "client-a";
        window.__voicx.state.myChannelID = 0;
        window.__voicx.state.clients = [{
            client_id: "client-a", unique_id: "user-a", nickname: "Alice",
            channel_id: 0, is_speaking: false,
        }];
        const moved = JSON.stringify({ type: "user_moved", data: { client_id: "client-a", channel_id: 42 } });
        for (const cb of window.__events.event || []) cb(moved);
    });

    await expect(page.locator("#voice-join")).toHaveCount(0);
    await expect.poll(() => page.evaluate(() => window.__getUserMediaCalls)).toBeGreaterThan(0);
    await expect.poll(() => page.evaluate(
        () => window.__playedMedia.some((src) => src.includes("channel_join")),
    )).toBe(true);

    const firstCueCount = await page.evaluate(() => window.__playedMedia.length);
    await page.evaluate(() => {
        const emitMove = (clientID, channelID) => {
            const moved = JSON.stringify({ type: "user_moved", data: { client_id: clientID, channel_id: channelID } });
            for (const cb of window.__events.event || []) cb(moved);
        };
        emitMove("someone-else", 42);
        emitMove("client-a", 42);
    });
    await expect.poll(() => page.evaluate(() => window.__playedMedia.length)).toBe(firstCueCount);

    await page.evaluate(() => {
        const moved = JSON.stringify({ type: "user_moved", data: { client_id: "client-a", channel_id: 43 } });
        for (const cb of window.__events.event || []) cb(moved);
    });
    await expect.poll(() => page.evaluate(() => window.__playedMedia.length)).toBe(firstCueCount + 1);
});

test("shows the files toolbar and opens the upload picker", async ({ page }) => {
    await page.evaluate(() => {
        document.getElementById("login-overlay").classList.add("hidden");
        document.getElementById("app").classList.remove("hidden");
        window.__voicx.state.myChannelID = 42;
        window.__voicx.state.channels = [{ ChannelID: 42, Name: "Uploads" }];
    });

    await page.locator("#tab-files").click();
    await expect(page.locator("#files-pane")).toBeVisible();
    await expect(page.locator("#files-pane .fb-upload")).toBeVisible();
    await page.locator("#files-pane .fb-upload").click();
    await expect.poll(() => page.evaluate(() => window.__calls.PickUploadPaths || 0)).toBe(1);
});

test("keeps details contextual and opens it when a user is selected", async ({ page }) => {
    await page.evaluate(() => {
        document.getElementById("login-overlay").classList.add("hidden");
        document.getElementById("app").classList.remove("hidden");
    });
    await expect(page.locator("body")).toHaveClass(/details-collapsed/);
    await page.evaluate(() => {
        for (const cb of window.__events.snapshot || []) cb(JSON.stringify({
            root_channels: [{
                ChannelID: 1,
                ParentID: 0,
                Name: "Lobby",
                clients: [{
                    client_id: "client-a",
                    unique_id: "user-a",
                    nickname: "Alice",
                    channel_id: 1,
                    is_speaking: false,
                }],
                children: [],
            }],
        }));
    });
    await page.locator('.client[data-clid="client-a"]').click();
    await expect(page.locator("body")).not.toHaveClass(/details-collapsed/);
    await expect(page.locator("#client-card .card-nick")).toHaveText("Alice");
    await page.getByRole("button", { name: "Close details" }).click();
    await expect(page.locator("body")).toHaveClass(/details-collapsed/);
    await expect(page.locator("#details-toggle")).toBeVisible();
});

test("lets a guest redeem a privilege key and promotes the live session", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.state.myClientID = "guest-client";
        window.__voicx.state.isGuest = true;
        window.__voicxPerms.openTokenRedeem();
    });
    await expect(page.locator(".tk-use-input")).toBeVisible();
    await page.locator(".tk-use-input").fill("bootstrap-key");
    await page.getByRole("button", { name: "Redeem", exact: true }).click();
    await expect.poll(() => page.evaluate(() => window.__calls.TokenUse || 0)).toBe(1);

    await page.evaluate(() => {
        const event = JSON.stringify({
            type: "token_used",
            data: { client_id: "guest-client", group_id: 5, promoted: true },
        });
        for (const cb of window.__events.event || []) cb(event);
    });
    await expect.poll(() => page.evaluate(() => window.__voicx.state.isGuest)).toBe(false);
    await expect(page.locator(".toast")).toContainText("privilege key redeemed");
});

test("nests connected members below channels and offers them as direct-message targets", async ({ page }) => {
    await page.evaluate(() => {
        document.getElementById("login-overlay").classList.add("hidden");
        document.getElementById("app").classList.remove("hidden");
        window.__voicx.state.myClientID = "client-a";
        window.__voicx.state.myChannelID = 1;
        for (const cb of window.__events.snapshot || []) cb(JSON.stringify({
            root_channels: [
                {
                    ChannelID: 1, ParentID: 0, Name: "Lobby",
                    clients: [
                        { client_id: "client-a", unique_id: "user-a", nickname: "Alice", channel_id: 1, is_speaking: false },
                        { client_id: "client-b", unique_id: "user-b", nickname: "Bob", channel_id: 1, is_speaking: true },
                    ],
                    children: [],
                },
                {
                    ChannelID: 2, ParentID: 0, Name: "Workshop",
                    clients: [
                        { client_id: "client-c", unique_id: "user-c", nickname: "Carol", channel_id: 2, is_speaking: true },
                    ],
                    children: [],
                },
            ],
        }));
    });

    const lobby = page.locator('.channel-node:has(> .channel[data-chid="1"])');
    await expect(lobby.locator(':scope > .channel-members > .client')).toHaveCount(2);
    await expect(page.locator('.channel[data-chid="1"] .client')).toHaveCount(0);
    await expect(page.locator('.client[data-clid="client-b"] .client-voice-state')).toBeVisible();
    await expect(page.locator('.client[data-clid="client-c"] .client-voice-state')).toHaveCount(0);

    await page.locator("#chat-scope").selectOption("direct");
    await page.locator("#chat-target").focus();
    await expect(page.locator("#chat-target-options .target-option")).toHaveCount(2);
    await page.locator("#chat-target-options .target-option", { hasText: "Carol" }).click();
    await expect(page.locator("#chat-target")).toHaveValue("user-c");
});

test("uses the reference control-room composition without losing responsive navigation", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.evaluate(() => {
        document.getElementById("login-overlay").classList.add("hidden");
        document.getElementById("app").classList.remove("hidden");
        window.__tabs = [{
            id: "tab-a", addr: "127.0.0.1:12333", nickname: "Test",
            active: true, connected: true, unread: 0, mentions: 0,
        }];
        for (const cb of window.__events.tab_update || []) cb(structuredClone(window.__tabs));
        window.__voicx.state.myClientID = "client-a";
        window.__voicx.state.myChannelID = 1;
        for (const cb of window.__events.snapshot || []) cb(JSON.stringify({
            root_channels: [{
                ChannelID: 1, ParentID: 0, Name: "Main Lounge",
                clients: [
                    { client_id: "client-a", unique_id: "user-a", nickname: "Daniel", channel_id: 1, is_speaking: false },
                    { client_id: "client-b", unique_id: "user-b", nickname: "Benedikt", channel_id: 1, is_speaking: true },
                ],
                children: [{ ChannelID: 2, ParentID: 1, Name: "Alpha Squad", clients: [], children: [] }],
            }],
        }));
    });

    const desktop = await page.evaluate(() => ({
        accent: getComputedStyle(document.documentElement).getPropertyValue("--accent").trim(),
        appColumns: getComputedStyle(document.getElementById("app")).gridTemplateColumns,
        railDirection: getComputedStyle(document.getElementById("server-tabs")).flexDirection,
        texture: getComputedStyle(document.getElementById("center"), "::before").backgroundImage,
    }));
    expect(desktop.accent).toBe("#00f2ff");
    expect(desktop.appColumns).toMatch(/^72px /);
    expect(desktop.railDirection).toBe("column");
    expect(desktop.texture).toContain("radial-gradient");
    await expect(page.locator("#server-tabs .srv-tab.active")).toBeVisible();
    await page.locator('.client[data-clid="client-a"]').click();

    await page.setViewportSize({ width: 700, height: 800 });
    await expect.poll(() => page.evaluate(
        () => getComputedStyle(document.getElementById("server-tabs")).flexDirection,
    )).toBe("row");
    await expect(page.locator("#center")).toBeVisible();
});
