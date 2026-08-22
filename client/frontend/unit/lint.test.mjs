import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { htmlTicketSuffixes, uiTicketSuffixes, unsafeConsoleMethods } from "../scripts/lint.mjs";

describe("frontend console lint rule", () => {
    it("rejects executable global console methods except warn and error", () => {
        assert.deepEqual(unsafeConsoleMethods("console.log('x'); console.info('y'); console.warn('ok'); console.error('ok');"), ["log", "info"]);
    });

    it("catches optional, computed, and parenthesized console calls", () => {
        assert.deepEqual(unsafeConsoleMethods(`
            console?.log("optional");
            console["info"]("computed static");
            (console.debug)("parenthesized");
            console[method]("computed dynamic");
            console["warn"]("allowed");
            console.error("allowed");
        `), ["log", "info", "debug", "computed"]);
    });

    it("does not flag documentation, strings, templates, regexes, property names, or shadowed console", () => {
        assert.deepEqual(unsafeConsoleMethods(`
            // console.log("documentation")
            /* console.info("documentation") */
            const one = "console.debug('text')";
            const two = 'console.table("text")';
            const three = \`console.trace("text")\`;
            const pattern = /console\\.log\\(/;
            foo.console.log("property");
            {
                const console = { log() {} };
                console.log("local");
            }
        `), []);
    });

    it("still rejects calls inside template interpolation expressions", () => {
        assert.deepEqual(unsafeConsoleMethods("const message = `before ${console.debug('x')} after`;"), ["debug"]);
    });
});

describe("frontend UI ticket-suffix lint rule", () => {
    it("finds static ticket IDs at titles, templates, and UI helper sinks", () => {
        const findings = uiTicketSuffixes(`
            button.title = "Manage custom emoji (272)";
            panel.innerHTML = \`<th>custom beep (384)</th>\`;
            row("Theme (294/295)", control);
            hint("The identity backup is portable (353).");
            menuAction("Compact mode (293)", toggle);
            modal("permissions", \`<input placeholder="filter permissions (154)">\`);
            V().toast("Copy failed (280)");
        `);
        assert.deepEqual(findings.map((finding) => finding.suffix), ["(272)", "(384)", "(294/295)", "(353)", "(293)", "(154)", "(280)"]);
    });

    it("ignores comments and legitimate numeric semantics", () => {
        const findings = uiTicketSuffixes(`
            // Keep implementation context (346/348).
            row("Watch threshold (0 = off)", input);
            hint("Bans use 0 = permanent and ports use 12333.");
            const example = "Not a UI sink (346)";
        `);
        assert.deepEqual(findings, []);
    });

    it("finds ticket IDs in translation catalog values", () => {
        const findings = uiTicketSuffixes('const de = { "settings.language": "Sprache (336)" };');
        assert.deepEqual(findings.map((finding) => finding.suffix), ["(336)"]);
    });

    it("does not descend implementation callbacks passed to UI helpers", () => {
        const findings = uiTicketSuffixes(`
            menuAction("Settings", () => {
                const internal = "implementation token (293)";
            });
        `);
        assert.deepEqual(findings, []);
    });

    it("finds visible and attributed ticket IDs in HTML but ignores comments", () => {
        const findings = htmlTicketSuffixes(`
            <!-- Keep the implementation note (346). -->
            <button title="Open settings (113)">Settings (113)</button>
            <input aria-label="Filter permissions (154)" placeholder="Search (154)">
        `);
        assert.deepEqual(findings.map((finding) => finding.suffix), ["(113)", "(113)", "(154)", "(154)"]);
    });
});
