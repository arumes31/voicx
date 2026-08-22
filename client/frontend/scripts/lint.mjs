import { execFile } from "node:child_process";
import { readdir, readFile } from "node:fs/promises";
import { extname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { parse } from "acorn";
import { analyze } from "eslint-scope";

const execFileAsync = promisify(execFile);
const root = resolve(fileURLToPath(new URL("..", import.meta.url)));
const sourceRoots = ["src", "tests", "unit", "scripts"];
const checkedExtensions = new Set([".css", ".html", ".js", ".mjs"]);
const problems = [];

async function collectFiles(path) {
    const entries = await readdir(path, { withFileTypes: true });
    const files = [];
    for (const entry of entries) {
        const entryPath = resolve(path, entry.name);
        if (entry.isDirectory()) files.push(...await collectFiles(entryPath));
        else if (checkedExtensions.has(extname(entry.name))) files.push(entryPath);
    }
    return files;
}

function displayPath(path) {
    return relative(root, path).replaceAll("\\", "/");
}

function report(path, line, message) {
    problems.push(`${displayPath(path)}${line ? `:${line}` : ""}: ${message}`);
}

function visit(node, callback) {
    if (!node || typeof node !== "object") return;
    if (typeof node.type === "string") callback(node);
    for (const [key, value] of Object.entries(node)) {
        if (key === "parent" || key === "loc" || key === "range") continue;
        if (Array.isArray(value)) {
            for (const child of value) visit(child, callback);
        } else if (value && typeof value === "object") {
            visit(value, callback);
        }
    }
}

function unwrapChain(node) {
    return node?.type === "ChainExpression" ? node.expression : node;
}

function staticMemberName(member) {
    if (!member.computed && member.property.type === "Identifier") return member.property.name;
    if (member.computed && member.property.type === "Literal" && typeof member.property.value === "string") {
        return member.property.value;
    }
    return "computed";
}

const UI_MEMBER_SINKS = new Set(["ariaLabel", "innerHTML", "innerText", "placeholder", "textContent", "title"]);
const UI_ATTRIBUTE_SINKS = new Set(["aria-label", "placeholder", "title"]);
const UI_CALL_SINKS = new Set([
    "alert", "btn", "confirm", "confirmDialog", "hint", "menuAction", "mk", "prompt",
    "promptDialog", "row", "sysMsg", "toast", "modal",
]);
const TRANSLATION_CATALOGS = new Set(["catalog", "catalogs", "de", "en", "i18n", "translation", "translations"]);
const TICKET_SUFFIX = /\((?:\d{2,3})(?:\/\d{2,3})?(?:,\s*[^)]*)?\)/g;

function staticTextNodes(node, nodes = []) {
    if (!node || typeof node !== "object") return nodes;
    if (node.type === "Literal" && typeof node.value === "string") {
        nodes.push(node);
        return nodes;
    }
    if (node.type === "TemplateElement") {
        nodes.push({ value: node.value.cooked || "", loc: node.loc });
        return nodes;
    }
    // Only follow value-shaped expressions. In particular, callbacks and
    // classes passed to UI helpers contain implementation text, not visible
    // copy, so generic AST recursion here would produce false positives.
    switch (node.type) {
    case "ArrayExpression":
        node.elements.forEach((element) => staticTextNodes(element, nodes));
        break;
    case "BinaryExpression":
    case "LogicalExpression":
        staticTextNodes(node.left, nodes);
        staticTextNodes(node.right, nodes);
        break;
    case "ConditionalExpression":
        staticTextNodes(node.consequent, nodes);
        staticTextNodes(node.alternate, nodes);
        break;
    case "ObjectExpression":
        for (const property of node.properties) {
            if (property.type === "Property" && property.kind === "init") staticTextNodes(property.value, nodes);
            else if (property.type === "SpreadElement") staticTextNodes(property.argument, nodes);
        }
        break;
    case "TemplateLiteral":
        node.quasis.forEach((quasi) => staticTextNodes(quasi, nodes));
        break;
    }
    return nodes;
}

function isTranslationCatalog(node) {
    return node.type === "VariableDeclarator" && node.id?.type === "Identifier" &&
        node.init?.type === "ObjectExpression" && TRANSLATION_CATALOGS.has(node.id.name.toLowerCase());
}

function ticketSuffixesInText(node) {
    const text = String(node.value || "");
    const suffixes = [];
    for (const match of text.matchAll(TICKET_SUFFIX)) {
        suffixes.push({ line: node.loc?.start.line || 0, suffix: match[0] });
    }
    return suffixes;
}

function isUISinkCall(node) {
    const callee = unwrapChain(node.callee);
    if (callee?.type === "Identifier") return UI_CALL_SINKS.has(callee.name);
    if (callee?.type !== "MemberExpression") return false;
    const name = staticMemberName(callee);
    if (name === "setAttribute") {
        const attribute = node.arguments[0];
        return attribute?.type === "Literal" && UI_ATTRIBUTE_SINKS.has(attribute.value);
    }
    return UI_CALL_SINKS.has(name);
}

