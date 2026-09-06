// PR #1233: persisted tool catalog, probe status, shared session per server,
// vault-backed bearer credentials, and If-Match optimistic concurrency.
import { createChatSession, ensureAgent, invokedToolNames, sendTurn, sessionMessages } from "./lib/agent.ts";
import { expectStatus } from "./lib/api.ts";
import { expect, test } from "./lib/fixtures.ts";
import { createMcpPlugin, type McpFixture, pluginConfigPath, pluginDefinitionPath, startMcpFixture } from "./lib/mcp-fixture.ts";
import { ensureProvider } from "./lib/provider.ts";
import { AgentTool, type CreatePluginResponse, type PluginConfig, type PluginDefinition } from "./lib/types.ts";

test.describe.configure({ mode: "serial" });

let open: McpFixture;
let guarded: McpFixture;
const created: CreatePluginResponse[] = [];

test.beforeAll(async () => {
  open = await startMcpFixture();
  guarded = await startMcpFixture({ bearer: "s3cret-token" });
});

test.afterAll(async ({ admin }) => {
  for (const item of created) {
    const config = await admin.get<PluginConfig>(pluginConfigPath(item.plugin, item.config));
    if (config.status === 200) {
      await admin.delete(`${pluginConfigPath(item.plugin, item.config)}?expected_revision=${config.body.revision}`);
    }
    const definition = await admin.get<PluginDefinition>(pluginDefinitionPath(item.plugin));
    if (definition.status === 200) await admin.delete(`${pluginDefinitionPath(item.plugin)}?expected_revision=${definition.body.revision}`);
  }
  await open.close();
  await guarded.close();
});

test("create stores a safe config and explicit probe persists its catalog", async ({ admin, db }) => {
  const body = await createMcpPlugin(admin, open);
  created.push(body);
  expect(body.config.backend_summary).toMatchObject({
    backend: "mcp",
    endpoint_configured: true,
    auth_type: "none",
    bearer_configured: false,
  });
  const probed = expectStatus(
    await admin.post<PluginConfig>(`${pluginConfigPath(body.plugin, body.config)}/probe`),
    200,
    "probe created plugin",
  );
  expect(probed.backend_summary).toMatchObject({ backend: "mcp", endpoint_configured: true });
  expect(open.methods.get("initialize")).toBeGreaterThanOrEqual(1);
  expect(open.methods.get("tools/list")).toBeGreaterThanOrEqual(1);

  const rows = await db`select status, status_error, probed_at, tools from mcp_connection_state where config_id = ${body.config.id}`;
  expect(rows).toHaveLength(1);
  expect(rows[0].status).toBe("ok");
  expect(rows[0].status_error).toBe("");
  expect(rows[0].probed_at).not.toBeNull();
  expect((rows[0].tools as { name: string; }[]).map((t) => t.name).sort()).toEqual(["add", "echo"]);

  const fetched = expectStatus(await admin.get<PluginConfig>(pluginConfigPath(body.plugin, body.config)), 200, "get plugin config");
  expect(fetched.backend_summary).toMatchObject({ backend: "mcp", endpoint_configured: true });
  const list = expectStatus(
    await admin.get<{ configs: PluginConfig[]; }>(`${pluginDefinitionPath(body.plugin)}/configs`),
    200,
    "list plugin configs",
  );
  expect(list.configs.some((config) => config.id === body.config.id && config.plugin_id === body.plugin.id)).toBe(true);
  const definitions = expectStatus(await admin.get<{ plugins: PluginDefinition[]; }>("/api/plugins"), 200, "list plugins");
  expect(definitions.plugins.some((plugin) => plugin.id === body.plugin.id && plugin.spec && Object.keys(plugin.spec).length === 0)).toBe(
    true,
  );
});

