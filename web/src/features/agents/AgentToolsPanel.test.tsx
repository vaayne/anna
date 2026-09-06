import type { ComponentProps, ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { describe, expect, it, vi } from "vitest";
import type { Tool } from "@/lib/types";
import {
  groupedRegularTools,
  McpServerGroup,
  RegularToolFamilyCard,
  runBoundedFamilyUpdates,
  SystemSettingsSection,
  ToolRow,
} from "./AgentToolsPanel";

vi.hoisted(() => {
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    value: { getItem: () => "en", setItem: () => undefined },
  });
});

// SAFETY: fixed response fixture satisfies the policy catalog shape.
const settingsAction = {
  name: "settings_agent_update",
  description: "Update one agent.",
  source: "builtin",
  control: "system",
  family: "agent_management",
  policy_reason: "settings_policy",
} as Tool & { family: "agent_management" };

// SAFETY: fixed response fixture satisfies the system core row shape.
const coreTool = {
  name: "bash",
  description: "Run a command.",
  source: "core",
  control: "system",
  policy_reason: "core_sandbox",
} as Tool;

// SAFETY: fixed response fixture satisfies the override-controlled row shape.
const overrideTool = {
  name: "vault_secret_list",
  description: "List secret names.",
  source: "builtin",
  control: "override",
  enabled: true,
  origin: "default",
  family: "vault",
} as Tool;

// SAFETY: fixed response fixture satisfies the runtime-unavailable catalog shape.
const runtimeUnavailableTool = {
  name: "recally_article_list",
  description: "List saved articles.",
  source: "builtin",
  control: "system",
  family: "recally",
  policy_reason: "runtime_unavailable",
} as Tool;

// SAFETY: fixed response fixture satisfies a generated builtin catalog row.
const generatedGoalTool = {
  name: "goal_list",
  description: "List goals.",
  source: "builtin",
  control: "override",
  enabled: true,
  origin: "default",
  family: "goal",
} as Tool;

// SAFETY: derived fixture preserves the generated builtin catalog shape.
const generatedGoalCreateTool = {
  ...generatedGoalTool,
  name: "goal_create",
  description: "Create a goal.",
} as Tool;

// SAFETY: derived fixture preserves the generated builtin catalog shape.
const adminDisabledGoalTool = {
  ...generatedGoalTool,
  enabled: false,
  origin: "system",
} as Tool;

// SAFETY: fixed response fixture mirrors every unavailable email action after
// the server's EMAIL_CONFIG availability check, including its explicit reason.
const unavailableEmailTool = {
  name: "email_account_list",
  description: "List configured email accounts.",
  source: "builtin",
  control: "system",
  family: "email",
  policy_reason: "runtime_unavailable",
  availability_reason: "email_config_required",
} as Tool;

// SAFETY: this derived fixture preserves the unavailable-email API shape.
const unavailableEmailSendTool = {
  ...unavailableEmailTool,
  name: "email_message_send",
  description: "Send email.",
} as Tool;

// SAFETY: fixed response fixture satisfies the plugin fallback catalog shape.
const generatedLookingPlugin = {
  name: "goal_helper",
  description: "Plugin helper.",
  source: "plugin",
  control: "override",
  enabled: true,
  origin: "default",
  family: "plugin_tools",
} as Tool;

