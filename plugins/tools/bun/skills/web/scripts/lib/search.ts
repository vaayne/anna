// Search: the nine providers in fixed order, Exa's anonymous MCP endpoint as
// the keyless fallback, and the resolver that takes the first provider that
// answers.

import { type Env, MAX_PROVIDER_BYTES, env, readCapped, request, requestJSON } from "./http.ts";

interface SearchResult {
  title: string;
  url: string;
  snippet: string;
  score?: number;
}

interface Provider {
  name: string;
  available: (get: Env) => boolean;
  validate?: (get: Env) => string | undefined;
  search: (query: string, limit: number, get: Env) => Promise<SearchResult[]>;
}

// rows rejects a syntactically valid but structurally invalid provider
// response so the resolver falls through instead of returning nothing.
function rows(value: unknown): SearchResult[] {
  if (!Array.isArray(value)) throw new Error("search results field is missing or invalid");
  return value.map((item) => {
    if (typeof item !== "object" || item === null) throw new Error("search result row is invalid");
    const row = item as Record<string, unknown>;
    return {
      title: stringValue(row, "title", "name"),
      url: stringValue(row, "url", "href", "link"),
      snippet: stringValue(row, "description", "snippet", "content", "body", "highlights", "excerpts"),
      score: typeof row.score === "number" ? row.score : undefined,
    };
  });
}

function stringValue(row: Record<string, unknown>, ...names: string[]): string {
  for (const name of names) {
    const value = row[name];
    if (typeof value === "string" && value !== "") return value;
    if (Array.isArray(value)) {
      const parts = value.filter((item): item is string => typeof item === "string" && item !== "");
      if (parts.length > 0) return parts.join(" ");
    }
  }
  return "";
}

function baseURL(value: string, fallback: string): string {
  return (value || fallback).replace(/\/+$/, "");
}

function validHTTPURL(value: string, name: string): string | undefined {
  try {
    const parsed = new URL(value);
    if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || parsed.username || parsed.password) throw new Error();
    return undefined;
  } catch {
    return `${name} must be an http or https URL without userinfo`;
  }
}

const optionalURL = (name: string) => (get: Env) => (get(name) ? validHTTPURL(get(name), name) : undefined);

const EXA_MCP_URL = "https://mcp.exa.ai/mcp?tools=web_search_exa";

const providers: Provider[] = [
  {
    name: "firecrawl",
    available: (get) => Boolean(get("FIRECRAWL_API_KEY") || get("FIRECRAWL_API_URL")),
    validate: optionalURL("FIRECRAWL_API_URL"),
    async search(query, limit, get) {
      const headers: Record<string, string> = {};
      if (get("FIRECRAWL_API_KEY")) headers.Authorization = `Bearer ${get("FIRECRAWL_API_KEY")}`;
      const data = await requestJSON("firecrawl", `${baseURL(get("FIRECRAWL_API_URL"), "https://api.firecrawl.dev")}/v2/search`, {
        method: "POST",
        headers,
        body: { query, limit },
      });
      if (typeof data?.data !== "object" || data.data === null) throw new Error("firecrawl: response has no data object");
      return rows(data.data.web);
    },
  },
  {
    name: "parallel",
    available: (get) => Boolean(get("PARALLEL_API_KEY")),
    validate: (get) => {
      const mode = get("PARALLEL_SEARCH_MODE");
      return mode === "" || ["fast", "one-shot", "agentic"].includes(mode) ? undefined : "PARALLEL_SEARCH_MODE must be agentic, fast, or one-shot";
    },
    async search(query, limit, get) {
      const data = await requestJSON("parallel", "https://api.parallel.ai/v1beta/search", {
        method: "POST",
        headers: { "X-API-Key": get("PARALLEL_API_KEY") },
        body: { search_queries: [query], objective: query, mode: get("PARALLEL_SEARCH_MODE") || "agentic", max_results: limit },
      });
      return rows(data?.results);
    },
  },
  {
    name: "tavily",
    available: (get) => Boolean(get("TAVILY_API_KEY")),
    validate: optionalURL("TAVILY_BASE_URL"),
    async search(query, limit, get) {
      const data = await requestJSON("tavily", `${baseURL(get("TAVILY_BASE_URL"), "https://api.tavily.com")}/search`, {
        method: "POST",
        headers: { Authorization: `Bearer ${get("TAVILY_API_KEY")}` },
        body: { query, max_results: limit, include_raw_content: false, include_images: false },
      });
      return rows(data?.results);
    },
  },
  {
    name: "exa",
    available: (get) => Boolean(get("EXA_API_KEY")),
    async search(query, limit, get) {
      const data = await requestJSON("exa", "https://api.exa.ai/search", {
        method: "POST",
        headers: { "x-api-key": get("EXA_API_KEY") },
        body: { query, numResults: limit, contents: { highlights: {} } },
      });
      return rows(data?.results);
    },
  },
  {
    name: "jina",
    available: (get) => Boolean(get("JINA_API_KEY")),
    async search(query, limit, get) {
      const data = await requestJSON("jina", `https://s.jina.ai/${encodeURIComponent(query)}?count=${limit}`, {
        headers: { Authorization: `Bearer ${get("JINA_API_KEY")}`, "User-Agent": "Stella/1.0", "X-Respond-With": "no-content" },
      });
      return rows(data?.data);
    },
  },
  {
    name: "searxng",
    available: (get) => Boolean(get("SEARXNG_URL")),
    validate: (get) => validHTTPURL(get("SEARXNG_URL"), "SEARXNG_URL"),
    async search(query, _limit, get) {
      const url = new URL(`${baseURL(get("SEARXNG_URL"), "")}/search`);
      url.searchParams.set("q", query);
      url.searchParams.set("format", "json");
      url.searchParams.set("pageno", "1");
      const data = await requestJSON("searxng", url.toString());
      return rows(data?.results).sort((a, b) => (b.score ?? 0) - (a.score ?? 0));
    },
  },
  {
    name: "brave",
    available: (get) => Boolean(get("BRAVE_SEARCH_API_KEY")),
    async search(query, limit, get) {
      const url = new URL("https://api.search.brave.com/res/v1/web/search");
      url.searchParams.set("q", query);
      url.searchParams.set("count", String(limit));
      const data = await requestJSON("brave", url.toString(), { headers: { "X-Subscription-Token": get("BRAVE_SEARCH_API_KEY") } });
      if (typeof data?.web !== "object" || data.web === null) throw new Error("brave: response has no web result object");
      return rows(data.web.results);
    },
  },
  {
    name: "keenable",
    available: (get) => Boolean(get("KEENABLE_API_KEY")),
    async search(query, limit, get) {
      const data = await requestJSON("keenable", "https://api.keenable.ai/v1/search", {
        method: "POST",
        headers: { Authorization: `Bearer ${get("KEENABLE_API_KEY")}`, "X-Keenable-Title": "stella" },
        body: { query, max_results: limit },
      });
      return rows(data?.results);
    },
  },
  {
    // Anonymous zero-config fallback. It steps aside when EXA_API_KEY is set so
    // the same query is never retried anonymously.
    name: "exa",
    available: (get) => !get("EXA_API_KEY"),
    async search(query, limit) {
      const resp = await request("exa", EXA_MCP_URL, {
        method: "POST",
        accept: "application/json, text/event-stream",
        redirect: "error",
        body: { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "web_search_exa", arguments: { query, numResults: limit } } },
      });
      return parseExaMCP(await readCapped("exa", resp, MAX_PROVIDER_BYTES));
    },
  },
];

