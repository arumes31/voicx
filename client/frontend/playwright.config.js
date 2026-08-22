import { defineConfig } from "@playwright/test";

const ci = Boolean(process.env.CI);

export default defineConfig({
    testDir: "./tests",
    timeout: 30_000,
    retries: ci ? 2 : 0,
    reporter: ci ? [["github"], ["html", { open: "never" }]] : "list",
    use: {
        baseURL: "http://127.0.0.1:12364",
        headless: true,
        trace: "on-first-retry",
        screenshot: "only-on-failure",
    },
    webServer: {
        command: "npm run dev -- --host 127.0.0.1 --port 12364",
        url: "http://127.0.0.1:12364",
        reuseExistingServer: !process.env.CI,
    },
});
