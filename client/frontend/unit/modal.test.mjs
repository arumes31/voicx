import test from "node:test";
import assert from "node:assert/strict";

import {
    canFocusTarget,
    closeDialog,
    closeServerDialogs,
    dialogFocusableSelector,
    dialogStackInsertionIndex,
    initModalSystem,
    isCurrentServerDialog,
    mountDialog,
    mountServerDialog,
    registerDialogLifecycle,
    topDialog,
} from "../src/modal.js";

class FakeClassList {
    constructor(element) { this.element = element; }
    values() { return this.element.className.split(/\s+/).filter(Boolean); }
    contains(name) { return this.values().includes(name); }
    add(...names) { this.element.className = [...new Set([...this.values(), ...names])].join(" "); }
    remove(...names) { this.element.className = this.values().filter((name) => !names.includes(name)).join(" "); }
}

class FakeElement extends EventTarget {
    constructor(tagName = "div", ownerDocument = null) {
        super();
        this.tagName = tagName.toUpperCase();
        this.ownerDocument = ownerDocument;
        this.parentElement = null;
        this.children = [];
        this.attributes = new Map();
        this.className = "";
        this.classList = new FakeClassList(this);
        this.dataset = {};
        this.style = {};
        this.hidden = false;
        this.inert = false;
        this.disabled = false;
        this.tabIndex = ["BUTTON", "INPUT", "SELECT", "TEXTAREA", "SUMMARY"].includes(this.tagName) ? 0 : -1;
        this.id = "";
        this.textContent = "";
        this.htmlFor = "";
        this.autofocus = false;
        this.controls = false;
        this.href = "";
        this.contentEditable = "false";
        this._rects = true;
        this._focusSucceeds = true;
        this._isRoot = false;
    }

    get isConnected() { return this._isRoot || !!this.parentElement?.isConnected; }
    get firstElementChild() { return this.children[0] || null; }
    get nextElementSibling() {
        if (!this.parentElement) return null;
        const index = this.parentElement.children.indexOf(this);
        return this.parentElement.children[index + 1] || null;
    }

    appendChild(child) {
        child.remove();
        child.parentElement = this;
        child.ownerDocument = this.ownerDocument;
        this.children.push(child);
        return child;
    }

    removeChild(child) {
        const index = this.children.indexOf(child);
        if (index >= 0) this.children.splice(index, 1);
        child.parentElement = null;
        return child;
    }

    remove() { this.parentElement?.removeChild(this); }
    contains(candidate) {
        for (let node = candidate; node; node = node.parentElement) if (node === this) return true;
        return false;
    }

    setAttribute(name, value) {
        const text = String(value);
        this.attributes.set(name, text);
        if (name === "id") this.id = text;
        if (name === "for") this.htmlFor = text;
        if (name === "tabindex") this.tabIndex = Number(text);
        if (name === "autofocus") this.autofocus = true;
    }

    getAttribute(name) {
        if (name === "id") return this.id || null;
        if (name === "for") return this.htmlFor || null;
        return this.attributes.has(name) ? this.attributes.get(name) : null;
    }

    hasAttribute(name) {
        if (name === "id") return !!this.id;
        if (name === "for") return !!this.htmlFor;
        return this.attributes.has(name);
    }

    removeAttribute(name) {
        this.attributes.delete(name);
        if (name === "for") this.htmlFor = "";
    }

    matches(selector) {
        if (selector.includes(",")) return selector.split(",").some((part) => this.matches(part.trim()));
        if (selector === ".dlg-overlay") return this.classList.contains("dlg-overlay");
        if (selector === ".menu-item") return this.classList.contains("menu-item");
        if (selector.startsWith(".")) return this.classList.contains(selector.slice(1));
        if (selector.startsWith("#")) return this.id === selector.slice(1);
        if (selector === "label:not([for])") return this.tagName === "LABEL" && !this.htmlFor;
        if (selector === "[autofocus]") return this.autofocus || this.hasAttribute("autofocus");
        if (selector === "[href]") return !!this.href;
        if (selector === "audio[controls]") return this.tagName === "AUDIO" && this.controls;
        if (selector === "video[controls]") return this.tagName === "VIDEO" && this.controls;
        if (selector === "[contenteditable=true]") return this.contentEditable === "true";
        if (selector === '[tabindex]:not([tabindex="-1"])') return this.hasAttribute("tabindex") && this.tabIndex !== -1;
        if (selector === "input:not([type=hidden])") return this.tagName === "INPUT" && this.getAttribute("type") !== "hidden";
        return this.tagName === selector.toUpperCase();
    }

