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
    onboarding_done: true,
    alpha_dismissed: "test",
};

test.beforeEach(async ({ page }) => {
    await page.addInitScript(({ initialSettings }) => {
        window.__events = {};
        window.__calls = {};
        window.__callArgs = {};
        window.__savedSettings = null;
        window.__tabs = [];
        window.runtime = {
            EventsOn(name, callback) {
                const listeners = (window.__events[name] ||= []);
                listeners.push(callback);
                return () => {
                    const index = listeners.indexOf(callback);
                    if (index >= 0) listeners.splice(index, 1);
                };
            },
            EventsEmit() {},
            WindowIsFullscreen: async () => false,
        };
        const app = new Proxy({}, {
            get(_target, method) {
                return async (...args) => {
                    window.__calls[method] = (window.__calls[method] || 0) + 1;
                    (window.__callArgs[method] ||= []).push(structuredClone(args));
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
                    if (method === "FileList") {
                        if (window.__fileListGate) await window.__fileListGate;
                        return { entries: [], folders: [], used_bytes: 0, quota_bytes: 0 };
                    }
                    if (method === "GetPermissions") return structuredClone(window.__permissions || []);
                    if (method === "GroupList") return structuredClone(window.__groups || { groups: [] });
                    if (method === "PermList") {
                        const response = structuredClone(window.__permEntries || { entries: [] });
                        const gate = window.__permListGate;
                        window.__permListGate = null;
                        if (gate) await gate;
                        return response;
                    }
                    if (method === "PermSet") { window.__lastPermSet = structuredClone(args); return ""; }
                    if (method === "BanList") return structuredClone(window.__bans || { bans: [] });
                    if (method === "BanRemove") {
                        const gate = window.__banRemoveGate;
                        window.__banRemoveGate = null;
                        if (gate) await gate;
                        return window.__banRemoveResult || "";
                    }
                    if (method === "CheckForUpdate") return structuredClone(window.__updateInfo || { available: false, version: "test", size: 0 });
                    if (method === "IdentityInfo") return {};
                    if (method === "GetAvatar") return structuredClone(window.__avatarResponse || {});
                    if (method === "ServerIconGet" || method === "ServerBannerGet" || method === "ChannelIconGet" || method === "GroupIconGet" || method === "EmojiGet") {
                        return structuredClone(window.__assetResponse || {});
                    }
                    if (method === "GetClientInfo") {
                        const response = structuredClone(window.__clientInfoResponse || {
                            nickname: "Alice", unique_id: "user-a", connected_at: Date.now() / 1000 - 120,
                            idle_seconds: 5, ping_ms: 12, ip: "127.0.0.1", port: 12333,
                            bytes_in: 1024, bytes_out: 2048,
                        });
                        const gate = window.__clientInfoGate;
                        window.__clientInfoGate = null;
                        if (gate) await gate;
                        return response;
                    }
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

async function auditAccessibility(page, context) {
    const snapshot = await page.locator("body").ariaSnapshot();
    const issues = await page.evaluate(() => {
        const found = [];
        const ids = new Map();
        for (const element of document.querySelectorAll("[id]")) {
            const id = element.id;
            if (!id) continue;
            ids.set(id, (ids.get(id) || 0) + 1);
        }
        for (const [id, count] of ids) {
            if (count > 1) found.push(`duplicate id #${id} (${count} instances)`);
        }

        for (const attribute of ["aria-labelledby", "aria-describedby", "aria-controls"]) {
            for (const element of document.querySelectorAll(`[${attribute}]`)) {
                for (const id of element.getAttribute(attribute).trim().split(/\s+/)) {
                    if (id && !document.getElementById(id)) {
                        found.push(`${element.tagName.toLowerCase()}[${attribute}] references missing #${id}`);
                    }
                }
            }
        }
        for (const label of document.querySelectorAll("label[for]")) {
            const id = label.getAttribute("for");
            if (id && !document.getElementById(id)) found.push(`label[for] references missing #${id}`);
        }
        for (const image of document.querySelectorAll("img:not([alt])")) {
            found.push(`image is missing alt text${image.id ? ` (#${image.id})` : ""}`);
        }
        return found;
    });

    const namedRoles = new Set([
        "button", "checkbox", "combobox", "dialog", "link", "menuitem", "menuitemcheckbox",
        "menuitemradio", "radio", "searchbox", "slider", "spinbutton", "switch", "tab", "textbox",
    ]);
    for (const line of snapshot.split("\n")) {
        const match = line.match(/^\s*-\s+([a-z]+)\b(.*)$/);
        if (!match || !namedRoles.has(match[1])) continue;
        const name = match[2].match(/"((?:[^"\\]|\\.)*)"/);
        if (!name || !name[1].trim()) issues.push(`unnamed ${match[1]} in accessibility tree: ${line.trim()}`);
    }

    expect(issues, `${context} accessibility issues\n\n${snapshot}`).toEqual([]);
}

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
    await page.evaluate(() => {
        const state = window.__voicx.state;
        state.serverGroups = [{ id: 7, name: "Old server admins" }];
        state.groupByUID = new Map([["old-user", [{ id: 7 }]]]);
        state.groupIcons = new Map([[7, "old-group-icon"]]);
        state.avatars = new Map([["old-user", "old-avatar"]]);
        state.avatarPending = new Set(["old-user"]);
        state.myPerms = new Map([["b_server_admin", { value: 1 }]]);
        const serverIcon = document.getElementById("server-icon");
        serverIcon.src = "data:image/png;base64,AAAA";
        serverIcon.classList.remove("hidden");
    });
    await page.locator('.srv-tab[data-tab-id="tab-b"]').click();
    await expect(page.locator('.srv-tab[data-tab-id="tab-b"]')).toHaveClass(/active/);
    await expect.poll(() => page.evaluate(() => window.__voicx.state.myClientID)).toBe("client-b");
    await expect.poll(() => page.evaluate(() => ({
        groups: window.__voicx.state.serverGroups.length,
        memberships: window.__voicx.state.groupByUID.size,
        groupIcons: window.__voicx.state.groupIcons.size,
        avatars: window.__voicx.state.avatars.size,
        avatarPending: window.__voicx.state.avatarPending.size,
        permissions: window.__voicx.state.myPerms.size,
    }))).toEqual({ groups: 0, memberships: 0, groupIcons: 0, avatars: 0, avatarPending: 0, permissions: 0 });
    await expect(page.locator("#server-icon")).toHaveClass(/hidden/);
    await expect(page.locator("#server-icon")).not.toHaveAttribute("src", /.+/);
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

test("removes cascaded deleted channels and displaces every cached member safely", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.myClientID = "";
        for (const callback of window.__events.snapshot || []) callback(JSON.stringify({
            root_channels: [
                {
                    ChannelID: 10, ParentID: 0, Name: "Parent",
                    clients: [],
                    children: [{
                        ChannelID: 11, ParentID: 10, Name: "Child",
                        clients: [{ client_id: "client-a", unique_id: "user-a", nickname: "Alice", channel_id: 11 }],
                        children: [{
                            ChannelID: 12, ParentID: 11, Name: "Grandchild",
                            clients: [{ client_id: "client-b", unique_id: "user-b", nickname: "Bob", channel_id: 12 }],
                            children: [],
                        }],
                    }],
                },
                {
                    ChannelID: 20, ParentID: 0, Name: "Unaffected",
                    clients: [],
                    children: [{
                        ChannelID: 21, ParentID: 20, Name: "Legacy child",
                        clients: [],
                        children: [{
                            ChannelID: 22, ParentID: 21, Name: "Legacy grandchild",
                            clients: [{ client_id: "client-c", unique_id: "user-c", nickname: "Carol", channel_id: 22 }],
                            children: [],
                        }],
                    }],
                },
            ],
        }));
        window.__voicx.state.myClientID = "client-a";
        window.__voicx.state.myChannelID = 11;
        window.__voiceTracksStopped = 0;
        window.__voicx.state.localStream = {
            getTracks: () => [{ stop: () => { window.__voiceTracksStopped++; } }],
            getAudioTracks: () => [],
            getVideoTracks: () => [],
        };
        document.getElementById("voice-status").textContent = "voice on";
        window.__voicx.state.collapsedChannels.add(10);
        window.__voicx.state.collapsedChannels.add(20);
        window.__voicx.state.expandedVirtual.add(12);
        window.__voicx.state.expandedVirtual.add(22);
        window.__voicx.renderTree();
    });

    await page.locator("#tab-files").click();
    await expect(page.locator("#files-pane .fb-list")).not.toContainText("Join a channel");
    await page.evaluate(() => {
        const event = JSON.stringify({
            type: "channel_deleted",
            data: { channel_id: 10, channel_ids: [10, 11, 12] },
        });
        for (const callback of window.__events.event || []) callback(event);
    });

    await expect.poll(() => page.evaluate(() => ({
        channels: window.__voicx.state.channels.map((channel) => channel.ChannelID),
        clients: Object.fromEntries(window.__voicx.state.clients.map((client) => [client.client_id, client.channel_id])),
        myChannelID: window.__voicx.state.myChannelID,
        localStreamCleared: window.__voicx.state.localStream === null,
        stopped: window.__voiceTracksStopped,
        collapsedDeleted: !window.__voicx.state.collapsedChannels.has(10),
        expandedDeleted: !window.__voicx.state.expandedVirtual.has(12),
    }))).toEqual({
        channels: [20, 21, 22],
        clients: { "client-a": 0, "client-b": 0, "client-c": 22 },
        myChannelID: 0,
        localStreamCleared: true,
        stopped: 1,
        collapsedDeleted: true,
        expandedDeleted: true,
    });
    await expect(page.locator('.channel[data-chid="10"], .channel[data-chid="11"], .channel[data-chid="12"]')).toHaveCount(0);
    await expect(page.locator('.channel[data-chid="20"]')).toHaveCount(1);
    await expect(page.locator("#voice-status")).toHaveText("voice off");
    await expect(page.locator("#files-pane .fb-list")).toContainText("Join a channel to browse its files");

    // Legacy servers send only the parent channel_id. Re-enter the surviving
    // subtree so this second deletion exercises descendant member and voice
    // cleanup rather than merely deleting a leaf.
    await page.evaluate(() => {
        const state = window.__voicx.state;
        state.myClientID = "client-c";
        state.myChannelID = 22;
        state.localStream = {
            getTracks: () => [{ stop: () => { window.__voiceTracksStopped++; } }],
            getAudioTracks: () => [],
            getVideoTracks: () => [],
        };
        document.getElementById("voice-status").textContent = "voice on";
        const event = JSON.stringify({ type: "channel_deleted", data: { channel_id: 20 } });
        for (const callback of window.__events.event || []) callback(event);
    });
    await expect.poll(() => page.evaluate(() => ({
        channels: window.__voicx.state.channels.length,
        carolChannel: window.__voicx.state.clients.find((client) => client.client_id === "client-c")?.channel_id,
        myChannelID: window.__voicx.state.myChannelID,
        localStreamCleared: window.__voicx.state.localStream === null,
        stopped: window.__voiceTracksStopped,
        collapsedDeleted: !window.__voicx.state.collapsedChannels.has(20),
        expandedDeleted: !window.__voicx.state.expandedVirtual.has(22),
    }))).toEqual({
        channels: 0,
        carolChannel: 0,
        myChannelID: 0,
        localStreamCleared: true,
        stopped: 2,
        collapsedDeleted: true,
        expandedDeleted: true,
    });
    await expect(page.locator('.channel[data-chid="20"], .channel[data-chid="21"], .channel[data-chid="22"]')).toHaveCount(0);
    await expect(page.locator("#voice-status")).toHaveText("voice off");
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
        window.__voicx.showWorkspace(false);
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
        window.__voicx.showWorkspace(false);
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
        window.__voicx.showWorkspace(false);
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
        window.__voicx.showWorkspace(false);
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
    const mainLounge = page.locator('.channel-node:has(> .channel[data-chid="1"])');
    await expect(mainLounge.locator(":scope > .channel-children")).toHaveAttribute("role", "group");
    await expect(mainLounge.locator(":scope > .channel-children")).toHaveAttribute("aria-label", "Main Lounge subchannels");
    await expect(page.locator("#server-tabs .srv-tab.active")).toBeVisible();
    await page.locator('.client[data-clid="client-a"]').click();

    await page.setViewportSize({ width: 700, height: 800 });
    await expect.poll(() => page.evaluate(
        () => getComputedStyle(document.getElementById("server-tabs")).flexDirection,
    )).toBe("row");
    await expect(page.locator("#center")).toBeVisible();
});

test("exposes named landmarks, controls, live regions, and a visible focus ring", async ({ page }) => {
    await expect(page.getByRole("dialog", { name: "voicx" })).toBeVisible();
    await expect(page.getByRole("textbox", { name: /^server$/i })).toBeVisible();
    await expect(page.getByRole("button", { name: "Connect" })).toBeVisible();
    await expect(page.locator("#login-error")).toHaveAttribute("role", "alert");
    await expect(page.locator("#toasts")).toHaveAttribute("aria-label", "Notifications");
    await expect(page.locator("#voice-status")).toHaveAttribute("role", "status");
    await expect(page.locator("#chat-log")).toHaveAttribute("aria-live", "off");
    await expect(page.locator("#chat-announcer")).toHaveAttribute("aria-live", "polite");
    await expect(page.locator("#alert-announcer")).toHaveAttribute("aria-live", "assertive");
    await expect(page.locator("#conn-pill")).not.toHaveAttribute("aria-live", /.+/);

    await page.locator("#login-addr").focus();
    await expect.poll(() => page.locator("#login-addr").evaluate((el) => {
        const style = getComputedStyle(el);
        return `${style.outlineStyle} ${style.outlineWidth}`;
    })).toBe("solid 2px");

    await expect(page.locator(".skip-link")).toBeHidden();
    await page.evaluate(() => window.__voicx.showWorkspace(false));
    await page.locator(".skip-link").focus();
    await expect(page.locator(".skip-link")).toBeVisible();
    await page.evaluate(() => {
        const message = JSON.stringify({
            type: "chat",
            data: { id: 101, from: "Bob", from_unique_id: "user-b", text: "hello from Bob", channel_id: 0 },
        });
        for (const cb of window.__events.event || []) cb(message);
    });
    await expect(page.locator("#chat-announcer")).toHaveText("Bob: hello from Bob");
    await expect(page.locator("#alert-announcer")).toBeEmpty();
    await expect(page.locator("#toasts [role=status], #toasts [role=alert]")).toHaveCount(0);
    await expect(page.locator("#toasts .toast").first()).toHaveAttribute("aria-hidden", "true");
    await page.evaluate(() => { document.getElementById("chat-log").innerHTML = "<p>rerendered history</p>"; });
    await expect(page.locator("#chat-announcer")).toHaveText("Bob: hello from Bob");
});

test("@a11y audits primary login, workspace, settings, and permission-dialog states", async ({ page }) => {
    await auditAccessibility(page, "login");

    await page.evaluate(() => window.__voicx.showWorkspace(false));
    await auditAccessibility(page, "connected workspace");

    await page.evaluate(() => window.__voicx.openSettings("application"));
    await expect(page.getByRole("dialog", { name: "Settings" })).toBeVisible();
    await auditAccessibility(page, "settings dialog");
    await page.keyboard.press("Escape");

    await page.evaluate(() => {
        window.__voicx.state.isAdmin = true;
        window.__groups = { groups: [{ id: 7, name: "Operators", member_count: 0, color: "" }] };
        window.__permEntries = { entries: [{ key: "b_channel_join_permanent", value: 1, grant: 1, skip: false, negate: false }] };
        window.__voicxPerms.openPermissionManager();
    });
    await page.locator(".pm-target", { hasText: "Operators" }).click();
    await expect(page.locator(".pm-edit-grid")).toBeVisible();
    await auditAccessibility(page, "permission manager dialog");
});

test("serializes live-region bursts without coalescing identical messages", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__spokenAnnouncements = [];
        for (const id of ["chat-announcer", "alert-announcer"]) {
            const region = document.getElementById(id);
            new MutationObserver(() => {
                if (region.textContent) window.__spokenAnnouncements.push([id, region.textContent]);
            }).observe(region, { childList: true, characterData: true, subtree: true });
        }
        window.__voicx.announceLive("repeated update");
        window.__voicx.announceLive("repeated update");
        window.__voicx.announceLive("urgent update", "assertive");
    });

    await expect.poll(() => page.evaluate(() => window.__spokenAnnouncements)).toEqual([
        ["chat-announcer", "repeated update"],
        ["alert-announcer", "urgent update"],
        ["chat-announcer", "repeated update"],
    ]);
});

test("announces only eligible chat in the visible scope", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        const state = window.__voicx.state;
        state.myClientID = "client-a";
        state.myUniqueID = "user-a";
        state.myNickname = "Alice";
        state.myChannelID = 1;
        state.lastConnect = { addr: "voice.example:12333" };
        state.settings.chat_notification_level = "all";
        state.settings.notify_matrix = {};
        state.settings.channel_notify = {};
        window.__spokenChat = [];
        const region = document.getElementById("chat-announcer");
        new MutationObserver(() => {
            if (region.textContent) window.__spokenChat.push(region.textContent);
        }).observe(region, { childList: true, characterData: true, subtree: true });
        for (const cb of window.__events.snapshot || []) cb(JSON.stringify({
            root_channels: [
                { ChannelID: 1, ParentID: 0, Name: "Lobby", clients: [], children: [] },
                { ChannelID: 2, ParentID: 0, Name: "Elsewhere", clients: [], children: [] },
            ],
        }));
        const dispatch = (data) => {
            const message = JSON.stringify({ type: "chat", data });
            for (const callback of window.__events.event || []) callback(message);
        };
        dispatch({ id: 201, from: "Bob", from_unique_id: "user-b", text: "inactive scope", channel_id: 2 });
        state.settings.channel_notify["voice.example:12333#1"] = { muted: true };
        dispatch({ id: 202, from: "Bob", from_unique_id: "user-b", text: "muted", channel_id: 1 });
        delete state.settings.channel_notify["voice.example:12333#1"];
        state.settings.dnd_enabled = true;
        dispatch({ id: 203, from: "Bob", from_unique_id: "user-b", text: "dnd", channel_id: 1 });
        state.settings.dnd_enabled = false;
        state.settings.chat_notification_level = "direct";
        dispatch({ id: 204, from: "Bob", from_unique_id: "user-b", text: "category filtered", channel_id: 1 });
        state.settings.chat_notification_level = "all";
        state.settings.notify_matrix.channel_message = { toast: false, sound: false, flash: false, native: false };
        dispatch({ id: 205, from: "Bob", from_unique_id: "user-b", text: "matrix filtered", channel_id: 1 });
    });
    await page.waitForTimeout(450);
    expect(await page.evaluate(() => window.__spokenChat)).toEqual([]);

    await page.evaluate(() => {
        window.__voicx.state.settings.notify_matrix = {};
        const message = JSON.stringify({
            type: "chat",
            data: { id: 206, from: "Bob", from_unique_id: "user-b", text: "visible message", channel_id: 1 },
        });
        for (const callback of window.__events.event || []) callback(message);
    });
    await expect.poll(() => page.evaluate(() => window.__spokenChat)).toEqual(["Bob: visible message"]);
});

