import { useEffect, useState } from "react";
import type {
  ComponentsPluginConfigInputWritable,
  PluginConfig,
  PluginDefinition,
} from "@/lib/api-client";
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { McpServerFields } from "@/features/mcp/McpServerFields";
import type { McpAuthType, McpTransport } from "@/features/mcp/McpServerFields";
import { useI18n } from "@/lib/i18n";

export type PluginConfigPayload = {
  config?: Record<string, unknown>;
  binary_versions?: Record<string, string>;
};
export type PluginConfigCredentials = NonNullable<
  ComponentsPluginConfigInputWritable["credentials"]
>;

type Translate = ReturnType<typeof useI18n>["t"];

type SaveConfig = (payload: PluginConfigPayload, credentials: PluginConfigCredentials) => void;

function mcpSummary(config?: PluginConfig) {
  return config?.backend_summary.backend === "mcp" ? config.backend_summary : undefined;
}

function cliSummary(config?: PluginConfig) {
  return config?.backend_summary.backend === "cli" ? config.backend_summary : undefined;
}

function ConfigEditor({
  plugin,
  config,
  initialMcpUrl,
  onSave,
  onCancel,
  busy,
  t,
}: {
  plugin: Pick<PluginDefinition, "backend" | "display_name">;
  config?: PluginConfig;
  initialMcpUrl?: string;
  onSave: SaveConfig;
  onCancel: () => void;
  busy: boolean;
  t: Translate;
}) {
  const mcp = mcpSummary(config);
  const cli = cliSummary(config);
  const [url, setURL] = useState("");
  const [transport, setTransport] = useState<McpTransport>("streamable_http");
  const [authType, setAuthType] = useState<McpAuthType>("none");
  const [credentialMode, setCredentialMode] = useState<"shared" | "per_user">("shared");
  const [token, setToken] = useState("");
  const [oauthClientId, setOauthClientId] = useState("");
  const [oauthClientSecret, setOauthClientSecret] = useState("");
  const [versions, setVersions] = useState<Record<string, string>>({});

  useEffect(() => {
    setURL(initialMcpUrl ?? "");
    setTransport(mcp?.transport ?? "streamable_http");
    setAuthType(mcp?.auth_type ?? "none");
    setCredentialMode(mcp?.credential_mode ?? "shared");
    setToken("");
    setOauthClientId("");
    setOauthClientSecret("");
    setVersions(Object.fromEntries((cli?.binaries ?? []).map((binary) => [binary.name, ""])));
  }, [cli, initialMcpUrl, mcp]);

  const save = () => {
    if (plugin.backend === "mcp") {
      const config: Record<string, unknown> = {
        transport,
        auth_type: authType,
        credential_mode: credentialMode,
      };
      if (url.trim()) config.url = url.trim();
      const credentials: PluginConfigCredentials = {};
      if (authType === "bearer" && token.trim()) credentials.token = token;
      if (authType === "oauth") {
        if (oauthClientId.trim()) credentials.oauth_client_id = oauthClientId.trim();
        if (oauthClientSecret.trim()) credentials.oauth_client_secret = oauthClientSecret;
      }
      onSave({ config }, credentials);
      return;
    }
    if (plugin.backend === "cli") {
      const payload: PluginConfigPayload = {};
      const binaryVersions = Object.fromEntries(
        Object.entries(versions).filter(([, version]) => version.trim()),
      );
      if (Object.keys(binaryVersions).length > 0) payload.binary_versions = binaryVersions;
      onSave(payload, {});
      return;
    }
    onSave({}, {});
  };

  if (plugin.backend === "mcp") {
    return (
      <div className="space-y-4">
        <McpServerFields
          name={plugin.display_name}
          onNameChange={() => undefined}
          url={url}
          onUrlChange={setURL}
          transport={transport}
          onTransportChange={setTransport}
          authType={authType}
          onAuthTypeChange={setAuthType}
          token={token}
          onTokenChange={setToken}
          editing={!!config}
          oauthClientId={oauthClientId}
          onOauthClientIdChange={setOauthClientId}
          oauthClientSecret={oauthClientSecret}
          onOauthClientSecretChange={setOauthClientSecret}
          credentialMode={credentialMode}
          onCredentialModeChange={setCredentialMode}
          showCredentialMode
          showName={false}
        />
        {config && <p className="text-sm text-muted-foreground">{t("plugins.blankPreserves")}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onCancel} disabled={busy}>
            {t("common.cancel")}
          </Button>
          <Button size="sm" onClick={save} loading={busy}>
            {t("common.save")}
          </Button>
        </div>
      </div>
    );
  }

  if (plugin.backend === "cli" && cli) {
    return (
      <div className="space-y-4">
        <div className="space-y-2">
          <p className="text-xs font-semibold text-muted-foreground">
            {t("plugins.binaryVersions")}
          </p>
          {cli.binaries.map((binary) => (
            <Field key={binary.name}>
              <FieldLabel>{binary.name}</FieldLabel>
              <Input
                value={versions[binary.name] ?? ""}
                onChange={(event) =>
                  setVersions((current) => ({
                    ...current,
                    [binary.name]: event.target.value,
                  }))
                }
                placeholder={binary.version}
                nativeInput
              />
            </Field>
          ))}
        </div>
        <p className="text-sm text-muted-foreground">{t("plugins.blankPreserves")}</p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onCancel} disabled={busy}>
            {t("common.cancel")}
          </Button>
          <Button size="sm" onClick={save} loading={busy}>
            {t("common.save")}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="flex justify-end">
      <Button variant="ghost" size="sm" onClick={onCancel} disabled={busy}>
        {t("common.cancel")}
      </Button>
    </div>
  );
}

export function PluginConfigEditor(props: Omit<Parameters<typeof ConfigEditor>[0], "t">) {
  const { t } = useI18n();
  return <ConfigEditor {...props} t={t} />;
}