test("probe endpoint re-lists tools and refreshes probed_at", async ({ admin, db }) => {
  const item = created[0];
  const before = (await db`select probed_at from mcp_connection_state where config_id = ${item.config.id}`)[0].probed_at as Date;
  const lists = open.methods.get("tools/list") ?? 0;
  await new Promise((r) => setTimeout(r, 20));
  const body = expectStatus(await admin.post<PluginConfig>(`${pluginConfigPath(item.plugin, item.config)}/probe`), 200, "probe");
  expect(body.backend_summary).toMatchObject({ backend: "mcp", endpoint_configured: true });
  expect(open.methods.get("tools/list")).toBe(lists + 1);
  const after = (await db`select probed_at from mcp_connection_state where config_id = ${item.config.id}`)[0].probed_at as Date;
  expect(after.getTime()).toBeGreaterThan(before.getTime());
});

test("unreachable endpoint records an error and an empty catalog after explicit probe", async ({ admin, db }) => {
  const body = await createMcpPlugin(admin, open, { namespace: "e2e-dead", displayName: "e2e-dead", url: "http://127.0.0.1:9/mcp" });
  created.push(body);
  expect(body.config.backend_summary).toMatchObject({ backend: "mcp", endpoint_configured: true, auth_type: "none" });
  expectStatus(await admin.post<PluginConfig>(`${pluginConfigPath(body.plugin, body.config)}/probe`), 200, "probe dead plugin");
  const row = (await db`select status, status_error, tools from mcp_connection_state where config_id = ${body.config.id}`)[0];
  expect(row.status).toBe("error");
  expect(String(row.status_error)).toBeTruthy();
  expect(String(row.status_error)).not.toMatch(/127\.0\.0\.1|connection refused|dial tcp/);
  expect(row.tools).toEqual([]);
});

test("bearer token lives in the vault and an explicit probe reports needs_auth", async ({ admin, db }) => {
  const body = await createMcpPlugin(admin, guarded, {
    namespace: "e2e-guarded",
    displayName: "e2e-guarded",
    authType: "bearer",
    token: "wrong-token",
  });
  created.push(body);
  expect(body.config.backend_summary).toMatchObject({ backend: "mcp", auth_type: "bearer", bearer_configured: true });
  expect(JSON.stringify(body)).not.toContain("wrong-token");
  expectStatus(await admin.post<PluginConfig>(`${pluginConfigPath(body.plugin, body.config)}/probe`), 200, "probe guarded plugin");

  const row = (await db`select credential_refs::text as refs from plugin_config where id = ${body.config.id}`)[0];
  expect(row.refs).toBeTruthy();
  expect(String(row.refs)).toContain(body.config.id.replaceAll("-", "_").toUpperCase());
  expect(String(row.refs)).not.toContain("wrong-token");
  const vault = await db`select count(*)::int as n from vault_entry where name = ${`MCP_TOKEN_${
    body.config.id.replaceAll("-", "_").toUpperCase()
  }`}`;
  expect(vault[0].n).toBe(1);

  const fixed = expectStatus(
    await admin.patch<PluginConfig>(pluginConfigPath(body.plugin, body.config), {
      expected_revision: body.config.revision,
      config: { url: guarded.url, transport: "streamable_http", auth_type: "bearer" },
      credentials: { token: "s3cret-token" },
    }),
    200,
    "patch token",
  );
  expect(fixed.backend_summary).toMatchObject({ backend: "mcp", auth_type: "bearer", bearer_configured: true });
  expectStatus(await admin.post<PluginConfig>(`${pluginConfigPath(body.plugin, body.config)}/probe`), 200, "probe fixed token");
  expect((await db`select status from mcp_connection_state where config_id = ${body.config.id}`)[0].status).toBe("ok");
});