test("summarizes visible offline replay instead of announcing every message", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.myUniqueID = "user-a";
        window.__voicx.state.myNickname = "Alice";
        window.__voicx.state.settings.notify_matrix = {};
        window.__voicx.state.settings.dnd_enabled = false;
        window.__voicxChat.openPM("user-b", "Bob");
        window.__offlineSpoken = [];
        const region = document.getElementById("chat-announcer");
        new MutationObserver(() => {
            if (region.textContent) window.__offlineSpoken.push(region.textContent);
        }).observe(region, { childList: true, characterData: true, subtree: true });
        const dispatch = (id, uid, from, text) => {
            const message = JSON.stringify({
                type: "chat",
                data: { id, from, from_unique_id: uid, text, e2e: true, offline: true, client_msg_id: `offline-${id}` },
            });
            for (const callback of window.__events.event || []) callback(message);
        };
        dispatch(301, "user-c", "Carol", "inactive offline message");
        dispatch(302, "user-b", "Bob", "one");
        dispatch(303, "user-b", "Bob", "two");
        dispatch(304, "user-b", "Bob", "three");
    });

    await expect.poll(() => page.evaluate(() => window.__offlineSpoken)).toEqual([
        "3 offline messages from Bob",
    ]);
});

test("summarizes a visible reconnect burst instead of announcing every message", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        const state = window.__voicx.state;
        state.myClientID = "client-a";
        state.myUniqueID = "user-a";
        state.myNickname = "Alice";
        state.myChannelID = 1;
        state.settings.chat_notification_level = "all";
        state.settings.notify_matrix = {};
        state.settings.channel_notify = {};
        state.settings.dnd_enabled = false;
        window.__reconnectSpoken = [];
        const region = document.getElementById("chat-announcer");
        new MutationObserver(() => {
            if (region.textContent) window.__reconnectSpoken.push(region.textContent);
        }).observe(region, { childList: true, characterData: true, subtree: true });
        for (const callback of window.__events.snapshot || []) callback(JSON.stringify({
            root_channels: [
                { ChannelID: 1, ParentID: 0, Name: "Lobby", clients: [], children: [] },
            ],
        }));
        window.__voicxChat.beginReconnectAnnouncementBatch(2000);
        for (let id = 401; id <= 403; id++) {
            const message = JSON.stringify({
                type: "chat",
                data: {
                    id,
                    from: "Bob",
                    from_unique_id: "user-b",
                    text: `replayed ${id}`,
                    channel_id: 1,
                },
            });
            for (const callback of window.__events.event || []) callback(message);
        }
    });

    await expect.poll(() => page.evaluate(() => window.__reconnectSpoken)).toEqual([
        "3 messages from Bob received after reconnect",
    ]);
});

