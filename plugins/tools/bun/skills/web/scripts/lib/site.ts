// Site scripts: run Tap-format scripts (`<site>/<name>.js`: a `/* @meta {...} */
// header followed by an `async function(args)`) through Lightpanda's `run`
// mode, and install new scripts from the Tap catalog, a URL, or a file.

import { closeSync, existsSync, mkdirSync, openSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { env } from "./http.ts";

const BUNDLED_DIR = fileURLToPath(new URL("../../sites/", import.meta.url));
const CATALOG_URL = "https://tap.vaayne.com/api/scripts/{name}/content";
const NAME_RE = /^[a-z0-9_-]+\/[a-z0-9_-]+$/;
const META_RE = /^\s*\/\*\s*@meta\s*(\{.*?\})\s*\*\/\s*(async\s+function\b.*)/s;
const ENV_REF_RE = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
const DEFAULT_TIMEOUT = 60;

export class SiteError extends Error {}
class LightpandaTimeout extends Error {}

export { DEFAULT_TIMEOUT, NAME_RE };

interface CatalogEntry {
  path: string;
  meta: any;
  body: string;
}

function userDir(): string {
  // Where installed and hand-written scripts live: $XDG_CACHE_HOME/site-scripts.
  //
  // In the sandbox that is the principal's shared cache, so a script added by one
  // agent is visible to every agent of the same user and survives sessions.
  return join(env("XDG_CACHE_HOME") || join(homedir(), ".cache"), "site-scripts");
}

function loadCatalog(): Map<string, CatalogEntry> {
  // Return {name: entry}; a user script shadows a bundled one of the same name.
  const catalog = new Map<string, CatalogEntry>();
  for (const base of [userDir(), BUNDLED_DIR]) {
    let dirs: string[];
    try {
      dirs = readdirSync(base, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => e.name).sort();
    } catch {
      continue;
    }
    for (const dir of dirs) {
      let files: string[];
      try {
        files = readdirSync(join(base, dir)).filter((f) => f.endsWith(".js")).sort();
      } catch {
        continue;
      }
      for (const file of files) {
        const name = `${dir}/${file.slice(0, -3)}`;
        if (catalog.has(name)) continue;
        const path = join(base, dir, file);
        try {
          const { meta, body } = parseScript(readFileSync(path, "utf8"));
          catalog.set(name, { path, meta, body });
        } catch (err) {
          console.error(`skipping ${path}: ${(err as Error).message}`);
        }
      }
    }
  }
  return catalog;
}

function parseScript(source: string): { meta: any; body: string } {
  const match = source.match(META_RE);
  if (!match) throw new SiteError("missing /* @meta */ header or async function body");
  let meta: any;
  try {
    meta = JSON.parse(match[1]);
  } catch (err) {
    throw new SiteError(`invalid @meta JSON: ${(err as Error).message}`);
  }
  if (typeof meta?.domain !== "string" || !meta.domain) throw new SiteError("@meta.domain is required");
  return { meta, body: match[2] };
}

function resolveHeaders(meta: any): Record<string, string> {
  // Expand ${VAR} references from the environment; drop headers whose variable is unset.
  const headers: Record<string, string> = {};
  for (const [key, template] of Object.entries(meta.headers ?? {})) {
    const value = String(template);
    const missing = [...value.matchAll(ENV_REF_RE)].some((m) => process.env[m[1]] === undefined);
    if (missing) continue;
    headers[key] = value.replace(ENV_REF_RE, (whole, name) => process.env[name] ?? whole);
  }
  return headers;
}

function parseArgsKV(pairs: string[]): Record<string, string> {
  const args: Record<string, string> = {};
  for (const pair of pairs) {
    const index = pair.indexOf("=");
    if (index <= 0) throw new SiteError(`argument '${pair}' must be key=value`);
    args[pair.slice(0, index)] = pair.slice(index + 1);
  }
  return args;
}

function checkRequired(meta: any, args: Record<string, string>): void {
  const missing = Object.entries(meta.args ?? {})
    .filter(([key, spec]) => key !== undefined && typeof spec === "object" && spec !== null && (spec as any).required && !(key in args))
    .map(([key]) => key)
    .sort();
  if (missing.length > 0) throw new SiteError("missing required args: " + missing.join(", "));
}

// pyJSON emits JSON exactly like Python's json.dumps compact form (", " and
// ": " separators) so the generated program is byte-identical to site.py's and
// callers can parse either.
function pyJSON(value: unknown): string {
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return "[" + value.map(pyJSON).join(", ") + "]";
  }
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return "{}";
    return "{" + entries.map(([key, item]) => `${JSON.stringify(key)}: ${pyJSON(item)}`).join(", ") + "}";
  }
  return JSON.stringify(value);
}

