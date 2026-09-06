// PR #1235: OAuth 2.1 authorization-code + PKCE for remote MCP servers.
import { createChatSession, ensureAgent, invokedToolNames, sendTurn, sessionMessages } from "./lib/agent.ts";
import { expectStatus } from "./lib/api.ts";
import type { ApiClient } from "./lib/api.ts";
import { expect, loginWithPassword, test } from "./lib/fixtures.ts";
import { type McpFixture, startMcpFixture } from "./lib/mcp-fixture.ts";
import { expireAccessToken, type OAuthFixture, setTokenFailure, startOAuthFixture, tokenHits } from "./lib/oauth-fixture.ts";
import { ensureProvider } from "./lib/provider.ts";

test.describe.configure({ mode: "serial" });

let as: OAuthFixture;
let mcp: McpFixture;
type PluginConfig = {
  id: string;
  revision?: number;
  backend_summary?: Record<string, unknown>;
};
type PluginCreate = {
  plugin: { id: string; revision?: number; };
  config: PluginConfig;
};
type PluginDefinition = { id: string; revision?: number; };
type OAuthServer = {
  pluginId: string;
  configId: string;
  configRevision: number;
  pluginRevision: number;
};
type ConnectionState = {
  status: string;
  status_error: string;
  tools: { name: string; }[];
};

const created: OAuthServer[] = [];
let oauthServer: OAuthServer;
let agentID = "";

async function createOAuthPlugin(
  api: ApiClient,
  namespace: string,
  scope: string,
  url: string,
  credentialMode = "shared",
): Promise<OAuthServer> {
  const created = expectStatus(
    await api.post<PluginCreate>("/api/plugins", {
      namespace,
      display_name: namespace,
      backend: "mcp",
      definition_spec: {},
      initial_config: {
        scope,
        is_enabled: true,
        config: {
          url,
          transport: "streamable_http",
          auth_type: "oauth",
          credential_mode: credentialMode,
        },
      },
    }),
    201,
    `create ${namespace} plugin`,
  );
  return {
    pluginId: created.plugin.id,
    configId: created.config.id,
    configRevision: created.config.revision ?? 1,
    pluginRevision: created.plugin.revision ?? 1,
  };
}

function configPath(server: OAuthServer, suffix = ""): string {
  return `/api/plugins/${server.pluginId}/configs/${server.configId}${suffix}`;
}

async function connect(
  api: ApiClient,
  server: OAuthServer,
): Promise<{ flowID: string; callback: Response; }> {
  const started = expectStatus(
    await api.post<{ authorization_url: string; flow_id: string; }>(
      configPath(server, "/oauth/start"),
    ),
    201,
    "start OAuth",
  );
  const approved = await fetch(started.authorization_url, {
    redirect: "manual",
  });
  const location = approved.headers.get("location");
  expect(approved.status).toBe(302);
  expect(location).toBeTruthy();
  return {
    flowID: started.flow_id,
    callback: await fetch(location!, { redirect: "manual" }),
  };
}

async function getConfig(
  api: ApiClient,
  server: OAuthServer,
): Promise<PluginConfig> {
  return expectStatus(
    await api.get<PluginConfig>(configPath(server)),
    200,
    "get OAuth config",
  );
}

async function getDefinition(
  api: ApiClient,
  server: OAuthServer,
): Promise<PluginDefinition> {
  return expectStatus(
    await api.get<PluginDefinition>(`/api/plugins/${server.pluginId}`),
    200,
    "get OAuth plugin",
  );
}

async function probe(
  api: ApiClient,
  server: OAuthServer,
): Promise<PluginConfig> {
  return expectStatus(
    await api.post<PluginConfig>(configPath(server, "/probe")),
    200,
    "probe OAuth config",
  );
}

async function connectionState(
  db: import("./lib/db.ts").Sql,
  server: OAuthServer,
  userID: string | null = null,
): Promise<ConnectionState> {
  const rows = userID === null
    ? await db`select status, status_error, tools from mcp_connection_state where config_id = ${server.configId}`
    : await db`select status, status_error, tools from mcp_connection_state where config_id = ${server.configId} and credential_user_id = ${userID}`;
  if (rows.length !== 1) {
    throw new Error(
      `missing MCP state for ${server.configId}: ${JSON.stringify(rows)}`,
    );
  }
  return rows[0] as ConnectionState;
}

