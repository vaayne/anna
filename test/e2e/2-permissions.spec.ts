// PR #1234: MCP catalog tools use the four-scope tool_override model in the
// API, profile UI, persisted rows, and the real agent runner.
import { createChatSession, ensureAgent, invokedToolNames, sendTurn, sessionMessages } from "./lib/agent.ts";
import { expectStatus } from "./lib/api.ts";
import { expect, test } from "./lib/fixtures.ts";
import { type McpFixture, startMcpFixture } from "./lib/mcp-fixture.ts";
import { ensureProvider } from "./lib/provider.ts";
import type { AgentTool } from "./lib/types.ts";

test.describe.configure({ mode: "serial" });

let fixture: McpFixture;
let pluginId = "";
let configId = "";
let configRevision = 0;
let pluginRevision = 0;
let agentId = "";
let sessionId = "";

type PluginConfig = { id: string; revision?: number; };
type PluginCreate = {
  plugin: { id: string; revision?: number; };
  config: PluginConfig;
};
type AgentMCPServer = {
  config_id: string;
  namespace: string;
  scope: string;
  status: string;
  tools: { name: string; }[];
  readable: boolean;
  shadowed_scopes?: string[];
};

async function agentTools(
  admin: import("./lib/api.ts").ApiClient,
): Promise<AgentTool[]> {
  return expectStatus(
    await admin.get<{ tools: AgentTool[]; }>(`/api/agents/${agentId}/tools`),
    200,
    "list agent tools",
  ).tools;
}

function findTool(tools: AgentTool[], name: string): AgentTool {
  const tool = tools.find((item) => item.name === name);
  if (!tool) {
    throw new Error(`tool ${name} missing from ${JSON.stringify(tools)}`);
  }
  return tool;
}

test.beforeAll(async () => {
  fixture = await startMcpFixture();
});

test.afterAll(async ({ admin }) => {
  if (configId) {
    await admin.delete(
      `/api/plugins/${pluginId}/configs/${configId}?expected_revision=${configRevision}`,
    );
  }
  if (pluginRevision) {
    await admin.delete(
      `/api/plugins/${pluginId}?expected_revision=${pluginRevision}`,
    );
  }
  await fixture.close();
});

test("catalog endpoint exposes effective MCP registration and tools", async ({ admin, db }) => {
  const { modelRef } = await ensureProvider(admin);
  agentId = await ensureAgent(admin, modelRef, "e2e-mcp-permissions");

  const created = expectStatus(
    await admin.post<PluginCreate>("/api/plugins", {
      namespace: "permissions",
      display_name: "permissions",
      backend: "mcp",
      definition_spec: {},
      initial_config: {
        scope: "user",
        is_enabled: true,
        config: {
          url: fixture.url,
          transport: "streamable_http",
          auth_type: "none",
        },
      },
    }),
    201,
    "create permissions server",
  );
  pluginId = created.plugin.id;
  configId = created.config.id;
  configRevision = created.config.revision ?? 1;
  pluginRevision = created.plugin.revision ?? 1;
  const probed = expectStatus(
    await admin.post<{ status?: string; }>(
      `/api/plugins/${pluginId}/configs/${configId}/probe`,
    ),
    200,
    "probe permissions plugin",
  );
  expect(probed).toBeTruthy();

  const servers = expectStatus(
    await admin.get<{ servers: AgentMCPServer[]; }>(
      `/api/agents/${agentId}/mcp-servers`,
    ),
    200,
    "list agent MCP servers",
  );
  const registration = servers.servers.find(
    (server) => server.config_id === configId,
  );
  expect(registration).toMatchObject({
    namespace: "permissions",
    scope: "user",
    status: "ok",
    readable: true,
  });
  expect(registration?.tools.map((tool) => tool.name).sort()).toEqual([
    "add",
    "echo",
  ]);
  expect(registration?.shadowed_scopes ?? []).toEqual([]);

  const tools = await agentTools(admin);
  for (const name of ["permissions__add", "permissions__echo"]) {
    expect(findTool(tools, name)).toMatchObject({
      source: "mcp",
      control: "override",
      enabled: true,
      origin: "default",
      family: "mcp:permissions",
    });
  }

  const rows = await db`
    select d.display_name as name, c.scope, c.enabled, s.status, s.tools
    from plugin_config c
    join plugin_definition d on d.id = c.plugin_id
    left join mcp_connection_state s on s.config_id = c.id
    where c.id = ${configId}`;
  expect(rows).toHaveLength(1);
  expect(rows[0]).toMatchObject({
    name: "permissions",
    scope: "user",
    enabled: true,
    status: "ok",
  });
  expect(
    (rows[0].tools as { name: string; }[]).map((tool) => tool.name).sort(),
  ).toEqual(["add", "echo"]);
});