function pageProgram(body: string, args: Record<string, string>, headers: Record<string, string>, domain: string): string {
  return `(async () => {
  const __args = ${pyJSON(args)};
  const __headers = ${pyJSON(headers)};
  const __origin = "https://" + ${JSON.stringify(domain)};
  const __nativeFetch = globalThis.fetch.bind(globalThis);
  const fetch = (input, init = {}) => {
    const url = new URL(input instanceof Request ? input.url : String(input), location.href);
    const headers = new Headers(input instanceof Request ? input.headers : undefined);
    new Headers(init.headers || {}).forEach((value, name) => headers.set(name, value));
    if (url.origin === __origin) {
      for (const [name, value] of Object.entries(__headers)) headers.set(name, value);
    }
    return __nativeFetch(input, {...init, headers});
  };
  const result = await (${body})(__args);
  return JSON.stringify(result === undefined ? null : result);
})()`;
}

const NAVIGATE_TIMEOUT_MS = 10_000;
// Printed right before the result line so the runner can stop the browser as
// soon as the script has answered: Lightpanda otherwise keeps the process alive
// until every request the visited page started has finished or hit --http-timeout.
const RESULT_SENTINEL = "<<site-script-result>>";

function pandaScript(program: string, navigateURL: string): string {
  // Navigation gets its own short budget: a heavy site root that never reaches
  // domcontentloaded must not consume the whole run timeout before the script runs.
  return (
    "const page = new Page();\n" +
    `await page.goto(${JSON.stringify(navigateURL)}, { waitUntil: "domcontentloaded", timeout: ${NAVIGATE_TIMEOUT_MS} });\n` +
    `const result = await page.evaluate(${JSON.stringify(program)});\n` +
    `console.log(${JSON.stringify(RESULT_SENTINEL)});\n` +
    "return result;\n"
  );
}

function lightpandaBinary(): string {
  const binary = env("LIGHTPANDA_BIN") || Bun.which("lightpanda");
  if (!binary) {
    throw new SiteError(
      "lightpanda is not on PATH. The Lightpanda manifest tool installs it in the background after startup; retry shortly or ask an admin to enable tool/lightpanda.",
    );
  }
  return binary;
}

interface RunOutcome {
  returncode: number;
  resultLine: string | null;
  stderr: string;
}

async function runLightpanda(binary: string, program: string, navigateURL: string, timeoutSec: number): Promise<RunOutcome> {
  // Run the PandaScript and return the JSON line it produced.
  //
  // stdout is read line by line: the line after RESULT_SENTINEL is the answer,
  // and the browser is terminated at that point instead of waiting for the
  // page's background requests to drain. A process that exits on its own
  // (navigation failure, script exception) reports its exit code instead.
  const scriptPath = join(env("TMPDIR") || tmpdir(), `site-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2)}.js`);
  const stderrPath = `${scriptPath}.err`;
  writeFileSync(scriptPath, pandaScript(program, navigateURL));
  const cmd = [binary, "run", "--block-private-networks", "--http-timeout", String(timeoutSec * 1000), scriptPath];
  const deadline = Date.now() + timeoutSec * 1000;
  // stderr goes to a file, not a pipe: stdout is read first, and a chatty
  // browser log would otherwise fill the stderr pipe and stall the process.
  const stderrFd = openSync(stderrPath, "w");
  const proc = Bun.spawn(cmd, { stdout: "pipe", stderr: stderrFd, stdin: "ignore" });
  let resultLine: string | null = null;
  let lastLine: string | null = null;
  try {
    // A result must arrive before the deadline; the watchdog kill below
    // unblocks the stdout read when it does not.
    const watchdog = setTimeout(() => proc.kill(), Math.max(deadline - Date.now(), 0));
    try {
      let expectingResult = false;
      let buffer = "";
      const decoder = new TextDecoder();
      const reader = (proc.stdout as ReadableStream<Uint8Array>).getReader();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        for (;;) {
          const index = buffer.indexOf("\n");
          if (index < 0) break;
          const line = buffer.slice(0, index).replace(/\r$/, "");
          buffer = buffer.slice(index + 1);
          if (expectingResult) {
            resultLine = line;
            break;
          }
          if (line === RESULT_SENTINEL) expectingResult = true;
          else if (line.trim()) lastLine = line;
        }
        if (resultLine !== null) break;
      }
      // The last line may arrive without a trailing newline; it still counts.
      if (resultLine === null && buffer) {
        const line = buffer.replace(/\r$/, "");
        if (line === RESULT_SENTINEL) resultLine = "";
        else if (line.trim()) lastLine = line;
      }
      await reader.cancel().catch(() => {});
    } finally {
      clearTimeout(watchdog);
    }
    if (resultLine !== null) {
      // Nothing to flush: the temp script is ours and the answer is in hand.
      proc.kill();
    }
    const timedOut = resultLine === null && Date.now() >= deadline;
    // Wait up to 5s for a clean exit, then kill; a wedged browser must not
    // outlive the command that spawned it.
    await Promise.race([
      proc.exited,
      new Promise((resolve) =>
        setTimeout(() => {
          proc.kill();
          resolve(null);
        }, 5000),
      ),
    ]);
    if (timedOut) throw new LightpandaTimeout();
    const stderr = readFileSync(stderrPath, "utf8");
    if (resultLine === null && (proc.exitCode ?? -1) === 0) {
      // No sentinel (for example a stand-in binary): the last line is the answer.
      resultLine = lastLine;
    }
    return { returncode: proc.exitCode ?? -1, resultLine, stderr };
  } finally {
    closeSync(stderrFd);
    rmSync(scriptPath, { force: true });
    rmSync(stderrPath, { force: true });
  }
}

