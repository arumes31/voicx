// Shared modal lifecycle and focus stack.
//
// Dialog authors should use mountDialog() and put all resource teardown in
// onClose. That callback runs exactly once whether the dialog is closed by a
// button, its backdrop, Escape, or a direct DOM removal.
import { associateControlLabel, dialogLabelFromID } from "./a11y.js";

const records = new WeakMap();
const stack = [];
let observer = null;
let titleSerial = 0;
let initialized = false;
const backgroundInertState = new Map();
// Stay above every fixed workspace affordance (including the focused skip
// link); stack order then adds one layer per live dialog.
const dialogZIndexBase = 20000;
const labelableControlSelector = "input:not([type=hidden]), select, textarea";

export const dialogFocusableSelector = [
    "button",
    "[href]",
    "input",
    "select",
    "textarea",
    "summary",
    "audio[controls]",
    "video[controls]",
    "[contenteditable=true]",
    '[tabindex]:not([tabindex="-1"])',
].join(", ");

function visibleFocusableElements(container) {
    return [...container.querySelectorAll(dialogFocusableSelector)].filter((element) => {
        if (element.tabIndex < 0) return false;
        return canFocusTarget(element);
    });
}

export function canFocusTarget(target) {
    if (!(target instanceof HTMLElement) || !target.isConnected || target.disabled) return false;
    for (let element = target; element; element = element.parentElement) {
        if (element.hidden || element.inert || element.getAttribute("aria-hidden") === "true") return false;
    }
    return target.getClientRects().length > 0;
}

function focusTarget(target) {
    if (!canFocusTarget(target)) return false;
    target.focus({ preventScroll: true });
    return document.activeElement === target;
}

function titleElement(overlay) {
    return overlay.querySelector("h1, h2, h3, [role=heading]");
}

function syncControlLabels(overlay) {
    for (const label of overlay.querySelectorAll("label:not([for])")) {
        if (label.querySelector(labelableControlSelector)) continue;
        const sibling = label.nextElementSibling;
        let control = sibling?.matches?.(labelableControlSelector) ? sibling : null;
        if (!control && label.classList.contains("set-label")) {
            const nested = sibling?.querySelectorAll?.(labelableControlSelector) || [];
            if (nested.length === 1) [control] = nested;
        }
        if (control) associateControlLabel(label, control);
    }
}

function syncDialogSemantics(overlay) {
    syncControlLabels(overlay);
    overlay.setAttribute("role", "dialog");
    if (!overlay.hasAttribute("tabindex")) overlay.tabIndex = -1;
    if (overlay.hasAttribute("aria-label")) return;
    const title = titleElement(overlay);
    if (title) {
        if (!title.id) title.id = `voicx-dialog-title-${++titleSerial}`;
        overlay.setAttribute("aria-labelledby", title.id);
    } else {
        overlay.removeAttribute("aria-labelledby");
        const visual = overlay.firstElementChild;
        overlay.setAttribute("aria-label", dialogLabelFromID(overlay.id || visual?.className || ""));
    }
}

function topRecord() {
    return [...stack].reverse().find((record) => record.active && record.overlay.isConnected) || null;
}

function activeServerGeneration() {
    return Number(globalThis.window?.__voicx?.state?.serverGeneration || 0);
}

function serverGenerationFor(options) {
    if (options.serverScoped === true) return activeServerGeneration();
    if (options.serverScoped === false) return null;
    return topRecord()?.serverGeneration ?? null;
}

export function dialogStackInsertionIndex(blockingStates, incomingBlocking) {
    if (incomingBlocking) return blockingStates.length;
    const index = blockingStates.indexOf(true);
    return index < 0 ? blockingStates.length : index;
}

function resolveTarget(target) {
    let resolved = target;
    if (!resolved?.isConnected && target?.closest?.(".menu-item")) {
        const menu = target.closest(".menu-item");
        const label = menu.querySelector(":scope > span")?.textContent;
        resolved = [...document.querySelectorAll("#menubar > .menu-item")]
            .find((item) => item.querySelector(":scope > span")?.textContent === label);
    }
    return canFocusTarget(resolved) ? resolved : null;
}

function syncStack() {
    const live = stack.filter((record) => record.active && record.overlay.isConnected);
    const top = live.at(-1);
    if (top) {
        for (const element of document.body.children) {
            if (!(element instanceof HTMLElement) || element.classList.contains("dlg-overlay")) continue;
            if (live.some((record) => element.contains(record.overlay))) continue;
            if (!backgroundInertState.has(element)) backgroundInertState.set(element, element.inert);
            element.inert = true;
        }
    } else {
        for (const [element, wasInert] of backgroundInertState) {
            if (element.isConnected) element.inert = wasInert;
        }
        backgroundInertState.clear();
    }
    for (const [index, record] of live.entries()) {
        const active = record === top;
        record.overlay.style.zIndex = String(dialogZIndexBase + index);
        record.overlay.inert = !active;
        record.overlay.setAttribute("aria-modal", String(active));
        if (active) record.overlay.removeAttribute("aria-hidden");
        else record.overlay.setAttribute("aria-hidden", "true");
    }
}

