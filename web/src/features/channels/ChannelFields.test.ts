import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@/lib/i18n";

vi.hoisted(() => {
  vi.stubGlobal("localStorage", { getItem: () => null, setItem: () => {}, removeItem: () => {} });
});

import {
  channelConfig,
  ChannelConfigFields,
  normalizeChannel,
  parseConfig,
  platformConfigDefaults,
} from "./ChannelFields";

describe("Telegram channel configuration", () => {
  it("drops malformed values while decoding stored config", () => {
    expect(
      parseConfig(
        JSON.stringify({
          token: { leaked: true },
          allowed_chat_ids: [123],
          valid: "kept",
        }),
      ),
    ).toEqual({ valid: "kept" });
  });

  it("keeps supported scalar and list values when decoding stored config", () => {
    expect(
      parseConfig(
        JSON.stringify({
          enabled: true,
          limit: 10,
          allowed_chat_ids: ["-100", "-200"],
        }),
      ),
    ).toEqual({ enabled: true, limit: 10, allowed_chat_ids: ["-100", "-200"] });
  });

  it("does not carry another platform's or an unknown field into a save", () => {
    const config = JSON.parse(
      channelConfig({
        type: "telegram",
        token: "redacted",
        app_secret: "wrong-platform",
        unexpected: "not-a-channel-field",
      }),
    );

    expect(config.token).toBe("redacted");
    expect(config).not.toHaveProperty("app_secret");
    expect(config).not.toHaveProperty("unexpected");
  });

  it("keeps chat and topic allowlists when an existing channel is saved", () => {
    const channel = normalizeChannel({
      id: "telegram-main",
      name: "Telegram",
      type: "telegram",
      agent_id: "agent-1",
      is_active: true,
      config: JSON.stringify({
        token: "redacted",
        allowed_chat_ids: ["-100"],
        allowed_topic_ids: ["-100:42"],
      }),
    });

    expect(platformConfigDefaults("telegram")).toMatchObject({
      allowed_chat_ids: [],
      allowed_topic_ids: [],
    });
    expect(JSON.parse(channelConfig(channel))).toMatchObject({
      allowed_chat_ids: ["-100"],
      allowed_topic_ids: ["-100:42"],
    });
  });

  it("keeps the instance active state outside the platform config", () => {
    const channel = normalizeChannel({
      id: "telegram-main",
      name: "Telegram",
      type: "telegram",
      agent_id: "agent-1",
      is_active: false,
      config: JSON.stringify({ token: "redacted" }),
    });

    expect(channel.is_active).toBe(false);
    expect(channel).not.toHaveProperty("enabled");
    expect(JSON.parse(channelConfig(channel))).not.toHaveProperty("is_active");
  });
});

describe("Feishu group configuration", () => {
  it("renders the group-rule settings within valid Field roots", () => {
    const queryClient = new QueryClient();
    const markup = renderToStaticMarkup(
      createElement(
        I18nProvider,
        null,
        createElement(
          QueryClientProvider,
          { client: queryClient },
          createElement(ChannelConfigFields, {
            channel: { type: "feishu", ...platformConfigDefaults("feishu") },
            onChange: vi.fn(),
          }),
        ),
      ),
    );

    expect(markup).toContain("Feishu group rules");
  });

  it("decodes stored group rules into editable drafts", () => {
    expect(parseConfig(JSON.stringify({ groups: { oc_platform: { enabled: true } } })).groups).toBe(
      JSON.stringify({ oc_platform: { enabled: true } }),
    );
  });

  it("serializes group rules without UI-only draft IDs or blank chat IDs", () => {
    const config = JSON.parse(
      channelConfig({
        type: "feishu",
        groups: JSON.stringify([
          {
            id: "draft-1",
            chatId: "oc_platform",
            enabled: true,
            systemPrompt: "Be concise.",
            allowedUsers: ["on_allowed"],
            disallowedUsers: ["on_denied"],
            toolAllow: ["shell"],
            toolDeny: ["danger"],
          },
          {
            id: "draft-2",
            chatId: "  ",
            systemPrompt: "discard me",
            allowedUsers: [],
            disallowedUsers: [],
          },
        ]),
      }),
    );

    expect(config.groups).toEqual({
      oc_platform: {
        enabled: true,
        system_prompt: "Be concise.",
        allowed_users: ["on_allowed"],
        disallowed_users: ["on_denied"],
        tool_allow: ["shell"],
        tool_deny: ["danger"],
      },
    });
  });
});