function vaultName(prefix: string, id: string): string {
  return `${prefix}${id.replaceAll("-", "_").toUpperCase()}`;
}

async function vaultCount(
  db: import("./lib/db.ts").Sql,
  name: string,
): Promise<number> {
  const rows = await db`select count(*)::int as n from vault_entry where name = ${name}`;
  return Number(rows[0].n);
}

test.beforeAll(async () => {
  as = await startOAuthFixture();
  mcp = await startMcpFixture({
    protectedResourceMetadata: `${as.url}/.well-known/oauth-protected-resource`,
    bearerValidator: (token) => as.issuedAccessTokens.has(token) && !as.revokedAccessTokens.has(token),
  });
  as.resource = mcp.url;
});

test.afterAll(async ({ admin }) => {
  for (const server of created) {
    const config = await getConfig(admin, server);
    const definition = await getDefinition(admin, server);
    await admin.delete(
      configPath(server)
        + `?expected_revision=${config.revision ?? server.configRevision}`,
    );
    await admin.delete(
      `/api/plugins/${server.pluginId}?expected_revision=${definition.revision ?? server.pluginRevision}`,
    );
  }
  await mcp.close();
  await as.close();
});

test("API + DB complete the PKCE flow and persist only a vault bundle", async ({ admin, db }) => {
  oauthServer = await createOAuthPlugin(admin, "oauth-e2e", "user", mcp.url);
  created.push(oauthServer);
  const initial = await probe(admin, oauthServer);
  expect(initial.backend_summary).toMatchObject({
    auth_type: "oauth",
    oauth_client_id_configured: false,
  });
  expect((await connectionState(db, oauthServer)).status).toBe("needs_auth");

  const started = expectStatus(
    await admin.post<{
      authorization_url: string;
      flow_id: string;
      expires_at: string;
    }>(configPath(oauthServer, "/oauth/start")),
    201,
    "start OAuth",
  );
  expect(started.authorization_url).toContain("code_challenge=");
  const flows = await db`select server_id, user_id, pkce_verifier, consumed_at from mcp_oauth_flow where id = ${started.flow_id}`;
  expect(flows).toHaveLength(1);
  expect(flows[0].server_id).toBe(oauthServer.configId);
  expect(flows[0].pkce_verifier).toBeTruthy();
  expect(flows[0].consumed_at).toBeNull();

  const approved = await fetch(started.authorization_url, {
    redirect: "manual",
  });
  const callbackURL = approved.headers.get("location");
  expect(approved.status).toBe(302);
  expect(callbackURL).toContain("code=");
  expect(callbackURL).toContain("state=");
  const callback = await fetch(callbackURL!, { redirect: "manual" });
  expect(callback.status, await callback.text()).toBe(302);
  expect(callback.headers.get("location")).toContain("connected=");

  const connected = await getConfig(admin, oauthServer);
  expect(connected.backend_summary).toMatchObject({
    auth_type: "oauth",
    oauth_client_id_configured: true,
    oauth_client_secret_configured: true,
  });
  expect((await connectionState(db, oauthServer)).status).toBe("ok");
  expect(
    (await connectionState(db, oauthServer)).tools.map((tool) => tool.name),
  ).toEqual(["add", "echo"]);
  expect(JSON.stringify(connected)).not.toContain("e2e-access");
  expect(JSON.stringify(connected)).not.toContain("e2e-refresh");
  expect(
    await vaultCount(db, vaultName("MCP_OAUTH_", oauthServer.configId)),
  ).toBe(1);
  expect(
    await vaultCount(db, vaultName("MCP_OAUTH_CLIENT_", oauthServer.configId)),
  ).toBe(1);
  const flow = (
    await db`select consumed_at from mcp_oauth_flow where id = ${started.flow_id}`
  )[0];
  expect(flow.consumed_at).not.toBeNull();
  expect(as.counters.get("register") ?? 0).toBe(1);

  const replay = await fetch(callbackURL!, { redirect: "manual" });
  expect(replay.status).toBe(302);
  expect(replay.headers.get("location")).toContain("oauth_error=expired");
});