function stderrDetail(stderr: string): string {
  // Pick the lines of Lightpanda's stderr that explain a failed run.
  //
  // Lightpanda logs fatal errors at level=fatal and a mise shim that cannot
  // resolve the binary prints "mise ERROR", so matching only level=error would
  // swallow both and report "no output". Fall back to the last few lines so the
  // real reason always reaches the caller.
  const lines = stderr.split("\n").filter((line) => line.trim());
  const marked = lines.filter((line) => line.includes("level=error") || line.includes("level=fatal") || line.includes("Error") || line.includes("ERROR"));
  const picked = marked.length > 0 ? marked : lines.slice(-5);
  return picked.join("\n").trim() || "no output";
}

async function runScript(name: string, catalog: Map<string, CatalogEntry>, pairs: string[], timeoutSec: number): Promise<unknown> {
  const entry = name ? catalog.get(name) : undefined;
  if (!entry) {
    throw new SiteError(`unknown script '${name}'; run \`web.ts site list\`, or \`web.ts site add <site/name>\` to install it from the catalog`);
  }
  const { meta, body } = entry;
  if (meta.authRequired) throw new SiteError(`${name} needs a logged-in browser session, which Lightpanda does not provide`);
  const args = parseArgsKV(pairs);
  checkRequired(meta, args);
  const program = pageProgram(body, args, resolveHeaders(meta), meta.domain);
  const binary = lightpandaBinary();

  // Navigate to the declared domain first so same-origin requests carry its
  // cookies; a site whose root redirects in a loop still works from about:blank
  // because Lightpanda does not enforce CORS on fetch().
  let last: RunOutcome | undefined;
  for (const navigateURL of [`https://${meta.domain}/`, "about:blank"]) {
    try {
      last = await runLightpanda(binary, program, navigateURL, timeoutSec);
    } catch (err) {
      if (err instanceof LightpandaTimeout) throw new SiteError(`${name} exceeded ${timeoutSec}s`);
      throw err;
    }
    if (last.resultLine !== null) break;
  }
  if (!last || last.resultLine === null) {
    throw new SiteError(`lightpanda exited ${last?.returncode}: ${stderrDetail(last?.stderr ?? "")}`);
  }
  // The program returns JSON.stringify(result), which `run` prints verbatim.
  let result: any;
  try {
    result = JSON.parse(last.resultLine);
  } catch {
    throw new SiteError(`${name} returned non-JSON output: ${JSON.stringify(last.resultLine.slice(0, 300))}`);
  }
  // Catalog scripts wrap their payload in a versioned envelope; only the data matters here.
  if (result !== null && typeof result === "object" && "__pinix_site_result" in result && "data" in result) return result.data;
  return result;
}

async function fetchText(url: string): Promise<string> {
  let resp: Response;
  try {
    resp = await fetch(url, { headers: { "User-Agent": "stella-site-scripts" }, signal: AbortSignal.timeout(30_000) });
  } catch (err) {
    throw new SiteError(`cannot fetch ${url}: ${(err as Error).message}`);
  }
  if (!resp.ok) throw new SiteError(`${url} answered HTTP ${resp.status}`);
  try {
    return await resp.text();
  } catch (err) {
    throw new SiteError(`cannot fetch ${url}: ${(err as Error).message}`);
  }
}

