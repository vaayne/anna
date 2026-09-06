import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { type IncomingMessage, type ServerResponse } from "node:http";
import { z } from "zod";
import { type ApiClient, expectStatus } from "./api.ts";
import { startFixtureServer } from "./fixture-server.ts";
import { type CreatePluginResponse, type PluginConfig, type PluginDefinition } from "./types.ts";

export interface McpFixtureOptions {
  // Requests must carry `Authorization: Bearer <bearer>`; anything else is 401.
  bearer?: string;
  // OAuth-protected mode: advertise the AS through RFC 9728 metadata and
  // accept only tokens currently approved by the fixture.
  protectedResourceMetadata?: string;
  bearerValidator?: (token: string) => boolean;
  // Extra tool names to advertise, each echoing its arguments.
  extraTools?: string[];
}

export interface RecordedCall {
  tool: string;
  args: Record<string, unknown>;
}

export interface McpFixture {
  url: string;
  port: number;
  calls: RecordedCall[];
  // JSON-RPC method counts (initialize, tools/list, tools/call, ...), so a spec
  // can prove sessions are shared rather than reopened per call.
  methods: Map<string, number>;
  close(): Promise<void>;
}

export interface CreateMcpPluginOptions {
  namespace?: string;
  displayName?: string;
  scope?: "system" | "system_agent" | "user" | "user_agent";
  agentId?: string;
  enabled?: boolean | null;
  authType?: "none" | "bearer" | "oauth";
  credentialMode?: "shared" | "per_user";
  token?: string;
  metadata?: Record<string, unknown>;
  url?: string;
}

// Management tests use the common definition/config API. The MCP backend owns
// endpoint and credential decoding behind this narrow fixture helper, so tests
// never seed the retired registration table directly.
export async function createMcpPlugin(
  api: ApiClient,
  fixture: Pick<McpFixture, "url">,
  options: CreateMcpPluginOptions = {},
): Promise<CreatePluginResponse> {
  const namespace = options.namespace ?? "e2e";
  const displayName = options.displayName ?? namespace;
  const config: Record<string, unknown> = {
    url: options.url ?? fixture.url,
    transport: "streamable_http",
    auth_type: options.authType ?? "none",
  };
  if (options.credentialMode !== undefined) config.credential_mode = options.credentialMode;
  if (options.metadata !== undefined) config.metadata = options.metadata;
  const credentials = options.token === undefined ? undefined : { token: options.token };
  return expectStatus(
    await api.post<CreatePluginResponse>("/api/plugins", {
      namespace,
      display_name: displayName,
      backend: "mcp",
      definition_spec: {},
      initial_config: {
        scope: options.scope ?? "user",
        ...(options.agentId === undefined ? {} : { agent_id: options.agentId }),
        is_enabled: options.enabled === undefined ? true : options.enabled,
        config,
        ...(credentials === undefined ? {} : { credentials }),
      },
    }),
    201,
    "create MCP plugin",
  );
}

export function pluginDefinitionPath(definition: PluginDefinition | string): string {
  return `/api/plugins/${typeof definition === "string" ? definition : definition.id}`;
}

export function pluginConfigPath(definition: PluginDefinition | string, config: PluginConfig | string): string {
  const pluginID = typeof definition === "string" ? definition : definition.id;
  const configID = typeof config === "string" ? config : config.id;
  return `${pluginDefinitionPath(pluginID)}/configs/${configID}`;
}

function buildServer(options: McpFixtureOptions, fixture: McpFixture): McpServer {
  const server = new McpServer({ name: "stella-e2e-fixture", version: "1.0.0" });
  server.registerTool(
    "add",
    {
      description: "Add two integers and return their sum.",
      inputSchema: { a: z.number().describe("first addend"), b: z.number().describe("second addend") },
    },
    async ({ a, b }) => {
      fixture.calls.push({ tool: "add", args: { a, b } });
      return { content: [{ type: "text", text: String(a + b) }] };
    },
  );
  server.registerTool(
    "echo",
    {
      description: "Echo the given text back verbatim.",
      inputSchema: { text: z.string() },
    },
    async ({ text }) => {
      fixture.calls.push({ tool: "echo", args: { text } });
      return { content: [{ type: "text", text }] };
    },
  );
  for (const name of options.extraTools ?? []) {
    server.registerTool(name, { description: `Fixture tool ${name}.`, inputSchema: {} }, async (args) => {
      fixture.calls.push({ tool: name, args: (args ?? {}) as Record<string, unknown> });
      return { content: [{ type: "text", text: `${name} ok` }] };
    });
  }
  return server;
}

async function readJSON(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  const raw = Buffer.concat(chunks).toString("utf8");
  return raw ? JSON.parse(raw) : undefined;
}

// A stateless Streamable HTTP MCP server on a random loopback port. Stateless
// means every POST is served by a fresh transport, which is exactly what the
// Go SDK client expects from a server that does not hand out session ids.
export async function startMcpFixture(options: McpFixtureOptions = {}): Promise<McpFixture> {
  const fixture: McpFixture = {
    url: "",
    port: 0,
    calls: [],
    methods: new Map(),
    close: async () => {},
  };
  const fixtureServer = await startFixtureServer(async (req: IncomingMessage, res: ServerResponse) => {
    try {
      const authorization = req.headers.authorization ?? "";
      const token = authorization.startsWith("Bearer ") ? authorization.slice("Bearer ".length) : "";
      if (
        (options.bearer && authorization !== `Bearer ${options.bearer}`)
        || (options.bearerValidator && !options.bearerValidator(token))
      ) {
        const challenge = options.protectedResourceMetadata
          ? `Bearer error="invalid_token", resource_metadata="${options.protectedResourceMetadata}"`
          : "Bearer";
        res.writeHead(401, { "Content-Type": "application/json", "WWW-Authenticate": challenge });
        res.end(JSON.stringify({ error: "unauthorized" }));
        return;
      }
      if (req.method !== "POST") {
        res.writeHead(405, { Allow: "POST" });
        res.end();
        return;
      }
      const body = await readJSON(req);
      for (const msg of Array.isArray(body) ? body : [body]) {
        const method = (msg as { method?: string; })?.method;
        if (method) fixture.methods.set(method, (fixture.methods.get(method) ?? 0) + 1);
      }
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
      const server = buildServer(options, fixture);
      res.on("close", () => {
        void transport.close();
        void server.close();
      });
      await server.connect(transport);
      await transport.handleRequest(req, res, body);
    } catch (err) {
      if (!res.headersSent) {
        res.writeHead(500, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: String(err) }));
      }
    }
  });
  fixture.port = Number(new URL(fixtureServer.state.url).port);
  fixture.url = `${fixtureServer.state.url}/mcp`;
  fixture.close = fixtureServer.close;
  return fixture;
}
