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
                    if (method === "GetSettings") return structuredClone(initialSettings);
                    if (method === "SaveSettings") { window.__savedSettings = structuredClone(args[0]); return ""; }
                    if (method === "ListTabs") return structuredClone(window.__tabs);
                    if (method === "ClientID") return window.__activeClient || "client-a";
                    if (method === "IsAdmin") return true;
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
            { id: "tab-a", addr: "a.example:10011", nickname: "alice", active: true, connected: true, unread: 0, mentions: 0 },
            { id: "tab-b", addr: "b.example:10011", nickname: "bob", active: false, connected: true, unread: 2, mentions: 1 },
        ];
        for (const cb of window.__events.tab_update || []) cb(structuredClone(window.__tabs));
        for (const cb of window.__events.tab_reset || []) cb("tab-a");
    });
    await expect(page.locator('.srv-tab[data-tab-id="tab-a"]')).toHaveClass(/active/);
    await page.locator('.srv-tab[data-tab-id="tab-b"]').click();
    await expect(page.locator('.srv-tab[data-tab-id="tab-b"]')).toHaveClass(/active/);
    await expect.poll(() => page.evaluate(() => window.__voicx.state.myClientID)).toBe("client-b");
});
