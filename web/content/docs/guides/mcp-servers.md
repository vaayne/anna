---
title: MCP Servers
---

## What MCP Servers Do

Stella can connect to external [Model Context Protocol](https://modelcontextprotocol.io) servers and expose their tools to your agents. Register a server once and its tools appear in the agent's toolbox, namespaced as `mcp__<server>__<tool>` so they never collide with skills or built-in tools.

Stella is an MCP **client** over HTTP-based transports only:

- `streamable_http` — the streamable HTTP transport (default).
- `sse` — HTTP + Server-Sent Events.

Local `stdio` servers are intentionally not supported: the multi-user sandbox never spawns local processes.

Endpoints must resolve to public addresses. Loopback and private-network URLs are refused unless the operator starts `stellad` with `STELLA_MCP_ALLOW_PRIVATE_ENDPOINTS=1`, which is meant for local development servers.

## Scopes

Registrations use the same four scopes as skills and the vault, so a server can be shared or private:

| Scope          | Visible to                       |
| -------------- | -------------------------------- |
| `system`       | every agent, every user          |
| `system_agent` | one agent, across all users      |
| `user`         | one user, across their agents    |
| `user_agent`   | one user with one specific agent |

When two registrations share a name, the most specific wins: `user_agent` > `user` > `system_agent` > `system`.

## Authentication

A server may need a bearer token. Configure it when creating or editing its MCP plugin configuration in the Web UI; credential inputs are write-only, stored **encrypted in the vault** under the configuration scope (see [Secrets and Keys](/docs/guides/secrets-and-keys)), and never returned by the API. Servers that need no auth need no credential and work even without the vault configured.

## OAuth connections

Servers that require OAuth 2.1 (Authorization Code + PKCE) can be connected from the Web UI. Pick **OAuth 2.1** as the authentication type, then press **Connect** on the server card. Stella discovers the authorization server automatically, registers a client when needed, and opens the provider's consent screen; on success it stores the tokens **encrypted in the vault** and probes the server.

- **Redirect URI**: `<STELLA_BASE_URL>/api/mcp/oauth/callback`. The base URL must be reachable by your browser — behind an ingress or a published port, not just inside the cluster.
- **Pre-registered vs dynamic clients**: if the provider requires a fixed client, paste its **client ID** (and secret, if any) into the registration form. Otherwise Stella registers a client dynamically (DCR) the first time you connect and reuses it afterwards.
- **Credential mode**: a `system` or `system_agent` registration can be `shared` (one connection for everyone) or `per_user` (every user connects their own account; users without a connection see the tools as `needs_auth`). Ordinary users can only connect `per_user` registrations they can see, never shared system credentials.
- **Refresh and reconnect**: access tokens refresh automatically just before they expire. If the server rejects the credential, the server flips to `needs_auth`; press **Reconnect** to authorize again. **Disconnect** removes the stored authorization and fails subsequent tool calls closed.

## Status and probing

Stella probes each registered server — connects and fetches its tool list — and records the result on the registration:

| Status       | Meaning                                                          |
| ------------ | ---------------------------------------------------------------- |
| `unknown`    | Not probed yet                                                   |
| `ok`         | The last probe connected and listed tools                        |
| `error`      | The last probe or tool call failed; the redacted reason is shown |
| `needs_auth` | The server rejected the stored credential with 401/403           |

Creating or updating a configuration does not probe the server. Use **Probe** in the Web UI, or `POST /api/plugins/{kind}/{name}/configs/{config_id}/probe`, to check the saved connection. Stella also refreshes discovery when an agent session needs a missing tool catalog or the last snapshot is older than 24 hours. A failed probe updates the connection status and shows a redacted reason.

When a tool call is rejected with 401/403, the server moves to `needs_auth`; update the credential in the Web UI and probe again.

## Per-tool permissions

Every tool a server exposes can be switched on or off individually, using the same four-scope override model as every other tool:

| Scope          | Who it applies to                               |
| -------------- | ----------------------------------------------- |
| `user_agent`   | you, for one specific agent (most specific)     |
| `user`         | you, across all your agents                     |
| `system_agent` | the agent, for every user (administrators only) |
| `system`       | the whole deployment (administrators only)      |

An administrator's **disable** always wins over a user's enable; otherwise the more user-specific layer wins. Switch a tool in **Personal Settings → Agents → Tools** (or the admin console for system scopes), or with `PATCH /api/agents/{id}/tools/{toolName}`.

The server's **enable switch is separate**: it turns the whole registration on or off. While a server is disabled, unreachable, or rejecting credentials, its tools stay listed but their switches have no effect until the server is healthy again — the header shows why.

Because overrides are keyed by tool name (`mcp__<server>__<tool>`), renaming a server migrates its tools' overrides to the new prefix automatically, and deleting a server removes them. Both only happen once no other registration in any scope still uses that name. If two registrations share a name in different scopes, an override applies to whichever registration wins for the context.

## Marketplace

Instead of typing a URL, browse the **Marketplace** tab: it lists remote servers from the official [MCP Registry](https://registry.modelcontextprotocol.io), filtered to entries with a streamable-HTTP endpoint. Each entry shows how it authenticates:

- **No auth** — install and go.
- **Bearer** — install, then paste the API key the entry's header template asks for.
- **Needs manual setup** (`unsupported`) — the entry requires custom headers the marketplace cannot fill in for you; install is still possible from the Manual tab after configuring it out of band.
- **OAuth** — entries with no declared auth may still require OAuth; after install the first probe detects it and the server shows _needs auth_ with a Connect button.

Installing writes the registry source, id, and version onto the registration so it can be re-checked later. Installing a URL that already exists in the same scope returns the existing registration instead of creating a twin.

The registry source can be overridden (e.g. for a mirror) with the `STELLA_MCP_REGISTRY_URL` environment variable; the default is the official registry.

## Managing Servers

Manage personal `user` and `user_agent` configurations from **Personal Settings → Plugins**. Administrators manage deployment-owned `system` and `system_agent` configurations from **Admin Console → Integrations → Plugins**. Add the MCP plugin configuration, choose whether it applies to every agent or one agent, and provide credentials when required. There is no separate MCP management API: the Web UI and the common plugin API under `/api/plugins` provide management, while agents retain the `settings_mcp_server_*` tools for model-facing administration.

## Troubleshooting

| Symptom                                    | Meaning                                                              | Fix                                                                            |
| ------------------------------------------ | -------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| Server shows **needs auth**                | The stored credential (or the OAuth grant) was rejected with 401/403 | Reconnect for OAuth; update the bearer token otherwise, then probe             |
| Server shows **error**                     | The endpoint was unreachable at the last probe                       | Check the URL is reachable from the server, then probe again                   |
| Tool switches have no effect               | The server is disabled or unhealthy                                  | Fix the server state first; the header explains why                            |
| **Needs manual setup** in the marketplace  | The entry requires custom headers                                    | Configure the headers out of band, add the server from the Manual tab          |
| OAuth callback fails to return             | `STELLA_BASE_URL` is not reachable by your browser                   | Make the base URL reachable (ingress, published port) and reconnect            |
| Tools missing after a server adds new ones | The persisted catalog predates the change                            | Probe the server — or wait: `tools/list_changed` triggers a background refresh |
