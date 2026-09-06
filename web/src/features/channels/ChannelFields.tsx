import { Plus, Trash2 } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import {
  Combobox,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
  ComboboxPopup,
} from "@/components/ui/combobox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Fieldset, FieldsetLegend } from "@/components/ui/fieldset";
import { targetValue } from "@/lib/utils";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { platformLabel } from "@/components/PlatformIcon";
import { feishuChannelChatsQueryOptions } from "@/lib/queries/channels";
import type { Channel, JsonValue } from "@/lib/types";
import { useI18n } from "@/lib/i18n";

// ─── platform metadata ────────────────────────────────────────────────────────

export interface FeishuGroupDraft {
  id: string;
  chatId: string;
  systemPrompt: string;
  enabled?: boolean;
  requireMention?: boolean;
  allowedUsers: string[];
  disallowedUsers: string[];
  toolAllow: string[];
  toolDeny: string[];
}

export type ChannelFormValue = string | boolean | number | string[] | undefined;
export type ChannelForm = Record<string, ChannelFormValue>;

type PersistedFeishuGroup = {
  system_prompt?: string;
  enabled?: boolean;
  require_mention?: boolean;
  allowed_users?: string[];
  disallowed_users?: string[];
  tool_allow?: string[];
  tool_deny?: string[];
};
type PersistedFeishuGroups = Record<string, PersistedFeishuGroup>;

type SerializedPlatformConfigValue = Exclude<ChannelFormValue, undefined> | PersistedFeishuGroups;
type SerializedPlatformConfig = Record<string, SerializedPlatformConfigValue>;

export interface PlatformDefaults {
  [key: string]: ChannelFormValue;
}

/**
 * The credential fields each platform stores on a channel row. This map is the
 * only definition of a channel's config shape: it drives the rendered fields,
 * the defaults a draft starts from, and — through `channelConfig` — exactly
 * which keys survive a save. A key absent here is dropped on the next write.
 */
export const platformDefaults = {
  telegram: {
    token: "",
    channel_id: "",
    allow_group: false,
    allowed_chat_ids: [],
    allowed_topic_ids: [],
    allow_dm: true,
    allow_unlinked_dm: false,
    guest_message_limit_per_minute: 10,
    guest_max_per_channel: 1000,
    guest_retention_days: 30,
    require_mention: true,
  },
  discord: {
    token: "",
    allow_group: false,
    allow_all_guilds: false,
    allowed_guild_ids: [],
    allowed_channel_ids: [],
    allowed_user_ids: [],
    allowed_role_ids: [],
    allow_dm: true,
    allow_unlinked_dm: false,
    guest_message_limit_per_minute: 10,
    guest_max_per_channel: 1000,
    guest_retention_days: 30,
    require_mention: true,
  },
  qq: { app_id: "", app_secret: "" },
  feishu: {
    app_id: "",
    app_secret: "",
    encrypt_key: "",
    verification_token: "",
    tenant_key: "",
    auto_provision: false,
    allow_group: false,
    allow_dm: true,
    allow_unlinked_dm: false,
    guest_message_limit_per_minute: 10,
    guest_max_per_channel: 1000,
    guest_retention_days: 30,
    require_mention: true,
    groups: "{}",
  },
  dingtalk: {
    client_id: "",
    client_secret: "",
    allow_group: false,
    allow_dm: true,
    allow_unlinked_dm: false,
    guest_message_limit_per_minute: 10,
    guest_max_per_channel: 1000,
    guest_retention_days: 30,
    require_mention: true,
  },
  weixin: { bot_token: "", base_url: "", bot_id: "", user_id: "" },
} satisfies Record<string, PlatformDefaults>;

export const channelTypes = Object.keys(platformDefaults).map((id) => ({
  id,
  label: platformLabel(id),
}));

export const defaultChannelType = channelTypes[0]?.id || "";

