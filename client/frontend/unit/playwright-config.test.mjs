import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";
import test from "node:test";

const frontendRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const configScript = `import config from "./playwright.config.js";
console.log(JSON.stringify({ retries: config.retries, reporter: config.reporter, trace: config.use.trace, screenshot: config.use.screenshot }));`;

function readConfig(ci) {
    const env = { ...process.env };
    if (ci) env.CI = "1";
    else delete env.CI;
    return JSON.parse(execFileSync(process.execPath, ["--input-type=module", "--eval", configScript], {
        cwd: frontendRoot,
        encoding: "utf8",
        env,
    }));
}

test("uses local and CI Playwright retry/reporting policies", () => {
    assert.deepEqual(readConfig(false), {
        retries: 0,
        reporter: "list",
        trace: "on-first-retry",
        screenshot: "only-on-failure",
    });
    assert.deepEqual(readConfig(true), {
        retries: 2,
        reporter: [["github"], ["html", { open: "never" }]],
        trace: "on-first-retry",
        screenshot: "only-on-failure",
    });
});