test("expired flow is rejected and refresh is single-shot", async ({ admin, db }) => {
  const started = expectStatus(
    await admin.post<{ authorization_url: string; flow_id: string; }>(
      configPath(oauthServer, "/oauth/start"),
    ),
    201,
    "start second OAuth",
  );
  const expired = await db`update mcp_oauth_flow set expires_at = now() - interval '1 minute' where id = ${started.flow_id} returning id`;
  expect(expired).toHaveLength(1);
  const approved = await fetch(started.authorization_url, {
    redirect: "manual",
  });
  const callback = await fetch(approved.headers.get("location")!, {
    redirect: "manual",
  });
  expect(callback.headers.get("location")).toContain("oauth_error=expired");

  // A one-second access token is expired by the callback's real post-connect
  // probe, forcing exactly one refresh. The refresh response is long-lived.
  const before = tokenHits(as);
  as.expiresIn = 1;
  const fresh = await connect(admin, oauthServer);
  expect(fresh.callback.status).toBe(302);
  expect(tokenHits(as)).toBe(before + 2); // authorization-code exchange + refresh
  expect((await connectionState(db, oauthServer)).status).toBe("ok");
});

test("rejected access and refresh failure fail closed without a retry loop", async ({ admin, db }) => {
  expireAccessToken(as);
  const rejected = await probe(admin, oauthServer);
  expect(rejected.backend_summary).toMatchObject({ auth_type: "oauth" });
  const rejectedState = await connectionState(db, oauthServer);
  expect(rejectedState.status).toBe("needs_auth");
  expect(rejectedState.status_error).not.toContain("e2e-access");

  // Make the refreshed access token expire, then reject the refresh grant.
  setTokenFailure(as, 0, "");
  as.expiresIn = 1;
  as.refreshExpiresIn = 1;
  const reconnected = await connect(admin, oauthServer);
  expect(reconnected.callback.status).toBe(302);
  setTokenFailure(as, 400, JSON.stringify({ error: "invalid_grant" }));
  await new Promise((resolve) => setTimeout(resolve, 1100));
  await probe(admin, oauthServer);
  const failedState = await connectionState(db, oauthServer);
  expect(failedState.status).toBe("needs_auth");
  expect(failedState.status_error).toContain("reconnect");
  const after = tokenHits(as);
  await probe(admin, oauthServer);
  expect(tokenHits(as)).toBe(after);
  expect((await connectionState(db, oauthServer)).status).toBe("needs_auth");
  setTokenFailure(as, 0, "");
  as.refreshExpiresIn = 3600;
});

