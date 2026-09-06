// PR #1237: browser coverage for the MCP marketplace, drawer, scoped install,
// and the shared tool-permission surface.
import { createChatSession, ensureAgent, invokedToolNames, sendTurn, sessionMessages } from "./lib/agent.ts";
import { type ApiClient, expectStatus } from "./lib/api.ts";
import { expect, test } from "./lib/fixtures.ts";
import { createMcpPlugin, type McpFixture, pluginConfigPath, pluginDefinitionPath, startMcpFixture } from "./lib/mcp-fixture.ts";
import { type OAuthFixture, startOAuthFixture } from "./lib/oauth-fixture.ts";
import { ensureProvider } from "./lib/provider.ts";
import { loadRegistryFixtureState } from "./lib/registry-fixture.ts";
import { type CreatePluginResponse, type PluginConfig, type PluginDefinition } from "./lib/types.ts";

test.describe.configure({ mode: "serial" });

const registry = loadRegistryFixtureState();
let oauthAS: OAuthFixture;
let oauthMcp: McpFixture;
let agentId = "";
let agentServerId = "";
let agentPluginId = "";
const created: string[] = [];

interface PluginList {
  plugins: PluginDefinition[];
  next_page_token?: string | null;
}
interface PluginConfigList {
  configs: PluginConfig[];
  next_page_token?: string | null;
}

async function pluginConfig(
  admin: ApiClient,
  pluginID: string,
  configID: string,
): Promise<PluginConfig> {
  return expectStatus(
    await admin.get<PluginConfig>(pluginConfigPath(pluginID, configID)),
    200,
    `get config ${configID}`,
  );
}

async function pluginConfigs(
  admin: ApiClient,
  pluginID: string,
  scope: string,
  agentID?: string,
): Promise<PluginConfig[]> {
  const query = new URLSearchParams({ scope });
  if (agentID) query.set("agent_id", agentID);
  return expectStatus(
    await admin.get<PluginConfigList>(
      `${pluginDefinitionPath(pluginID)}/configs?${query}`,
    ),
    200,
    `list configs ${pluginID}`,
  ).configs;
}

async function pluginDefinition(
  admin: ApiClient,
  pluginID: string,
): Promise<PluginDefinition> {
  return expectStatus(
    await admin.get<PluginDefinition>(pluginDefinitionPath(pluginID)),
    200,
    `get plugin ${pluginID}`,
  );
}

async function deletePlugin(
  admin: ApiClient,
  pluginID: string,
  configs: Array<{ id: string; revision: number; }>,
) {
  const definitionResponse = await admin.get<PluginDefinition>(
    pluginDefinitionPath(pluginID),
  );
  if (definitionResponse.status === 404) return;
  const definition = expectStatus(
    definitionResponse,
    200,
    `get plugin ${pluginID}`,
  );
  for (const config of configs) {
    await admin.delete(
      `${pluginConfigPath(pluginID, config.id)}?expected_revision=${config.revision}`,
    );
  }
  await admin.delete(
    `${pluginDefinitionPath(pluginID)}?expected_revision=${definition.revision}`,
  );
}

async function chooseScope(
  page: import("@playwright/test").Page,
  label: string,
) {
  await page.getByRole("radio", { name: new RegExp(label) }).check();
  await page
    .getByRole("button", { name: /^(Install|安装)$/ })
    .last()
    .click();
}

async function openRegistry(page: import("@playwright/test").Page) {
  await page
    .getByRole("button", {
      name: /^(Add MCP server|Browse MCP registry|添加 MCP 服务器|浏览 MCP 注册表)$/,
    })
    .first()
    .click();
}

test.beforeAll(async () => {
  oauthAS = await startOAuthFixture();
  oauthMcp = await startMcpFixture({
    protectedResourceMetadata: `${oauthAS.url}/.well-known/oauth-protected-resource`,
    bearerValidator: (token) =>
      oauthAS.issuedAccessTokens.has(token)
      && !oauthAS.revokedAccessTokens.has(token),
  });
  oauthAS.resource = oauthMcp.url;
});

test.afterAll(async ({ admin, db }) => {
  for (const id of created) {
    const configs = await db`select id, revision from plugin_config where plugin_id = ${id}`;
    await deletePlugin(
      admin,
      id,
      configs as unknown as Array<{ id: string; revision: number; }>,
    );
  }
  await oauthMcp.close();
  await oauthAS.close();
});