test("PATCH writes all four scopes and admin disable wins", async ({ admin, db }) => {
  const add = "permissions__add";

  for (
    const [scope, enabled, origin] of [
      ["user", false, "user"],
      ["user_agent", true, "user_agent"],
      ["system_agent", false, "system_agent"],
      ["system", true, "system_agent"],
    ] as const
  ) {
    const body = expectStatus(
      await admin.patch<AgentTool>(`/api/agents/${agentId}/tools/${add}`, {
        enabled,
        scope,
      }),
      200,
      `set ${scope} override`,
    );
    expect(body.name).toBe(add);
  }

  const rows = await db`
    select tool_name, scope, user_id, agent_id, enabled
    from tool_override
    where tool_name is null and plugin_id = ${pluginId}
      and local_tool_name = ${"add"}
    order by scope`;
  expect(rows).toHaveLength(4);
  expect(rows.map((row) => [row.scope, row.enabled])).toEqual([
    ["system", true],
    ["system_agent", false],
    ["user", false],
    ["user_agent", true],
  ]);
  expect(rows.find((row) => row.scope === "system")?.user_id).toBeNull();
  expect(rows.find((row) => row.scope === "system_agent")?.agent_id).toBe(
    agentId,
  );

  expect(findTool(await agentTools(admin), add)).toMatchObject({
    enabled: false,
    origin: "system_agent",
  });

  const unknown = await admin.patch(
    `/api/agents/${agentId}/tools/permissions__missing`,
    { enabled: false },
  );
  expect(unknown.status).toBe(400);
});

test("profile UI groups MCP tools and persists a browser toggle", async ({ page, admin, db, loginAsAdmin }) => {
  // Leave add enabled for the agent turn and disable echo through the same API
  // surface the browser uses, so the UI has both effective states to render.
  expectStatus(
    await admin.patch<AgentTool>(
      `/api/agents/${agentId}/tools/permissions__add`,
      {
        enabled: true,
        scope: "user_agent",
      },
    ),
    200,
    "enable add for UI",
  );
  expectStatus(
    await admin.patch<AgentTool>(
      `/api/agents/${agentId}/tools/permissions__echo`,
      {
        enabled: false,
        scope: "user_agent",
      },
    ),
    200,
    "disable echo for UI",
  );

  await loginAsAdmin();
  await page.goto(`/agents/${agentId}/profile?tab=tools`);
  await expect(page.getByText("MCP servers", { exact: true })).toBeVisible();
  await expect(page.getByText("permissions", { exact: true })).toBeVisible();
  await page.getByText("permissions", { exact: true }).click();
  await expect(
    page.getByText("permissions__add", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("permissions__echo", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Disabled", { exact: true }).first(),
  ).toBeVisible();

  const echoCard = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "permissions__echo" });
  await echoCard.getByRole("switch").click();
  await expect
    .poll(async () => {
      const row = await db`
      select enabled from tool_override
      where tool_name is null and plugin_id = ${pluginId}
        and local_tool_name = 'echo' and scope = 'user_agent'
        and agent_id = ${agentId}`;
      return row[0]?.enabled;
    })
    .toBe(true);
});