test("cancels stale reconnect batches when switching server tabs", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        const state = window.__voicx.state;
        state.myClientID = "client-a";
        state.myUniqueID = "user-a";
        state.myNickname = "Alice";
        state.myChannelID = 1;
        state.settings.chat_notification_level = "all";
        state.settings.notify_matrix = {};
        state.settings.channel_notify = {};
        state.settings.dnd_enabled = false;
        window.__resetSpoken = [];
        const region = document.getElementById("chat-announcer");
        new MutationObserver(() => {
            if (region.textContent) window.__resetSpoken.push(region.textContent);
        }).observe(region, { childList: true, characterData: true, subtree: true });
        const snapshot = () => {
            for (const callback of window.__events.snapshot || []) callback(JSON.stringify({
                root_channels: [
                    { ChannelID: 1, ParentID: 0, Name: "Lobby", clients: [], children: [] },
                ],
            }));
        };
        const dispatch = (id, text) => {
            const message = JSON.stringify({
                type: "chat",
                data: { id, from: "Bob", from_unique_id: "user-b", text, channel_id: 1 },
            });
            for (const callback of window.__events.event || []) callback(message);
        };
        snapshot();
        window.__voicxChat.beginReconnectAnnouncementBatch(3000);
        dispatch(451, "old server replay");
        for (const callback of window.__events.tab_reset || []) callback("manual-switch");
        state.myClientID = "client-a";
        state.myChannelID = 1;
        snapshot();
        dispatch(452, "new server message");
    });

    await expect.poll(() => page.evaluate(() => window.__resetSpoken)).toEqual([
        "Bob: new server message",
    ]);
    await page.waitForTimeout(850);
    expect(await page.evaluate(() => window.__resetSpoken.some((text) => text.includes("after reconnect")))).toBe(false);
});