test("marketplace search, detail, global scope, install provenance, and next page", async ({ page, admin, db, loginAsAdmin }) => {
  await loginAsAdmin();
  await page.goto("/admin/resources/mcp");
  await openRegistry(page);
  const sheet = page.getByRole("dialog").last();
  const search = sheet.getByPlaceholder("Search the MCP registry…");
  await search.fill("registry-add");
  await expect(
    sheet.getByRole("button", { name: /com\.stella\/registry-add/ }),
  ).toBeVisible();
  await expect(sheet.getByText("No auth", { exact: true })).toBeVisible();
  await sheet
    .locator("[data-slot='scroll-area-viewport'], .overflow-y-auto")
    .last()
    .evaluate((el) => el.scrollTo(0, el.scrollHeight));
  await expect(
    sheet.getByRole("button", { name: /com\.stella\/unsupported/ }),
  ).toBeVisible();
  await sheet
    .getByRole("button", { name: /com\.stella\/registry-add/ })
    .click();
  await expect(
    sheet.getByText("Connection URL", { exact: true }),
  ).toBeVisible();
  await sheet.getByRole("button", { name: "Install" }).click();
  await expect(
    sheet.getByRole("radio", { name: /Mine.*all agents/ }),
  ).toBeVisible();
  await expect(
    sheet.getByRole("radio", { name: /System.*all agents/ }),
  ).toBeVisible();
  await chooseScope(page, "Mine.*all agents");
  await expect(
    page.getByRole("heading", { name: "com.stella/registry-add", exact: true }),
  ).toBeVisible();

  const definitions = expectStatus(
    await admin.get<PluginList>("/api/plugins"),
    200,
    "list plugins",
  ).plugins;
  const definition = definitions.find(
    (item) => item.namespace === "com-stella-registry-add",
  );
  expect(definition).toBeDefined();
  created.push(definition!.id);
  const installed = (await pluginConfigs(admin, definition!.id, "user"))[0];
  expect(installed).toBeDefined();
  await expect
    .poll(
      async () =>
        String(
          (
            await db`select status from mcp_connection_state where config_id = ${installed.id}`
          )[0]?.status,
        ),
      {
        timeout: 15_000,
      },
    )
    .toBe("ok");
  const probed = await pluginConfig(admin, definition!.id, installed.id);
  expect(probed.backend_summary.endpoint_configured).toBe(true);
  const row = (
    await db`select c.config, s.status, s.tools from plugin_config c join mcp_connection_state s on s.config_id = c.id where c.id = ${installed.id}`
  )[0];
  expect(row.status).toBe("ok");
  expect(row.config.metadata).toMatchObject({
    registry: {
      source: "official",
      id: "com.stella/registry-add",
      version: "1.0.0",
    },
  });
  expect(
    (row.tools as { name: string; }[]).map((tool) => tool.name).sort(),
  ).toEqual(["add", "echo"]);
});

test("bearer secret uses the registry template and only creates vault-backed material", async ({ page, admin, db, loginAsAdmin }) => {
  await loginAsAdmin();
  await page.goto("/admin/resources/mcp");
  await openRegistry(page);
  const sheet = page.getByRole("dialog").last();
  await sheet.getByPlaceholder("Search the MCP registry…").fill("anything");
  const card = sheet
    .locator("div.flex.flex-col.gap-3.rounded-lg.border")
    .filter({ hasText: "com.stella/bearer" });
  await card.getByRole("button", { name: "Install" }).click();
  await expect(
    sheet.getByText("Connection URL", { exact: true }),
  ).toBeVisible();
  await sheet.getByRole("button", { name: "Install" }).click();
  await expect(
    sheet.locator("label").filter({ hasText: "Bearer {api_key}" }),
  ).toBeVisible();
  const secret = sheet.locator('input[type="password"]').last();
  await expect(secret).toHaveAttribute("type", "password");
  await secret.fill("browser-bearer-secret");
  await sheet.getByRole("button", { name: "Next" }).click();
  await sheet.getByRole("radio", { name: /Mine.*all agents/ }).check();
  await sheet.getByRole("button", { name: "Install" }).last().click();

  const dbRow = (
    await db`select id, scope, agent_id, credential_refs #>> '{bearer,name}' as credential_ref, row_to_json(plugin_config)::text as raw from plugin_config where plugin_id in (select id from plugin_definition where namespace = 'com-stella-bearer') order by created_at desc limit 1`
  )[0];
  expect(dbRow).toBeDefined();
  const definitions = expectStatus(
    await admin.get<PluginList>("/api/plugins"),
    200,
    "list plugins",
  ).plugins;
  const definition = definitions.find(
    (item) => item.namespace === "com-stella-bearer",
  );
  expect(definition).toBeDefined();
  created.push(definition!.id);
  const installed = (
    await pluginConfigs(
      admin,
      definition!.id,
      String(dbRow.scope),
      dbRow.agent_id ? String(dbRow.agent_id) : undefined,
    )
  )[0];
  expect(installed).toBeDefined();
  expect(JSON.stringify(installed)).not.toContain("browser-bearer-secret");
  expect(dbRow.credential_ref).toBeTruthy();
  expect(String(dbRow.raw)).not.toContain("browser-bearer-secret");
  const vault = await db`select count(*)::int as n from vault_entry where name = ${dbRow.credential_ref as string}`;
  expect(Number(vault[0].n)).toBe(1);
  expect(await page.locator("body").textContent()).not.toContain(
    "browser-bearer-secret",
  );
});

