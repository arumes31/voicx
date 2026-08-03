import assert from "node:assert/strict";
import { afterEach, describe, it, mock } from "node:test";

import { applyStaticLabels, currentLanguage, setLanguage, t } from "../src/i18n.js";

const originalDocument = globalThis.document;
const originalNavigator = globalThis.navigator;

afterEach(() => {
    setLanguage("en");
    if (originalDocument === undefined) delete globalThis.document;
    else globalThis.document = originalDocument;
    Object.defineProperty(globalThis, "navigator", {
        configurable: true,
        value: originalNavigator,
    });
    mock.restoreAll();
});

describe("language selection and translation", () => {
    it("switches between English and German", () => {
        setLanguage("de");
        assert.equal(currentLanguage(), "de");
        assert.equal(t("menu.connections"), "Verbindungen");

        setLanguage("en");
        assert.equal(currentLanguage(), "en");
        assert.equal(t("menu.connections"), "Connections");
    });

    it("uses the system locale and falls back from unsupported languages", () => {
        Object.defineProperty(globalThis, "navigator", {
            configurable: true,
            value: { language: "de-AT" },
        });
        setLanguage("system");
        assert.equal(currentLanguage(), "de");

        Object.defineProperty(globalThis, "navigator", {
            configurable: true,
            value: { language: "fr-FR" },
        });
        setLanguage();
        assert.equal(currentLanguage(), "en");

        setLanguage("fr");
        assert.equal(currentLanguage(), "en");
    });

    it("substitutes all supplied placeholders", () => {
        setLanguage("de");
        assert.equal(
            t("status.retry", { n: 2, max: 5, s: 8 }),
            "Versuch 2/5 in 8s…",
        );
    });

    it("warns only once for a missing key and returns the key", () => {
        const warn = mock.method(console, "warn", () => {});
        assert.equal(t("missing.unit.key"), "missing.unit.key");
        assert.equal(t("missing.unit.key"), "missing.unit.key");
        assert.equal(warn.mock.callCount(), 1);
        assert.deepEqual(warn.mock.calls[0].arguments, ["[i18n] missing key:", "missing.unit.key"]);
    });
});

describe("applyStaticLabels", () => {
    it("updates the login labels, button, and recent-servers heading", () => {
        setLanguage("de");
        const labels = Array.from({ length: 4 }, () => ({ firstChild: { textContent: "old" } }));
        const connect = { textContent: "old" };
        const paneHeads = [{ textContent: "other" }, { textContent: "old" }];
        globalThis.document = {
            getElementById(id) {
                return id === "login-connect" ? connect : null;
            },
            querySelectorAll(selector) {
                if (selector === ".login-card label") return labels;
                if (selector === ".login-card .pane-head") return paneHeads;
                return [];
            },
        };

        applyStaticLabels();

        assert.deepEqual(
            labels.map((label) => label.firstChild.textContent),
            ["SERVER ", "SPITZNAME ", "PASSWORT ", "SERVER-PASSWORT "],
        );
        assert.equal(connect.textContent, "VERBINDEN");
        assert.equal(paneHeads[1].textContent, "LETZTE SERVER");
    });

    it("tolerates missing optional DOM elements", () => {
        globalThis.document = {
            getElementById() { return null; },
            querySelectorAll() { return []; },
        };
        assert.doesNotThrow(() => applyStaticLabels());
    });
});
