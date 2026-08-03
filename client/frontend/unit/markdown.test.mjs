import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { EMOJI, escapeHTML, renderMarkdown } from "../src/markdown.js";

describe("escapeHTML", () => {
    it("escapes every HTML-significant character and coerces values", () => {
        assert.equal(escapeHTML(`<&>"'`), "&lt;&amp;&gt;&quot;&#39;");
        assert.equal(escapeHTML(42), "42");
    });
});

describe("renderMarkdown", () => {
    it("returns an empty string for empty input", () => {
        assert.equal(renderMarkdown(""), "");
        assert.equal(renderMarkdown(null), "");
    });

    it("renders supported inline markup and emoji", () => {
        const html = renderMarkdown("**bold** __under__ ~~gone~~ *soft* :SMILE: :unknown:");
        assert.equal(
            html,
            "<strong>bold</strong> <u>under</u> <del>gone</del> <em>soft</em> 😄 :unknown:",
        );
        assert.equal(EMOJI.thumbsup, "👍");
    });

    it("keeps inline code opaque and escapes its contents", () => {
        assert.equal(
            renderMarkdown("`<img src=x onerror=alert(1)> **not bold**`"),
            '<code class="md-code">&lt;img src=x onerror=alert(1)&gt; **not bold**</code>',
        );
    });

    it("escapes raw HTML and rejects non-http image URLs", () => {
        const html = renderMarkdown('<script>alert("x")</script> ![x](javascript:alert(1))');
        assert.match(html, /&lt;script&gt;/);
        assert.doesNotMatch(html, /<script|<img/);
        assert.match(html, /!\[x\]\(javascript:alert\(1\)\)/);
    });

    it("renders safe images and autolinks with escaped query separators", () => {
        const html = renderMarkdown("![diagram](https://example.test/a.png) https://example.test/?a=1&b=2");
        assert.match(html, /<img class="md-img" alt="diagram" src="https:\/\/example\.test\/a\.png">/);
        assert.match(html, /class="md-link"/);
        assert.match(html, /data-url="https:\/\/example\.test\/\?a=1&amp;b=2"/);
    });

    it("renders valid tables and leaves malformed tables as text", () => {
        const valid = renderMarkdown("A | B\n--- | :---:\none | two");
        assert.match(valid, /<table class="md-table">/);
        assert.match(valid, /<th>A<\/th><th>B<\/th>/);
        assert.match(valid, /<td>one<\/td><td>two<\/td>/);
        assert.doesNotMatch(valid, /<br>/);

        const malformed = renderMarkdown("A | B\n---\none | two");
        assert.doesNotMatch(malformed, /<table/);
        assert.match(malformed, /A \| B<br>---<br>one \| two/);
    });

    it("stops a table when a row has the wrong number of cells", () => {
        const html = renderMarkdown("A | B\n--- | ---\none | two\nonly-one");
        assert.match(html, /<td>one<\/td><td>two<\/td>/);
        assert.match(html, /<\/div>only-one$/);
    });

    it("detects common fenced-code languages and highlights code", () => {
        const cases = [
            ["package main\nfunc main() {}", "go"],
            ["const answer = () => 42", "javascript"],
            ["def answer():\n  return 42", "python"],
            ["SELECT * FROM users", "sql"],
            [".card { color: red; }", "css"],
            ["<section>hello</section>", "markup"],
            ["ordinary text", "plain"],
        ];
        for (const [code, language] of cases) {
            assert.match(renderMarkdown("```\n" + code + "\n```"), new RegExp(`language-${language}`));
        }

        const highlighted = renderMarkdown('```js\nconst value = "x"; // note\n```');
        assert.match(highlighted, /class="tk-kw">const<\/span>/);
        assert.match(highlighted, /class="tk-str">&quot;x&quot;<\/span>/);
        assert.match(highlighted, /class="tk-com">\/\/ note<\/span>/);
    });

    it("accepts an unterminated fenced block and trims its final newline", () => {
        assert.equal(
            renderMarkdown("```txt\nhello\n"),
            '<pre class="md-pre language-txt"><code>hello</code></pre>',
        );
    });
});
