import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import type { PluginConfig, PluginDefinition } from "@/lib/api-client";
import { PluginConfigEditor } from "./PluginConfigEditor";

vi.hoisted(() => {
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    value: { getItem: () => "en", setItem: () => undefined },
  });
});

const plugin = (backend: PluginDefinition["backend"]): PluginDefinition =>
  ({
    backend,
    display_name: backend === "cli" ? "Lark CLI" : "Remote MCP",
  }) as PluginDefinition;

const config = (backend_summary: PluginConfig["backend_summary"]): PluginConfig =>
  ({
    id: "0198f9a4-1b2c-7def-8123-456789abcdef",
    plugin_id: "custom/0198f9a4-1b2c-7def-8123-456789abcdef",
    scope: "user",
    is_enabled: false,
    backend_summary,
    revision: 2,
    created_at: "2026-09-06T00:00:00Z",
    updated_at: "2026-09-06T00:00:00Z",
  }) as PluginConfig;

describe("PluginConfigEditor", () => {
  it("shows independently editable CLI versions without skill mutation fields", () => {
    const html = renderToStaticMarkup(
      <PluginConfigEditor
        plugin={plugin("cli")}
        config={config({
          backend: "cli",
          binaries: [{ name: "lark", version: "1.2.3" }],
          skills: [{ name: "calendar" }],
          session_env: [],
          oauth_provider_configured: false,
        })}
        onSave={() => undefined}
        onCancel={() => undefined}
        busy={false}
      />,
    );

    expect(html).toContain("Binary versions");
    expect(html).not.toContain("Skill sources");
    expect(html).toContain("lark");
    expect(html).not.toContain("calendar");
  });

  it("does not echo an existing MCP endpoint while preparing its edit form", () => {
    const html = renderToStaticMarkup(
      <PluginConfigEditor
        plugin={plugin("mcp")}
        config={config({
          backend: "mcp",
          transport: "streamable_http",
          auth_type: "bearer",
          credential_mode: "shared",
          endpoint_configured: true,
          bearer_configured: true,
          oauth_client_id_configured: false,
          oauth_client_secret_configured: false,
        })}
        onSave={() => undefined}
        onCancel={() => undefined}
        busy={false}
      />,
    );

    expect(html).toContain("Blank fields keep the current value");
    expect(html).not.toContain("https://secret.example");
    expect(html).not.toContain("bearer-token");
  });
});