function initialFocus(record) {
    if (!record.active || record !== topRecord() || !record.overlay.isConnected) return false;
    syncDialogSemantics(record.overlay);
    let preferred = null;
    if (typeof record.initialFocus === "function") preferred = record.initialFocus(record.overlay);
    else if (typeof record.initialFocus === "string") preferred = record.overlay.querySelector(record.initialFocus);
    else if (record.initialFocus instanceof HTMLElement) preferred = record.initialFocus;
    const candidates = [
        preferred,
        record.overlay.querySelector("[autofocus]"),
        ...visibleFocusableElements(record.overlay),
        record.overlay,
    ];
    for (const target of new Set(candidates)) {
        if (focusTarget(target)) return true;
    }
    return false;
}

function addToStack(record) {
    const oldIndex = stack.indexOf(record);
    if (oldIndex >= 0) stack.splice(oldIndex, 1);
    const blockingStates = stack.map((item) => item.active && item.overlay.isConnected && item.overlay.dataset.blocking === "true");
    const insertionIndex = dialogStackInsertionIndex(blockingStates, record.overlay.dataset.blocking === "true");
    stack.splice(insertionIndex, 0, record);
    syncDialogSemantics(record.overlay);
    syncStack();
    requestAnimationFrame(() => initialFocus(record));
}

function adoptDialog(overlay) {
    if (records.has(overlay)) {
        const record = records.get(overlay);
        if (record.active && !stack.includes(record)) addToStack(record);
        return record;
    }
    const active = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const record = {
        overlay,
        launcher: active?.closest?.(".menu-item") || active,
        lastFocused: null,
        initialFocus: null,
        onCancel: null,
        onClose: null,
        active: true,
        closing: false,
        serverGeneration: topRecord()?.serverGeneration ?? null,
    };
    records.set(overlay, record);
    addToStack(record);
    return record;
}

function restoreFocus(record) {
    const underlying = [...stack].reverse().find((item) => item.active && item.overlay.isConnected);
    const captured = resolveTarget(record.launcher);
    if (underlying) {
        const nestedTarget = captured && underlying.overlay.contains(captured)
            ? captured
            : resolveTarget(underlying.lastFocused);
        requestAnimationFrame(() => {
            if (!focusTarget(nestedTarget) && !initialFocus(underlying)) focusFallback();
        });
        return;
    }
    if (captured) {
        requestAnimationFrame(() => {
            if (!focusTarget(captured)) focusFallback();
        });
        return;
    }
    requestAnimationFrame(focusFallback);
}

function focusFallback() {
    const login = document.getElementById("login-overlay");
    if (login && !login.classList.contains("hidden")) {
        if (focusTarget(document.getElementById("login-addr"))) return true;
    }
    const app = document.getElementById("app");
    if (app && !app.classList.contains("hidden")) return focusTarget(document.getElementById("center"));
    return false;
}

function finalize(record, reason) {
    if (!record?.active) return;
    record.active = false;
    record.closing = false;
    const index = stack.indexOf(record);
    if (index >= 0) stack.splice(index, 1);
    try {
        try {
            record.onClose?.(reason);
        } catch (error) {
            // One dialog's cleanup must not abort a MutationObserver batch and
            // strand the remaining removed dialogs in the focus/inert stack.
            console.error("dialog cleanup failed", error);
        }
    } finally {
        syncStack();
        restoreFocus(record);
    }
}

export function registerDialogLifecycle(overlay, options = {}) {
    const current = records.get(overlay);
    if (current?.active) {
        Object.assign(current, options);
        if (Object.hasOwn(options, "serverScoped")) current.serverGeneration = serverGenerationFor(options);
        syncDialogSemantics(overlay);
        return current;
    }
    const active = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const record = {
        overlay,
        launcher: options.launcher || active?.closest?.(".menu-item") || active,
        lastFocused: null,
        initialFocus: options.initialFocus || null,
        onCancel: options.onCancel || null,
        onClose: options.onClose || null,
        active: true,
        closing: false,
        serverGeneration: serverGenerationFor(options),
    };
    records.set(overlay, record);
    if (overlay.isConnected) addToStack(record);
    return record;
}