    querySelectorAll(selector) {
        const found = [];
        const visit = (node) => {
            for (const child of node.children) {
                if (child.matches(selector)) found.push(child);
                visit(child);
            }
        };
        visit(this);
        return found;
    }

    querySelector(selector) {
        if (selector === ":scope > span") return this.children.find((child) => child.tagName === "SPAN") || null;
        return this.querySelectorAll(selector)[0] || null;
    }

    closest(selector) {
        for (let node = this; node; node = node.parentElement) if (node.matches(selector)) return node;
        return null;
    }

    getClientRects() { return this._rects ? [{}] : []; }
    focus() {
        if (!this._focusSucceeds || !this.isConnected || !this._rects) return;
        for (let node = this; node; node = node.parentElement) {
            if (node.hidden || node.inert || node.getAttribute("aria-hidden") === "true") return;
        }
        this.ownerDocument.activeElement = this;
    }
}

class FakeDocument extends EventTarget {
    constructor() {
        super();
        this.body = new FakeElement("body", this);
        this.body._isRoot = true;
        this.activeElement = this.body;
    }
    createElement(tagName) { return new FakeElement(tagName, this); }
    getElementById(id) { return this.body.id === id ? this.body : this.body.querySelectorAll(`#${id}`)[0] || null; }
    querySelectorAll(selector) {
        if (selector === "#menubar > .menu-item") {
            return this.getElementById("menubar")?.children.filter((child) => child.classList.contains("menu-item")) || [];
        }
        return this.body.querySelectorAll(selector);
    }
}

class FakeMutationObserver {
    constructor(callback) { this.callback = callback; FakeMutationObserver.instance = this; }
    observe(target, options) { this.target = target; this.options = options; }
    trigger(mutations) { this.callback(mutations); }
}

function element(document, tag, options = {}) {
    const node = document.createElement(tag);
    if (options.id) node.id = options.id;
    if (options.className) node.className = options.className;
    if (options.text) node.textContent = options.text;
    if (options.autofocus) node.autofocus = true;
    return node;
}

function dialog(document, name = "Dialog", controls = []) {
    const overlay = element(document, "div", { className: "dlg-overlay" });
    const panel = element(document, "div", { className: "dlg" });
    const title = element(document, "h3", { text: name });
    panel.appendChild(title);
    for (const control of controls) panel.appendChild(control);
    overlay.appendChild(panel);
    return { overlay, panel, title };
}

function keyEvent(key, shiftKey = false) {
    const event = new Event("keydown", { cancelable: true });
    Object.defineProperties(event, { key: { value: key }, shiftKey: { value: shiftKey } });
    return event;
}

function focusEvent(target) {
    const event = new Event("focusin");
    Object.defineProperty(event, "target", { value: target });
    return event;
}