function parseExaMCP(data: string): SearchResult[] {
  const candidates = data
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).trim());
  candidates.push(data.trim());
  let envelope: any;
  for (const candidate of candidates) {
    if (!candidate) continue;
    try {
      const parsed = JSON.parse(candidate);
      if (parsed?.result || parsed?.error) {
        envelope = parsed;
        break;
      }
    } catch {
      // not this line
    }
  }
  if (!envelope) throw new Error("exa: MCP returned invalid JSON-RPC content");
  if (envelope.error) throw new Error(`exa: MCP error ${envelope.error.code}`);
  for (const item of envelope.result?.content ?? []) {
    if (item.type !== "text" || !String(item.text ?? "").trim()) continue;
    if (envelope.result.isError) throw new Error("exa: MCP returned an error");
    return parseExaMCPText(item.text);
  }
  throw new Error("exa: MCP returned empty content");
}

function parseExaMCPText(text: string): SearchResult[] {
  const results: SearchResult[] = [];
  for (const block of text.replace(/\r\n/g, "\n").split("\n---")) {
    const result: SearchResult = { title: "", url: "", snippet: "" };
    const content: string[] = [];
    let capture = false;
    for (const line of block.split("\n")) {
      if (!result.title && line.startsWith("Title: ")) result.title = line.slice(7).trim();
      else if (!result.url && line.startsWith("URL: ")) result.url = line.slice(5).trim();
      else if (line.startsWith("Text: ")) {
        capture = true;
        content.push(line.slice(6).trim());
      } else if (line === "Highlights:") capture = true;
      else if (capture) content.push(line);
    }
    if (result.url) {
      result.snippet = content.join("\n").split(/\s+/).filter(Boolean).join(" ");
      results.push(result);
    }
  }
  if (results.length === 0) throw new Error("exa: MCP response has no result list");
  return results;
}

export async function search(query: string, limit: number): Promise<{ provider: string; results: SearchResult[] }> {
  const failures: string[] = [];
  for (const provider of providers) {
    if (!provider.available(env)) continue;
    const problem = provider.validate?.(env);
    if (problem) {
      failures.push(`${provider.name}: ${problem}`);
      continue;
    }
    try {
      const results = (await provider.search(query, limit, env)).filter((r) => r.url).slice(0, limit);
      return { provider: provider.name, results };
    } catch (err) {
      failures.push((err as Error).message);
    }
  }
  throw new Error(`no search provider succeeded:\n  ${failures.join("\n  ")}`);
}
