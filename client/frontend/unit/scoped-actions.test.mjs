import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { captureScope, runScopedDialogAction, scopeIsCurrent } from "../src/scoped-actions.js";

describe("scoped dialog actions", () => {
    it("freezes the exact scope captured before asynchronous work", () => {
        const scope = captureScope(() => ({ generation: 4, channelID: 1, folder: "drafts" }));
        assert.deepEqual(scope, { generation: 4, channelID: 1, folder: "drafts" });
        assert.equal(Object.isFrozen(scope), true);
        assert.equal(scopeIsCurrent(scope, () => ({ generation: 4, channelID: 1, folder: "drafts" })), true);
        assert.equal(scopeIsCurrent(scope, () => ({ generation: 4, channelID: 2, folder: "drafts" })), false);
        assert.equal(scopeIsCurrent(scope, () => ({ generation: 4, channelID: 1, folder: "drafts", extra: true })), false);
    });

    it("does not call the backend after a deferred confirmation crosses a view change", async () => {
        let current = { generation: 9, channelID: 1, folder: "old" };
        let resolveDialog;
        const calls = [];
        const pending = runScopedDialogAction({
            readScope: () => current,
            openDialog: () => new Promise((resolve) => { resolveDialog = resolve; }),
            isAccepted: Boolean,
            perform: async (scope, value) => { calls.push([scope, value]); },
        });
        current = { generation: 9, channelID: 2, folder: "" };
        resolveDialog(true);
        assert.deepEqual(await pending, {
            performed: false,
            scope: { generation: 9, channelID: 1, folder: "old" },
            value: true,
        });
        assert.deepEqual(calls, []);
    });

    it("passes only the captured scope to accepted backend work", async () => {
        let current = { generation: 2, channelID: 7, folder: "reports" };
        const calls = [];
        const result = await runScopedDialogAction({
            readScope: () => current,
            openDialog: async () => "renamed.txt",
            isAccepted: (value) => value.length > 0,
            perform: async (scope, value) => {
                current = { generation: 2, channelID: 99, folder: "new" };
                calls.push([scope.channelID, scope.folder, value]);
            },
        });
        assert.equal(result.performed, true);
        assert.deepEqual(calls, [[7, "reports", "renamed.txt"]]);

        const rejected = await runScopedDialogAction({
            readScope: () => current,
            openDialog: async () => "",
            isAccepted: Boolean,
            perform: async () => { throw new Error("must not run"); },
        });
        assert.equal(rejected.performed, false);
    });
});