test.describe("real model permissions turn", () => {
  test.describe.configure({ retries: 1 });

  test("real agent turn only calls the enabled MCP tool @model", async ({ admin }) => {
    test.setTimeout(300_000);
    if (!configId) {
      const { modelRef } = await ensureProvider(admin);
      agentId = await ensureAgent(admin, modelRef, "e2e-mcp-permissions");
      const setup = expectStatus(
        await admin.post<PluginCreate>("/api/plugins", {
          namespace: "permissions",
          display_name: "permissions",
          backend: "mcp",
          definition_spec: {},
          initial_config: {
            scope: "user",
            is_enabled: true,
            config: {
              url: fixture.url,
              transport: "streamable_http",
              auth_type: "none",
            },
          },
        }),
        201,
        "create model permissions server",
      );
      pluginId = setup.plugin.id;
      configId = setup.config.id;
      configRevision = setup.config.revision ?? 1;
      pluginRevision = setup.plugin.revision ?? 1;
      await admin.post(`/api/plugins/${pluginId}/configs/${configId}/probe`);
    }
    const add = "permissions__add";
    const echo = "permissions__echo";
    // The serial mutation cases above leave higher-precedence overrides on these
    // tools (an admin `system_agent` disable on add wins over any user setting).
    // Clear every scope first, then set only the two effective user_agent
    // overrides this journey needs, so the turn tests permission, not leftovers.
    const scopes = ["user", "user_agent", "system", "system_agent"] as const;
    for (const tool of [add, echo]) {
      for (const scope of scopes) {
        expectStatus(
          await admin.patch<AgentTool>(`/api/agents/${agentId}/tools/${tool}`, {
            scope,
          }),
          200,
          `clear ${scope} override on ${tool}`,
        );
      }
    }
    expectStatus(
      await admin.patch<AgentTool>(`/api/agents/${agentId}/tools/${add}`, {
        enabled: true,
        scope: "user_agent",
      }),
      200,
      "enable add for runner",
    );
    expectStatus(
      await admin.patch<AgentTool>(`/api/agents/${agentId}/tools/${echo}`, {
        enabled: false,
        scope: "user_agent",
      }),
      200,
      "disable echo for runner",
    );
    // A freshly created MCP server publishes its catalog asynchronously; the
    // first runner built before discovery finishes would invoke the proxy and
    // hit "tool not found". Wait until the enabled tool is listed for the agent
    // before the model turn, so the assertion tests permission, not timing.
    await expect
      .poll(
        async () => (await agentTools(admin)).some((t) => t.name === add && t.enabled),
        {
          timeout: 30_000,
        },
      )
      .toBe(true);
    sessionId = await createChatSession(admin, agentId);
    const callsBefore = fixture.calls.length;
    const turn = await sendTurn(
      admin,
      agentId,
      sessionId,
      "Use permissions__add with a=17 and b=25. Do not use echo. Reply with only the result.",
    );
    expect(turn.errors, JSON.stringify(turn.events.slice(-5))).toEqual([]);
    expect(turn.text).toContain("42");
    // Code Mode may wrap the remote call in an outer `code` tool event; the
    // fixture call and persisted child-call audit are the authoritative proof.
    expect(turn.toolCalls.map((call) => call.toolName)).not.toContain(echo);
    const calls = fixture.calls.slice(callsBefore);
    expect(
      calls.some(
        (call) => call.tool === "add" && call.args.a === 17 && call.args.b === 25,
      ),
      JSON.stringify({
        text: turn.text,
        toolCalls: turn.toolCalls,
        fixtureCalls: calls,
        events: turn.events.slice(-8),
      }).slice(0, 4000),
    ).toBe(true);
    expect(calls.some((call) => call.tool === "echo")).toBe(false);

    const messages = await sessionMessages(admin, agentId, sessionId);
    const invoked = invokedToolNames(messages);
    expect(invoked).toContain(add);
    expect(invoked).not.toContain(echo);
  });
});
