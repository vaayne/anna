import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { PluginMcpBackendSummary } from "@/lib/api-client";
import { useI18n } from "@/lib/i18n";

export type McpTransport = PluginMcpBackendSummary["transport"];
export type McpAuthType = PluginMcpBackendSummary["auth_type"];

export function transportLabel(transport: McpTransport) {
  return transport === "streamable_http" ? "Streamable HTTP" : "SSE";
}

/**
 * The registration fields every MCP server form asks for — name, URL, transport,
 * auth. Shared so the settings inventory (`MCPServersPanel`) and the agent
 * profile's tools tab ask for a server in exactly the same words; each caller
 * still owns the destination (scope) question, which differs between them.
 */
export function McpServerFields({
  name,
  onNameChange,
  url,
  onUrlChange,
  transport,
  onTransportChange,
  authType,
  onAuthTypeChange,
  token,
  onTokenChange,
  editing,
  oauthClientId,
  onOauthClientIdChange,
  oauthClientSecret,
  onOauthClientSecretChange,
  credentialMode,
  onCredentialModeChange,
  showCredentialMode = false,
  showName = true,
}: {
  name: string;
  onNameChange: (value: string) => void;
  url: string;
  onUrlChange: (value: string) => void;
  transport: McpTransport;
  onTransportChange: (value: McpTransport) => void;
  authType: McpAuthType;
  onAuthTypeChange: (value: McpAuthType) => void;
  token: string;
  onTokenChange: (value: string) => void;
  /** An existing server keeps its stored token when the field is left blank. */
  editing: boolean;
  oauthClientId?: string;
  onOauthClientIdChange?: (value: string) => void;
  oauthClientSecret?: string;
  onOauthClientSecretChange?: (value: string) => void;
  credentialMode?: "shared" | "per_user";
  onCredentialModeChange?: (value: "shared" | "per_user") => void;
  /** Only system/system_agent scopes can share one connection across users. */
  showCredentialMode?: boolean;
  /** Definition name is fixed when editing a common plugin configuration. */
  showName?: boolean;
}) {
  const { t } = useI18n();
  // SAFETY: the transport Select's options are the two McpTransport values (below).
  const onTransportChangeLocal = (value: string | null) =>
    value && onTransportChange(value as McpTransport);
  // SAFETY: the transport options render back through transportLabel which takes an McpTransport.
  const renderTransportLabel = (value: string) =>
    transportLabel((value as McpTransport) || transport);
  // SAFETY: the auth Select's options are the McpAuthType values (below).
  const onAuthTypeChangeLocal = (value: string | null) =>
    value && onAuthTypeChange(value as McpAuthType);
  // SAFETY: credential_mode is the two-value closed enum above.
  const onCredentialModeChangeLocal = (value: string | null) =>
    value && onCredentialModeChange?.(value as "shared" | "per_user");
  return (
    <>
      {showName && (
        <Field>
          <FieldLabel>{t("mcp.name")}</FieldLabel>
          <Input
            value={name}
            onChange={(e) => onNameChange(e.target.value)}
            placeholder="github"
            nativeInput
          />
          <FieldDescription>{t("mcp.name.description")}</FieldDescription>
        </Field>
      )}

      <Field>
        <FieldLabel>{t("mcp.url")}</FieldLabel>
        <Input
          value={url}
          onChange={(e) => onUrlChange(e.target.value)}
          placeholder="https://mcp.example.com/mcp"
          nativeInput
        />
      </Field>

      <Field>
        <FieldLabel>{t("mcp.transport")}</FieldLabel>
        <Select value={transport} onValueChange={onTransportChangeLocal}>
          <SelectTrigger>
            <SelectValue>{renderTransportLabel}</SelectValue>
          </SelectTrigger>
          <SelectPopup>
            <SelectItem value="streamable_http">Streamable HTTP</SelectItem>
            <SelectItem value="sse">SSE</SelectItem>
          </SelectPopup>
        </Select>
      </Field>

      <Field>
        <FieldLabel>{t("mcp.auth")}</FieldLabel>
        <Select value={authType} onValueChange={onAuthTypeChangeLocal}>
          <SelectTrigger>
            <SelectValue>
              {(value) => (value === "bearer" ? t("mcp.auth.bearer") : t("mcp.auth.none"))}
            </SelectValue>
          </SelectTrigger>
          <SelectPopup>
            <SelectItem value="none">{t("mcp.auth.none")}</SelectItem>
            <SelectItem value="bearer">{t("mcp.auth.bearer")}</SelectItem>
            <SelectItem value="oauth">{t("mcp.auth.oauth")}</SelectItem>
          </SelectPopup>
        </Select>
      </Field>

      {authType === "oauth" && (
        <>
          <Field>
            <FieldLabel>{t("mcp.oauth.clientId")}</FieldLabel>
            <Input
              value={oauthClientId ?? ""}
              onChange={(e) => onOauthClientIdChange?.(e.target.value)}
              autoComplete="off"
              nativeInput
            />
            <FieldDescription>{t("mcp.oauth.clientId.description")}</FieldDescription>
          </Field>
          <Field>
            <FieldLabel>{t("mcp.oauth.clientSecret")}</FieldLabel>
            <Input
              type="password"
              value={oauthClientSecret ?? ""}
              onChange={(e) => onOauthClientSecretChange?.(e.target.value)}
              autoComplete="off"
              nativeInput
            />
            <FieldDescription>
              {editing
                ? t("mcp.oauth.clientSecret.editDescription")
                : t("mcp.oauth.clientSecret.description")}
            </FieldDescription>
          </Field>
          {showCredentialMode && (
            <Field>
              <FieldLabel>{t("mcp.credentialMode")}</FieldLabel>
              <Select
                value={credentialMode ?? "shared"}
                onValueChange={onCredentialModeChangeLocal}
              >
                <SelectTrigger>
                  <SelectValue>
                    {(value) =>
                      value === "per_user"
                        ? t("mcp.credentialMode.perUser")
                        : t("mcp.credentialMode.shared")
                    }
                  </SelectValue>
                </SelectTrigger>
                <SelectPopup>
                  <SelectItem value="shared">{t("mcp.credentialMode.shared")}</SelectItem>
                  <SelectItem value="per_user">{t("mcp.credentialMode.perUser")}</SelectItem>
                </SelectPopup>
              </Select>
              <FieldDescription>{t("mcp.credentialMode.description")}</FieldDescription>
            </Field>
          )}
        </>
      )}

      {authType === "bearer" && (
        <Field>
          <FieldLabel>{t("mcp.token")}</FieldLabel>
          <Input
            type="password"
            value={token}
            onChange={(e) => onTokenChange(e.target.value)}
            autoComplete="off"
            nativeInput
          />
          <FieldDescription>
            {editing ? t("mcp.token.editDescription") : t("mcp.token.description")}
          </FieldDescription>
        </Field>
      )}
    </>
  );
}
