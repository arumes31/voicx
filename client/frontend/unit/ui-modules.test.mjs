import assert from "node:assert/strict";
import test from "node:test";

const globalRestorations = new WeakMap();

function replaceGlobal(t, name, value) {
    let restorations = globalRestorations.get(t);
    if (!restorations) {
        restorations = new Map();
        globalRestorations.set(t, restorations);
        t.after(() => {
            for (const [property, previous] of restorations) {
                if (previous) Object.defineProperty(globalThis, property, previous);
                else delete globalThis[property];
            }
        });
    }
    if (!restorations.has(name)) restorations.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, value, writable: true });
}

function classList() {
    const values = new Set();
    return {
        add: (name) => values.add(name),
        contains: (name) => values.has(name),
        remove: (name) => values.delete(name),
        toggle(name, enabled) {
            if (enabled) values.add(name);
            else values.delete(name);
            return !!enabled;
        },
    };
}

function element() {
    const attributes = new Map();
    return {
        attributes,
        classList: classList(),
        focusCalls: 0,
        hidden: false,
        tabIndex: 0,
        focus(options) {
            this.focusCalls++;
            this.focusOptions = options;
        },
        getAttribute: (name) => attributes.get(name) || null,
        setAttribute: (name, value) => attributes.set(name, String(value)),
    };
}

function fileBrowserPane() {
    const pane = element();
    const list = {
        attributes: new Map(),
        innerHTML: "",
        removeAttribute(name) { this.attributes.delete(name); },
        setAttribute(name, value) { this.attributes.set(name, String(value)); },
    };
    const crumb = {
        children: [],
        lastChild: null,
        appendChild(child) {
            this.children.push(child);
            this.lastChild = child;
        },
        removeChild(child) {
            this.children.splice(this.children.indexOf(child), 1);
            this.lastChild = this.children.at(-1) || null;
        },
    };
    const quota = { classList: classList(), removeAttribute() {}, setAttribute() {}, title: "" };
    pane.querySelector = (selector) => ({
        ".fb-crumb": crumb,
        ".fb-list": list,
        ".fb-quota": quota,
    }[selector] || null);
    return { list, pane };
}