test("preserves reconnect batching across the tab created by a real reconnect", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        const state = window.__voicx.state;
        state.settings.chat_notification_level = "all";
        state.settings.notify_matrix = {};
        state.settings.channel_notify = {};
        state.settings.dnd_enabled = false;
        window.__preservedReconnectSpoken = [];
        const region = document.getElementById("chat-announcer");
        new MutationObserver(() => {
            if (region.textContent) window.__preservedReconnectSpoken.push(region.textContent);
        }).observe(region, { childList: true, characterData: true, subtree: true });
        window.__voicxChat.beginReconnectAnnouncementBatch(3000);
        state.reconnectInFlight = true;
        for (const callback of window.__events.tab_reset || []) callback("reconnected-tab");
        state.reconnectInFlight = false;
        state.myClientID = "client-a";
        state.myUniqueID = "user-a";
        state.myNickname = "Alice";
        state.myChannelID = 1;
        for (const callback of window.__events.snapshot || []) callback(JSON.stringify({
            root_channels: [
                { ChannelID: 1, ParentID: 0, Name: "Lobby", clients: [], children: [] },
            ],
        }));
        for (let id = 461; id <= 462; id++) {
            const message = JSON.stringify({
                type: "chat",
                data: { id, from: "Bob", from_unique_id: "user-b", text: `replayed ${id}`, channel_id: 1 },
            });
            for (const callback of window.__events.event || []) callback(message);
        }
    });

    await expect.poll(() => page.evaluate(() => window.__preservedReconnectSpoken)).toEqual([
        "2 messages from Bob received after reconnect",
    ]);
});

test("keeps reconnect countdown changes visual and announces the failure once", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.settings.reconnect_on_loss = true;
        window.__voicx.state.settings.notify_connection = true;
        window.__voicx.state.lastConnect = { addr: "voice.example:12333", nick: "Alice", pw: "", spw: "" };
        for (const callback of window.__events.disconnected || []) callback();
    });
    await expect(page.locator("#conn-pill")).toContainText("retry 1/5 in 5s");
    await expect(page.locator("#alert-announcer")).toHaveText("Connection lost");
    await expect(page.locator("#conn-pill")).not.toHaveAttribute("aria-live", /.+/);
    await page.waitForTimeout(1100);
    await expect(page.locator("#conn-pill")).toContainText("retry 1/5 in 4s");
    await expect(page.locator("#alert-announcer")).toHaveText("Connection lost");
});

test("dispatches one DM notification only for actual E2EE direct messages", async ({ page }) => {
    await page.evaluate(() => {
        const originalNotify = window.__voicxNotify.notify;
        window.__notificationDispatches = [];
        window.__voicxNotify.notify = (event, text, context) => {
            window.__notificationDispatches.push(event);
            return originalNotify(event, text, context);
        };
        Object.defineProperty(document, "hasFocus", { configurable: true, value: () => false });
        window.__voicx.state.myNickname = "Alice";
        window.__voicx.state.myUniqueID = "user-a";
        window.__voicx.state.clients = [
            { client_id: "client-b", unique_id: "user-b", nickname: "Bob", channel_id: 0 },
            { client_id: "client-c", unique_id: "user-c", nickname: "Carol", channel_id: 0 },
        ];

        const direct = JSON.stringify({
            type: "chat",
            data: {
                id: 801, from: "Bob", from_client_id: "client-b", from_unique_id: "user-b",
                text: "private hello", e2e: true, client_msg_id: "dm-801",
            },
        });
        const global = JSON.stringify({
            type: "chat",
            data: {
                id: 802, from: "Carol", from_client_id: "client-c", from_unique_id: "user-c",
                text: "global hello", channel_id: 0, e2e: false,
            },
        });
        for (const callback of window.__events.event || []) callback(direct);
        for (const callback of window.__events.event || []) callback(global);
    });

    await expect.poll(() => page.evaluate(() => window.__notificationDispatches)).toEqual([
        "dm",
        "channel_message",
    ]);
    expect(await page.evaluate(() => window.__notificationDispatches.filter((event) => event === "dm").length)).toBe(1);
    expect(await page.evaluate(() => window.__calls.TrayMention || 0)).toBe(1);
    expect(await page.evaluate(() => window.__voicx.state.lastWhispererUID)).toBe("user-b");
});

test("renders hostile update, permission, and image metadata as inert data", async ({ page }) => {
    const attack = `<img src=x onerror="document.body.dataset.remoteXss='yes'">`;
    await page.evaluate(async (payload) => {
        window.__permissions = [{
            key: payload,
            value: 7,
            skip: true,
            negate: false,
            inherited: true,
            source_tier: payload,
        }];
        window.__updateInfo = { available: true, version: payload, size: 1048576 };
        await window.__voicx.refreshPermissions();
        await window.__voicx.checkForUpdatesInteractive();
    }, attack);

    expect(await page.evaluate(() => document.body.dataset.remoteXss || "")).toBe("");
    await expect(page.locator("#perm-area tbody .mono")).toHaveText(attack);
    await expect(page.locator("#perm-area tbody tr")).toHaveAttribute("title", `effective from ${attack} (inherited)`);
    await expect(page.locator(".upd-status")).toHaveText(`update available: ${attack} (1.0 MiB)`);

    await page.evaluate(async (payload) => {
        const host = document.createElement("span");
        host.className = "avatar hostile-avatar";
        host.dataset.uid = "hostile-user";
        document.body.appendChild(host);
        window.__avatarResponse = {
            content_type: `image/png\" onerror=\"document.body.dataset.remoteXss='image'`,
            data_base64: "AAAA",
        };
        await window.__voicx.fetchAvatar("hostile-user");
        window.__voicx.state.avatars.delete("valid-user");
        window.__voicx.state.avatarPending.delete("valid-user");
        const validHost = document.createElement("span");
        validHost.className = "avatar valid-avatar";
        validHost.dataset.uid = "valid-user";
        document.body.appendChild(validHost);
        window.__avatarResponse = { content_type: "image/png", data_base64: "AAAA" };
        await window.__voicx.fetchAvatar("valid-user");
        void payload;
    }, attack);

    expect(await page.evaluate(() => document.body.dataset.remoteXss || "")).toBe("");
    await expect(page.locator(".hostile-avatar img")).toHaveCount(0);
    expect(await page.evaluate(() => window.__voicx.state.avatars.get("hostile-user"))).toBe(null);
    await expect(page.locator(".valid-avatar img")).toHaveAttribute("src", "data:image/png;base64,AAAA");
});

