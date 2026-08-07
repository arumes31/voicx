import test from "node:test";
import assert from "node:assert/strict";

import { associateControlLabel, dialogLabelFromID, isActivationKey, wrappedIndex } from "../src/a11y.js";

test("activation keys match native button keyboard behavior", () => {
    assert.equal(isActivationKey("Enter"), true);
    assert.equal(isActivationKey(" "), true);
    assert.equal(isActivationKey("Spacebar"), false);
    assert.equal(isActivationKey("Escape"), false);
});

test("wrapped keyboard navigation cycles in either direction", () => {
    assert.equal(wrappedIndex(0, 3, 1), 1);
    assert.equal(wrappedIndex(2, 3, 1), 0);
    assert.equal(wrappedIndex(0, 3, -1), 2);
    assert.equal(wrappedIndex(-1, 3, 1), 0);
    assert.equal(wrappedIndex(-1, 3, -1), 2);
    assert.equal(wrappedIndex(0, 0, 1), -1);
});

test("dialog fallback labels are human-readable and deterministic", () => {
    assert.equal(dialogLabelFromID("settings-overlay"), "Settings");
    assert.equal(dialogLabelFromID("token_redeem_dialog"), "Token Redeem");
    assert.equal(dialogLabelFromID("dlg avatar-full"), "Avatar Full");
    assert.equal(dialogLabelFromID(""), "Dialog");
});

test("visual sibling labels receive a stable native control association", () => {
    const label = { htmlFor: "", contains: () => false };
    const generatedControl = { id: "" };
    assert.equal(associateControlLabel(label, generatedControl), true);
    assert.match(generatedControl.id, /^voicx-control-\d+$/);
    assert.equal(label.htmlFor, generatedControl.id);

    const existingLabel = { htmlFor: "", contains: () => false };
    const existingControl = { id: "quality-preset" };
    assert.equal(associateControlLabel(existingLabel, existingControl), true);
    assert.equal(existingLabel.htmlFor, "quality-preset");
});

test("native or explicit label associations are never overwritten", () => {
    const control = { id: "control" };
    assert.equal(associateControlLabel(null, control), false);
    assert.equal(associateControlLabel({ htmlFor: "", contains: () => false }, null), false);
    assert.equal(associateControlLabel({ htmlFor: "", contains: () => true }, control), false);
    assert.equal(associateControlLabel({ htmlFor: "existing", contains: () => false }, control), false);
});
