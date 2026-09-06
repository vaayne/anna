// Fetch: read one page through the tiers (plain HTTP + Defuddle, Lightpanda
// render, Jina Reader) and render it for output.

import { existsSync, mkdirSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { UNTRUSTED, USER_AGENT, MAX_BODY_BYTES, TerminalError, UsageError, env, readCapped, request } from "./http.ts";

const JINA_READER = "https://r.jina.ai/";

interface Article {
  title?: string;
  author?: string;
  description?: string;
  site?: string;
  published?: string;
  content: string; // HTML, or Markdown when requested
  wordCount?: number;
}

interface Page {
  url: string;
  article?: Article;
  raw?: string;
}

// Dependencies live in a per-user cache keyed by the package.json hash, so an
// upgrade installs fresh and two agents of one user share one install.
async function loadExtractor(): Promise<{ parseHTML: (html: string) => { document: any }; Defuddle: (document: any, url: string, options: any) => Promise<Article> }> {
  const pkg = await Bun.file(new URL("../package.json", import.meta.url)).text();
  const hash = new Bun.CryptoHasher("sha256").update(pkg).digest("hex").slice(0, 12);
  const dir = join(env("XDG_CACHE_HOME") || join(homedir(), ".cache"), "web-skill", `deps-${hash}`);
  const entry = join(dir, "entry.ts");
  if (!existsSync(entry)) {
    const staging = `${dir}.tmp-${process.pid}`;
    rmSync(staging, { recursive: true, force: true });
    mkdirSync(staging, { recursive: true });
    writeFileSync(join(staging, "package.json"), pkg);
    const proc = Bun.spawnSync(["bun", "install", "--production", "--no-progress"], { cwd: staging, stdout: "pipe", stderr: "pipe" });
    if (proc.exitCode !== 0) {
      const detail = proc.stderr.toString().trim().split("\n").slice(-5).join("\n");
      throw new Error(`installing extractor dependencies failed (needs network on first run):\n${detail}`);
    }
    writeFileSync(join(staging, "entry.ts"), 'export { parseHTML } from "linkedom";\nexport { Defuddle } from "defuddle/node";\n');
    try {
      renameSync(staging, dir);
    } catch {
      rmSync(staging, { recursive: true, force: true }); // another run won the race
    }
  }
  return await import(entry);
}

function mediaType(resp: Response): string {
  return (resp.headers.get("content-type") ?? "").split(";")[0].trim().toLowerCase();
}

function parseFetchURL(raw: string): URL {
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    throw new UsageError(`invalid url: ${raw}`);
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") throw new UsageError(`unsupported scheme ${parsed.protocol} (only http and https)`);
  return parsed;
}

// extract runs Defuddle over a DOM built by linkedom. Inline SVG survives
// Defuddle as raw markup, so it is dropped first; an extractor crash on an odd
// page (linkedom rejects some selectors) counts as no readable content.
async function extract(html: string, url: string, markdown: boolean): Promise<Article> {
  const { parseHTML, Defuddle } = await loadExtractor();
  const cleaned = html.replace(/<svg[\s\S]*?<\/svg>/gi, "").replace(/<noscript[\s\S]*?<\/noscript>/gi, "");
  const stderr = console.error;
  console.error = () => {}; // Defuddle logs recoverable selector errors with a stack trace
  try {
    return await Defuddle(parseHTML(cleaned).document, url, { markdown });
  } catch (err) {
    throw new Error(`extractor failed: ${String((err as Error).message ?? err).split("\n")[0]}`);
  } finally {
    console.error = stderr;
  }
}

// renderWithLightpanda returns the DOM after JavaScript ran, or undefined when
// Lightpanda is not installed. The manifest plugin installs it in the
// background after startup, so a fresh deployment may lack it for a minute.
function renderWithLightpanda(url: URL): string | undefined {
  const binary = env("LIGHTPANDA_BIN") || Bun.which("lightpanda");
  if (!binary) return undefined;
  const proc = Bun.spawnSync([binary, "fetch", "--dump", "html", "--dump-max-bytes", String(MAX_BODY_BYTES), "--fail-on-http-error", url.toString()], {
    stdout: "pipe",
    stderr: "pipe",
    timeout: 45_000,
  });
  const html = proc.stdout.toString();
  if (proc.exitCode !== 0 || !html.trim()) {
    const detail = proc.stderr.toString().trim().split("\n").slice(-3).join(" ");
    throw new Error(`lightpanda: exit ${proc.exitCode ?? "timeout"}${detail ? `: ${detail}` : ""}`);
  }
  return html;
}

async function fetchPage(url: URL, markdown: boolean): Promise<Page> {
  const resp = await request("fetch", url.toString(), {
    headers: { "User-Agent": USER_AGENT },
    accept: "text/markdown, text/html;q=0.9, application/json;q=0.8, */*;q=0.5",
    maxBytes: MAX_BODY_BYTES,
  });
  const type = mediaType(resp);
  if (type === "text/plain" || type === "text/markdown" || type === "application/json") {
    return { url: resp.url || url.toString(), raw: await readCapped("fetch", resp, MAX_BODY_BYTES) };
  }
  if (type !== "text/html" && type !== "application/xhtml+xml" && type !== "") {
    throw new Error(`unsupported content type ${type}; download it with curl -o and use xberg extract for documents`);
  }
  const html = await readCapped("fetch", resp, MAX_BODY_BYTES);
  const finalURL = resp.url || url.toString();
  return { url: finalURL, article: await extract(html, finalURL, markdown) };
}

async function fetchJinaReader(url: URL): Promise<string> {
  const resp = await request("jina reader", JINA_READER + url.toString(), { accept: "text/markdown", headers: { "X-No-Cache": "true" }, maxBytes: MAX_BODY_BYTES });
  const body = await readCapped("jina reader", resp, MAX_BODY_BYTES);
  const index = body.indexOf("Markdown Content:");
  const content = index >= 0 ? body.slice(index + "Markdown Content:".length).trim() : "";
  if (!content) throw new Error("jina reader: response has no markdown content");
  return content;
}

function htmlToText(html: string): string {
  return html
    .replace(/<script[\s\S]*?<\/script>|<style[\s\S]*?<\/style>/gi, " ")
    .replace(/<[^>]+>/g, " ")
    .replace(/&nbsp;/g, " ")
    .replace(/&amp;/g, "&")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .split(/\s+/)
    .filter(Boolean)
    .join(" ");
}

function thin(article: Article | undefined): boolean {
  return !article || !article.content.trim() || (article.wordCount === 0 && htmlToText(article.content) === "");
}

// readPage walks the tiers: plain HTTP, then a Lightpanda render when the
// plain page has no readable body, then Jina Reader. Each tier's failure is
// kept so the final error explains every attempt.
async function readPage(raw: string, format: string, render: boolean): Promise<{ page: Page; fallback?: string }> {
  const url = parseFetchURL(raw);
  const markdown = format === "markdown";
  const failures: string[] = [];
  let page: Page = { url: url.toString() };
  if (!render) {
    try {
      page = await fetchPage(url, markdown);
      if (page.raw !== undefined || !thin(page.article)) return { page };
      failures.push("plain fetch: no readable content");
    } catch (err) {
      if (err instanceof UsageError || err instanceof TerminalError) throw err;
      failures.push((err as Error).message);
    }
  }
  try {
    const html = renderWithLightpanda(url);
    if (html === undefined) failures.push("lightpanda: not on PATH");
    else {
      const article = await extract(html, url.toString(), markdown);
      if (!thin(article)) return { page: { url: url.toString(), article }, fallback: "lightpanda" };
      page.article ??= article;
      failures.push("lightpanda: rendered page has no readable content");
    }
  } catch (err) {
    failures.push((err as Error).message);
  }
  try {
    return { page: { url: url.toString(), raw: await fetchJinaReader(url) }, fallback: "jina reader" };
  } catch (err) {
    failures.push((err as Error).message);
  }
  const title = page.article?.title ? ` (title: ${page.article.title})` : "";
  throw new Error(`no readable content at ${url}${title}:\n  ${failures.join("\n  ")}\nThe page probably blocks bots, needs a login, or has no article-like body; for an app-like site use a site script instead.`);
}

function render(page: Page, format: string, fallback?: string): string {
  const source = `> ${UNTRUSTED} Source: ${page.url}${fallback ? ` (via ${fallback})` : ""}`;
  if (format === "json") {
    const article = page.article;
    return JSON.stringify({
      url: page.url,
      title: article?.title || undefined,
      author: article?.author || undefined,
      description: article?.description || undefined,
      site: article?.site || undefined,
      published: article?.published || undefined,
      content: article ? htmlToText(article.content) : page.raw ?? "",
      untrusted: true,
      note: UNTRUSTED,
    });
  }
  if (page.raw !== undefined) return `${source}\n\n${page.raw}`;
  const article = page.article!;
  if (format === "html") return `<!-- ${UNTRUSTED} -->\n${article.content}`;
  if (format === "text") return `${source}\n\n${article.title ? `${article.title}\n\n` : ""}${htmlToText(article.content)}`;
  const head = [article.title ? `# ${article.title}` : "", article.author ? `**Author:** ${article.author}` : "", article.published ? `**Published:** ${article.published}` : ""].filter(Boolean);
  return `${source}\n\n${head.length ? `${head.join("\n\n")}\n\n` : ""}${article.content.trim()}`;
}

// Output above this size is spilled to a file; bash truncates long results
// anyway, and the head plus a path is more useful than a cut-off page.
const INLINE_LIMIT_CHARS = 40_000;

function spillPath(url: string, format: string): string {
  const slug = url.replace(/^https?:\/\//, "").replace(/[^a-z0-9]+/gi, "-").replace(/^-|-$/g, "").slice(0, 60).toLowerCase();
  const hash = new Bun.CryptoHasher("sha256").update(url).digest("hex").slice(0, 8);
  const ext = format === "json" ? "json" : format === "html" ? "html" : format === "text" ? "txt" : "md";
  const dir = join(env("TMPDIR") || tmpdir(), "web-fetch");
  mkdirSync(dir, { recursive: true });
  return join(dir, `${slug}-${hash}.${ext}`);
}

export { INLINE_LIMIT_CHARS, readPage, render, spillPath };