// uiTicketSuffixes checks only static text flowing to known UI sinks. Acorn
// excludes comments from the AST, so issue references in implementation notes
// remain allowed while text that reaches a title, label, hint, menu, or live
// announcement is rejected.
export function uiTicketSuffixes(source) {
    let ast;
    try {
        ast = parse(String(source), { ecmaVersion: "latest", sourceType: "module", locations: true, ranges: true });
    } catch {
        return [];
    }
    const findings = [];
    visit(ast, (node) => {
        let values = [];
        if (node.type === "AssignmentExpression" && node.left?.type === "MemberExpression" &&
            UI_MEMBER_SINKS.has(staticMemberName(node.left))) {
            values = staticTextNodes(node.right);
        } else if (node.type === "CallExpression" && isUISinkCall(node)) {
            const callee = unwrapChain(node.callee);
            const args = callee?.type === "MemberExpression" && staticMemberName(callee) === "setAttribute"
                ? node.arguments.slice(1) : node.arguments;
            values = args.flatMap((arg) => staticTextNodes(arg));
        } else if (isTranslationCatalog(node)) {
            values = staticTextNodes(node.init);
        }
        for (const value of values) findings.push(...ticketSuffixesInText(value));
    });
    return findings;
}

export function htmlTicketSuffixes(source) {
    const clean = String(source).replace(/<!--[\s\S]*?-->/g, (comment) => comment.replace(/[^\n]/g, " "));
    const findings = [];
    const scan = /(?:\b(?:title|placeholder|aria-label)\s*=\s*["']([^"']*)["']|>([^<>]+)<)/g;
    for (const match of clean.matchAll(scan)) {
        const text = match[1] || match[2] || "";
        for (const suffix of text.matchAll(TICKET_SUFFIX)) {
            const before = clean.slice(0, match.index + suffix.index);
            findings.push({ line: before.split(/\r?\n/).length, suffix: suffix[0] });
        }
    }
    return findings;
}

// Console calls are identified from the parsed syntax tree and the resolver's
// unresolved global references. This catches optional/computed/parenthesized
// calls without treating examples, property names, regexes, or locally scoped
// variables as global console access.
export function unsafeConsoleMethods(source) {
    let ast;
    try {
        ast = parse(String(source), { ecmaVersion: "latest", sourceType: "module", ranges: true });
    } catch {
        return [];
    }
    const scopeManager = analyze(ast, {
        ecmaVersion: 2022,
        sourceType: "module",
        optimistic: true,
        ignoreEval: true,
    });
    const globalConsoleReferences = new Set(scopeManager.globalScope.through
        .filter((reference) => reference.identifier.name === "console")
        .map((reference) => reference.identifier));
    const methods = [];
    visit(ast, (node) => {
        if (node.type !== "CallExpression") return;
        const callee = unwrapChain(node.callee);
        if (callee?.type !== "MemberExpression") return;
        const object = unwrapChain(callee.object);
        if (object?.type !== "Identifier" || object.name !== "console" || !globalConsoleReferences.has(object)) return;
        const method = staticMemberName(callee);
        if (method !== "warn" && method !== "error") methods.push(method);
    });
    return methods;
}

export function lintText(path, source, reportProblem = report) {
    if (!source.endsWith("\n")) reportProblem(path, 0, "file must end with a newline");

    const lines = source.split(/\r?\n/);
    for (let index = 0; index < lines.length; index++) {
        const line = lines[index];
        if (/[ \t]+$/.test(line)) reportProblem(path, index + 1, "trailing whitespace");
        if (line.includes("\t")) reportProblem(path, index + 1, "use spaces instead of tabs");
        if (/^(<{7}|={7}|>{7})/.test(line)) reportProblem(path, index + 1, "unresolved merge marker");
    }

    const sourcePath = displayPath(path);
    const ticketSuffixes = sourcePath === "index.html"
        ? htmlTicketSuffixes(source)
        : sourcePath.startsWith("src/") ? uiTicketSuffixes(source) : [];
    for (const finding of ticketSuffixes) {
        reportProblem(path, finding.line, `user-facing ticket suffix ${finding.suffix} is not allowed`);
    }

    if (!sourcePath.startsWith("src/")) return;
    const unsafePatterns = [
        [/\beval\s*\(/, "eval()"],
        [/\bnew\s+Function\s*\(/, "new Function()"],
        [/\bdocument\.write\s*\(/, "document.write()"],
    ];
    for (let index = 0; index < lines.length; index++) {
        const suppression = `${lines[index - 1] || ""}\n${lines[index]}`.includes("lint-allow unsafe-js");
        if (suppression) continue;
        for (const [pattern, label] of unsafePatterns) {
            if (pattern.test(lines[index])) reportProblem(path, index + 1, `${label} requires an explicit lint-allow unsafe-js comment`);
        }
    }
    const unsafeConsole = unsafeConsoleMethods(source);
    for (const method of unsafeConsole) {
        const access = method === "computed" ? "console[computed]()" : `console.${method}()`;
        reportProblem(path, 0, `${access} is not allowed in src (use console.warn/error only)`);
    }
}

export async function lintProject() {
    problems.length = 0;
    const files = [resolve(root, "index.html")];
    for (const sourceRoot of sourceRoots) files.push(...await collectFiles(resolve(root, sourceRoot)));
    files.sort();

    for (const file of files) {
        const source = await readFile(file, "utf8");
        lintText(file, source);
        if ([".js", ".mjs"].includes(extname(file))) {
            try {
                await execFileAsync(process.execPath, ["--check", file]);
            } catch (error) {
                report(file, 0, String(error.stderr || error.message).trim());
            }
        }
    }

    return { files, problems: [...problems] };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
    const result = await lintProject();
    if (result.problems.length > 0) {
        console.error(`Frontend lint failed with ${result.problems.length} problem${result.problems.length === 1 ? "" : "s"}:`);
        for (const problem of result.problems) console.error(`- ${problem}`);
        process.exitCode = 1;
    } else {
        console.log(`Frontend lint passed (${result.files.length} files checked).`);
    }
}