test("frontend UI module behaviors", { concurrency: false }, async (t) => {
    await t.test("audio handles mute, PTT, profiles, and local volume", async (t) => {
        const state = {
            settings: {
                capture_device_id: "capture-1",
                echo_cancellation: false,
                muted_users: [],
                noise_suppression: true,
                user_volumes: { alice: 60 },
            },
        };
        const saved = [];
        replaceGlobal(t, "window", {
            __voicx: { state },
            go: {
                main: {
                    App: {
                        GetSettings: async () => saved.at(-1),
                        SaveSettings: async (settings) => { saved.push(settings); return ""; },
                    },
                },
            },
        });
        const audio = await import("../src/audio.js");
        const button = element();

        audio.syncMuteButton(button, true);
        assert.equal(button.classList.contains("active"), true);
        assert.equal(button.getAttribute("aria-pressed"), "true");
        assert.equal(button.textContent, "🔊");
        assert.equal(button.getAttribute("aria-label"), "Unmute microphone");

        audio.syncMuteButton(button, false);
        assert.equal(button.classList.contains("active"), false);
        assert.equal(button.getAttribute("aria-pressed"), "false");
        assert.equal(button.textContent, "🔇");
        assert.equal(button.title, "Mute");
        assert.equal(audio.isMusicChannel({ AudioProfile: "broadcast" }), true);
        assert.equal(audio.isMusicChannel({ IsMusic: false }), false);
        assert.equal(audio.isMusicChannel({ OpusStereo: true, OpusBitrate: audio.MUSIC_MIN_BITRATE }), true);
        assert.deepEqual(audio.captureConstraints({ AudioProfile: "music" }), {
            deviceId: { exact: "capture-1" },
            channelCount: 2,
            echoCancellation: false,
            noiseSuppression: false,
            autoGainControl: false,
        });
        assert.deepEqual(audio.captureConstraints(null), {
            deviceId: { exact: "capture-1" },
            echoCancellation: false,
            noiseSuppression: true,
        });

        const timers = new Map();
        let nextTimer = 0;
        replaceGlobal(t, "setTimeout", (callback) => {
            const id = ++nextTimer;
            timers.set(id, callback);
            return id;
        });
        replaceGlobal(t, "clearTimeout", (id) => timers.delete(id));
        state.settings.ptt_release_delay_ms = 30;
        const ptt = [];
        audio.pttRelease(false, (active) => ptt.push(active));
        audio.pttRelease(true, (active) => ptt.push(active));
        assert.deepEqual(ptt, [true]);
        assert.equal(timers.size, 0);
        audio.pttRelease(false, (active) => ptt.push(active));
        [...timers.values()][0]();
        assert.deepEqual(ptt, [true, false]);

        const captured = [];
        let freshTrack;
        replaceGlobal(t, "navigator", {
            mediaDevices: {
                getUserMedia: async (constraints) => {
                    captured.push(constraints);
                    return { getAudioTracks: () => [freshTrack], getTracks: () => [freshTrack] };
                },
            },
        });
        const oldTrack = { enabled: false, kind: "audio", stopped: false, stop() { this.stopped = true; } };
        freshTrack = { kind: "audio", stopped: false, stop() { this.stopped = true; } };
        const changes = [];
        const stream = {
            addTrack: (track) => changes.push(["add", track]),
            getAudioTracks: () => [oldTrack],
            removeTrack: (track) => changes.push(["remove", track]),
        };
        const sender = { track: oldTrack, replaceTrack: async (track) => changes.push(["replace", track]) };
        const applied = await audio.applyCaptureProfile({ getSenders: () => [sender] }, stream, { AudioProfile: "music" });
        assert.equal(applied.changed, true);
        assert.equal(applied.track, freshTrack);
        assert.equal(freshTrack.enabled, false);
        assert.equal(freshTrack.contentHint, "music");
        assert.equal(oldTrack.stopped, true);
        assert.deepEqual(changes.map(([kind]) => kind), ["replace", "remove", "add"]);
        assert.deepEqual(captured[0], { audio: audio.captureConstraints({ AudioProfile: "music" }) });

        const retained = { enabled: true, kind: "audio", stop() {} };
        audio.markCaptureProfile(retained, { AudioProfile: "music" });
        assert.deepEqual(await audio.applyCaptureProfile(null, { getAudioTracks: () => [retained] }, { AudioProfile: "music" }), {
            changed: false,
            track: retained,
        });

        const gainNode = { gain: { value: 0 } };
        const muteNode = { gain: { value: 0 } };
        audio.registerUserChain("alice", gainNode, muteNode);
        assert.equal(gainNode.gain.value, 0.6);
        audio.setDucking(true, []);
        assert.equal(gainNode.gain.value, 0.15);
        audio.setDucking(true, ["alice"]);
        assert.equal(gainNode.gain.value, 0.6);
        await audio.setUserMuted("alice", true);
        assert.equal(gainNode.gain.value, 0);
        assert.equal(muteNode.gain.value, 0);
        assert.deepEqual(saved[0].muted_users, ["alice"]);
        audio.setDucking(false, []);
        audio.unregisterUserChain("alice");

        const compressor = {
            attack: { value: 0 }, knee: { value: 0 }, ratio: { value: 0 }, release: { value: 0 }, threshold: { value: 0 },
        };
        assert.equal(audio.makeLimiter({ createDynamicsCompressor: () => compressor }), compressor);
        assert.equal(compressor.threshold.value, -12);
        assert.equal(compressor.ratio.value, 8);
        const normalized = audio.makeNormalizer(
            { createGain: () => ({ gain: { value: 1 } }) },
            { frequencyBinCount: 2, getByteTimeDomainData: (buffer) => buffer.set([128, 144]) },
        );
        normalized.tick();
        assert.ok(normalized.gain.gain.value > 1);
    });

    await t.test("chat parses messages, subscriptions, mentions, replies, and switcher scores", async (t) => {
        replaceGlobal(t, "window", { __voicx: { state: { myNickname: "Dan" } } });
        t.mock.module(new URL("../src/sounds.js", import.meta.url), {
            exports: { playEvent() {} },
        });
        const chat = await import("../src/chat-ui.js");

        assert.deepEqual(chat.normalizeSubscriptionState('{"channel_ids":[4,"2",4,0,-1,"bad"]}'), [2, 4]);
        assert.equal(chat.normalizeSubscriptionState("not json"), null);
        assert.deepEqual(chat.normalize({
            body: "hello", enc_verified: true, from_nickname: "Ada", id: "8", reply_to_id: "3", sent_at: 7,
        }, 5), {
            channelID: 5, clientMsgID: "", deleted: false, edited: false, e2e: false, enc: true, from: "Ada", fromUID: "",
            id: 8, mentioned: false, mentions: [], offline: false, reactions: null, replyToID: 3, self: false, text: "hello", ts: 7000, version: 1,
        });
        assert.equal(chat.mentionsMe("hello @dan and @here"), true);
        assert.equal(chat.mentionsMe("hello @daniela"), false);
        const first = { from: "Ada", id: 1, ts: 10 };
        const reply = { from: "Bob", id: 2, replyToID: 1, text: "plain", ts: 20 };
        const legacyReply = { from: "Bob", id: 3, replyToID: 0, text: "↪ Ada: earlier", ts: 30 };
        assert.equal(chat.resolveParent([first, reply, legacyReply], reply), first);
        assert.equal(chat.resolveParent([first, reply, legacyReply], legacyReply), first);
        assert.equal(chat.fmtSlowMode(3600), "1h");
        assert.equal(chat.fmtSlowMode(120), "2m");
        assert.equal(chat.fmtSlowMode(7), "7s");
        assert.equal(chat.qsScore("alp", "Alpha"), 100);
        assert.ok(chat.qsScore("apa", "Alpha") > 0);
        assert.equal(chat.qsScore("zzz", "Alpha"), -1);
    });

    await t.test("files switches both workspace tabs, restores focus, and protects sealed data", async (t) => {
        const tabChat = element();
        const tabFiles = element();
        const chatPane = element();
        const { pane: filesPane, list } = fileBrowserPane();
        const voiceMute = Object.assign(element(), {
            closest: () => null,
            getClientRects: () => [{}],
            isConnected: true,
        });
        const elements = new Map([
            ["tab-chat", tabChat],
            ["tab-files", tabFiles],
            ["chat-pane", chatPane],
            ["files-pane", filesPane],
            ["voice-mute", voiceMute],
        ]);
        replaceGlobal(t, "window", {
            __voicx: { state: { channels: [], myChannelID: 0 } },
            getComputedStyle: () => ({ display: "block", visibility: "visible" }),
        });
        replaceGlobal(t, "document", {
            activeElement: null,
            body: {},
            createElement: () => element(),
            createTextNode: (text) => ({ text }),
            getElementById: (id) => elements.get(id) || null,
        });
        const files = await import("../src/files-ui.js");

        assert.equal(files.activateWorkspaceTab("unknown"), false);
        assert.equal(files.activateWorkspaceTab("chat", { focus: true }), true);
        assert.equal(tabChat.classList.contains("active"), true);
        assert.equal(tabFiles.classList.contains("active"), false);
        assert.equal(tabChat.getAttribute("aria-selected"), "true");
        assert.equal(tabFiles.getAttribute("aria-selected"), "false");
        assert.equal(chatPane.hidden, false);
        assert.equal(filesPane.hidden, true);
        assert.equal(tabChat.focusCalls, 1);
        assert.equal(files.activateWorkspaceTab("files", { focus: true }), true);
        assert.equal(tabChat.getAttribute("aria-selected"), "false");
        assert.equal(tabFiles.getAttribute("aria-selected"), "true");
        assert.equal(chatPane.hidden, true);
        assert.equal(filesPane.hidden, false);
        assert.equal(tabFiles.focusCalls, 1);
        assert.match(list.innerHTML, /Join a channel/);
        assert.equal(files.restoreVisibleWorkspaceFocus(), true);
        assert.equal(voiceMute.focusCalls, 1);
        assert.deepEqual(voiceMute.focusOptions, { preventScroll: true });
        document.activeElement = voiceMute;
        assert.equal(files.restoreVisibleWorkspaceFocus(), false);
        assert.equal(files.isChatAttachment({ encrypted: true, name: "plain.txt" }), true);
        assert.equal(files.isChatAttachment({ encrypted: false, name: "sealed.vcx" }), true);
        assert.equal(files.isChatAttachment({ name: "plain.txt" }), false);
        assert.equal(files.bytesToBase64(new Uint8Array([0, 255, 1])), "AP8B");
        files.activateWorkspaceTab("chat");
    });

    await t.test("grid compositor rejects unavailable canvases and composes empty through four streams", async (t) => {
        replaceGlobal(t, "OffscreenCanvas", undefined);
        const { GridCompositor } = await import("../src/grid-compositor.js");
        assert.throws(() => new GridCompositor(), /unavailable/);

        class TestCanvas {
            constructor(width, height) {
                this.width = width;
                this.height = height;
                this.draws = [];
                this.ctx = {
                    drawImage: (...args) => this.draws.push(args),
                    fillRect: (...args) => { this.fill = args; },
                    fillStyle: "",
                };
            }

            getContext(kind) { return kind === "2d" ? this.ctx : null; }
            transferToImageBitmap() { return { draws: this.draws, fill: this.fill }; }
        }
        replaceGlobal(t, "OffscreenCanvas", TestCanvas);
        assert.deepEqual(await new GridCompositor(200, 200).compose([]), { draws: [], fill: [0, 0, 200, 200] });
        const first = { readyState: 2 };
        assert.deepEqual((await new GridCompositor(200, 200).compose([first])).draws, [[first, 0, 0, 200, 200]]);
        const videos = [{ readyState: 2 }, { readyState: 2 }, { readyState: 2 }, { readyState: 2 }];
        assert.deepEqual((await new GridCompositor(200, 200).compose(videos)).draws, [
            [videos[0], 0, 0, 100, 100], [videos[1], 100, 0, 100, 100],
            [videos[2], 0, 100, 100, 100], [videos[3], 100, 100, 100, 100],
        ]);
    });

    await t.test("grid compositor closes WebGL bitmaps and skips decode errors", async (t) => {
        const calls = { close: 0, draw: 0, flush: 0, viewport: [] };
        const gl = {
            ARRAY_BUFFER: 1, CLAMP_TO_EDGE: 2, COLOR_BUFFER_BIT: 3, COMPILE_STATUS: 4, FLOAT: 5, FRAGMENT_SHADER: 6,
            LINEAR: 7, LINK_STATUS: 8, RGBA: 9, STATIC_DRAW: 10, TEXTURE_2D: 11, TEXTURE_MAG_FILTER: 12,
            TEXTURE_MIN_FILTER: 13, TEXTURE_WRAP_S: 14, TEXTURE_WRAP_T: 15, TRIANGLE_STRIP: 16, UNSIGNED_BYTE: 17, VERTEX_SHADER: 18,
            attachShader() {}, bindBuffer() {}, bindTexture() {}, bufferData() {}, clear() {}, clearColor() {}, compileShader() {},
            createBuffer: () => ({}), createProgram: () => ({}), createShader: () => ({}), createTexture: () => ({}),
            drawArrays() { calls.draw++; }, enableVertexAttribArray() {}, flush() { calls.flush++; }, getAttribLocation: () => 0,
            getProgramParameter: () => true, getShaderParameter: () => true, linkProgram() {}, shaderSource() {}, texImage2D() {}, texParameteri() {},
            useProgram() {}, vertexAttribPointer() {}, viewport: (...args) => calls.viewport.push(args),
        };
        class WebGLCanvas {
            constructor(width, height) { this.width = width; this.height = height; }
            getContext(kind) { return kind === "webgl" ? gl : null; }
            transferToImageBitmap() { return { composed: true }; }
        }
        replaceGlobal(t, "OffscreenCanvas", WebGLCanvas);
        replaceGlobal(t, "createImageBitmap", async (video) => {
            if (video.bad) throw new Error("decode failed");
            return { close: () => { calls.close++; } };
        });
        const { GridCompositor } = await import("../src/grid-compositor.js");
        assert.deepEqual(await new GridCompositor(400, 200).compose([{ readyState: 2 }, { bad: true, readyState: 2 }]), { composed: true });
        assert.equal(calls.draw, 1);
        assert.equal(calls.close, 1);
        assert.equal(calls.flush, 1);
        assert.deepEqual(calls.viewport, [[0, 0, 200, 200]]);
    });

    await t.test("image pickers cancel, enforce limits, scale icons, and release object URLs", async (t) => {
        let selected = null;
        let canvas;
        const toasts = [];
        replaceGlobal(t, "document", {
            createElement(tag) {
                if (tag === "input") {
                    return {
                        click() { this.onchange?.(); },
                        files: selected ? [selected] : [],
                    };
                }
                if (tag === "canvas") {
                    canvas = {
                        draw: [], height: 0, width: 0,
                        getContext: () => ({ drawImage: (...args) => canvas.draw.push(args), fillRect() {}, fillStyle: "" }),
                        toDataURL: () => "data:image/jpeg;base64,YQ==",
                    };
                    return canvas;
                }
                return element();
            },
        });
        replaceGlobal(t, "window", {
            __voicx: { state: { serverGeneration: 7 }, toast: (...args) => toasts.push(args) },
        });
        const imageTools = await import("../src/image-tools.js");

        assert.equal(await imageTools.pickAvatar(), null);
        selected = { size: 9 * 1024 * 1024, type: "image/png" };
        assert.equal(await imageTools.pickAvatar(), null);
        assert.match(toasts.at(-1)[0], /8 MiB/);
        selected = { size: 300 * 1024, type: "image/gif" };
        assert.equal(await imageTools.pickAvatar(), null);
        assert.match(toasts.at(-1)[0], /animated image too large/);

        const revoked = [];
        replaceGlobal(t, "URL", {
            createObjectURL: () => "blob:icon",
            revokeObjectURL: (url) => revoked.push(url),
        });
        replaceGlobal(t, "Image", class {
            constructor() { this.height = 1000; this.width = 2000; }
            set src(value) { this.value = value; this.onload?.(); }
        });
        selected = { size: 1024, type: "image/png" };
        assert.deepEqual(await imageTools.pickIcon(1000, 0.85), { contentType: "image/jpeg", dataBase64: "YQ==" });
        assert.equal(canvas.width, 1000);
        assert.equal(canvas.height, 500);
        assert.deepEqual(canvas.draw[0].slice(1), [0, 0, 1000, 500]);
        assert.deepEqual(revoked, ["blob:icon"]);
        assert.equal(imageTools.generationCurrent({}), true);
        assert.equal(imageTools.generationCurrent({ serverGeneration: 7 }), true);
        assert.equal(imageTools.generationCurrent({ serverGeneration: 6 }), false);
        assert.equal(imageTools.base64Bytes("YQ=="), 1);
        assert.equal(imageTools.base64Bytes("YWI="), 2);
        assert.equal(imageTools.base64Bytes("YWJj"), 3);
    });

    await t.test("client info derives audio stats, durations, channel trees, and presets", async (t) => {
        replaceGlobal(t, "window", {
            __voicx: {
                state: {
                    channels: [
                        { ChannelID: 1 }, { ChannelID: 2, ParentID: 1 }, { ChannelID: 3, ParentID: 2 }, { ChannelID: 4 },
                    ],
                },
            },
        });
        const clientInfo = await import("../src/clientinfo.js");

        const direct = { kind: "audio", trackIdentifier: "42", type: "inbound-rtp" };
        const legacyTrack = { id: "track-a", trackIdentifier: "99" };
        const legacyInbound = { kind: "audio", trackId: "track-a", type: "inbound-rtp" };
        const video = { kind: "video", trackIdentifier: "no", type: "inbound-rtp" };
        const mapped = clientInfo.inboundAudioByPublisher([direct, legacyTrack, legacyInbound, video]);
        assert.equal(mapped.get("42"), direct);
        assert.equal(mapped.get("99"), legacyInbound);
        assert.equal(clientInfo.humanBytes(0), "0 B");
        assert.equal(clientInfo.humanBytes(1024), "1.0 KiB");
        assert.equal(clientInfo.humanBytes(1024 * 1024), "1.0 MiB");
        assert.equal(clientInfo.humanDuration(65), "1m 5s");
        assert.equal(clientInfo.humanDuration(3661), "1h 1m 1s");
        assert.equal(clientInfo.matchPreset(128000, true, false, true), "music");
        assert.equal(clientInfo.matchPreset(1, false, false, false), "custom");
        assert.equal(clientInfo.countSubtree(1), 2);
        assert.deepEqual(clientInfo.subtreeOf(1), new Set([1, 2, 3]));
    });
});