async function renderWithRouter(component: () => ReactNode) {
  const rootRoute = createRootRoute();
  const cardRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([cardRoute]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  await router.load();
  return renderToStaticMarkup(<RouterProvider router={router} />);
}

function renderRegularToolFamilyCard(props: ComponentProps<typeof RegularToolFamilyCard>) {
  return renderWithRouter(() => <RegularToolFamilyCard {...props} />);
}

function renderSystemSettingsSection(props: ComponentProps<typeof SystemSettingsSection>) {
  return renderWithRouter(() => <SystemSettingsSection {...props} />);
}

describe("AgentToolsPanel control contract", () => {
  it("renders an enabled Agent's Settings as a read-only policy catalog", () => {
    const html = renderToStaticMarkup(
      <SystemSettingsSection agentId="agent" enabled tools={[settingsAction]} />,
    );

    expect(html).toContain("System settings");
    expect(html).not.toContain("Stella only");
    expect(html).toContain("Foreground 1:1 chat only");
    expect(html).toContain("Agent management");
    // Family cards share the tool-family shell; the state badge says read-only
    // where a regular family would say enabled.
    expect(html).toContain("Read-only");
    expect(html).not.toContain("settings_agent_update");
    expect(html).toContain('aria-expanded="false"');
    expect(html).toMatch(/<h3[^>]*><button/);
    expect(html).not.toMatch(/<button[^>]*><h3/);
    expect(html).not.toContain('role="switch"');
  });

  it("shows an owner-only disabled state instead of the policy catalog", async () => {
    const html = await renderSystemSettingsSection({
      agentId: "agent",
      enabled: false,
      tools: [settingsAction],
    });

    expect(html).toContain("System settings tools are disabled for this agent.");
    expect(html).toContain("Open advanced configuration");
    expect(html).not.toContain("Agent management");
  });

  it("groups regular rows by backend family without treating source or a name prefix as navigation", () => {
    const groups = groupedRegularTools([
      generatedGoalTool,
      generatedLookingPlugin,
      runtimeUnavailableTool,
      generatedGoalCreateTool,
    ]);

    expect(groups.map((group) => [group.family, group.tools.map((tool) => tool.name)])).toEqual([
      ["goal", ["goal_create", "goal_list"]],
      ["recally", ["recally_article_list"]],
      ["plugin_tools", ["goal_helper"]],
    ]);
  });

  it("renders Email as a locked family with its authoritative setup CTA, separate from Goals", async () => {
    const emailHtml = await renderRegularToolFamilyCard({
      family: "email",
      tools: [unavailableEmailTool, unavailableEmailSendTool],
      defaultOpen: false,
      canEdit: true,
      isAdmin: true,
      busyToolName: null,
      familyBusy: false,
      onToggle: vi.fn(),
      onSetFamilyEnabled: vi.fn(),
    });
    const goalHtml = await renderRegularToolFamilyCard({
      family: "goal",
      tools: [generatedGoalTool, generatedGoalCreateTool],
      defaultOpen: true,
      canEdit: true,
      isAdmin: true,
      busyToolName: null,
      familyBusy: false,
      onToggle: vi.fn(),
      onSetFamilyEnabled: vi.fn(),
    });

    expect(emailHtml).toContain('data-slot="collapsible"');
    expect(emailHtml).toContain("data-closed");
    expect(emailHtml).toMatch(/<h3[^>]*><button/);
    expect(emailHtml).not.toMatch(/<button[^>]*><h3/);
    expect(emailHtml).toContain("Email");
    expect(emailHtml).toContain("2 actions");
    expect(emailHtml).toContain("Email setup required");
    expect(emailHtml).toContain(
      "Configure a personal email account in Credentials to manage this tool.",
    );
    expect(emailHtml).toContain('href="/settings/credentials"');
    expect(emailHtml).not.toContain("Enable all");
    expect(emailHtml).not.toContain('role="switch"');
    expect(emailHtml).not.toContain("Runtime availability decides when this tool is registered.");
    expect(goalHtml).toContain("Goals");
    expect(goalHtml).toContain("2 actions");
    expect(goalHtml).toContain("All enabled");
    expect(goalHtml).toContain("Disable all");
    expect(goalHtml).toContain('role="switch"');
    expect(goalHtml).not.toContain(
      "Configure a personal email account in Credentials to manage this tool.",
    );
  });

  it("waits for every bounded family update before reporting a failure", async () => {
    let active = 0;
    let completed = 0;
    let maxActive = 0;
    const update = async (item: number) => {
      active++;
      maxActive = Math.max(maxActive, active);
      await new Promise((resolve) => setTimeout(resolve, 1));
      active--;
      completed++;
      if (item === 0) throw new Error("changed while updating");
    };

    await expect(
      runBoundedFamilyUpdates(
        Array.from({ length: 9 }, (_, i) => i),
        update,
      ),
    ).rejects.toThrow("changed while updating");
    expect(completed).toBe(9);
    expect(maxActive).toBeLessThanOrEqual(4);
  });

  it("keeps unbounded plugin families on per-tool controls", async () => {
    const html = await renderRegularToolFamilyCard({
      family: "plugin_tools",
      tools: [generatedLookingPlugin],
      defaultOpen: false,
      canEdit: true,
      isAdmin: true,
      busyToolName: null,
      familyBusy: false,
      onToggle: vi.fn(),
      onSetFamilyEnabled: vi.fn(),
    });

    expect(html).toContain("Plugin tools");
    expect(html).not.toContain("Disable all");
  });

  it("does not offer a family enable action for an admin-level disabled override", async () => {
    const html = await renderRegularToolFamilyCard({
      family: "goal",
      tools: [adminDisabledGoalTool],
      defaultOpen: false,
      canEdit: true,
      isAdmin: true,
      busyToolName: null,
      familyBusy: false,
      onToggle: vi.fn(),
      onSetFamilyEnabled: vi.fn(),
    });

    expect(html).toContain("Disabled");
    expect(html).not.toContain("Enable all");
  });

  it("offers a switch only for rows the backend marks override-controlled", () => {
    const systemHtml = renderToStaticMarkup(
      <ToolRow tool={coreTool} canEdit isAdmin busy={false} onToggle={vi.fn()} />,
    );
    const runtimeUnavailableHtml = renderToStaticMarkup(
      <ToolRow tool={runtimeUnavailableTool} canEdit isAdmin busy={false} onToggle={vi.fn()} />,
    );
    const overrideHtml = renderToStaticMarkup(
      <ToolRow tool={overrideTool} canEdit isAdmin busy={false} onToggle={vi.fn()} />,
    );

    expect(systemHtml).toContain("System managed");
    expect(systemHtml).toContain("Core sandbox tools are system-managed.");
    expect(systemHtml).not.toContain('role="switch"');
    expect(runtimeUnavailableHtml).toContain(
      "Runtime availability decides when this tool is registered.",
    );
    expect(runtimeUnavailableHtml).not.toContain('role="switch"');
    expect(overrideHtml).toContain('role="switch"');
    expect(overrideHtml).toContain("Builtin");
    expect(overrideHtml).toContain("Default");
  });
});

// SAFETY: fixed response fixture satisfies the AgentMCPServer projection shape.
const healthyServer = {
  plugin_id: "mcp/github",
  config_id: "srv-1",
  namespace: "github",
  scope: "user",
  enabled: true,
  status: "ok",
  credential_mode: "shared",
  needs_auth: false,
  tools: [],
  readable: true,
} as import("@/lib/api-client/types.gen").AgentMcpServer;

// SAFETY: derived fixture preserves the projection shape with a rejected credential.
const needsAuthServer = {
  ...healthyServer,
  config_id: "srv-2",
  namespace: "notion",
  status: "needs_auth",
  needs_auth: true,
} as import("@/lib/api-client/types.gen").AgentMcpServer;

// SAFETY: fixed fixture preserves the MCP override-controlled tool row shape.
const mcpTool = {
  name: "mcp__github__create_issue",
  description: "Create an issue.",
  source: "mcp",
  control: "override",
  enabled: true,
  origin: "default",
  family: "mcp:github",
  input_schema: { type: "object" },
} as Tool;

describe("McpServerGroup", () => {
  it("renders the server header with a status badge and per-tool override switches", async () => {
    const html = await renderWithRouter(() => (
      <McpServerGroup
        server={healthyServer}
        tools={[mcpTool]}
        defaultOpen
        canEdit
        isAdmin={false}
        busyToolName={null}
        familyBusy={false}
        toggleBusy={false}
        onToggle={vi.fn()}
        onSetFamilyEnabled={vi.fn()}
        onToggleServer={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
        onConnect={vi.fn()}
        onDisconnect={vi.fn()}
      />
    ));

    expect(html).toContain("github");
    expect(html).toContain("Healthy");
    expect(html).toContain("mcp__github__create_issue");
    expect(html).toContain('role="switch"');
    expect(html).toContain("MCP");
    expect(html).toContain("Disable all");
  });

  it("shows the needs-auth reason on the header without removing the tool controls", async () => {
    const html = await renderWithRouter(() => (
      <McpServerGroup
        server={needsAuthServer}
        tools={[{ ...mcpTool, name: "mcp__notion__search", family: "mcp:notion" }]}
        defaultOpen
        canEdit
        isAdmin={false}
        busyToolName={null}
        familyBusy={false}
        toggleBusy={false}
        onToggle={vi.fn()}
        onSetFamilyEnabled={vi.fn()}
        onToggleServer={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
        onConnect={vi.fn()}
        onDisconnect={vi.fn()}
      />
    ));

    expect(html).toContain("Needs auth");
    expect(html).toContain("Credential rejected");
    expect(html).toContain('role="switch"');
  });

  it("hides the server toggle and manage menu from a caller who cannot read the row", async () => {
    const html = await renderWithRouter(() => (
      <McpServerGroup
        server={{ ...healthyServer, readable: false }}
        tools={[]}
        defaultOpen
        canEdit
        isAdmin={false}
        busyToolName={null}
        familyBusy={false}
        toggleBusy={false}
        onToggle={vi.fn()}
        onSetFamilyEnabled={vi.fn()}
        onToggleServer={vi.fn()}
        onEdit={vi.fn()}
        onDelete={vi.fn()}
        onConnect={vi.fn()}
        onDisconnect={vi.fn()}
      />
    ));

    expect(html).toContain("No tools cataloged yet");
    expect(html).not.toContain('aria-label="Server enabled"');
    expect(html).not.toContain('aria-label="More actions"');
  });
});
