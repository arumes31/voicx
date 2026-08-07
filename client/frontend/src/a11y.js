// Small accessibility helpers shared by keyboard and form interactions.
// They avoid browser globals so the edge cases remain easy to regression-test
// without a browser-only test dependency.

let controlLabelSerial = 0;

export function isActivationKey(key) {
    return key === "Enter" || key === " ";
}

export function wrappedIndex(current, count, delta) {
    if (!Number.isInteger(count) || count <= 0) return -1;
    const start = Number.isInteger(current) && current >= 0 && current < count
        ? current
        : (delta < 0 ? 0 : -1);
    return (start + delta + count) % count;
}

export function dialogLabelFromID(id) {
    const words = String(id || "")
        .replace(/[-_](overlay|dialog|modal)$/, "")
        .split(/[\s_-]+/)
        .filter((word) => word && !["dlg", "overlay", "dialog", "modal", "wide"].includes(word.toLowerCase()));
    if (words.length === 0) return "Dialog";
    return words.map((word) => word[0].toUpperCase() + word.slice(1)).join(" ");
}

// Associate a visual label with a sibling form control. Nested controls are
// already labelled by native HTML, and an explicit existing `for` always wins.
export function associateControlLabel(label, control) {
    if (!label || !control || label.contains(control) || label.htmlFor) return false;
    if (!control.id) control.id = `voicx-control-${++controlLabelSerial}`;
    label.htmlFor = control.id;
    return true;
}