test("renders editable permission keys as inert text", async ({ page }) => {
    const key = '<span data-permission-key-injection="true">unexpected node</span>';
    await page.evaluate(({ permissionKey }) => {
        window.__voicx.state.isAdmin = true;
        window.__groups = { groups: [{ id: 7, name: "Operators", member_count: 0, color: "" }] };
        window.__permEntries = {
            entries: [{ key: permissionKey, value: 7, grant: 5, skip: true, negate: false }],
        };
        window.__voicxPerms.openPermissionManager();
    }, { permissionKey: key });

    await page.locator(".pm-target", { hasText: "Operators" }).click();
    const keyCell = page.locator(".pm-edit-grid tbody tr.set td.mono");
    await expect(keyCell).toHaveText(key);
    await expect(page.locator('[data-permission-key-injection="true"]')).toHaveCount(0);

    await keyCell.click();
    await expect(page.locator(".pm-editor-row .pe-value")).toHaveValue("7");
    await expect(page.locator(".pm-editor-row .pe-grant")).toHaveValue("5");
    await page.locator(".pm-editor-row .pe-set").click();
    await expect.poll(() => page.evaluate(() => window.__lastPermSet?.[4])).toBe(key);
});

test("invalidates server dialogs and delayed responses when the active tab changes", async ({ page }) => {
    const oldKey = "old_server_permission";
    const newKey = "new_server_permission";
    await page.evaluate(({ staleKey }) => {
        window.__voicx.state.isAdmin = true;
        window.__groups = { groups: [{ id: 7, name: "Old Operators", member_count: 0, color: "" }] };
        window.__permEntries = { entries: [{ key: staleKey, value: 7, grant: 7, skip: false, negate: false }] };
        let releasePermList;
        window.__permListGate = new Promise((resolve) => { releasePermList = resolve; });
        window.__releaseOldPermList = releasePermList;
        let releaseClientInfo;
        window.__clientInfoGate = new Promise((resolve) => { releaseClientInfo = resolve; });
        window.__releaseOldClientInfo = releaseClientInfo;
        window.__clientInfoResponse = {
            nickname: "Old Alice", unique_id: "old-user", connected_at: Date.now() / 1000 - 60,
            idle_seconds: 1, ping_ms: 20, ip: "127.0.0.7", port: 12333, bytes_in: 7, bytes_out: 7,
        };
        window.__voicxPerms.openPermissionManager();
    }, { staleKey: oldKey });

    await page.locator(".pm-target", { hasText: "Old Operators" }).click();
    await expect.poll(() => page.evaluate(() => window.__calls.PermList || 0)).toBe(1);
    await page.evaluate(() => {
        window.__voicx.openClientInfo({ client_id: "old-client", unique_id: "old-user", nickname: "Old Alice" });
        window.__voicxPerms.openTokenManager();
        window.__voicxPerms.openAuditViewer();
        window.__voicxPerms.openBanList();
    });
    await expect.poll(() => page.evaluate(() => window.__calls.GetClientInfo || 0)).toBeGreaterThan(0);
    await expect(page.locator(".dlg-overlay").filter({ hasText: "Permission Manager" })).toHaveCount(1);
    await expect(page.locator(".dlg-overlay").filter({ hasText: "Privilege Keys" })).toHaveCount(1);
    await expect(page.locator(".dlg-overlay").filter({ hasText: "Audit Log" })).toHaveCount(1);
    await expect(page.locator(".dlg-overlay").filter({ hasText: "Bans" })).toHaveCount(1);
    await expect(page.locator(".dlg-overlay").filter({ hasText: "Connection Info" })).toHaveCount(1);

    await page.evaluate(({ freshKey }) => {
        window.__groups = { groups: [{ id: 9, name: "New Operators", member_count: 0, color: "" }] };
        window.__permEntries = { entries: [{ key: freshKey, value: 9, grant: 9, skip: false, negate: false }] };
        window.__clientInfoResponse = {
            nickname: "New Bob", unique_id: "new-user", connected_at: Date.now() / 1000 - 30,
            idle_seconds: 2, ping_ms: 9, ip: "127.0.0.9", port: 12333, bytes_in: 9, bytes_out: 9,
        };
        for (const callback of window.__events.tab_reset || []) callback("tab-b");
        window.__voicx.state.isAdmin = true;
    }, { freshKey: newKey });

    await expect(page.locator(".dlg-overlay")).toHaveCount(0);
    await page.evaluate(() => window.__voicxPerms.openPermissionManager());
    await page.locator(".pm-target", { hasText: "New Operators" }).click();
    await expect(page.locator(".pm-edit-grid tbody tr.set td.mono")).toHaveText(newKey);
    await page.evaluate(() => {
        window.__voicx.openClientInfo({ client_id: "new-client", unique_id: "new-user", nickname: "New Bob" });
    });
    await expect(page.getByRole("dialog", { name: "Connection Info" }).locator('[data-f="nick"]')).toHaveText("New Bob");

    await page.evaluate(() => {
        window.__releaseOldPermList();
        window.__releaseOldClientInfo();
    });
    await page.waitForTimeout(100);
    await expect(page.getByRole("dialog", { name: "Connection Info" }).locator('[data-f="nick"]')).toHaveText("New Bob");
    await page.getByRole("dialog", { name: "Connection Info" }).getByRole("button", { name: "Close" }).click();
    await expect(page.locator(".pm-edit-grid tbody tr.set td.mono")).toHaveText(newKey);
    await expect(page.locator(".pm-edit-grid tbody tr.set td.mono")).not.toHaveText(oldKey);
    await page.locator(".pm-edit-grid tbody tr.set td.mono").click();
    await page.locator(".pm-editor-row .pe-set").click();
    await expect.poll(() => page.evaluate(() => window.__lastPermSet)).toEqual([
        "server_group", 9, "", 0, newKey, 9, 9, false, false,
    ]);
});

