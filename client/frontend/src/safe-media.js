// Remote image metadata is untrusted: it comes from the connected server.
// Keep the accepted formats aligned with the server-side asset decoders and
// never let a MIME string become part of HTML markup.
const IMAGE_MIME_TYPES = new Set([
    "image/png",
    "image/jpeg",
    "image/gif",
    "image/webp",
]);

const BASE64_PAYLOAD = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
const IMAGE_DATA_URL = /^data:(image\/(?:png|jpeg|gif|webp));base64,([A-Za-z0-9+/]*={0,2})$/;

export function isBase64Payload(value) {
    return typeof value === "string" && value.length > 0 && BASE64_PAYLOAD.test(value);
}

export function imageDataURL(asset) {
    if (!asset || typeof asset !== "object") return null;
    const contentType = String(asset.content_type || "").trim().toLowerCase();
    const payload = asset.data_base64;
    if (!IMAGE_MIME_TYPES.has(contentType) || !isBase64Payload(payload)) return null;
    return `data:${contentType};base64,${payload}`;
}

export function isSafeImageDataURL(value) {
    if (typeof value !== "string") return false;
    const match = IMAGE_DATA_URL.exec(value);
    return !!match && IMAGE_MIME_TYPES.has(match[1]) && isBase64Payload(match[2]);
}

export function createSafeImage(dataURL, options = {}) {
    if (!isSafeImageDataURL(dataURL)) return null;
    const ownerDocument = options.ownerDocument || globalThis.document;
    if (!ownerDocument?.createElement) return null;
    const image = ownerDocument.createElement("img");
    image.alt = String(options.alt || "");
    if (options.className) image.className = String(options.className);
    image.src = dataURL;
    return image;
}

export function setSafeImage(container, dataURL, options = {}) {
    if (!container?.replaceChildren) return null;
    container.replaceChildren();
    const image = createSafeImage(dataURL, {
        ...options,
        ownerDocument: container.ownerDocument || options.ownerDocument,
    });
    if (image) container.appendChild(image);
    return image;
}