test("unsupported registry entry hands off to the prefilled manual form", async ({ page, admin, db, loginAsAdmin }) => {
  await loginAsAdmin();
  await page.goto("/admin/resources/mcp");
  await openRegistry(page);
  const sheet = page.getByRole("dialog").last();
  await sheet.getByPlaceholder("Search the MCP registry…").fill("anything");
  await sheet
    .locator("div.flex.flex-col.gap-3.rounded-lg.border")
    .filter({ hasText: "com.stella/unsupported" })
    .getByRole("button", {
      name: "Install",
    })
    .click();
  await page
    .getByRole("dialog")
    .last()
    .getByRole("button", { name: "Install" })
    .click();
  const manualForm = page.getByRole("dialog").last();
  const manualInputs = manualForm.locator('input:not([aria-hidden="true"]):visible');
  const manualName = manualInputs.nth(0);
  const manualNamespace = manualInputs.nth(1);
  const manualURL = manualInputs.nth(3);
  await expect(manualName).toHaveValue("com.stella/unsupported");
  await expect(manualNamespace).toHaveValue("com-stella-unsupported");
  await expect(manualURL).toHaveValue("http://127.0.0.1:1/unsupported");
  await page
    .getByRole("button", {
      name: /^(Save|保存)$/,
    })
    .last()
    .click();
  let manual: PluginDefinition | undefined;
  await expect
    .poll(
      async () => {
        const plugins = expectStatus(
          await admin.get<PluginList>("/api/plugins"),
          200,
          "list plugins",
        ).plugins;
        manual = plugins.find(
          (item) => item.namespace === "com-stella-unsupported",
        );
        return manual !== undefined;
      },
      { timeout: 15_000 },
    )
    .toBe(true);
  created.push(manual!.id);
  expect(
    (
      await db`select config->>'url' as url from plugin_config where plugin_id = ${manual!.id}`
    )[0],
  ).toMatchObject({
    url: "http://127.0.0.1:1/unsupported",
  });
});

test("OAuth connect and disconnect run through the browser", async ({ page, admin, db, loginAsAdmin }) => {
  await loginAsAdmin();
  const createdOAuth = expectStatus(
    await admin.post<CreatePluginResponse>("/api/plugins", {
      namespace: "browser-oauth",
      display_name: "browser-oauth",
      backend: "mcp",
      definition_spec: {},
      initial_config: {
        scope: "user",
        config: {
          url: oauthMcp.url,
          transport: "streamable_http",
          auth_type: "oauth",
          credential_mode: "per_user",
        },
      },
    }),
    201,
    "create OAuth server",
  );
  created.push(createdOAuth.plugin.id);
  expectStatus(
    await admin.post(
      `${pluginConfigPath(createdOAuth.plugin.id, createdOAuth.config.id)}/probe`,
    ),
    200,
    "probe OAuth server",
  );
  await page.goto("/settings/mcp");
  const row = await pluginConfig(
    admin,
    createdOAuth.plugin.id,
    createdOAuth.config.id,
  );
  expect(row.backend_summary.auth_type).toBe("oauth");
  await expect
    .poll(async () =>
      String(
        (
          await db`select status from mcp_connection_state where config_id = ${createdOAuth.config.id}`
        )[0]?.status,
      )
    )
    .toBe("needs_auth");
  const card = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "browser-oauth" });
  await card.click();
  await expect(
    page.getByRole("button", { name: /Authorize account|授权/ }),
  ).toBeVisible();
  await page.getByRole("button", { name: /Authorize account|授权/ }).click();
  await page.waitForURL(
    (url) => url.pathname === "/settings/mcp" && url.searchParams.has("connected"),
  );
  await page.goto("/settings/mcp");
  const connectedCard = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "browser-oauth" });
  await expect(connectedCard).toBeVisible();
  await connectedCard.click();
  await expect
    .poll(
      async () =>
        (
          await db`select s.status, c.credential_refs #>> '{oauth_bundle,name}' as bundle from mcp_connection_state s join plugin_config c on c.id = s.config_id where s.config_id = ${createdOAuth.config.id}`
        )[0],
    )
    .toMatchObject({ status: "ok" });
  const connected = (
    await db`select credential_refs #>> '{oauth_bundle,name}' as bundle from plugin_config where id = ${createdOAuth.config.id}`
  )[0]?.bundle;
  expect(connected).toBeTruthy();
  await page.getByRole("button", { name: /Disconnect account|断开/ }).click();
  await expect(
    page.getByRole("button", { name: /Authorize account|授权/ }),
  ).toBeVisible();
  expect(
    String(
      (
        await db`select status from mcp_connection_state where config_id = ${createdOAuth.config.id}`
      )[0]?.status,
    ),
  ).toBe("needs_auth");
  expect(oauthAS.counters.get("authorize")).toBeGreaterThanOrEqual(1);
});