test("cancels server-bound image actions across active-tab resets", async ({ page }) => {
    const image = {
        name: "one-pixel.png",
        mimeType: "image/png",
        buffer: Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=", "base64"),
    };
    const prompts = [];
    page.on("dialog", async (dialog) => {
        prompts.push(dialog.message());
        await dialog.accept("late-emoji");
    });
    const connect = () => page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.myClientID = "client-a";
        window.__voicx.state.isAdmin = true;
    });
    const reset = (tabID) => page.evaluate((id) => {
        for (const callback of window.__events.tab_reset || []) callback(id);
    }, tabID);
    const selfMenu = page.locator("#menubar > .menu-item > span").filter({ hasText: /^Self$/ }).locator("..");
    const chooseFromSelf = async (name) => {
        await selfMenu.click();
        const pending = page.waitForEvent("filechooser");
        await page.getByRole("menuitem", { name }).click();
        return pending;
    };

    // A crop dialog that already exists is scoped to the old server and closes
    // as part of the reset. Closing resolves the picker as cancelled.
    await connect();
    const cropChooser = await chooseFromSelf(/^Set avatar/);
    await cropChooser.setFiles(image);
    await expect(page.getByRole("dialog", { name: "Set avatar" })).toBeVisible();
    await reset("avatar-dialog-reset");
    await expect(page.getByRole("dialog", { name: "Set avatar" })).toHaveCount(0);
    await expect.poll(() => page.evaluate(() => window.__calls.SetAvatar || 0)).toBe(0);

    // A reset while the native picker is open must not allow the crop dialog
    // to mount late under the new generation.
    await connect();
    const lateCropChooser = await chooseFromSelf(/^Set avatar/);
    await reset("avatar-picker-reset");
    await lateCropChooser.setFiles(image);
    await page.waitForTimeout(300);
    await expect(page.getByRole("dialog", { name: "Set avatar" })).toHaveCount(0);
    expect(await page.evaluate(() => window.__calls.SetAvatar || 0)).toBe(0);

    // Server icon compression and quick-emoji upload have no DOM dialog after
    // file selection, so their caller-owned generation tokens block the write.
    await connect();
    const iconChooser = await chooseFromSelf(/^Set server icon/);
    await reset("server-icon-reset");
    await iconChooser.setFiles(image);
    await page.waitForTimeout(300);
    expect(await page.evaluate(() => window.__calls.ServerIconSet || 0)).toBe(0);

    await connect();
    await page.locator("#chat-emoji").click();
    const emojiChooserPromise = page.waitForEvent("filechooser");
    await page.locator(".emoji-upload").click();
    const emojiChooser = await emojiChooserPromise;
    await reset("emoji-picker-reset");
    await emojiChooser.setFiles(image);
    await page.waitForTimeout(300);
    expect(prompts).toEqual([]);
    expect(await page.evaluate(() => window.__calls.EmojiUpload || 0)).toBe(0);
});

test("drops late ban-lift responses without refreshing the new server", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.isAdmin = true;
        window.__bans = { bans: [{
            id: 17,
            value: "old-user",
            reason: "old server ban",
            banned_by: "old-admin",
            expires_at: 0,
        }] };
        window.__banRemoveResult = "old server rejected the lift";
        let release;
        window.__banRemoveGate = new Promise((resolve) => { release = resolve; });
        window.__releaseBanRemove = release;
        window.__voicxPerms.openBanList();
    });
    await expect(page.locator(".ban-lift")).toHaveCount(1);
    await page.locator(".ban-lift").click();
    await expect(page.getByRole("dialog", { name: "Lift ban" })).toBeVisible();
    await page.evaluate(() => {
        for (const callback of window.__events.tab_reset || []) callback("ban-confirm-reset");
    });
    await expect(page.locator(".dlg-overlay")).toHaveCount(0);
    expect(await page.evaluate(() => window.__calls.BanRemove || 0)).toBe(0);

    // Once an old-server write is already in flight it cannot be cancelled,
    // but its response must not toast or schedule a list read on the new tab.
    await page.evaluate(() => {
        window.__voicx.state.isAdmin = true;
        let release;
        window.__banRemoveGate = new Promise((resolve) => { release = resolve; });
        window.__releaseBanRemove = release;
        window.__voicxPerms.openBanList();
    });
    await expect(page.locator(".ban-lift")).toHaveCount(1);
    await page.locator(".ban-lift").click();
    await page.getByRole("dialog", { name: "Lift ban" }).getByRole("button", { name: "Lift", exact: true }).click();
    await expect.poll(() => page.evaluate(() => window.__calls.BanRemove || 0)).toBe(1);
    await expect.poll(() => page.evaluate(() => window.__calls.BanList || 0)).toBe(2);

    await page.evaluate(() => {
        for (const callback of window.__events.tab_reset || []) callback("ban-reset");
        window.__releaseBanRemove();
    });
    await expect(page.locator(".dlg-overlay")).toHaveCount(0);
    await page.waitForTimeout(600);
    expect(await page.evaluate(() => window.__calls.BanList || 0)).toBe(2);
    await expect(page.locator("#toasts")).not.toContainText("old server rejected the lift");
});

test("supports keyboard menus and restores focus after a trapped modal", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
    });

    const tools = page.locator("#menubar > .menu-item > span").filter({ hasText: /^Tools$/ }).locator("..");
    await tools.focus();
    await page.keyboard.press("Enter");
    await expect(tools).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByRole("menuitem", { name: /^Settings/ })).toBeFocused();
    await page.keyboard.press("Enter");

    const dialog = page.getByRole("dialog", { name: "Settings" });
    await expect(dialog).toBeVisible();
    await expect.poll(() => page.locator("#app").evaluate((el) => el.inert)).toBe(true);
    await expect(page.locator("#settings-page-application")).toBeFocused();

    await page.keyboard.press("Shift+Tab");
    await expect(page.locator("#set-apply")).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(page.locator("#settings-page-application")).toBeFocused();

    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(tools).toBeFocused();
    await expect.poll(() => page.locator("#app").evaluate((el) => el.inert)).toBe(false);
});

test("removes an unfinished hotkey capture when settings closes", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.openSettings("hotkeys");
    });
    const capture = page.locator("#settings-content .hotkey-capture").first();
    await capture.click();
    await expect(capture).toHaveClass(/capturing/);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Settings" })).toHaveCount(0);

    const prevented = await page.evaluate(() => {
        const event = new KeyboardEvent("keydown", { key: "K", bubbles: true, cancelable: true });
        return !document.dispatchEvent(event);
    });
    expect(prevented).toBe(false);
});

test("activates tree rows and workspace views from the keyboard with loading feedback", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.myClientID = "client-a";
        window.__voicx.state.myChannelID = 1;
        for (const cb of window.__events.snapshot || []) cb(JSON.stringify({
            root_channels: [{
                ChannelID: 1, ParentID: 0, Name: "Lobby",
                clients: [{ client_id: "client-a", unique_id: "user-a", nickname: "Alice", channel_id: 1 }],
                children: [],
            }],
        }));
    });

    const client = page.locator('.client[data-clid="client-a"]');
    await client.focus();
    await page.keyboard.press("Space");
    await expect(page.locator("body")).not.toHaveClass(/details-collapsed/);
    await expect(client).toHaveAttribute("aria-selected", "true");
    await expect(client).toBeFocused();

    await page.evaluate(() => {
        let release;
        window.__fileListGate = new Promise((resolve) => { release = resolve; });
        window.__releaseFileList = release;
    });
    await page.locator("#tab-chat").focus();
    await page.keyboard.press("ArrowRight");
    await expect(page.locator("#tab-files")).toHaveAttribute("aria-pressed", "true");
    await expect(page.locator("#files-pane")).toHaveAttribute("aria-hidden", "false");
    await expect(page.locator("#files-pane .fb-list")).toHaveAttribute("aria-busy", "true");
    await expect(page.locator('#files-pane .fb-list [role="status"]')).toContainText("Loading channel files");

    await page.evaluate(() => {
        window.__releaseFileList();
        window.__fileListGate = null;
    });
    await expect(page.locator("#files-pane .fb-list")).not.toHaveAttribute("aria-busy", "true");
    await expect(page.locator("#files-pane .empty-state")).toContainText("Empty folder");
    await page.keyboard.press("ArrowLeft");
    await expect(page.locator("#tab-chat")).toHaveAttribute("aria-pressed", "true");
});