test("shared modal lifecycle handles focus, cancellation, removal, and restoration", async (t) => {
    const document = new FakeDocument();
    const frames = [];
    globalThis.Element = FakeElement;
    globalThis.HTMLElement = FakeElement;
    globalThis.MutationObserver = FakeMutationObserver;
    globalThis.document = document;
    globalThis.window = { __voicx: { state: { serverGeneration: 1 } } };
    globalThis.requestAnimationFrame = (callback) => {
        frames.push(callback);
        return frames.length;
    };
    const flushFrames = () => {
        let guard = 100;
        while (frames.length && guard-- > 0) frames.shift()();
        assert.ok(guard > 0, "animation frames drained");
    };

    const login = element(document, "div", { id: "login-overlay", className: "hidden" });
    const loginAddress = element(document, "input", { id: "login-addr" });
    login.appendChild(loginAddress);
    const app = element(document, "div", { id: "app" });
    const center = element(document, "main", { id: "center" });
    center.setAttribute("tabindex", "-1");
    const launcher = element(document, "button", { id: "launcher", text: "Open" });
    app.appendChild(center);
    app.appendChild(launcher);
    document.body.appendChild(login);
    document.body.appendChild(app);

    initModalSystem();
    initModalSystem();
    assert.equal(FakeMutationObserver.instance.target, document.body);
    assert.deepEqual(FakeMutationObserver.instance.options, { childList: true, subtree: true });
    assert.match(dialogFocusableSelector, /audio\[controls\]/);
    assert.match(dialogFocusableSelector, /video\[controls\]/);
    assert.match(dialogFocusableSelector, /summary/);
    assert.match(dialogFocusableSelector, /contenteditable/);
    assert.equal(dialogStackInsertionIndex([false, true], false), 1);
    assert.equal(dialogStackInsertionIndex([false, false], false), 2);
    assert.equal(dialogStackInsertionIndex([false, true], true), 2);

    await t.test("rejects hidden, inert, zero-rect, and failed focus targets", () => {
        const hiddenParent = element(document, "div");
        hiddenParent.hidden = true;
        const hiddenButton = element(document, "button", { className: "preferred" });
        hiddenParent.appendChild(hiddenButton);
        app.appendChild(hiddenParent);
        const inertParent = element(document, "div");
        inertParent.inert = true;
        const inertButton = element(document, "button");
        inertParent.appendChild(inertButton);
        app.appendChild(inertParent);
        const noRects = element(document, "button");
        noRects._rects = false;
        app.appendChild(noRects);
        const disabled = element(document, "button");
        disabled.disabled = true;
        app.appendChild(disabled);
        assert.equal(canFocusTarget(hiddenButton), false);
        assert.equal(canFocusTarget(inertButton), false);
        assert.equal(canFocusTarget(noRects), false);
        assert.equal(canFocusTarget(disabled), false);
        assert.equal(canFocusTarget(null), false);

        launcher.focus();
        const failed = element(document, "button", { className: "failed" });
        failed._focusSucceeds = false;
        const fallback = element(document, "button", { className: "fallback" });
        const current = dialog(document, "Focus choices", [failed, fallback]);
        let closed = 0;
        mountDialog(current.overlay, {
            launcher,
            initialFocus: () => failed,
            onClose: () => { closed++; },
        });
        flushFrames();
        assert.equal(document.activeElement, fallback);
        assert.equal(app.inert, true);
        assert.equal(topDialog(), current.overlay);
        assert.equal(closeDialog(current.overlay), true);
        flushFrames();
        assert.equal(document.activeElement, launcher);
        assert.equal(app.inert, false);
        assert.equal(closed, 1);
        assert.equal(closeDialog(current.overlay), false);
        assert.equal(closed, 1);
    });

    await t.test("honors cancel vetoes and blocking dialogs", () => {
        const vetoed = dialog(document, "Vetoed", [element(document, "button")]);
        let closeReason = "";
        mountDialog(vetoed.overlay, {
            onCancel: () => false,
            onClose: (reason) => { closeReason = reason; },
        });
        flushFrames();
        assert.equal(closeDialog(vetoed.overlay, "cancel"), false);
        assert.equal(vetoed.overlay.isConnected, true);
        registerDialogLifecycle(vetoed.overlay, { onCancel: () => true });
        assert.equal(closeDialog(vetoed.overlay, "cancel"), true);
        flushFrames();
        assert.equal(closeReason, "cancel");

        const legacy = dialog(document, "Legacy veto", [element(document, "button")]);
        const prevent = (event) => event.preventDefault();
        legacy.overlay.addEventListener("voicx-dialog-cancel", prevent);
        mountDialog(legacy.overlay);
        flushFrames();
        assert.equal(closeDialog(legacy.overlay, "cancel"), false);
        legacy.overlay.removeEventListener("voicx-dialog-cancel", prevent);
        assert.equal(closeDialog(legacy.overlay), true);
        flushFrames();

        const blocking = dialog(document, "Blocking", [element(document, "button")]);
        blocking.overlay.dataset.blocking = "true";
        mountDialog(blocking.overlay);
        flushFrames();
        assert.equal(closeDialog(blocking.overlay, "cancel"), false);
        assert.equal(closeDialog(blocking.overlay), true);
        flushFrames();
    });

    await t.test("closes server-scoped dialog stacks and invalidates delayed work", () => {
        const globalDialog = dialog(document, "Global", [element(document, "button")]);
        mountDialog(globalDialog.overlay, { serverScoped: false });
        const serverDialog = dialog(document, "Server", [element(document, "button")]);
        mountServerDialog(serverDialog.overlay);
        const nested = dialog(document, "Nested server", [element(document, "button")]);
        mountDialog(nested.overlay);
        flushFrames();

        assert.equal(isCurrentServerDialog(serverDialog.overlay), true);
        assert.equal(isCurrentServerDialog(nested.overlay), true);
        globalThis.window.__voicx.state.serverGeneration++;
        assert.equal(isCurrentServerDialog(serverDialog.overlay), false);
        assert.equal(closeServerDialogs(), 2);
        flushFrames();
        assert.equal(serverDialog.overlay.isConnected, false);
        assert.equal(nested.overlay.isConnected, false);
        assert.equal(globalDialog.overlay.isConnected, true);
        closeDialog(globalDialog.overlay);
        flushFrames();
    });

    await t.test("finalizes direct and subtree removals exactly once", () => {
        let directClose = 0;
        const direct = dialog(document, "Direct removal", [element(document, "button")]);
        mountDialog(direct.overlay, { onClose: (reason) => {
            directClose++;
            assert.equal(reason, "removed");
        } });
        flushFrames();
        direct.overlay.remove();
        const directMutation = { addedNodes: [], removedNodes: [direct.overlay], target: document.body };
        FakeMutationObserver.instance.trigger([directMutation]);
        FakeMutationObserver.instance.trigger([directMutation]);
        flushFrames();
        assert.equal(directClose, 1);
        assert.equal(closeDialog(direct.overlay), false);

        let subtreeClose = 0;
        let cleanup = "running";
        const wrapper = element(document, "section");
        const nested = dialog(document, "Nested removal", [element(document, "button")]);
        wrapper.appendChild(nested.overlay);
        document.body.appendChild(wrapper);
        registerDialogLifecycle(nested.overlay, { onClose: () => {
            subtreeClose++;
            cleanup = "stopped";
        } });
        flushFrames();
        wrapper.remove();
        const subtreeMutation = { addedNodes: [], removedNodes: [wrapper], target: document.body };
        FakeMutationObserver.instance.trigger([subtreeMutation]);
        FakeMutationObserver.instance.trigger([subtreeMutation]);
        flushFrames();
        assert.equal(subtreeClose, 1);
        assert.equal(cleanup, "stopped");

        const adoptedWrapper = element(document, "section");
        const adopted = dialog(document, "Adopted", [element(document, "button")]);
        adoptedWrapper.appendChild(adopted.overlay);
        document.body.appendChild(adoptedWrapper);
        FakeMutationObserver.instance.trigger([{
            addedNodes: [adoptedWrapper], removedNodes: [], target: document.body,
        }]);
        flushFrames();
        assert.equal(topDialog(), adopted.overlay);
        adoptedWrapper.remove();
        FakeMutationObserver.instance.trigger([{
            addedNodes: [], removedNodes: [adoptedWrapper], target: document.body,
        }]);
        flushFrames();
        assert.equal(topDialog(), null);

        const errorWrapper = element(document, "section");
        const survives = dialog(document, "Cleanup continues", [element(document, "button")]);
        const throws = dialog(document, "Cleanup throws", [element(document, "button")]);
        let survivingCloses = 0;
        let reportedErrors = 0;
        errorWrapper.appendChild(survives.overlay);
        errorWrapper.appendChild(throws.overlay);
        document.body.appendChild(errorWrapper);
        registerDialogLifecycle(survives.overlay, { onClose: () => { survivingCloses++; } });
        registerDialogLifecycle(throws.overlay, { onClose: () => { throw new Error("cleanup boom"); } });
        flushFrames();
        const originalConsoleError = console.error;
        console.error = () => { reportedErrors++; };
        try {
            errorWrapper.remove();
            FakeMutationObserver.instance.trigger([{
                addedNodes: [], removedNodes: [errorWrapper], target: document.body,
            }]);
        } finally {
            console.error = originalConsoleError;
        }
        flushFrames();
        assert.equal(survivingCloses, 1);
        assert.equal(reportedErrors, 1);
        assert.equal(topDialog(), null);
    });

    await t.test("restores nested focus and falls back after workspace state changes", () => {
        launcher.focus();
        const outerButton = element(document, "button", { text: "Outer" });
        const outer = dialog(document, "Outer", [outerButton]);
        mountDialog(outer.overlay);
        flushFrames();
        assert.equal(document.activeElement, outerButton);
        document.dispatchEvent(focusEvent(outerButton));

        const innerButton = element(document, "button", { text: "Inner" });
        const inner = dialog(document, "Inner", [innerButton]);
        mountDialog(inner.overlay);
        flushFrames();
        assert.equal(document.activeElement, innerButton);
        assert.equal(outer.overlay.inert, true);
        assert.equal(closeDialog(inner.overlay), true);
        flushFrames();
        assert.equal(document.activeElement, outerButton);
        closeDialog(outer.overlay);
        flushFrames();
        assert.equal(document.activeElement, launcher);

        launcher.focus();
        const transition = dialog(document, "Transition", [element(document, "button")]);
        mountDialog(transition.overlay, { launcher });
        flushFrames();
        app.classList.add("hidden");
        app.setAttribute("aria-hidden", "true");
        login.classList.remove("hidden");
        login.setAttribute("aria-hidden", "false");
        assert.equal(closeDialog(transition.overlay), true);
        flushFrames();
        assert.equal(document.activeElement, loginAddress);

        login.classList.add("hidden");
        app.classList.remove("hidden");
        app.setAttribute("aria-hidden", "false");
        launcher._focusSucceeds = false;
        document.activeElement = launcher;
        const failedRestore = dialog(document, "Restore fallback", [element(document, "button")]);
        mountDialog(failedRestore.overlay, { launcher });
        flushFrames();
        closeDialog(failedRestore.overlay);
        flushFrames();
        assert.equal(document.activeElement, center);
        launcher._focusSucceeds = true;
    });

    await t.test("traps Tab, handles Escape, and refocuses after rerenders", () => {
        const first = element(document, "button", { text: "First" });
        const second = element(document, "button", { text: "Second" });
        const tabs = dialog(document, "Tabs", [first, second]);
        mountDialog(tabs.overlay, { initialFocus: ".missing" });
        flushFrames();
        assert.equal(document.activeElement, first);
        document.dispatchEvent(keyEvent("Tab"));
        assert.equal(document.activeElement, second);
        document.dispatchEvent(keyEvent("Tab"));
        assert.equal(document.activeElement, first);
        document.dispatchEvent(keyEvent("Tab", true));
        assert.equal(document.activeElement, second);

        second.remove();
        FakeMutationObserver.instance.trigger([{
            addedNodes: [], removedNodes: [second], target: tabs.overlay,
        }]);
        flushFrames();
        assert.equal(document.activeElement, first);
        document.dispatchEvent(keyEvent("Escape"));
        flushFrames();
        assert.equal(tabs.overlay.isConnected, false);

        const empty = dialog(document, "Empty");
        mountDialog(empty.overlay);
        flushFrames();
        assert.equal(document.activeElement, empty.overlay);
        document.dispatchEvent(keyEvent("Tab"));
        assert.equal(document.activeElement, empty.overlay);
        closeDialog(empty.overlay);
        flushFrames();
    });

    await t.test("derives semantics, associates controls, and rebuilds menu launchers", () => {
        const menuBar = element(document, "nav", { id: "menubar" });
        const oldMenu = element(document, "div", { className: "menu-item" });
        oldMenu.appendChild(element(document, "span", { text: "Tools" }));
        menuBar.appendChild(oldMenu);
        document.body.appendChild(menuBar);
        oldMenu.setAttribute("tabindex", "0");
        oldMenu.focus();

        const label = element(document, "label", { className: "dlg-label", text: "Name" });
        const input = element(document, "input");
        const settingsLabel = element(document, "label", { className: "set-label", text: "Volume" });
        const settingsWrap = element(document, "div");
        settingsWrap.appendChild(element(document, "input"));
        const nestedLabel = element(document, "label", { text: "Native" });
        nestedLabel.appendChild(element(document, "input"));
        const semantic = dialog(document, "Semantics", [label, input, settingsLabel, settingsWrap, nestedLabel]);
        mountDialog(semantic.overlay);
        flushFrames();
        assert.equal(semantic.overlay.getAttribute("role"), "dialog");
        assert.equal(semantic.overlay.getAttribute("aria-labelledby"), semantic.title.id);
        assert.equal(label.htmlFor, input.id);
        assert.equal(settingsLabel.htmlFor, settingsWrap.children[0].id);

        oldMenu.remove();
        const newMenu = element(document, "div", { className: "menu-item" });
        newMenu.setAttribute("tabindex", "0");
        newMenu.appendChild(element(document, "span", { text: "Tools" }));
        menuBar.appendChild(newMenu);
        closeDialog(semantic.overlay);
        flushFrames();
        assert.equal(document.activeElement, newMenu);

        const fallback = element(document, "div", { id: "token_redeem_dialog", className: "dlg-overlay" });
        fallback.appendChild(element(document, "div", { className: "dlg wide" }));
        mountDialog(fallback);
        flushFrames();
        assert.equal(fallback.getAttribute("aria-label"), "Token Redeem");
        closeDialog(fallback);
        flushFrames();

        const named = element(document, "div", { className: "dlg-overlay" });
        named.setAttribute("aria-label", "Custom dialog");
        named.appendChild(element(document, "div", { className: "dlg" }));
        mountDialog(named);
        flushFrames();
        assert.equal(named.getAttribute("aria-label"), "Custom dialog");
        closeDialog(named);
        flushFrames();
    });
});