test("plugin detail edits and deletes with revisions", async ({ page, admin, db, loginAsAdmin }) => {
  await loginAsAdmin();
  const dead = expectStatus(
    await admin.post<CreatePluginResponse>("/api/plugins", {
      namespace: "drawer-dead",
      display_name: "drawer-dead",
      backend: "mcp",
      definition_spec: {},
      initial_config: {
        scope: "user",
        config: {
          url: "http://127.0.0.1:9/mcp",
          transport: "streamable_http",
          auth_type: "none",
          credential_mode: "shared",
        },
      },
    }),
    201,
    "create dead",
  );
  created.push(dead.plugin.id);
  await page.goto("/settings/mcp");
  const card = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "drawer-dead" });
  await card.click();
  await expect(page.getByRole("button", { name: "Edit" })).toBeVisible();
  await page.getByRole("button", { name: "Edit" }).click();
  const form = page.getByRole("dialog").last();
  await form
    .locator('input:not([aria-hidden="true"]):visible')
    .first()
    .fill("http://127.0.0.1:8/edited");
  const configURL = pluginConfigPath(dead.plugin.id, dead.config.id);
  const patch = page.waitForRequest(
    (request) => request.method() === "PATCH" && request.url().includes(configURL),
  );
  await form.getByRole("button", { name: "Save" }).click();
  expect((await patch).postDataJSON()).toMatchObject({
    expected_revision: dead.config.revision,
  });
  await expect
    .poll(
      async () =>
        (
          await db`select config->>'url' as url from plugin_config where id = ${dead.config.id}`
        )[0]?.url,
    )
    .toBe("http://127.0.0.1:8/edited");

  const current = await pluginConfig(admin, dead.plugin.id, dead.config.id);
  expectStatus(
    await admin.patch(configURL, {
      expected_revision: current.revision,
      config: {
        url: "http://127.0.0.1:7/out-of-band",
        transport: "streamable_http",
        auth_type: "none",
        credential_mode: "shared",
      },
    }),
    200,
    "out of band update",
  );
  await page.getByRole("button", { name: "Edit" }).click();
  const staleForm = page.getByRole("dialog").last();
  await staleForm
    .locator('input:not([aria-hidden="true"]):visible')
    .first()
    .fill("http://127.0.0.1:6/must-not-win");
  const staleResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "PATCH"
      && response.url().includes(configURL),
  );
  await staleForm.getByRole("button", { name: "Save" }).click();
  expect((await staleResponse).status()).toBe(409);
  await staleForm
    .getByRole("button", { name: "Cancel" })
    .click({ force: true });
  await page.reload();
  await expect(page.getByText("out-of-band", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Delete", exact: true }).first().click();
  const configDeleteDialog = page
    .getByRole("dialog")
    .filter({ hasText: /Delete configuration\?|删除配置/ });
  await expect(configDeleteDialog).toBeVisible();
  await configDeleteDialog
    .getByRole("button", { name: "Delete" })
    .click();
  await expect
    .poll(
      async () => (await admin.get(pluginConfigPath(dead.plugin.id, dead.config.id))).status,
    )
    .toBe(404);
  await page.getByRole("button", { name: "Delete", exact: true }).last().click();
  const pluginDeleteDialog = page
    .getByRole("dialog")
    .filter({ hasText: /Remove plugin\?|Delete plugin\?|移除插件/ });
  await expect(pluginDeleteDialog).toBeVisible();
  await pluginDeleteDialog
    .getByRole("button", { name: "Delete" })
    .click();
  await expect
    .poll(
      async () => (await admin.get(pluginDefinitionPath(dead.plugin.id))).status,
    )
    .toBe(404);
  await page.reload();
  await expect(page.getByText("out-of-band", { exact: true })).toHaveCount(0);
});

