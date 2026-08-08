import test from "node:test";
import assert from "node:assert/strict";

import {
    createSafeImage,
    imageDataURL,
    isBase64Payload,
    isSafeImageDataURL,
    setSafeImage,
} from "../src/safe-media.js";

const PNG_PIXEL = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB";

test("builds image URLs only from the supported MIME and base64 grammar", () => {
    const url = imageDataURL({ content_type: " IMAGE/PNG ", data_base64: PNG_PIXEL });
    assert.equal(url, `data:image/png;base64,${PNG_PIXEL}`);
    assert.equal(isSafeImageDataURL(url), true);
    assert.equal(isBase64Payload(PNG_PIXEL), true);

    for (const contentType of [
        "image/svg+xml",
        "text/html",
        'image/png\" onerror=\"globalThis.pwned=true',
        "IMAGE/PNG;CHARSET=UTF-8",
    ]) {
        assert.equal(imageDataURL({ content_type: contentType, data_base64: PNG_PIXEL }), null);
    }
    for (const payload of ["", "not base64!", 'AAAA\" onerror=\"pwned', "A===", "AAA"] ) {
        assert.equal(imageDataURL({ content_type: "image/png", data_base64: payload }), null);
    }
    assert.equal(imageDataURL(null), null);
    assert.equal(imageDataURL({}), null);
    assert.equal(isSafeImageDataURL("javascript:alert(1)"), false);
    assert.equal(isSafeImageDataURL(`data:image/svg+xml;base64,${PNG_PIXEL}`), false);
});

test("creates image elements through src properties and rejects unsafe URLs", () => {
    const created = [];
    const ownerDocument = {
        createElement(tagName) {
            const element = { tagName, alt: "", className: "", src: "" };
            created.push(element);
            return element;
        },
    };
    const children = [];
    const host = {
        ownerDocument,
        replaceChildren() { children.length = 0; },
        appendChild(child) { children.push(child); },
    };
    const url = `data:image/webp;base64,${PNG_PIXEL}`;
    const image = setSafeImage(host, url, { alt: "avatar", className: "preview" });
    assert.equal(image, children[0]);
    assert.deepEqual(image, { tagName: "img", alt: "avatar", className: "preview", src: url });
    assert.equal(createSafeImage("data:text/html;base64,PGgxPkJvb208L2gxPg==", { ownerDocument }), null);
    assert.equal(setSafeImage(host, "javascript:alert(1)"), null);
    assert.deepEqual(children, []);
    assert.equal(created.length, 1);
    assert.equal(setSafeImage(null, url), null);
});