export function parseConfig(raw: string) {
  try {
    const parsed: JsonValue = JSON.parse(raw || "{}");
    if (!isJsonObject(parsed)) return {} satisfies ChannelForm;
    const config: ChannelForm = {};
    for (const [key, value] of Object.entries(parsed)) {
      if (key === "groups") {
        config.groups = JSON.stringify(value);
        continue;
      }
      if (isChannelFormValue(value)) config[key] = value;
    }
    return config;
  } catch {
    return {} satisfies ChannelForm;
  }
}

export function platformConfigDefaults(type: string): PlatformDefaults {
  const defaults = Object.entries(platformDefaults).find(([key]) => key === type)?.[1];
  return { ...defaults };
}

/** Splits comma- or newline-separated IDs, trimming blanks and duplicates. */
function isStringValue(value: JsonValue | undefined): value is string {
  return typeof value === "string";
}

function isBooleanValue(value: JsonValue | undefined): value is boolean {
  return typeof value === "boolean";
}

function isNumberValue(value: JsonValue | undefined): value is number {
  return typeof value === "number";
}

function isChannelFormValue(value: JsonValue): value is Exclude<ChannelFormValue, undefined> {
  if (isStringValue(value) || isBooleanValue(value)) return true;
  if (isNumberValue(value)) return Number.isFinite(value);
  return Array.isArray(value) && value.every(isStringValue);
}

