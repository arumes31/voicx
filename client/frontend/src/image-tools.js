// image-tools.js — wave-7 image upload helpers (268/269/274). Avatars get a
// preview dialog with crop/zoom and are resized to 256x256 PNG on a canvas;
// icons are downscaled to max 1024px and recompressed as JPEG q0.85 (274).
// Animated GIF/WebP always pass through untouched — canvas re-encoding
// would flatten the animation (269).
const V = () => window.__voicx;

const MAX_UPLOAD = 256 * 1024; // server-side image cap (same as avatars)

// readFileBase64 reads a File as base64.
async function readFileBase64(file) {
    const buf = await file.arrayBuffer();
    return btoa(String.fromCharCode(...new Uint8Array(buf)));
}

// pickFile opens the file picker and resolves with the File (or null).
function pickFile() {
    return new Promise((resolve) => {
        const input = document.createElement("input");
        input.type = "file";
        input.accept = "image/png,image/jpeg,image/gif,image/webp";
        input.onchange = () => resolve(input.files[0] || null);
        input.click();
    });
}

// loadImage decodes a File into an Image.
function loadImage(file) {
    return new Promise((resolve, reject) => {
        const url = URL.createObjectURL(file);
        const img = new Image();
        img.onload = () => resolve({ img, url });
        img.onerror = () => {
            URL.revokeObjectURL(url);
            reject(new Error("cannot read image"));
        };
        img.src = url;
    });
}

// passthroughAnimated returns the file unchanged when it is an animated
// format (269); null otherwise.
async function passthroughAnimated(file) {
    if (file.type !== "image/gif" && file.type !== "image/webp") return null;
    if (file.size > MAX_UPLOAD) {
        V().toast("animated image too large (max 256 KiB)", "warn");
        return { dataBase64: "", contentType: "" };
    }
    return { dataBase64: await readFileBase64(file), contentType: file.type };
}

// pickAvatar opens the picker + crop dialog (268) and resolves with
// {dataBase64, contentType} or null when cancelled.
export async function pickAvatar() {
    const file = await pickFile();
    if (!file) return null;
    const anim = await passthroughAnimated(file);
    if (anim) return anim.dataBase64 ? anim : null;
    if (file.size > 8 * 1024 * 1024) {
        V().toast("image too large (max 8 MiB)", "warn");
        return null;
    }
    return cropDialog(file);
}

// pickIcon opens the picker and returns a compressed image (274): downscale
// to maxDim, JPEG q0.85 (animated formats pass through, 269).
export async function pickIcon(maxDim = 1024, quality = 0.85) {
    const file = await pickFile();
    if (!file) return null;
    const anim = await passthroughAnimated(file);
    if (anim) return anim.dataBase64 ? anim : null;
    if (file.size > 8 * 1024 * 1024) {
        V().toast("image too large (max 8 MiB)", "warn");
        return null;
    }
    try {
        const { img, url } = await loadImage(file);
        const scale = Math.min(1, maxDim / Math.max(img.width, img.height));
        const out = document.createElement("canvas");
        out.width = Math.max(1, Math.round(img.width * scale));
        out.height = Math.max(1, Math.round(img.height * scale));
        const ctx = out.getContext("2d");
        ctx.fillStyle = "#0b0f14"; // JPEG has no alpha: flatten onto ink navy
        ctx.fillRect(0, 0, out.width, out.height);
        ctx.drawImage(img, 0, 0, out.width, out.height);
        URL.revokeObjectURL(url);
        const dataUrl = out.toDataURL("image/jpeg", quality);
        return { dataBase64: dataUrl.split(",")[1], contentType: "image/jpeg" };
    } catch {
        V().toast("cannot read image", "warn");
        return null;
    }
}

// cropDialog shows the 256x256 preview/crop editor (268): zoom slider and
// drag-to-reposition on a canvas, output PNG.
function cropDialog(file) {
    return new Promise(async (resolve) => {
        let img, url;
        try {
            ({ img, url } = await loadImage(file));
        } catch {
            V().toast("cannot read image", "warn");
            return resolve(null);
        }
        const overlay = document.createElement("div");
        overlay.className = "dlg-overlay";
        overlay.innerHTML = `
            <div class="dlg img-edit">
                <h3>Set avatar</h3>
                <div class="img-preview"><canvas width="256" height="256"></canvas></div>
                <label class="dlg-label">zoom</label>
                <input type="range" class="img-zoom" min="100" max="300" value="100" />
                <div class="dlg-text img-hint">drag the preview to reposition · output 256×256 PNG</div>
                <div class="dlg-buttons">
                    <button class="dlg-ok">Use image</button>
                    <button class="dlg-cancel">Cancel</button>
                </div>
            </div>`;
        const canvas = overlay.querySelector("canvas");
        const zoomEl = overlay.querySelector(".img-zoom");
        const ctx = canvas.getContext("2d");
        const st = { zoom: 1, dx: 0, dy: 0, drag: null };

        const draw = () => {
            ctx.fillStyle = "#0b0f14";
            ctx.fillRect(0, 0, canvas.width, canvas.height);
            const base = Math.max(canvas.width / img.width, canvas.height / img.height);
            const scale = base * st.zoom;
            const w = img.width * scale, h = img.height * scale;
            ctx.drawImage(img, (canvas.width - w) / 2 + st.dx, (canvas.height - h) / 2 + st.dy, w, h);
        };
        zoomEl.oninput = () => { st.zoom = zoomEl.value / 100; draw(); };
        canvas.addEventListener("mousedown", (e) => {
            st.drag = { x: e.clientX - st.dx, y: e.clientY - st.dy };
        });
        const onMove = (e) => {
            if (!st.drag) return;
            st.dx = e.clientX - st.drag.x;
            st.dy = e.clientY - st.drag.y;
            draw();
        };
        const onUp = () => { st.drag = null; };
        window.addEventListener("mousemove", onMove);
        window.addEventListener("mouseup", onUp);
        draw();

        const close = (result) => {
            window.removeEventListener("mousemove", onMove);
            window.removeEventListener("mouseup", onUp);
            URL.revokeObjectURL(url);
            overlay.remove();
            resolve(result);
        };
        overlay.querySelector(".dlg-ok").onclick = () => {
            const dataUrl = canvas.toDataURL("image/png");
            close({ dataBase64: dataUrl.split(",")[1], contentType: "image/png" });
        };
        overlay.querySelector(".dlg-cancel").onclick = () => close(null);
        overlay.onclick = (e) => { if (e.target === overlay) close(null); };
        document.body.appendChild(overlay);
    });
}