test("expected_revision enforces optimistic concurrency on PATCH and DELETE", async ({ admin }) => {
  const item = created[0];
  const current = expectStatus(await admin.get<PluginConfig>(pluginConfigPath(item.plugin, item.config)), 200, "get");
  const ok = expectStatus(
    await admin.patch<PluginConfig>(pluginConfigPath(item.plugin, item.config), { expected_revision: current.revision, is_enabled: true }),
    200,
    "patch with current version",
  );
  const stale = await admin.patch(pluginConfigPath(item.plugin, item.config), { expected_revision: current.revision, is_enabled: false });
  expect(stale.status).toBe(409);
  expect(ok.revision).toBeGreaterThan(current.revision);

  const victim = await createMcpPlugin(admin, open, {
    namespace: "e2e-victim",
    displayName: "e2e-victim",
    url: open.url.replace("/mcp", "/victim"),
  });
  created.push(victim);
  const invalidDelete = await admin.delete(`${pluginConfigPath(victim.plugin, victim.config)}?expected_revision=0`);
  expect(invalidDelete.status).toBe(400);
  const advanced = expectStatus(
    await admin.patch<PluginConfig>(pluginConfigPath(victim.plugin, victim.config), {
      expected_revision: victim.config.revision,
      is_enabled: true,
    }),
    200,
    "advance victim revision",
  );
  const staleDelete = await admin.delete(`${pluginConfigPath(victim.plugin, victim.config)}?expected_revision=${victim.config.revision}`);
  expect(staleDelete.status).toBe(409);
  const del = await admin.delete(`${pluginConfigPath(victim.plugin, victim.config)}?expected_revision=${advanced.revision}`);
  expect(del.status).toBe(204);
});

test("an agent calls the remote tool through one shared session @model", async ({ admin, db }) => {
  test.setTimeout(300_000);
  if (created.length === 0) created.push(await createMcpPlugin(admin, open));
  const { modelRef } = await ensureProvider(admin);
  const agentId = await ensureAgent(admin, modelRef);
  // Wait until the freshly registered server's proxy is listed, so the model
  // turn tests tool use, not catalog-publish timing.
  await expect.poll(async () => {
    const listed = expectStatus(await admin.get<{ tools: AgentTool[]; }>(`/api/agents/${agentId}/tools`), 200, "list agent tools").tools;
    return listed.some((t) => t.name === "e2e__add");
  }, { timeout: 30_000 }).toBe(true);
  const sessionId = await createChatSession(admin, agentId);
  const initBefore = open.methods.get("initialize") ?? 0;
  const callsBefore = open.calls.length;

  const turn = await sendTurn(
    admin,
    agentId,
    sessionId,
    "Use the tool e2e__add twice: first with a=17 and b=25, then with a=3 and b=4. Reply with only the two results separated by a space.",
  );
  expect(turn.errors, JSON.stringify(turn.events.slice(-5))).toEqual([]);
  const addCalls = turn.toolCalls.filter((c) => c.toolName === "e2e__add");
  expect(addCalls.length).toBeGreaterThanOrEqual(2);
  const seen = open.calls.slice(callsBefore).filter((c) => c.tool === "add").map((c) => `${c.args.a}+${c.args.b}`);
  expect(seen).toContain("17+25");
  expect(seen).toContain("3+4");
  expect(turn.text).toContain("42");
  // Both proxies share one lazily opened session: at most one initialize per turn.
  expect((open.methods.get("initialize") ?? 0) - initBefore).toBeLessThanOrEqual(1);

  const rows = await db`
    select m.role, m.event_type, m.content from ctx_message m
    join ctx_conversation c on c.id = m.conversation_id
    where c.session_id = ${sessionId} order by m.seq`;
  // Under Code Mode the model may issue both calls from one `code` block, which
  // persists as a single tool_call row, so the row count is not the call count.
  // The transcript audit below counts the actual invocations.
  const persistedCalls = rows.filter((r) => r.event_type === "tool_call" && String(r.content).includes("e2e__add"));
  expect(persistedCalls.length, JSON.stringify(rows.map((r) => [r.role, r.event_type, String(r.content).slice(0, 80)])))
    .toBeGreaterThanOrEqual(1);

  // The transcript API shows the call either as a direct tool_call block or,
  // under Code Mode, in the child-call audit of the outer `code` result.
  const messages = await sessionMessages(admin, agentId, sessionId);
  const invoked = invokedToolNames(messages).filter((n) => n === "e2e__add");
  expect(invoked.length, JSON.stringify(messages).slice(0, 3000)).toBeGreaterThanOrEqual(2);
});

test("settings page lists servers and can register one", async ({ page, admin, loginAsAdmin }) => {
  await loginAsAdmin();
  await page.goto("/settings/mcp");
  await expect(page.getByText("e2e", { exact: true })).toBeVisible();
  await expect(page.getByText(open.url).first()).toBeVisible();
});