test("moves focus explicitly between login and the connected workspace", async ({ page }) => {
    await expect(page.locator("#login-addr")).toBeFocused();
    await expect(page.locator(".skip-link")).toBeHidden();
    await expect(page.locator("#login-serverpw")).toHaveAttribute("autocomplete", "off");
    await expect(page.locator("#login-password")).toHaveAttribute("autocomplete", "current-password");

    await page.locator("#login-nick").fill("Alice");
    await page.getByRole("button", { name: "Connect" }).click();
    await expect(page.locator("#center")).toBeFocused();
    await expect(page.locator("#app")).toHaveAttribute("aria-hidden", "false");
    await expect(page.locator(".skip-link")).toBeAttached();

    await page.evaluate(() => window.__voicx.showLogin());
    await expect(page.locator("#login-addr")).toBeFocused();
    await expect(page.locator("#app")).toHaveAttribute("aria-hidden", "true");

    await page.evaluate(() => {
        window.__tabs = [{
            id: "auto-tab", addr: "auto.example:12333", nickname: "Alice",
            active: true, connected: true, unread: 0, mentions: 0,
        }];
        for (const callback of window.__events.tab_update || []) callback(structuredClone(window.__tabs));
    });
    await expect(page.locator("#center")).toBeFocused();
    await expect(page.locator("#app")).toHaveAttribute("aria-hidden", "false");
});

test("computes names for settings and generated dialog controls", async ({ page }) => {
    await page.evaluate(() => window.__voicx.openSettings("application"));
    await expect(page.locator('#settings-content input[type="number"]').first()).toHaveAccessibleName("Chat max lines");
    await expect(page.locator("#settings-content select").first()).toHaveAccessibleName("Theme (294/295)");
    await expect(page.locator('#settings-content input[type="range"]').first()).toHaveAccessibleName("UI font size");
    await page.keyboard.press("Escape");

    await page.evaluate(() => window.__voicx.showWorkspace());
    await page.locator("#channel-create-btn").click();
    const create = page.getByRole("dialog", { name: "Create channel" });
    await expect(create.locator(".cc-name")).toHaveAccessibleName("Name");
    await expect(create.locator(".cc-type")).toHaveAccessibleName("Type");
    await expect(create.locator(".cc-maxclients")).toHaveAccessibleName("Max clients (0 = unlimited)");
    await page.keyboard.press("Escape");
});

test("closes menus when keyboard focus exits and keeps expansion state in sync", async ({ page }) => {
    await page.evaluate(() => window.__voicx.showWorkspace(false));
    const tools = page.locator("#menubar > .menu-item").filter({ hasText: /^Tools/ });
    await tools.focus();
    await page.keyboard.press("Enter");
    await expect(tools).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByRole("menuitem", { name: /^Settings/ })).toBeFocused();

    await page.keyboard.press("Tab");
    await expect(tools).toHaveAttribute("aria-expanded", "false");
    await expect(tools.locator(".menu-dropdown")).not.toHaveClass(/open/);
});

test("uses standard Left and Right behavior in the channel tree", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.myClientID = "client-a";
        for (const cb of window.__events.snapshot || []) cb(JSON.stringify({
            root_channels: [{
                ChannelID: 1, ParentID: 0, Name: "Parent", clients: [],
                children: [{
                    ChannelID: 2, ParentID: 1, Name: "Child",
                    clients: [{ client_id: "client-a", unique_id: "user-a", nickname: "Alice", channel_id: 2 }],
                    children: [],
                }],
            }],
        }));
    });

    const parent = page.locator('.channel[data-chid="1"]');
    const child = page.locator('.channel[data-chid="2"]');
    await parent.focus();
    await page.keyboard.press("ArrowLeft");
    await expect(parent).toHaveAttribute("aria-expanded", "false");
    await expect(child).toHaveCount(0);

    await page.keyboard.press("ArrowRight");
    await expect(parent).toHaveAttribute("aria-expanded", "true");
    await page.keyboard.press("ArrowRight");
    await expect(child).toBeFocused();
    await page.keyboard.press("ArrowLeft");
    await expect(child).toHaveAttribute("aria-expanded", "false");
    await page.keyboard.press("ArrowLeft");
    await expect(parent).toBeFocused();
});