// Install a script from a catalog name, a URL, or a local file into userDir().
// The catalog is the Tap site-script index; a name like `bilibili/ranking` is
// fetched from it, so the small bundled set is a floor, not a ceiling.
async function addScript(source: string, name?: string): Promise<{ name: string; path: string }> {
  let text: string;
  let defaultName: string | undefined;
  if (source.startsWith("http://") || source.startsWith("https://")) {
    text = await fetchText(source);
  } else if (NAME_RE.test(source) && !existsSync(source)) {
    text = await fetchText(CATALOG_URL.replace("{name}", source));
    defaultName = source;
  } else if (existsSync(source) && statSync(source).isFile()) {
    text = readFileSync(source, "utf8");
    defaultName = `${basename(dirname(resolve(source)))}/${basename(source).replace(/\.js$/, "")}`;
  } else {
    throw new SiteError(`'${source}' is not a catalog name, URL, or existing file`);
  }
  const { meta } = parseScript(text);
  const finalName = name || meta.name || defaultName;
  if (!finalName || !NAME_RE.test(finalName)) {
    throw new SiteError("cannot infer a site/name for this script; pass --name <site>/<name>");
  }
  if (meta.authRequired) {
    console.error(`warning: ${finalName} declares authRequired and will be refused at run time (no login session)`);
  }
  const target = join(userDir(), `${finalName}.js`);
  mkdirSync(dirname(target), { recursive: true });
  writeFileSync(target, text);
  return { name: finalName, path: target };
}

function describe(name: string, meta: any): string {
  const parts = Object.entries(meta.args ?? {}).map(([key, spec]) =>
    typeof spec === "object" && spec !== null && (spec as any).required ? key : `[${key}]`,
  );
  return `${name.padEnd(34)} ${String(meta.domain).padEnd(24)} ${parts.join(" ")}\n    ${String(meta.description ?? "").trim()}`;
}

export interface SiteFlags {
  json?: string | true;
  raw?: string | true;
  timeout?: string | true;
  name?: string | true;
}

// siteMain handles one `web.ts site` invocation and returns the exit code:
// 0 success, 1 when a run's result is `{"error": ...}`, 2 usage/runtime
// failure (SiteError prints "error: ..." on stderr at the CLI boundary).
export async function siteMain(positional: string[], flags: SiteFlags): Promise<number> {
  const [sub = "", ...rest] = positional;
  if (sub === "add") {
    const source = rest[0];
    if (!source) throw new SiteError("add needs a source: <site/name>, a URL, or a file");
    const { name, path } = await addScript(source, typeof flags.name === "string" ? flags.name : undefined);
    console.log(`installed ${name} -> ${path}`);
    return 0;
  }
  const catalog = loadCatalog();
  if (sub === "list") {
    if (flags.json) {
      const metas: Record<string, unknown> = {};
      for (const [name, entry] of catalog) metas[name] = entry.meta;
      console.log(JSON.stringify(metas, null, 2));
      return 0;
    }
    for (const [name, entry] of catalog) console.log(describe(name, entry.meta));
    return 0;
  }
  if (sub === "info") {
    const name = rest[0];
    const entry = name ? catalog.get(name) : undefined;
    if (!entry) {
      throw new SiteError(`unknown script '${name}'; run \`web.ts site list\`, or \`web.ts site add <site/name>\` to install it from the catalog`);
    }
    console.log(JSON.stringify({ ...entry.meta, name, path: entry.path }, null, 2));
    return 0;
  }
  if (sub === "run") {
    const name = rest[0];
    if (!name) throw new SiteError("run needs a script name; run `web.ts site list` for the catalog");
    const timeoutFlag = flags.timeout;
    const timeout = timeoutFlag === true || timeoutFlag === undefined ? DEFAULT_TIMEOUT : Number.parseInt(String(timeoutFlag), 10);
    if (!Number.isFinite(timeout) || timeout < 1) throw new SiteError("--timeout must be a number of seconds");
    const result = await runScript(name, catalog, rest.slice(1), timeout);
    console.log(flags.raw ? pyJSON(result) : JSON.stringify(result, null, 2));
    return result !== null && typeof result === "object" && "error" in result ? 1 : 0;
  }
  throw new SiteError(`unknown site command '${sub}'`);
}
