// OffscreenCanvas/WebGL grid compositor for the floating multi-stream view.
export class GridCompositor {
    constructor(width = 1280, height = 720) {
        if (typeof OffscreenCanvas === "undefined") throw new Error("OffscreenCanvas is unavailable");
        this.canvas = new OffscreenCanvas(width, height);
        this.gl = this.canvas.getContext("webgl", { alpha: false, antialias: false });
        this.ctx = this.gl ? null : this.canvas.getContext("2d", { alpha: false });
        if (this.gl) this.initGL();
    }

    initGL() {
        const gl = this.gl;
        const compile = (type, source) => {
            const shader = gl.createShader(type);
            gl.shaderSource(shader, source);
            gl.compileShader(shader);
            if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) throw new Error(gl.getShaderInfoLog(shader));
            return shader;
        };
        const program = gl.createProgram();
        gl.attachShader(program, compile(gl.VERTEX_SHADER,
            "attribute vec2 p;attribute vec2 uv;varying vec2 v;void main(){gl_Position=vec4(p,0.,1.);v=uv;}"));
        gl.attachShader(program, compile(gl.FRAGMENT_SHADER,
            "precision mediump float;varying vec2 v;uniform sampler2D tex;void main(){gl_FragColor=texture2D(tex,v);}"));
        gl.linkProgram(program);
        if (!gl.getProgramParameter(program, gl.LINK_STATUS)) throw new Error(gl.getProgramInfoLog(program));
        gl.useProgram(program);
        const buffer = gl.createBuffer();
        gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
        gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([
            -1, -1, 0, 1, 1, -1, 1, 1, -1, 1, 0, 0, 1, 1, 1, 0,
        ]), gl.STATIC_DRAW);
        for (const [name, offset] of [["p", 0], ["uv", 2]]) {
            const location = gl.getAttribLocation(program, name);
            gl.enableVertexAttribArray(location);
            gl.vertexAttribPointer(location, 2, gl.FLOAT, false, 16, offset * 4);
        }
        this.texture = gl.createTexture();
        gl.bindTexture(gl.TEXTURE_2D, this.texture);
        gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
        gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
        gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
        gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
    }

    async compose(videos) {
        const ready = videos.filter((video) => video.readyState >= 2);
        const columns = Math.max(1, Math.ceil(Math.sqrt(ready.length || 1)));
        const rows = Math.max(1, Math.ceil((ready.length || 1) / columns));
        if (this.gl) await this.composeGL(ready, columns, rows);
        else this.compose2D(ready, columns, rows);
        return this.canvas.transferToImageBitmap();
    }

    async composeGL(videos, columns, rows) {
        const gl = this.gl;
        gl.clearColor(0.04, 0.06, 0.09, 1);
        gl.clear(gl.COLOR_BUFFER_BIT);
        for (let i = 0; i < videos.length; i++) {
            let bitmap;
            try { bitmap = await createImageBitmap(videos[i]); } catch { continue; }
            const col = i % columns, row = Math.floor(i / columns);
            const width = Math.floor(this.canvas.width / columns), height = Math.floor(this.canvas.height / rows);
            gl.viewport(col * width, this.canvas.height - (row + 1) * height, width, height);
            gl.bindTexture(gl.TEXTURE_2D, this.texture);
            gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, bitmap);
            gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
            bitmap.close();
        }
        gl.flush();
    }

    compose2D(videos, columns, rows) {
        const ctx = this.ctx;
        ctx.fillStyle = "#0b1018";
        ctx.fillRect(0, 0, this.canvas.width, this.canvas.height);
        const width = this.canvas.width / columns, height = this.canvas.height / rows;
        videos.forEach((video, i) => ctx.drawImage(video, (i % columns) * width, Math.floor(i / columns) * height, width, height));
    }
}
