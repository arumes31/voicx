import { execFile } from "node:child_process";
import { readdir, readFile } from "node:fs/promises";
import { extname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

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

function checkText(path, source) {
    if (!source.endsWith("\n")) report(path, 0, "file must end with a newline");

    const lines = source.split(/\r?\n/);
    for (let index = 0; index < lines.length; index++) {
        const line = lines[index];
        if (/[ \t]+$/.test(line)) report(path, index + 1, "trailing whitespace");
        if (line.includes("\t")) report(path, index + 1, "use spaces instead of tabs");
        if (/^(<{7}|={7}|>{7})/.test(line)) report(path, index + 1, "unresolved merge marker");
    }

    if (!displayPath(path).startsWith("src/")) return;
    const unsafePatterns = [
        [/\beval\s*\(/, "eval()"],
        [/\bnew\s+Function\s*\(/, "new Function()"],
        [/\bdocument\.write\s*\(/, "document.write()"],
    ];
    for (let index = 0; index < lines.length; index++) {
        const suppression = `${lines[index - 1] || ""}\n${lines[index]}`.includes("lint-allow unsafe-js");
        if (suppression) continue;
        for (const [pattern, label] of unsafePatterns) {
            if (pattern.test(lines[index])) report(path, index + 1, `${label} requires an explicit lint-allow unsafe-js comment`);
        }
    }
}

const files = [resolve(root, "index.html")];
for (const sourceRoot of sourceRoots) files.push(...await collectFiles(resolve(root, sourceRoot)));
files.sort();

for (const file of files) {
    const source = await readFile(file, "utf8");
    checkText(file, source);
    if ([".js", ".mjs"].includes(extname(file))) {
        try {
            await execFileAsync(process.execPath, ["--check", file]);
        } catch (error) {
            report(file, 0, String(error.stderr || error.message).trim());
        }
    }
}

if (problems.length > 0) {
    console.error(`Frontend lint failed with ${problems.length} problem${problems.length === 1 ? "" : "s"}:`);
    for (const problem of problems) console.error(`- ${problem}`);
    process.exitCode = 1;
} else {
    console.log(`Frontend lint passed (${files.length} files checked).`);
}