test("disconnect removes the bundle and the UI reconnects OAuth", async ({ admin, db, page, loginAsAdmin }) => {
  const connected = await getConfig(admin, oauthServer);
  const disconnected = expectStatus(
    await admin.post<PluginConfig>(
      configPath(oauthServer, "/oauth/disconnect"),
    ),
    200,
    "disconnect OAuth",
  );
  expect(disconnected.backend_summary).toMatchObject({
    auth_type: "oauth",
    oauth_client_secret_configured: true,
  });
  expect((await connectionState(db, oauthServer)).status).toBe("needs_auth");
  expect(
    await vaultCount(db, vaultName("MCP_OAUTH_", oauthServer.configId)),
  ).toBe(0);
  expect(
    await vaultCount(db, vaultName("MCP_OAUTH_CLIENT_", oauthServer.configId)),
  ).toBe(1);

  await loginAsAdmin();
  await page.goto("/settings/mcp");
  const card = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "oauth-e2e" });
  await card.click();
  await expect(
    page.getByRole("button", { name: /Authorize account|授权/ }),
  ).toBeVisible();
  expect(connected.backend_summary).toMatchObject({ auth_type: "oauth" });
  const fresh = await createOAuthPlugin(
    admin,
    "oauth-ui-connect",
    "user",
    mcp.url.replace("/mcp", "/ui-connect"),
  );
  created.push(fresh);
  await page.goto("/settings/mcp");
  const freshCard = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "oauth-ui-connect" });
  await expect(freshCard).toBeVisible();
  await freshCard.click();
  await expect(
    page.getByRole("button", { name: /Authorize account|授权/ }),
  ).toBeVisible();

  await page.goto("/settings/mcp");
  const reconnectCard = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "oauth-e2e" });
  await reconnectCard.click();
  await page.getByRole("button", { name: /Authorize account|授权/ }).click();
  await page.waitForURL(
    (url) => url.pathname === "/settings/mcp" && url.searchParams.has("connected"),
  );
  await page.goto("/settings/mcp");
  const connectedCard = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "oauth-e2e" });
  await connectedCard.click();
  await expect
    .poll(
      async () =>
        (
          await db`select s.status, c.credential_refs #>> '{oauth_bundle,name}' as bundle
      from mcp_connection_state s
      join plugin_config c on c.id = s.config_id
      where s.config_id = ${oauthServer.configId}`
        )[0],
    )
    .toMatchObject({ status: "ok" });
  const bundle = (
    await db`select c.credential_refs #>> '{oauth_bundle,name}' as bundle
      from plugin_config c where c.id = ${oauthServer.configId}`
  )[0]?.bundle;
  expect(bundle).toBeTruthy();
  await expect(
    page.getByRole("button", { name: /Disconnect account|断开/ }),
  ).toBeVisible();
  await page.getByRole("button", { name: /Disconnect account|断开/ }).click();
  await expect(
    page.getByRole("button", { name: /Authorize account|授权/ }),
  ).toBeVisible();
  expect((await connectionState(db, oauthServer)).status).toBe("needs_auth");
});

test("per-user bundles isolate users and a real agent calls OAuth MCP @model", async ({ admin, user, db }) => {
  const { modelRef } = await ensureProvider(admin);
  agentID = await ensureAgent(admin, modelRef, "e2e-oauth-agent");
  const perUser = await createOAuthPlugin(
    admin,
    "oauth_per_user",
    "system",
    mcp.url,
    "per_user",
  );
  created.push(perUser);
  expect((await getConfig(admin, perUser)).backend_summary).toMatchObject({
    auth_type: "oauth",
    credential_mode: "per_user",
  });

  const beforeConnect = await user.get<{
    tools: { name: string; availability_reason?: string; }[];
  }>(`/api/agents/${agentID}/tools`);
  expect(beforeConnect.status).toBe(200);

  const adminStart = await connect(admin, perUser);
  expect(adminStart.callback.status).toBe(302);
  const userStillNeedsAuth = await user.get<{
    tools: { name: string; availability_reason?: string; }[];
  }>(`/api/agents/${agentID}/tools`);
  expect(
    userStillNeedsAuth.body.tools.some(
      (tool) =>
        tool.name === "oauth_per_user__add"
        && tool.availability_reason === "mcp_needs_auth",
    ),
    JSON.stringify(userStillNeedsAuth.body),
  ).toBe(true);

  const userStart = await connect(user, perUser);
  expect(userStart.callback.status).toBe(302);
  const bundleRows = await db`select scope, user_id from vault_entry where name = ${
    vaultName("MCP_OAUTH_", perUser.configId)
  } order by user_id`;
  expect(bundleRows).toHaveLength(2);
  expect(new Set(bundleRows.map((row) => String(row.user_id))).size).toBe(2);

  const session = await createChatSession(admin, agentID);
  const turn = await sendTurn(
    admin,
    agentID,
    session,
    "Call oauth_per_user__add with a=17 and b=25. Reply with only the result.",
  );
  expect(turn.errors, JSON.stringify(turn.events.slice(-5))).toEqual([]);
  expect(turn.text).toContain("42");
  expect(
    mcp.calls.some(
      (call) => call.tool === "add" && call.args.a === 17 && call.args.b === 25,
    ),
  ).toBe(true);
  expect(
    invokedToolNames(await sessionMessages(admin, agentID, session)),
  ).toContain("oauth_per_user__add");
});