test("agent-scoped install and MCP tool permission toggle persist", async ({ page, admin, db, loginAsAdmin }) => {
  const { modelRef } = await ensureProvider(admin);
  agentId = await ensureAgent(admin, modelRef, "e2e-mcp-web-agent");
  await loginAsAdmin();
  const scoped = await createMcpPlugin(admin, { url: registry.mcpUrl }, {
    namespace: "agent-browser",
    displayName: "agent-browser",
    scope: "user_agent",
    agentId,
  });
  agentPluginId = scoped.plugin.id;
  agentServerId = scoped.config.id;
  created.push(agentPluginId);
  expectStatus(
    await admin.post(`${pluginConfigPath(agentPluginId, agentServerId)}/probe`),
    200,
    "probe agent browser server",
  );
  await page.goto(`/agents/${agentId}/profile?tab=tools`);
  expect(await pluginConfig(admin, agentPluginId, agentServerId)).toMatchObject(
    {
      plugin_id: agentPluginId,
      scope: "user_agent",
      agent_id: agentId,
    },
  );
  await page.getByText("agent-browser", { exact: true }).click();
  await expect(
    page.getByText("agent-browser__add", { exact: true }),
  ).toBeVisible();
  const tool = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "agent-browser__add" });
  await tool.getByRole("switch").click();
  await expect
    .poll(
      async () =>
        (
          await db`select enabled from tool_override where tool_name is null and plugin_id = ${agentPluginId} and local_tool_name = 'add' and scope = 'user_agent' and agent_id = ${agentId}`
        )[0]?.enabled,
    )
    .toBe(false);
});

test("a real agent calls add on the browser-installed server @model", async ({ admin }) => {
  test.setTimeout(300_000);
  if (!agentServerId) {
    const { modelRef } = await ensureProvider(admin);
    agentId = await ensureAgent(admin, modelRef, "e2e-mcp-web-agent");
    const setup = await admin.post<CreatePluginResponse>("/api/plugins", {
      namespace: "agent-browser",
      display_name: "agent-browser",
      backend: "mcp",
      definition_spec: {},
      initial_config: {
        scope: "user_agent",
        agent_id: agentId,
        config: {
          url: registry.mcpUrl,
          transport: "streamable_http",
          auth_type: "none",
          credential_mode: "shared",
        },
      },
    });
    if (setup.status === 201) {
      agentPluginId = setup.body.plugin.id;
      agentServerId = setup.body.config.id;
      created.push(agentPluginId);
    } else if (setup.status === 409) {
      const definitions = expectStatus(
        await admin.get<PluginList>("/api/plugins"),
        200,
        "list model plugins",
      ).plugins;
      const definition = definitions.find(
        (item) => item.namespace === "agent-browser",
      );
      if (!definition) {
        throw new Error("could not recover existing model browser plugin");
      }
      agentPluginId = definition.id;
      const configs = await pluginConfigs(
        admin,
        agentPluginId,
        "user_agent",
        agentId,
      );
      const config = configs.find((item) => item.agent_id === agentId);
      if (!config) {
        throw new Error("could not recover existing model browser config");
      }
      agentServerId = config.id;
    } else {
      throw new Error(`create model browser server: ${setup.status}`);
    }
  }
  expect(agentServerId).toBeTruthy();
  expectStatus(
    await admin.post(`${pluginConfigPath(agentPluginId, agentServerId)}/probe`),
    200,
    "probe model browser server",
  );
  expect(await pluginConfig(admin, agentPluginId, agentServerId)).toMatchObject(
    {
      plugin_id: agentPluginId,
      scope: "user_agent",
      agent_id: agentId,
    },
  );
  expectStatus(
    await admin.patch(`/api/agents/${agentId}/tools/agent-browser__add`, {
      enabled: true,
      scope: "user_agent",
    }),
    200,
    "enable add",
  );
  const session = await createChatSession(admin, agentId);
  const turn = await sendTurn(
    admin,
    agentId,
    session,
    "Call agent-browser__add with a=17 and b=25. Reply with only the result.",
  );
  expect(turn.errors, JSON.stringify(turn.events.slice(-5))).toEqual([]);
  expect(turn.text).toContain("42");
  expect(
    invokedToolNames(await sessionMessages(admin, agentId, session)),
  ).toContain("agent-browser__add");
});