function isJsonObject(value: JsonValue): value is { [key: string]: JsonValue } {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function channelString(value: ChannelFormValue): string {
  return isStringValue(value) ? value : "";
}

function splitIDList(value: string | string[]): string[] {
  const raw = Array.isArray(value) ? value.join(",") : isStringValue(value) ? value : "";
  const seen = new Set<string>();
  const out: string[] = [];
  for (const part of raw.split(/[,\n]/)) {
    const id = part.trim();
    if (!id || seen.has(id)) continue;
    seen.add(id);
    out.push(id);
  }
  return out;
}

function normalizeConfigValue(
  defaultValue: ChannelFormValue,
  value: ChannelFormValue,
): Exclude<ChannelFormValue, undefined> {
  if (isBooleanValue(defaultValue)) return Boolean(value);
  if (isNumberValue(defaultValue)) {
    if (isStringValue(value) && value.trim() === "") return defaultValue;
    const number = Number(value);
    return Number.isFinite(number) ? Math.trunc(number) : defaultValue;
  }
  if (Array.isArray(defaultValue)) {
    return splitIDList(Array.isArray(value) ? value : isStringValue(value) ? value : "");
  }
  // SAFETY: non-array single values are stored as strings in the channel draft.
  return isStringValue(value) ? value : "";
}

export function serializePlatformConfig(type: string, data: ChannelForm): SerializedPlatformConfig {
  return Object.fromEntries(
    Object.entries(platformConfigDefaults(type)).map(([key, defaultValue]) => {
      if (type === "feishu" && key === "groups") return [key, serializeFeishuGroups(data[key])];
      return [key, normalizeConfigValue(defaultValue, data[key])];
    }),
  );
}

export function hasConfig(type: string, data: ChannelForm): boolean {
  return Object.values(serializePlatformConfig(type, data)).some((v) => {
    if (isBooleanValue(v)) return v;
    if (isJsonObject(v)) return Object.keys(v).length > 0;
    return String(v).trim() !== "";
  });
}

function parseFeishuGroups(value: JsonValue): FeishuGroupDraft[] {
  if (!isJsonObject(value)) return [];
  return Object.entries(value).flatMap(([chatId, raw], index) => {
    if (!isJsonObject(raw)) return [];
    return [
      {
        id: `${chatId}:${index}`,
        chatId,
        systemPrompt: isStringValue(raw.system_prompt) ? raw.system_prompt : "",
        enabled: isBooleanValue(raw.enabled) ? raw.enabled : undefined,
        requireMention: isBooleanValue(raw.require_mention) ? raw.require_mention : undefined,
        allowedUsers: jsonStringList(raw.allowed_users),
        disallowedUsers: jsonStringList(raw.disallowed_users),
        toolAllow: jsonStringList(raw.tool_allow),
        toolDeny: jsonStringList(raw.tool_deny),
      },
    ];
  });
}

function jsonStringList(value: JsonValue | undefined): string[] {
  if (isStringValue(value)) return splitIDList(value);
  if (Array.isArray(value) && value.every(isStringValue)) return splitIDList(value);
  return [];
}

function feishuGroups(value: string): FeishuGroupDraft[] {
  try {
    const parsed: JsonValue = JSON.parse(value || "{}");
    if (Array.isArray(parsed)) return parseFeishuGroupDrafts(parsed);
    return parseFeishuGroups(parsed);
  } catch {
    return [];
  }
}

function parseFeishuGroupDrafts(value: JsonValue[]): FeishuGroupDraft[] {
  return value.flatMap((raw, index) => {
    if (!isJsonObject(raw) || !isStringValue(raw.chatId)) return [];
    return [
      {
        id: isStringValue(raw.id) ? raw.id : `${raw.chatId}:${index}`,
        chatId: raw.chatId,
        systemPrompt: isStringValue(raw.systemPrompt) ? raw.systemPrompt : "",
        enabled: isBooleanValue(raw.enabled) ? raw.enabled : undefined,
        requireMention: isBooleanValue(raw.requireMention) ? raw.requireMention : undefined,
        allowedUsers: jsonStringList(raw.allowedUsers),
        disallowedUsers: jsonStringList(raw.disallowedUsers),
        toolAllow: jsonStringList(raw.toolAllow),
        toolDeny: jsonStringList(raw.toolDeny),
      },
    ];
  });
}

function serializeFeishuGroups(value: ChannelFormValue) {
  const seen = new Set<string>();
  const entries = feishuGroups(channelString(value)).flatMap((group) => {
    const chatId = group.chatId.trim();
    if (!chatId || seen.has(chatId)) return [];
    seen.add(chatId);
    const config: PersistedFeishuGroup = {};
    if (group.systemPrompt.trim()) config.system_prompt = group.systemPrompt;
    if (group.enabled !== undefined) config.enabled = group.enabled;
    if (group.requireMention !== undefined) config.require_mention = group.requireMention;
    if (group.allowedUsers.length > 0) config.allowed_users = splitIDList(group.allowedUsers);
    if (group.disallowedUsers.length > 0)
      config.disallowed_users = splitIDList(group.disallowedUsers);
    if (group.toolAllow.length > 0) config.tool_allow = splitIDList(group.toolAllow);
    if (group.toolDeny.length > 0) config.tool_deny = splitIDList(group.toolDeny);
    return [[chatId, config] as const];
  });
  return Object.fromEntries(entries) satisfies PersistedFeishuGroups;
}

export interface NormalizedChannel extends ChannelForm {
  id: string;
  name: string;
  type: string;
  label?: string;
  agent_id: string;
  agent_name?: string;
  is_active: boolean;
}

/**
 * Flatten a stored channel into the draft every editor mutates: the config JSON
 * is spread onto the row so a field is one key, not a nested path.
 */
export function normalizeChannel(ch: Channel): NormalizedChannel {
  const { _config: _ignoredConfig, ...base } = ch;
  const type = base.type || base.id;
  return {
    ...base,
    name: ch.name || "",
    type,
    agent_id: ch.agent_id || "",
    ...platformConfigDefaults(type),
    ...parseConfig(ch.config),
  };
}

/**
 * A ready-to-use display name for a new channel: `{type}-{4 hex chars}`. The id
 * is minted by the server and is a uuid nobody wants to read, so the name is the
 * only handle a user has on a channel — prefilling it means they can click
 * straight through, and it stays editable. The server applies the same default.
 */
export function suggestChannelName(type: string): string {
  const bytes = new Uint8Array(2);
  crypto.getRandomValues(bytes);
  const suffix = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `${type}-${suffix}`;
}

export function newInstanceDraft(type = defaultChannelType, name = suggestChannelName(type)) {
  return {
    type,
    name,
    ...platformConfigDefaults(type),
  };
}

/** The `config` string a write request carries: only the platform's own keys. */
export function channelConfig(ch: ChannelForm): string {
  // SAFETY: the config serialization only reads the platform discriminant.
  return JSON.stringify(serializePlatformConfig(channelString(ch.type), ch));
}

// ─── fields ───────────────────────────────────────────────────────────────────

/**
 * The per-platform credential inputs. Labels stay in the platform's own words
 * (`Bot Token`, `App ID`) because that is what the operator reads in the
 * platform's console — translating them would break the match.
 */
export function ChannelConfigFields({
  channel,
  onChange,
}: {
  channel: ChannelForm;
  onChange: (key: string, value: ChannelFormValue) => void;
}) {
  const { t } = useI18n();
  const type = channel.type;
  const feishuChatsQuery = useQuery(
    feishuChannelChatsQueryOptions(
      channelString(channel.id),
      type === "feishu" && Boolean(channel.id),
    ),
  );

  const field = (key: string, label: string, inputType = "text", placeholder = "") => {
    // SAFETY: scalar channel fields store their string form value.
    const rawValue = channel[key];
    const stringValue = isStringValue(rawValue) ? rawValue : "";
    return (
      <Field key={key} className="w-full">
        <FieldLabel className="font-mono">{label}</FieldLabel>
        <Input
          nativeInput
          type={inputType}
          value={stringValue}
          onChange={(e) => onChange(key, e.target.value)}
          placeholder={placeholder}
          className="w-full font-mono"
        />
      </Field>
    );
  };

  /** A comma/newline editable text input that persists as a string array. */
  const arrayField = (key: string, label: string, description: string) => {
    const value = channel[key];
    // SAFETY: non-array values render as their string form; arrays join above.
    const display = Array.isArray(value) ? value.join(", ") : isStringValue(value) ? value : "";
    return (
      <Field key={key} className="w-full">
        <FieldLabel className="font-mono">{label}</FieldLabel>
        <Input
          nativeInput
          type="text"
          value={display}
          onChange={(e) => onChange(key, e.target.value)}
          placeholder="id-1, id-2"
          className="w-full font-mono"
        />
        <FieldDescription>{description}</FieldDescription>
      </Field>
    );
  };

  const numberField = (
    key: string,
    label: string,
    description: string,
    min: number,
    max: number,
  ) => {
    const value = channel[key];
    return (
      <Field key={key} className="w-full">
        <FieldLabel>{label}</FieldLabel>
        <Input
          nativeInput
          type="number"
          min={min}
          max={max}
          value={isNumberValue(value) && Number.isFinite(value) ? value : ""}
          onChange={(e) => onChange(key, e.target.value === "" ? "" : e.target.valueAsNumber)}
          className="w-full font-mono"
        />
        <FieldDescription>{description}</FieldDescription>
      </Field>
    );
  };

  const accessFields = (groupLabel: string, groupDescription: string) => (
    <>
      <Field>
        <FieldLabel>{groupLabel}</FieldLabel>
        <Switch
          checked={Boolean(channel.allow_group)}
          aria-label={groupLabel}
          onCheckedChange={(checked) => onChange("allow_group", checked)}
        />
        <FieldDescription>{groupDescription}</FieldDescription>
      </Field>
      <Field>
        <FieldLabel>{t("channels.allowDm")}</FieldLabel>
        <Switch
          checked={Boolean(channel.allow_dm)}
          aria-label={t("channels.allowDm")}
          onCheckedChange={(checked) => onChange("allow_dm", checked)}
        />
        <FieldDescription>{t("channels.allowDmDesc")}</FieldDescription>
      </Field>
      <Field>
        <FieldLabel>{t("channels.allowUnlinkedDm")}</FieldLabel>
        <Switch
          checked={Boolean(channel.allow_unlinked_dm)}
          aria-label={t("channels.allowUnlinkedDm")}
          onCheckedChange={(checked) => onChange("allow_unlinked_dm", checked)}
        />
        <FieldDescription>{t("channels.allowUnlinkedDmDesc")}</FieldDescription>
      </Field>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        {numberField(
          "guest_message_limit_per_minute",
          t("channels.guestMessageLimit"),
          t("channels.guestMessageLimitDesc"),
          1,
          120,
        )}
        {numberField(
          "guest_max_per_channel",
          t("channels.guestMaxPerChannel"),
          t("channels.guestMaxPerChannelDesc"),
          1,
          100000,
        )}
        {numberField(
          "guest_retention_days",
          t("channels.guestRetentionDays"),
          t("channels.guestRetentionDaysDesc"),
          1,
          365,
        )}
      </div>
      <Field>
        <FieldLabel>{t("channels.requireMention")}</FieldLabel>
        <Switch
          checked={Boolean(channel.require_mention)}
          aria-label={t("channels.requireMention")}
          onCheckedChange={(checked) => onChange("require_mention", checked)}
        />
        <FieldDescription>{t("channels.requireMentionDesc")}</FieldDescription>
      </Field>
    </>
  );

  const feishuGroupFields = () => {
    const groups = feishuGroups(channelString(channel.groups));
    const availableChats = feishuChatsQuery.data ?? [];
    const canSelectChat =
      availableChats.length > 0 && !feishuChatsQuery.isPending && !feishuChatsQuery.isError;
    const chatOptions = (selectedChatID: string) => {
      const options = availableChats.map((chat) => ({
        value: chat.id,
        label: chat.name || chat.id,
        description: chat.name ? chat.id : undefined,
      }));
      if (selectedChatID && !options.some((chat) => chat.value === selectedChatID)) {
        options.unshift({
          value: selectedChatID,
          label: selectedChatID,
          description: t("channels.feishuGroupNoLongerJoined"),
        });
      }
      return options;
    };
    const updateGroup = (id: string, patch: Partial<FeishuGroupDraft>) => {
      onChange(
        "groups",
        JSON.stringify(groups.map((group) => (group.id === id ? { ...group, ...patch } : group))),
      );
    };
    const removeGroup = (id: string) => {
      onChange("groups", JSON.stringify(groups.filter((group) => group.id !== id)));
    };
    const addGroup = () => {
      onChange(
        "groups",
        JSON.stringify([
          ...groups,
          {
            id: crypto.randomUUID(),
            chatId: "",
            systemPrompt: "",
            allowedUsers: [],
            disallowedUsers: [],
            toolAllow: [],
            toolDeny: [],
          },
        ]),
      );
    };

    return (
      <Fieldset className="flex flex-col gap-4">
        <div className="flex items-center justify-between gap-2">
          <FieldsetLegend>{t("channels.feishuGroups")}</FieldsetLegend>
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={addGroup}
            disabled={!canSelectChat}
          >
            <Plus aria-hidden="true" />
            {t("channels.addFeishuGroup")}
          </Button>
        </div>
        <Field>
          <FieldDescription>{t("channels.feishuGroupsDesc")}</FieldDescription>
        </Field>
        {!channel.id && (
          <Field>
            <FieldDescription>{t("channels.feishuGroupsSaveFirst")}</FieldDescription>
          </Field>
        )}
        {Boolean(channel.id) && feishuChatsQuery.isPending && (
          <Field>
            <FieldDescription>{t("channels.feishuGroupsLoading")}</FieldDescription>
          </Field>
        )}
        {Boolean(channel.id) && feishuChatsQuery.isError && (
          <Field>
            <FieldDescription>{t("channels.feishuGroupsLoadFailed")}</FieldDescription>
          </Field>
        )}
        {Boolean(channel.id) &&
          !feishuChatsQuery.isPending &&
          !feishuChatsQuery.isError &&
          availableChats.length === 0 && (
            <Field>
              <FieldDescription>{t("channels.feishuGroupsEmpty")}</FieldDescription>
            </Field>
          )}
        {groups.length === 0 && (
          <Field>
            <FieldDescription>{t("channels.noFeishuGroups")}</FieldDescription>
          </Field>
        )}
        {groups.map((group, index) => {
          const accessOverride = group.enabled !== undefined;
          const mentionOverride = group.requireMention !== undefined;
          return (
            <Fieldset key={group.id} className="flex flex-col gap-4 border-t pt-4">
              <div className="flex items-center justify-between gap-2">
                <FieldsetLegend>{t("channels.feishuGroup", { number: index + 1 })}</FieldsetLegend>
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={t("channels.removeFeishuGroup", { number: index + 1 })}
                  onClick={() => removeGroup(group.id)}
                >
                  <Trash2 aria-hidden="true" />
                </Button>
              </div>
              <Field>
                <FieldLabel>{t("channels.feishuGroupChatId")}</FieldLabel>
                <Combobox
                  items={chatOptions(group.chatId)}
                  value={
                    chatOptions(group.chatId).find((chat) => chat.value === group.chatId) ?? null
                  }
                  disabled={!canSelectChat}
                  itemToStringLabel={(chat) => chat.label}
                  itemToStringValue={(chat) => chat.value}
                  isItemEqualToValue={(chat, selected) => chat.value === selected.value}
                  onValueChange={(chat) => chat && updateGroup(group.id, { chatId: chat.value })}
                >
                  <ComboboxInput
                    placeholder={t("channels.feishuGroupChatPlaceholder")}
                    aria-label={t("channels.feishuGroupChatId")}
                    showClear={false}
                  />
                  <ComboboxPopup>
                    <ComboboxEmpty>{t("channels.feishuGroupsEmpty")}</ComboboxEmpty>
                    <ComboboxList>
                      {(chat: { value: string; label: string; description?: string }) => (
                        <ComboboxItem key={chat.value} value={chat}>
                          <div className="min-w-0">
                            <div className="truncate">{chat.label}</div>
                            {chat.description && (
                              <div className="truncate font-mono text-xs text-muted-foreground">
                                {chat.description}
                              </div>
                            )}
                          </div>
                        </ComboboxItem>
                      )}
                    </ComboboxList>
                  </ComboboxPopup>
                </Combobox>
                <FieldDescription>{t("channels.feishuGroupChatIdDesc")}</FieldDescription>
              </Field>
              <Field>
                <FieldLabel>{t("channels.feishuGroupSystemPrompt")}</FieldLabel>
                <Textarea
                  value={group.systemPrompt}
                  onChange={(event) => updateGroup(group.id, { systemPrompt: event.target.value })}
                />
                <FieldDescription>{t("channels.feishuGroupSystemPromptDesc")}</FieldDescription>
              </Field>
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <Field>
                  <FieldLabel>{t("channels.feishuOverrideGroupAccess")}</FieldLabel>
                  <Switch
                    checked={accessOverride}
                    aria-label={t("channels.feishuOverrideGroupAccess")}
                    onCheckedChange={(checked) =>
                      updateGroup(group.id, {
                        enabled: checked ? Boolean(channel.allow_group) : undefined,
                      })
                    }
                  />
                  <FieldDescription>{t("channels.feishuOverrideGroupAccessDesc")}</FieldDescription>
                </Field>
                <Field>
                  <FieldLabel>{t("channels.feishuGroupAccess")}</FieldLabel>
                  <Switch
                    checked={Boolean(group.enabled)}
                    disabled={!accessOverride}
                    aria-label={t("channels.feishuGroupAccess")}
                    onCheckedChange={(enabled) => updateGroup(group.id, { enabled })}
                  />
                </Field>
                <Field>
                  <FieldLabel>{t("channels.feishuOverrideMention")}</FieldLabel>
                  <Switch
                    checked={mentionOverride}
                    aria-label={t("channels.feishuOverrideMention")}
                    onCheckedChange={(checked) =>
                      updateGroup(group.id, {
                        requireMention: checked ? Boolean(channel.require_mention) : undefined,
                      })
                    }
                  />
                  <FieldDescription>{t("channels.feishuOverrideMentionDesc")}</FieldDescription>
                </Field>
                <Field>
                  <FieldLabel>{t("channels.feishuGroupRequireMention")}</FieldLabel>
                  <Switch
                    checked={Boolean(group.requireMention)}
                    disabled={!mentionOverride}
                    aria-label={t("channels.feishuGroupRequireMention")}
                    onCheckedChange={(requireMention) => updateGroup(group.id, { requireMention })}
                  />
                </Field>
              </div>
              <Field>
                <FieldLabel>{t("channels.feishuAllowedUsers")}</FieldLabel>
                <Input
                  nativeInput
                  type="text"
                  value={group.allowedUsers.join(", ")}
                  onChange={(event) =>
                    updateGroup(group.id, { allowedUsers: splitIDList(event.target.value) })
                  }
                  placeholder="on_user_1, on_user_2"
                  className="w-full font-mono"
                />
                <FieldDescription>{t("channels.feishuAllowedUsersDesc")}</FieldDescription>
              </Field>
              <Field>
                <FieldLabel>{t("channels.feishuDisallowedUsers")}</FieldLabel>
                <Input
                  nativeInput
                  type="text"
                  value={group.disallowedUsers.join(", ")}
                  onChange={(event) =>
                    updateGroup(group.id, { disallowedUsers: splitIDList(event.target.value) })
                  }
                  placeholder="on_user_1, on_user_2"
                  className="w-full font-mono"
                />
                <FieldDescription>{t("channels.feishuDisallowedUsersDesc")}</FieldDescription>
              </Field>
            </Fieldset>
          );
        })}
      </Fieldset>
    );
  };

  return (
    <div className="flex flex-col gap-4">
      {type === "telegram" && (
        <>
          {field("token", "Bot Token", "password", "From @BotFather")}
          {field("channel_id", "Channel ID", "text", "Default channel")}
          {accessFields(t("channels.allowGroup"), t("channels.allowGroupDesc"))}
          {arrayField(
            "allowed_chat_ids",
            t("channels.allowedTelegramChatIds"),
            t("channels.allowedTelegramChatIdsDesc"),
          )}
          {arrayField(
            "allowed_topic_ids",
            t("channels.allowedTelegramTopicIds"),
            t("channels.allowedTelegramTopicIdsDesc"),
          )}
        </>
      )}

      {type === "discord" && (
        <>
          {field("token", "Bot Token", "password", "Discord Developer Portal")}
          {accessFields(t("channels.allowGuild"), t("channels.allowGuildDesc"))}
          <Field>
            <FieldLabel>{t("channels.allowAllGuilds")}</FieldLabel>
            <Switch
              checked={Boolean(channel.allow_all_guilds)}
              aria-label={t("channels.allowAllGuilds")}
              onCheckedChange={(checked) => onChange("allow_all_guilds", checked)}
            />
            <FieldDescription>{t("channels.allowAllGuildsDesc")}</FieldDescription>
          </Field>
          {arrayField(
            "allowed_guild_ids",
            t("channels.allowedGuildIds"),
            t("channels.allowedGuildIdsDesc"),
          )}
          {arrayField(
            "allowed_channel_ids",
            t("channels.allowedChannelIds"),
            t("channels.allowedChannelIdsDesc"),
          )}
          {arrayField(
            "allowed_user_ids",
            t("channels.allowedUserIds"),
            t("channels.allowedUserIdsDesc"),
          )}
          {arrayField(
            "allowed_role_ids",
            t("channels.allowedRoleIds"),
            t("channels.allowedRoleIdsDesc"),
          )}
        </>
      )}

      {type === "qq" && (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          {field("app_id", "App ID", "text", "QQ Bot App ID")}
          {field("app_secret", "App Secret", "password")}
        </div>
      )}

      {type === "feishu" && (
        <>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            {field("app_id", "App ID")}
            {field("app_secret", "App Secret", "password")}
            {field("encrypt_key", "Encrypt Key", "password", "optional")}
            {field("verification_token", "Verification Token", "password", "optional")}
            {field("tenant_key", "Tenant Key", "text", "optional, auto-detected at startup")}
          </div>
          <Field>
            <FieldLabel>{t("channels.autoProvision")}</FieldLabel>
            <Switch
              checked={Boolean(channel.auto_provision)}
              aria-label={t("channels.autoProvision")}
              onCheckedChange={(checked) => onChange("auto_provision", checked)}
            />
            <FieldDescription>{t("channels.autoProvisionDesc")}</FieldDescription>
          </Field>
          {accessFields(t("channels.allowGroup"), t("channels.allowGroupDesc"))}
          {feishuGroupFields()}
        </>
      )}

      {type === "dingtalk" && (
        <>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            {field("client_id", "Client ID", "text", "DingTalk application Client ID")}
            {field("client_secret", "Client Secret", "password")}
          </div>
          {accessFields(t("channels.allowGroup"), t("channels.allowGroupDesc"))}
          {/* A standalone note, not a field: Base UI's Description must stay inside a Field.Root. */}
          <p className="text-muted-foreground text-xs">{t("channels.dingtalkNotifyDesc")}</p>
        </>
      )}

      {type === "weixin" && (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          {field("bot_token", "Bot Token", "password")}
          {field("base_url", "Base URL", "text", "https://ilinkai.weixin.qq.com")}
          {field("bot_id", "Bot ID", "text", "optional")}
          {field("user_id", "User ID", "text", "optional")}
        </div>
      )}
    </div>
  );
}

/**
 * Everything an existing channel is edited by — name, active state, credentials.
 * Shared so the settings inventory (`ChannelsPage`) and the agent profile's
 * channels tab ask for a channel in exactly the same words; neither owns the
 * binding, which is the agent page's dedicated bind endpoint.
 */
export function ChannelFields({
  channel,
  onChange,
}: {
  channel: NormalizedChannel;
  onChange: (key: string, value: ChannelFormValue) => void;
}) {
  const { t } = useI18n();
  const label = platformLabel(channel.type);
  const hasConfigFields = Object.keys(platformConfigDefaults(channel.type)).length > 0;

  return (
    <div className="flex flex-col gap-4">
      <Field>
        <FieldLabel>{t("common.name")}</FieldLabel>
        <Input
          nativeInput
          type="text"
          value={channel.name || ""}
          onChange={(e) =>
            // SAFETY: the target of a nativeInput change event is the input.
            onChange("name", targetValue(e))
          }
          placeholder={label}
          className="w-full"
        />
      </Field>

      <Field>
        <FieldLabel>{t("channels.active")}</FieldLabel>
        <Switch
          checked={Boolean(channel.is_active)}
          aria-label={t("channels.active")}
          onCheckedChange={(checked) => onChange("is_active", checked)}
        />
        <FieldDescription>{t("channels.activeDesc")}</FieldDescription>
      </Field>

      {hasConfigFields && <ChannelConfigFields channel={channel} onChange={onChange} />}
    </div>
  );
}