export function mountDialog(overlay, options = {}) {
    registerDialogLifecycle(overlay, options);
    if (!overlay.isConnected) document.body.appendChild(overlay);
    const record = records.get(overlay);
    if (!stack.includes(record)) addToStack(record);
    return overlay;
}

// Server-scoped dialogs contain identifiers owned by the currently active
// backend tab. Nested dialogs inherit the same scope automatically.
export function mountServerDialog(overlay, options = {}) {
    return mountDialog(overlay, { ...options, serverScoped: true });
}

// Async dialog work must check this after every await before it mutates UI or
// starts a follow-up backend call with identifiers captured before a tab reset.
export function isCurrentServerDialog(overlay) {
    const record = records.get(overlay);
    return !!record?.active && overlay.isConnected &&
        record.serverGeneration === activeServerGeneration();
}

// Close top-down so nested resource cleanup runs before its owning dialog.
export function closeServerDialogs(reason = "server-change") {
    const scoped = [...stack].reverse().filter((record) =>
        record.active && record.overlay.isConnected && record.serverGeneration !== null);
    for (const record of scoped) closeDialog(record.overlay, reason);
    return scoped.length;
}

export function closeDialog(overlay, reason = "close") {
    const record = records.get(overlay) || adoptDialog(overlay);
    if (!record?.active || record.closing) return false;
    if (reason === "cancel" && overlay.dataset.blocking === "true") return false;
    record.closing = true;
    if (reason === "cancel") {
        const legacy = new Event("voicx-dialog-cancel", { cancelable: true });
        const legacyAllowed = overlay.dispatchEvent(legacy);
        if (!record.active) return true;
        if (!legacyAllowed && !record.onCancel) {
            record.closing = false;
            return false;
        }
        if (record.onCancel?.() === false) {
            record.closing = false;
            return false;
        }
    }
    if (overlay.isConnected) overlay.remove();
    finalize(record, reason);
    return true;
}

export function topDialog() {
    return topRecord()?.overlay || null;
}

function trapTab(event, record) {
    const items = visibleFocusableElements(record.overlay);
    if (items.length === 0) {
        event.preventDefault();
        focusTarget(record.overlay);
        return;
    }
    const index = items.indexOf(document.activeElement);
    const next = event.shiftKey
        ? (index <= 0 ? items.length - 1 : index - 1)
        : (index < 0 || index === items.length - 1 ? 0 : index + 1);
    event.preventDefault();
    focusTarget(items[next]);
}

function dialogsInSubtree(node) {
    if (!(node instanceof Element)) return [];
    const dialogs = [];
    if (node.matches?.(".dlg-overlay")) dialogs.push(node);
    dialogs.push(...(node.querySelectorAll?.(".dlg-overlay") || []));
    return dialogs;
}

export function initModalSystem() {
    if (initialized) return;
    initialized = true;
    document.addEventListener("focusin", (event) => {
        const top = topRecord();
        if (top?.overlay.contains(event.target)) top.lastFocused = event.target;
    });
    document.addEventListener("keydown", (event) => {
        const top = topRecord();
        if (!top) return;
        if (event.key === "Escape") {
            event.preventDefault();
            event.stopImmediatePropagation();
            closeDialog(top.overlay, "cancel");
        } else if (event.key === "Tab") {
            trapTab(event, top);
        }
    }, true);
    observer = new MutationObserver((mutations) => {
        const changed = new Set();
        for (const mutation of mutations) {
            for (const node of mutation.addedNodes) {
                if (!(node instanceof Element)) continue;
                for (const overlay of dialogsInSubtree(node)) {
                    if (overlay.isConnected) adoptDialog(overlay);
                    changed.add(overlay);
                }
                const owner = node.closest?.(".dlg-overlay");
                if (owner) changed.add(owner);
            }
            for (const node of mutation.removedNodes) {
                if (!(node instanceof Element)) continue;
                for (const overlay of dialogsInSubtree(node).reverse()) {
                    const record = records.get(overlay);
                    if (record?.active && !overlay.isConnected) finalize(record, "removed");
                }
                const owner = mutation.target instanceof Element
                    ? mutation.target.closest?.(".dlg-overlay")
                    : null;
                if (owner) changed.add(owner);
            }
        }
        for (const overlay of changed) {
            const record = records.get(overlay);
            if (!record?.active || !overlay.isConnected) continue;
            syncDialogSemantics(overlay);
            if (record === topRecord() && !overlay.contains(document.activeElement)) {
                requestAnimationFrame(() => initialFocus(record));
            }
        }
        if (topDialog()) syncStack();
    });
    observer.observe(document.body, { childList: true, subtree: true });
    document.querySelectorAll(".dlg-overlay").forEach(adoptDialog);
}