test("maintains a nested dialog stack across media, rerenders, and zero-control dialogs", async ({ page }) => {
    await page.evaluate(async () => {
        window.__voicx.showWorkspace(false);
        const { mountDialog } = await import("/src/modal.js");
        const launcher = document.getElementById("chat-info-btn");
        launcher.classList.remove("hidden");
        launcher.focus();

        const outer = document.createElement("div");
        outer.className = "dlg-overlay";
        const renderOuter = (step) => {
            outer.innerHTML = `<div class="dlg"><h3>Outer step ${step}</h3><button class="open-media">Open media</button><button class="next-step">Next</button></div>`;
            outer.querySelector(".open-media").onclick = () => {
                const inner = document.createElement("div");
                inner.className = "dlg-overlay";
                inner.innerHTML = '<div class="dlg"><h3>Media preview</h3><video controls aria-label="Preview media"></video></div>';
                mountDialog(inner);
            };
            outer.querySelector(".next-step").onclick = () => renderOuter(step + 1);
        };
        renderOuter(1);
        mountDialog(outer);
    });

    const outer = page.locator('.dlg-overlay[aria-labelledby]:has-text("Outer step")');
    await expect(page.getByRole("dialog", { name: "Outer step 1" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Open media" })).toBeFocused();
    await page.getByRole("button", { name: "Open media" }).click();
    await expect(page.getByRole("dialog", { name: "Media preview" })).toBeVisible();
    await expect(page.getByLabel("Preview media")).toBeFocused();
    await expect(outer).toHaveJSProperty("inert", true);

    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Media preview" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Open media" })).toBeFocused();
    await page.getByRole("button", { name: "Next" }).click();
    await expect(page.getByRole("dialog", { name: "Outer step 2" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Open media" })).toBeFocused();

    await page.keyboard.press("Escape");
    await expect(page.locator("#chat-info-btn")).toBeFocused();
    await page.evaluate(async () => {
        const { mountDialog } = await import("/src/modal.js");
        const empty = document.createElement("div");
        empty.className = "dlg-overlay";
        empty.innerHTML = '<div class="dlg"><h3>Working</h3><p>Please wait.</p></div>';
        mountDialog(empty);
    });
    const zero = page.getByRole("dialog", { name: "Working" });
    await expect(zero).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(zero).toHaveCount(0);
    await expect(page.locator("#chat-info-btn")).toBeFocused();
});

test("falls back from invalid modal focus and restores login after a workspace transition", async ({ page }) => {
    await page.evaluate(async () => {
        window.__voicx.showWorkspace(false);
        const { mountDialog } = await import("/src/modal.js");
        const launcher = document.getElementById("chat-info-btn");
        launcher.classList.remove("hidden");
        launcher.focus();
        const overlay = document.createElement("div");
        overlay.className = "dlg-overlay transition-dialog";
        overlay.innerHTML = `
            <div class="dlg">
                <h3>Transition focus</h3>
                <div hidden><button class="hidden-target">Hidden target</button></div>
                <button class="visible-target">Visible target</button>
            </div>`;
        mountDialog(overlay, { launcher, initialFocus: ".hidden-target" });
    });
    await expect(page.locator(".visible-target")).toBeFocused();

    await page.evaluate(() => window.__voicx.showLogin());
    await page.keyboard.press("Escape");
    await expect(page.locator(".transition-dialog")).toHaveCount(0);
    await expect(page.locator("#login-addr")).toBeFocused();
});

test("finalizes a dialog removed inside an ancestor subtree exactly once", async ({ page }) => {
    await page.evaluate(async () => {
        window.__voicx.showWorkspace(false);
        const { mountDialog } = await import("/src/modal.js");
        window.__subtreeDialogCloses = 0;
        const wrapper = document.createElement("section");
        wrapper.id = "dialog-wrapper";
        document.body.appendChild(wrapper);
        const overlay = document.createElement("div");
        overlay.className = "dlg-overlay subtree-dialog";
        overlay.innerHTML = '<div class="dlg"><h3>Subtree dialog</h3><button>Ready</button></div>';
        mountDialog(overlay, { onClose: () => { window.__subtreeDialogCloses++; } });
        wrapper.appendChild(overlay);
    });
    await expect(page.getByRole("dialog", { name: "Subtree dialog" })).toBeVisible();
    await page.evaluate(() => document.getElementById("dialog-wrapper").remove());
    await expect(page.locator(".subtree-dialog")).toHaveCount(0);
    await expect.poll(() => page.evaluate(() => window.__subtreeDialogCloses)).toBe(1);
    await page.waitForTimeout(100);
    expect(await page.evaluate(() => window.__subtreeDialogCloses)).toBe(1);
});

test("keeps a blocking gate visually and semantically above deferred dialogs", async ({ page }) => {
    await page.evaluate(async () => {
        window.__voicx.showWorkspace(false);
        for (const callback of window.__events.server_rules || []) {
            callback(JSON.stringify({ hash: "rules-v1", text: "Be excellent to each other." }));
        }
        const { mountDialog } = await import("/src/modal.js");
        const deferred = document.createElement("div");
        deferred.className = "dlg-overlay deferred-dialog";
        deferred.innerHTML = '<div class="dlg"><h3>Deferred reminder</h3><button>Continue</button></div>';
        mountDialog(deferred);
    });

    const gate = page.locator(".server-rules-gate");
    const deferred = page.locator(".deferred-dialog");
    await expect(gate).toHaveAttribute("aria-modal", "true");
    await expect(gate).not.toHaveAttribute("aria-hidden", "true");
    await expect(deferred).toHaveAttribute("aria-hidden", "true");
    await expect.poll(() => gate.evaluate((element) => element.inert)).toBe(false);
    await expect.poll(() => deferred.evaluate((element) => element.inert)).toBe(true);
    const [gateZ, deferredZ, skipLinkZ] = await page.evaluate(() => [
        Number(getComputedStyle(document.querySelector(".server-rules-gate")).zIndex),
        Number(getComputedStyle(document.querySelector(".deferred-dialog")).zIndex),
        Number(getComputedStyle(document.querySelector(".skip-link")).zIndex),
    ]);
    expect(gateZ).toBeGreaterThan(deferredZ);
    expect(gateZ).toBeGreaterThan(skipLinkZ);
    await expect(page.getByRole("button", { name: "Decline and disconnect" })).toBeFocused();

    await page.evaluate(() => window.__voicxNotify.resetServerRules());
    await expect(gate).toHaveCount(0);
    await expect(deferred).toHaveAttribute("aria-modal", "true");
    await expect(page.getByRole("button", { name: "Continue" })).toBeFocused();
    await page.keyboard.press("Escape");
});

test("Escape runs polling-dialog cleanup and allows stateful dialogs to reopen", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.showWorkspace(false);
        window.__voicx.state.myClientID = "client-a";
        window.__voicx.openClientInfo({ client_id: "client-a", unique_id: "user-a", nickname: "Alice" });
    });
    await expect(page.getByRole("dialog", { name: "Connection Info" })).toBeVisible();
    await expect.poll(() => page.evaluate(() => window.__calls.GetClientInfo || 0)).toBeGreaterThan(0);
    await page.keyboard.press("Escape");
    const clientInfoCalls = await page.evaluate(() => window.__calls.GetClientInfo || 0);
    await page.waitForTimeout(2200);
    expect(await page.evaluate(() => window.__calls.GetClientInfo || 0)).toBe(clientInfoCalls);

    await page.evaluate(() => window.__voicxFiles.openTransfers());
    await expect(page.getByRole("dialog", { name: "Transfers" })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Transfers" })).toHaveCount(0);
    await page.evaluate(() => window.__voicxFiles.openTransfers());
    await expect(page.getByRole("dialog", { name: "Transfers" })).toBeVisible();
    await page.keyboard.press("Escape");

    await page.evaluate(() => window.__voicxMeta.openStatsPage());
    await expect(page.getByRole("dialog", { name: "Connection stats" })).toBeVisible();
    await page.keyboard.press("Escape");
    const statsCalls = await page.evaluate(() => window.__calls.GetClientInfo || 0);
    await page.waitForTimeout(1200);
    expect(await page.evaluate(() => window.__calls.GetClientInfo || 0)).toBe(statsCalls);
    await page.evaluate(() => window.__voicxMeta.openStatsPage());
    await expect(page.getByRole("dialog", { name: "Connection stats" })).toBeVisible();
});

test("keeps onboarding semantics and focus when each step rerenders", async ({ page }) => {
    await page.evaluate(() => {
        window.__voicx.state.settings.onboarding_done = false;
        window.__voicxMeta.maybeOnboard();
    });
    await expect(page.getByRole("dialog", { name: "Welcome to voicx" })).toBeVisible();
    await expect(page.locator(".ob-nick")).toBeFocused();
    await page.getByRole("button", { name: "Next" }).click();
    await expect(page.getByRole("dialog", { name: "Microphone check" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Open capture settings" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.locator(".onboarding")).toHaveCount(0);
    await expect.poll(() => page.evaluate(() => window.__voicx.state.settings.onboarding_done)).toBe(true);
});

test("debounces keyboard pane persistence and refreshes separator values", async ({ page }) => {
    await page.evaluate(() => window.__voicx.showWorkspace(false));
    const handle = page.getByRole("separator", { name: "Resize channels pane" });
    await handle.focus();
    const before = await page.evaluate(() => window.__calls.SaveSettings || 0);
    await page.keyboard.press("ArrowRight");
    await page.keyboard.press("ArrowRight");
    await page.keyboard.press("ArrowRight");
    await expect.poll(() => page.evaluate(() => window.__calls.SaveSettings || 0)).toBe(before + 1);
    await expect.poll(() => handle.evaluate((element) =>
        Number(element.getAttribute("aria-valuenow")) - Math.round(element.parentElement.getBoundingClientRect().width),
    )).toBe(0);
});
