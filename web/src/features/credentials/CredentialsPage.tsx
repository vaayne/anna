import { useCallback, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  deleteOAuthProviderConfig,
  deleteScopedVaultEntry as deleteVaultEntryRequest,
  disconnectOAuth as disconnectOAuthRequest,
  getScopedVaultEntry,
  listAgents,
  listOAuthProviders,
  listScopedVaultEntries,
  pollOAuthFlow,
  setOAuthProviderConfig,
  setScopedVaultEntry,
  startOAuthFlow,
} from "@/lib/api-client/sdk.gen";
import {
  oauthProviderConfigOptions,
  oauthProvidersQueryKey,
  oauthProvidersQueryOptions,
} from "@/lib/queries/oauth";
import { formatTime } from "@/lib/time";
import type { Agent, OAuthFlow, OAuthProvider, VaultEntry } from "@/lib/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Fieldset, FieldsetLegend } from "@/components/ui/fieldset";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useI18n } from "@/lib/i18n";
import type { MessageKey } from "@/lib/i18n/messages";
import { useToast } from "@/hooks/use-toast";
import { EmailAccountsPanel } from "@/features/credentials/EmailAccountsPanel";
import { buildOAuthScopeDraft, ScopeEditor } from "@/features/credentials/ScopeEditor";
import { ConfirmDialog } from "@/features/settings/ConfirmDialog";
import {
  SettingsDetailSheet,
  SettingsGridPage,
  SettingsList,
  SettingsRow,
  SettingsSection,
} from "@/features/settings/SettingsCardGrid";
import type { RowAction } from "@/features/settings/SettingsCardGrid";
import { DetailPanel, DetailPanelHeader } from "@/features/settings/SettingsDetailPanel";
import { KeyRound, Lock, Plug, Plus } from "lucide-react";
import { siGithub, siX } from "simple-icons";
import {
  isAgentManagedScope,
  scopeForRange,
  scopeQueriesForBand,
  scopesForBand,
  type ScopeBand,
} from "@/lib/scope-band";

// Brand marks carried by simple-icons, resolved by slug. Adding a simple-icons
// brand is one named import + one entry here; unknown slugs fall through to the
// generic glyph.
const SIMPLE_ICONS = {
  github: siGithub,
  x: siX,
} satisfies Record<string, { path: string }>;

type VaultScope = VaultEntry["scope"];
type ScopeRange = "all" | "specific";

// Reserved keys are written and rotated by stella itself. Surface them as
// read-only so users don't delete a managed credential.
const RESERVED_VAULT_KEYS = new Set(["STELLA_TOKEN", "GH_OAUTH"]);
const RESERVED_VAULT_PREFIXES = ["OAUTH_", "MCP_TOKEN_"];

function isReservedVaultKey(name: string) {
  return (
    RESERVED_VAULT_KEYS.has(name) ||
    RESERVED_VAULT_PREFIXES.some((prefix) => name.startsWith(prefix))
  );
}

function isAgentVaultScope(scope: VaultScope) {
  return isAgentManagedScope(scope);
}

function sameScopeSet(a: string[], b: string[]) {
  const left = new Set(a);
  const right = new Set(b);
  return left.size === right.size && [...left].every((scope) => right.has(scope));
}

// One hue per scope, drawn from the chart palette tokens: a scope is a category,
// which is what `chart-*` means. Reused by the list group rails, the row icon and
// the precedence ladder so a scope reads as the same color everywhere.
//
// There is deliberately no `text` entry. These tokens are tuned to be plotted as
// areas, and as words they run 2.4-3.8:1 — `chart-4` as a scope label measured
// 2.35:1. The dot carries the hue; the label is read, so it stays on
// `--foreground` and the active row is marked by weight and its own tint.
const SCOPE_COLOR = {
  user: { dot: "bg-chart-2", soft: "bg-chart-2/12" },
  user_agent: { dot: "bg-chart-1", soft: "bg-chart-1/12" },
  system: { dot: "bg-chart-4", soft: "bg-chart-4/12" },
  system_agent: { dot: "bg-chart-5", soft: "bg-chart-5/12" },
} satisfies Record<VaultScope, { dot: string; soft: string }>;

// Render order for the grouped vault list.
const SCOPE_ORDER: VaultScope[] = ["user", "user_agent", "system", "system_agent"];

// Resolution precedence, highest first: a higher scope's value overrides a lower
// one at runtime. Drives the precedence ladder so the override chain is visible.
const SCOPE_PRIORITY: VaultScope[] = ["user_agent", "user", "system_agent", "system"];

const SCOPE_LABEL_KEY = {
  user: "credentials.scope.user.label",
  user_agent: "credentials.scope.userAgent.label",
  system: "credentials.scope.system.label",
  system_agent: "credentials.scope.systemAgent.label",
} satisfies Record<VaultScope, MessageKey>;

const SCOPE_DESC_KEY = {
  user: "credentials.scope.user.desc",
  user_agent: "credentials.scope.userAgent.desc",
  system: "credentials.scope.system.desc",
  system_agent: "credentials.scope.systemAgent.desc",
} satisfies Record<VaultScope, MessageKey>;

// ProviderIcon resolves a brand mark from the provider's icon string (set in
// provider YAML) and falls back to a generic plug glyph.
function ProviderIcon({ icon, label }: { icon?: string; label: string }) {
  const [family, name] = (icon ?? "").split(":");
  if (family === "simpleicons") {
    // SAFETY: unknown provider slugs intentionally fall through to the generic glyph.
    const brand = SIMPLE_ICONS[name?.toLowerCase() as keyof typeof SIMPLE_ICONS];
    if (brand) {
      return (
        <svg viewBox="0 0 24 24" className="size-4" fill="currentColor" aria-label={label}>
          <path d={brand.path} />
        </svg>
      );
    }
  }
  return <Plug className="size-4" />;
}

export function CredentialsPage({ scopeBand }: { scopeBand: ScopeBand }) {
  const { t } = useI18n();
  // SAFETY: scopesForBand returns ManagedScope, the same literal union as VaultScope.
  const managedScopes = scopesForBand(scopeBand) as readonly VaultScope[];
  const personalSurface = scopeBand === "personal";

  const [vaultEntries, setVaultEntries] = useState<VaultEntry[]>([]);
  const [vaultLoading, setVaultLoading] = useState(false);
  const [vaultLoaded, setVaultLoaded] = useState(false);
  const [vaultSaving, setVaultSaving] = useState(false);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [editingEntry, setEditingEntry] = useState<VaultEntry | null>(null);
  const [existingSecretValue, setExistingSecretValue] = useState("");
  // Add-form scope state, independent of the list (which shows every visible scope).
  const [formRange, setFormRange] = useState<ScopeRange>("all");
  const [formAgentID, setFormAgentID] = useState("");
  const [newSecretName, setNewSecretName] = useState("");
  const [newSecretValue, setNewSecretValue] = useState("");
  const [addSheetOpen, setAddSheetOpen] = useState(false);
  // SAFETY: the agent Select offers agent-id options that come back as strings; null clears the field.
  const onSelectFormAgent = (value: string | null) =>
    setFormAgentID((value as string | null) ?? "");

  const resetVaultForm = useCallback(() => {
    setNewSecretName("");
    setNewSecretValue("");
    setExistingSecretValue("");
    setEditingEntry(null);
    setFormRange("all");
    setFormAgentID("");
  }, []);

  const openAddSheet = useCallback(() => {
    resetVaultForm();
    setAddSheetOpen(true);
  }, [resetVaultForm]);

  const queryClient = useQueryClient();
  // The provider list (with per-user connection/reconnect state) is server
  // cache: one query drives the whole OAuth section. Connect/disconnect/save
  // invalidate it instead of hand-refetching.
  const { data: oauthProviders = [], isLoading: oauthLoading } = useQuery({
    ...oauthProvidersQueryOptions,
    enabled: personalSurface,
  });
  const { data: systemOAuthProviders = [] } = useQuery({
    queryKey: ["system-oauth-providers"],
    queryFn: async () => {
      const { data } = await listOAuthProviders({ throwOnError: true });
      return (data?.providers ?? []).map(({ provider, configured }) => ({
        provider,
        configured,
      }));
    },
    enabled: !personalSurface,
  });
  // Flow progress and the last failure reason are ephemeral connect-flow UI,
  // not server cache, so they stay local.
  const [oauthFlow, setOauthFlow] = useState<Record<string, OAuthFlow | null>>({});
  const [oauthFlowActive, setOauthFlowActive] = useState<Record<string, boolean>>({});
  const [oauthFlowError, setOauthFlowError] = useState<Record<string, string | null>>({});

  const [sheetProvider, setSheetProvider] = useState<string | null>(null);
  const [configValues, setConfigValues] = useState<
    Record<string, { clientId: string; clientSecret: string; redirectUrl: string }>
  >({});
  const [configSaving, setConfigSaving] = useState<Record<string, boolean>>({});
  const [hasExistingSecret, setHasExistingSecret] = useState<Record<string, boolean>>({});
  // Scope editing stays separate from credential inputs. The saved and default
  // lists seed one checklist without another config fetch.
  const [scopeDraft, setScopeDraft] = useState<Record<string, string[]>>({});
  const [scopeMeta, setScopeMeta] = useState<
    Record<string, { saved: string[]; defaults: string[] }>
  >({});

  // One pending confirmation at a time; ConfirmDialog is controlled by this.
  const [confirm, setConfirm] = useState<{
    title: string;
    message: string;
    confirmLabel?: string;
    onConfirm: () => void;
  } | null>(null);

  const { showToast } = useToast();
  const pollAbortRef = useRef<Record<string, boolean>>({});

  // Fetch every scope the caller can see and merge into one flat list. Agent
  // scopes are keyed per-agent, so they need one query per agent; the page loads
  // once, so the fan-out stays bounded. Empty/failed scopes contribute nothing.
  const loadVaultEntries = useCallback(
    async (agentList: Agent[]) => {
      setVaultLoading(true);
      try {
        const fetchScope = async (scope: VaultScope, agentID?: string) => {
          try {
            const { data } = await listScopedVaultEntries({
              query: { scope, agent_id: agentID },
              throwOnError: true,
            });
            // SAFETY: listVaultEntries returns VaultEntry items under data.entries.
            return (data?.entries as VaultEntry[]) ?? [];
          } catch {
            return [];
          }
        };
        const jobs = scopeQueriesForBand(
          scopeBand,
          agentList.map((agent) => agent.id),
        ).map(({ scope, agentID }) =>
          // SAFETY: scopeQueriesForBand emits ManagedScope, the same literal union as VaultScope.
          fetchScope(scope as VaultScope, agentID),
        );
        const results = await Promise.all(jobs);
        setVaultEntries(results.flat());
      } finally {
        setVaultLoading(false);
        setVaultLoaded(true);
      }
    },
    [scopeBand],
  );

  // Refetch a single scope (plus agent, for agent-keyed scopes) and splice it
  // back into the flat list. A mutation only changes one slice, so this avoids
  // the full 2N+2 fan-out of loadVaultEntries on every add/delete.
  const reloadScope = useCallback(async (scope: VaultScope, agentID?: string) => {
    let fetched: VaultEntry[] = [];
    try {
      const { data } = await listScopedVaultEntries({
        query: { scope, agent_id: agentID },
        throwOnError: true,
      });
      // SAFETY: listVaultEntries returns VaultEntry items under data.entries.
      fetched = (data?.entries as VaultEntry[]) ?? [];
    } catch {
      fetched = [];
    }
    setVaultEntries((prev) => [
      ...prev.filter((e) => !(e.scope === scope && (agentID ? e.agent_id === agentID : true))),
      ...fetched,
    ]);
  }, []);
  const reloadEmailConfigMetadata = useCallback(() => reloadScope("user"), [reloadScope]);

  const loadAgents = useCallback(async () => {
    try {
      const { data } = await listAgents({
        query: { include_all: true },
        throwOnError: true,
      });
      // SAFETY: listAgents returns Agent items under data.agents.
      const list = (data?.agents as Agent[]) ?? [];
      setAgents(list);
      return list;
    } catch {
      setAgents([]);
      return [];
    }
  }, []);

  const invalidateProviders = useCallback(
    () => queryClient.invalidateQueries({ queryKey: oauthProvidersQueryKey }),
    [queryClient],
  );

  // Admin credentials + scope override for the open sheet only. The form edits
  // live in local state (configValues/scopeDraft), seeded from this query when
  // it loads; the query stays the source of truth for the saved baseline.
  const { data: providerConfig } = useQuery(
    oauthProviderConfigOptions(sheetProvider, !personalSurface && !!sheetProvider),
  );

  useEffect(() => {
    if (!sheetProvider || !providerConfig) return;
    const provider = sheetProvider;
    setConfigValues((prev) => ({
      ...prev,
      [provider]: {
        clientId: providerConfig.client_id,
        clientSecret: "",
        redirectUrl: providerConfig.redirect_url ?? "",
      },
    }));
    setHasExistingSecret((prev) => ({
      ...prev,
      [provider]: providerConfig.client_secret === "***",
    }));
    const saved = providerConfig.scopes ?? [];
    const defaults = providerConfig.default_scopes ?? [];
    // Show every built-in scope, using the saved override as the checked state.
    // Without an override, the built-in defaults start selected.
    setScopeDraft((prev) => ({
      ...prev,
      [provider]: buildOAuthScopeDraft(saved, defaults),
    }));
    setScopeMeta((prev) => ({ ...prev, [provider]: { saved, defaults } }));
  }, [sheetProvider, providerConfig]);

  useEffect(() => {
    const init = async () => {
      const agentList = await loadAgents();
      await loadVaultEntries(agentList);
    };
    void init();
    const pollAbort = pollAbortRef.current;
    return () => {
      for (const key of Object.keys(pollAbort)) {
        pollAbort[key] = true;
      }
    };
  }, [loadVaultEntries, loadAgents]);

  const openEditSheet = useCallback(async (entry: VaultEntry) => {
    setEditingEntry(entry);
    setNewSecretName(entry.name);
    setNewSecretValue("");
    setExistingSecretValue("");
    setFormRange(isAgentVaultScope(entry.scope) ? "specific" : "all");
    setFormAgentID(entry.agent_id ?? "");
    setAddSheetOpen(true);
    try {
      const { data } = await getScopedVaultEntry({
        path: { name: entry.name },
        query: {
          scope: entry.scope,
          agent_id: isAgentVaultScope(entry.scope) ? (entry.agent_id ?? undefined) : undefined,
        },
        throwOnError: true,
      });
      setExistingSecretValue(data?.value ?? "");
    } catch {
      setExistingSecretValue("");
    }
  }, []);

  const addVaultEntry = useCallback(async () => {
    if (!newSecretName) {
      showToast(t("credentials.secretNameRequired"), "error");
      return;
    }
    if (isReservedVaultKey(newSecretName)) {
      showToast(t("credentials.secretNameReserved"), "error");
      return;
    }
    const value = newSecretValue || existingSecretValue;
    if (!value) {
      showToast(t("credentials.secretValueRequired"), "error");
      return;
    }
    // SAFETY: scopeForRange returns ManagedScope, the same literal union as VaultScope.
    const scope = scopeForRange(scopeBand, formRange === "specific") as VaultScope;
    const agentScoped = isAgentVaultScope(scope);
    if (agentScoped && !formAgentID) {
      showToast(t("credentials.scope.agentMissing"), "error");
      return;
    }
    setVaultSaving(true);
    try {
      await setScopedVaultEntry({
        path: { name: newSecretName },
        body: {
          value,
          scope,
          agent_id: agentScoped ? formAgentID : undefined,
        },
        throwOnError: true,
      });
      showToast(t("credentials.secretSaved"));
      setAddSheetOpen(false);
      resetVaultForm();
      await reloadScope(scope, agentScoped ? formAgentID : undefined);
      if (editingEntry && (editingEntry.scope !== scope || editingEntry.agent_id !== formAgentID)) {
        await reloadScope(
          editingEntry.scope,
          isAgentVaultScope(editingEntry.scope) ? (editingEntry.agent_id ?? undefined) : undefined,
        );
      }
    } catch (e) {
      showToast(e instanceof Error ? e.message : t("credentials.secretSaveFailed"), "error");
    } finally {
      setVaultSaving(false);
    }
  }, [
    newSecretName,
    newSecretValue,
    existingSecretValue,
    formRange,
    scopeBand,
    formAgentID,
    editingEntry,
    showToast,
    reloadScope,
    resetVaultForm,
    t,
  ]);

  const deleteVaultEntry = useCallback(
    async (entry: VaultEntry) => {
      try {
        await deleteVaultEntryRequest({
          path: { name: entry.name },
          query: {
            scope: entry.scope,
            agent_id: isAgentVaultScope(entry.scope) ? (entry.agent_id ?? undefined) : undefined,
          },
          throwOnError: true,
        });
        showToast(t("credentials.secretDeleted"));
        await reloadScope(
          entry.scope,
          isAgentVaultScope(entry.scope) ? (entry.agent_id ?? undefined) : undefined,
        );
      } catch (e) {
        showToast(e instanceof Error ? e.message : t("credentials.secretDeleteFailed"), "error");
      }
    },
    [showToast, reloadScope, t],
  );

  const invalidateProviderConfig = useCallback(
    (provider: string) =>
      queryClient.invalidateQueries({
        queryKey: ["oauth-provider-config", provider],
      }),
    [queryClient],
  );

  const pollUntilDone = useCallback(
    async (provider: string, flowID: string) => {
      pollAbortRef.current[provider] = false;
      while (!pollAbortRef.current[provider]) {
        await new Promise((r) => setTimeout(r, 3000));
        if (pollAbortRef.current[provider]) break;
        let status: { state: string; error?: string } | null = null;
        try {
          const { data } = await pollOAuthFlow({
            path: { provider, flowId: flowID },
            throwOnError: true,
          });
          // SAFETY: the OAuth flow status endpoint returns {state, error?}.
          status = data as { state: string; error?: string };
        } catch {
          break;
        }
        if (!status || status.state !== "pending") {
          if (status?.state === "authorized")
            showToast(t("credentials.oauth.connectedSuccess", { provider }));
          else if (status) {
            // Surface the server-provided failure reason inline (and as a toast).
            setOauthFlowError((prev) => ({
              ...prev,
              [provider]:
                status.error ||
                t("credentials.oauth.authorizationState", {
                  provider,
                  state: status.state,
                }),
            }));
            showToast(
              status.error ||
                t("credentials.oauth.authorizationState", {
                  provider,
                  state: status.state,
                }),
              "error",
            );
          }
          break;
        }
      }
    },
    [showToast, t],
  );

  const connectOAuth = useCallback(
    async (provider: string) => {
      setOauthFlowActive((prev) => ({ ...prev, [provider]: true }));
      setOauthFlow((prev) => ({ ...prev, [provider]: null }));
      setOauthFlowError((prev) => ({ ...prev, [provider]: null }));
      try {
        const { data } = await startOAuthFlow({
          path: { provider },
          body: { scopes: [] },
          throwOnError: true,
        });
        // SAFETY: the OAuth flow create returns the flow object under data.
        const flow = data as OAuthFlow;
        setOauthFlow((prev) => ({ ...prev, [provider]: flow }));
        await pollUntilDone(provider, flow.flow_id);
      } catch (e) {
        const msg = e instanceof Error ? e.message : t("credentials.oauth.error");
        setOauthFlowError((prev) => ({ ...prev, [provider]: msg }));
        showToast(msg, "error");
      } finally {
        setOauthFlowActive((prev) => ({ ...prev, [provider]: false }));
        setOauthFlow((prev) => ({ ...prev, [provider]: null }));
        await invalidateProviders();
      }
    },
    [pollUntilDone, showToast, invalidateProviders, t],
  );

  const disconnectOAuth = useCallback(
    async (provider: string) => {
      try {
        await disconnectOAuthRequest({
          path: { provider },
          throwOnError: true,
        });
        showToast(t("credentials.oauth.disconnected", { provider }));
        await invalidateProviders();
      } catch (e) {
        showToast(
          e instanceof Error ? e.message : t("credentials.oauth.disconnectFailed"),
          "error",
        );
      }
    },
    [showToast, invalidateProviders, t],
  );

  const saveProviderConfig = useCallback(
    async (provider: string) => {
      const vals = configValues[provider];
      if (!vals?.clientId) {
        showToast(t("credentials.oauth.clientIdRequired"), "error");
        return;
      }
      const saved = scopeMeta[provider]?.saved ?? [];
      const defaults = scopeMeta[provider]?.defaults ?? [];
      const draft = scopeDraft[provider] ?? buildOAuthScopeDraft(saved, defaults);
      if (draft.length === 0 && defaults.length > 0) {
        showToast(t("credentials.oauth.scopes.emptyOverride"), "error");
        return;
      }
      // The database uses an empty override to mean "all built-in defaults".
      const scopes = sameScopeSet(draft, defaults) ? [] : draft;

      setConfigSaving((prev) => ({ ...prev, [provider]: true }));
      try {
        await setOAuthProviderConfig({
          path: { id: provider },
          body: {
            client_id: vals.clientId,
            client_secret: vals.clientSecret,
            redirect_url: vals.redirectUrl || undefined,
            scopes,
          },
          throwOnError: true,
        });
        showToast(t("credentials.oauth.configSaved", { provider }));
        await Promise.all([invalidateProviders(), invalidateProviderConfig(provider)]);
      } catch (e) {
        showToast(
          e instanceof Error ? e.message : t("credentials.oauth.configSaveFailed"),
          "error",
        );
      } finally {
        setConfigSaving((prev) => ({ ...prev, [provider]: false }));
      }
    },
    [
      configValues,
      scopeDraft,
      scopeMeta,
      showToast,
      invalidateProviders,
      invalidateProviderConfig,
      t,
    ],
  );

  const deleteProviderConfig = useCallback(
    async (provider: string) => {
      try {
        await deleteOAuthProviderConfig({
          path: { id: provider },
          throwOnError: true,
        });
        showToast(t("credentials.oauth.configReset", { provider }));
        await Promise.all([invalidateProviders(), invalidateProviderConfig(provider)]);
      } catch (e) {
        showToast(
          e instanceof Error ? e.message : t("credentials.oauth.configResetFailed"),
          "error",
        );
      }
    },
    [showToast, invalidateProviders, invalidateProviderConfig, t],
  );

  // Destructive actions route through one controlled ConfirmDialog rather than
  // the native browser prompt, so the modal matches the rest of the UI.
  const confirmDeleteVaultEntry = useCallback(
    (entry: VaultEntry) =>
      setConfirm({
        title: t("credentials.deleteSecretTitle"),
        message: t("credentials.deleteSecretConfirm", { name: entry.name }),
        onConfirm: () => void deleteVaultEntry(entry),
      }),
    [deleteVaultEntry, t],
  );
  const confirmDisconnectOAuth = useCallback(
    (provider: string) =>
      setConfirm({
        title: t("credentials.oauth.disconnectTitle"),
        message: t("credentials.oauth.disconnectConfirm", { provider }),
        confirmLabel: t("credentials.oauth.disconnect"),
        onConfirm: () => void disconnectOAuth(provider),
      }),
    [disconnectOAuth, t],
  );
  const confirmResetProviderConfig = useCallback(
    (provider: string) =>
      setConfirm({
        title: t("credentials.oauth.resetTitle"),
        message: t("credentials.oauth.resetConfirm", { provider }),
        confirmLabel: t("credentials.oauth.reset"),
        onConfirm: () => void deleteProviderConfig(provider),
      }),
    [deleteProviderConfig, t],
  );

  const hasStoredEmailConfig = vaultEntries.some(
    (entry) => entry.scope === "user" && entry.name === "EMAIL_CONFIG",
  );
  const filteredVaultEntries = vaultEntries.filter((entry) => entry.name !== "EMAIL_CONFIG");
  const agentName = (id?: string | null) =>
    (id && agents.find((a) => a.id === id)?.name) || id || "";
  const vaultGroups = SCOPE_ORDER.filter((scope) => managedScopes.includes(scope))
    .map((scope) => ({
      scope,
      entries: filteredVaultEntries.filter((e) => e.scope === scope),
    }))
    .filter((g) => g.entries.length > 0);
  // SAFETY: scopeForRange returns ManagedScope, the same literal union as VaultScope.
  const formScope = scopeForRange(scopeBand, formRange === "specific") as VaultScope;
  const editingVault = !!editingEntry;

  const selectScope = (scope: VaultScope) => {
    if (editingVault) return;
    if (!managedScopes.includes(scope)) return;
    setFormRange(isAgentVaultScope(scope) ? "specific" : "all");
  };

  const vaultAddPanel = (
    <DetailPanel>
      <DetailPanelHeader
        title={
          editingVault
            ? t("credentials.editTitle", { name: editingEntry?.name ?? "" })
            : t("credentials.addTitle")
        }
      />

      {/* The precedence ladder IS the scope picker: each row is selectable and
          its position shows where the secret lands in the runtime override order.
          One control replaces a separate picker plus a static legend. */}
      <div className="space-y-3">
        <p className="text-xs font-medium text-muted-foreground">
          {t("credentials.scope.priorityTitle")}
        </p>
        <ul className="space-y-1">
          {SCOPE_PRIORITY.filter((scope) => managedScopes.includes(scope)).map((scope) => {
            const active = scope === formScope;
            return (
              <li key={scope}>
                <button
                  type="button"
                  disabled={editingVault}
                  onClick={() => selectScope(scope)}
                  className={`flex w-full cursor-pointer items-center gap-2.5 rounded-md px-3 py-2 text-left text-sm transition-colors disabled:cursor-not-allowed disabled:opacity-64 ${
                    active ? SCOPE_COLOR[scope].soft : "hover:bg-muted/60"
                  }`}
                >
                  <span className={`size-2.5 shrink-0 rounded-full ${SCOPE_COLOR[scope].dot}`} />
                  <span className={active ? "font-semibold text-foreground" : "text-foreground"}>
                    {t(SCOPE_LABEL_KEY[scope])}
                  </span>
                  {active && (
                    <span className="ml-auto text-xs font-medium text-muted-foreground">
                      {t("credentials.scope.current")}
                    </span>
                  )}
                </button>
              </li>
            );
          })}
        </ul>

        <p className="px-1 text-xs text-muted-foreground">{t(SCOPE_DESC_KEY[formScope])}</p>

        {formRange === "specific" && (
          <Select
            value={formAgentID || null}
            disabled={editingVault}
            onValueChange={onSelectFormAgent}
          >
            <SelectTrigger>
              <SelectValue placeholder={t("credentials.scope.selectAgent")}>
                {(value) =>
                  value ? agents.find((agent) => agent.id === value)?.name || value : null
                }
              </SelectValue>
            </SelectTrigger>
            <SelectPopup>
              {agents.map((agent) => (
                <SelectItem key={agent.id} value={agent.id}>
                  {agent.name || agent.id}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        )}
      </div>

      <div className="space-y-3 border-t border-border pt-4">
        <div className="space-y-1.5">
          <label className="text-xs font-medium text-muted-foreground">
            {t("credentials.secretName")}
          </label>
          <Input
            type="text"
            value={newSecretName}
            onChange={(e) => setNewSecretName(e.target.value)}
            placeholder={t("credentials.secretNamePlaceholder")}
            autoComplete="off"
            disabled={editingVault}
            nativeInput
          />
        </div>
        <div className="space-y-1.5">
          <label className="text-xs font-medium text-muted-foreground">
            {t("credentials.value")}
          </label>
          <Input
            type="password"
            value={newSecretValue}
            onChange={(e) => setNewSecretValue(e.target.value)}
            placeholder={
              editingVault
                ? t("credentials.secretValueKeepExisting")
                : t("credentials.secretValuePlaceholder")
            }
            autoComplete="new-password"
            nativeInput
          />
        </div>
        <div className="flex items-center justify-end gap-2 pt-1">
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              setAddSheetOpen(false);
              resetVaultForm();
            }}
          >
            {t("common.cancel")}
          </Button>
          <Button size="sm" loading={vaultSaving} onClick={addVaultEntry}>
            {editingVault ? t("common.save") : t("credentials.addSecret")}
          </Button>
        </div>
      </div>
    </DetailPanel>
  );

  const sheetProviderData = sheetProvider
    ? oauthProviders.find((p) => p.provider === sheetProvider)
    : undefined;

  function statusBadge(p: OAuthProvider) {
    if (p.connected) {
      // A connected-but-stale credential needs the user to re-authorize.
      if (p.needs_reconnect)
        return (
          <Badge variant="warning" size="sm">
            {t("credentials.oauth.status.reconnect")}
          </Badge>
        );
      return (
        <Badge variant="success" size="sm">
          {t("credentials.oauth.status.connected")}
        </Badge>
      );
    }
    if (!p.available)
      return (
        <Badge variant="warning" size="sm">
          {t("credentials.oauth.status.setupRequired")}
        </Badge>
      );
    if (oauthLoading)
      return (
        <Badge variant="outline" size="sm">
          {t("credentials.oauth.status.checking")}
        </Badge>
      );
    return (
      <Badge variant="secondary" size="sm">
        {t("credentials.oauth.status.ready")}
      </Badge>
    );
  }

  // Scopes the connected token lacks vs. what the connect flow now requests.
  // granted_scopes absent means "unknown" (pre-capture token), so we only claim
  // a concrete gap when the grant is known.
  function missingScopes(p: OAuthProvider): string[] {
    if (!p.granted_scopes) return [];
    const granted = new Set(p.granted_scopes);
    return (p.requested_scopes ?? []).filter((s) => !granted.has(s));
  }

  const sp = sheetProviderData;
  const spConnected = sp?.connected ?? false;
  const spFlow = sp ? oauthFlow[sp.provider] : null;
  const spFlowError = sp ? oauthFlowError[sp.provider] : null;
  const spMissingScopes = sp ? missingScopes(sp) : [];
  const providerSheet = sp ? (
    <DetailPanel>
      <DetailPanelHeader title={sp.provider} subtitle={statusBadge(sp)} />

      {sp.available && !spConnected && (sp.required_by?.length ?? 0) > 0 && (
        <div className="rounded-lg border border-info/36 bg-info/8 p-3 text-xs">
          <p className="font-medium text-foreground">
            {t("credentials.oauth.connectToEnable", {
              tools: sp.required_by?.join(", ") ?? "",
            })}
          </p>
          <p className="mt-1 text-muted-foreground">
            {t("credentials.oauth.unauthenticatedWarning")}
          </p>
        </div>
      )}

      {spConnected && sp.needs_reconnect && (
        <div className="space-y-2 rounded-lg border border-warning/36 bg-warning/8 p-3 text-xs">
          <p className="font-medium text-foreground">
            {sp.reconnect_reason === "missing_scopes" && spMissingScopes.length > 0
              ? t("credentials.oauth.reconnectMissingScopes", {
                  count: spMissingScopes.length,
                })
              : sp.reconnect_reason === "credentials_rotated"
                ? t("credentials.oauth.reconnectRotated")
                : t("credentials.oauth.reconnectGeneric")}
          </p>
          {sp.reconnect_reason === "missing_scopes" && spMissingScopes.length > 0 && (
            <ul className="flex flex-wrap gap-1">
              {spMissingScopes.map((s) => (
                <li key={s}>
                  <Badge variant="warning" size="sm" className="font-mono">
                    {s}
                  </Badge>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {spConnected && (sp.access_expires_at || sp.refresh_expires_at) && (
        <dl className="space-y-1 rounded-lg border border-border bg-muted/40 p-3 text-xs">
          {sp.access_expires_at && (
            <div className="flex items-center justify-between gap-3">
              <dt className="text-muted-foreground">{t("credentials.oauth.accessExpires")}</dt>
              <dd className="font-mono text-foreground">{formatTime(sp.access_expires_at)}</dd>
            </div>
          )}
          {sp.refresh_expires_at && (
            <div className="flex items-center justify-between gap-3">
              <dt className="text-muted-foreground">{t("credentials.oauth.refreshExpires")}</dt>
              <dd className="font-mono text-foreground">{formatTime(sp.refresh_expires_at)}</dd>
            </div>
          )}
        </dl>
      )}

      <div className="flex flex-wrap items-center gap-2">
        {sp.available && !spConnected && (
          <Button
            size="sm"
            loading={oauthFlowActive[sp.provider]}
            onClick={() => connectOAuth(sp.provider)}
          >
            {t("credentials.oauth.connect")}
          </Button>
        )}
        {spConnected && sp.available && sp.needs_reconnect && (
          <Button
            size="sm"
            loading={oauthFlowActive[sp.provider]}
            onClick={() => connectOAuth(sp.provider)}
          >
            {t("credentials.oauth.reconnect")}
          </Button>
        )}
        {spConnected && sp.available && (
          <Button
            size="sm"
            variant="destructive-outline"
            className="text-destructive-foreground hover:bg-destructive/10"
            onClick={() => confirmDisconnectOAuth(sp.provider)}
          >
            {t("credentials.oauth.disconnect")}
          </Button>
        )}
      </div>

      {spFlowError && !spFlow && (
        <div className="rounded-lg border border-destructive/36 bg-destructive/8 p-3 text-xs">
          <p className="font-medium text-destructive-foreground">
            {t("credentials.oauth.flowFailed")}
          </p>
          <p className="mt-1 break-words text-muted-foreground">{spFlowError}</p>
        </div>
      )}

      {spFlow && (
        <div className="rounded-lg border border-info/36 bg-info/8 p-3 text-xs">
          <p className="font-semibold">{t("credentials.oauth.authorizeStella")}</p>
          <a
            href={spFlow.verification_uri}
            target="_blank"
            rel="noreferrer"
            className="mt-1 block break-all font-mono text-xs text-primary underline"
          >
            {spFlow.verification_uri}
          </a>
          {spFlow.user_code && (
            <p className="mt-1 font-medium">
              {t("credentials.oauth.code")}{" "}
              <span className="font-mono font-semibold text-foreground">{spFlow.user_code}</span>
            </p>
          )}
          <p className="mt-1 text-xs text-muted-foreground">
            {t("credentials.oauth.waitingAuthorization")}
          </p>
        </div>
      )}
    </DetailPanel>
  ) : null;

  const selectedSystemProvider = sheetProvider
    ? systemOAuthProviders.find((provider) => provider.provider === sheetProvider)
    : undefined;
  const systemProviderMeta = sheetProvider ? scopeMeta[sheetProvider] : undefined;
  const systemProviderSheet =
    sheetProvider && !personalSurface ? (
      <DetailPanel
        onSave={() => saveProviderConfig(sheetProvider)}
        isSaving={configSaving[sheetProvider]}
        saveLabel={t("common.save")}
        isSavingLabel={t("common.saving")}
        onDelete={
          selectedSystemProvider?.configured
            ? () => confirmResetProviderConfig(sheetProvider)
            : undefined
        }
        deleteLabel={t("credentials.oauth.reset")}
      >
        <DetailPanelHeader title={sheetProvider} />
        <Fieldset className="flex flex-1 min-h-0 flex-col gap-3">
          <FieldsetLegend>{t("credentials.oauth.app")}</FieldsetLegend>
          <Field>
            <FieldLabel>{t("credentials.oauth.clientId")}</FieldLabel>
            <Input
              type="text"
              value={configValues[sheetProvider]?.clientId ?? ""}
              onChange={(e) =>
                setConfigValues((prev) => ({
                  ...prev,
                  [sheetProvider]: {
                    ...prev[sheetProvider],
                    clientId: e.target.value,
                  },
                }))
              }
              placeholder={t("credentials.oauth.clientIdPlaceholder")}
              autoComplete="off"
              nativeInput
            />
          </Field>
          <Field>
            <FieldLabel>{t("credentials.oauth.clientSecret")}</FieldLabel>
            <Input
              type="password"
              value={configValues[sheetProvider]?.clientSecret ?? ""}
              onChange={(e) =>
                setConfigValues((prev) => ({
                  ...prev,
                  [sheetProvider]: {
                    ...prev[sheetProvider],
                    clientSecret: e.target.value,
                  },
                }))
              }
              placeholder={
                hasExistingSecret[sheetProvider]
                  ? t("credentials.oauth.keepExistingSecret")
                  : selectedSystemProvider?.configured
                    ? t("credentials.oauth.configured")
                    : t("credentials.oauth.clientSecretPlaceholder")
              }
              autoComplete="new-password"
              nativeInput
            />
          </Field>
          <Field className="min-h-0 flex-1">
            <ScopeEditor
              value={
                scopeDraft[sheetProvider] ??
                buildOAuthScopeDraft(
                  systemProviderMeta?.saved ?? [],
                  systemProviderMeta?.defaults ?? [],
                )
              }
              defaults={systemProviderMeta?.defaults ?? []}
              onChange={(next) => setScopeDraft((prev) => ({ ...prev, [sheetProvider]: next }))}
            />
            <FieldDescription>{t("credentials.oauth.scopes.saveHint")}</FieldDescription>
          </Field>
        </Fieldset>
      </DetailPanel>
    ) : null;

  return (
    <>
      <SettingsGridPage
        title={t(
          personalSurface ? "settings.nav.connections" : "admin.resources.credentials.title",
        )}
      >
        {personalSurface && (
          <SettingsSection
            icon={<Plug className="size-4" />}
            title={t("credentials.tab.oauth")}
            count={oauthProviders.length}
          >
            {oauthProviders.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("credentials.oauth.noProviders")}</p>
            ) : (
              <SettingsList>
                {oauthProviders.map((p) => {
                  const connected = p.connected;
                  const ready = p.available && !connected;
                  const requiredBy = p.required_by ?? [];
                  const subtitle = !p.configured
                    ? t("credentials.oauth.appNotConfigured")
                    : connected
                      ? t("credentials.oauth.status.connected")
                      : requiredBy.length > 0
                        ? t("credentials.oauth.connectToEnable", {
                            tools: requiredBy.join(", "),
                          })
                        : t("credentials.oauth.status.ready");

                  const menu: RowAction[] = connected
                    ? [
                        {
                          label: t("credentials.oauth.disconnect"),
                          destructive: true,
                          onClick: () => confirmDisconnectOAuth(p.provider),
                        },
                      ]
                    : [];

                  const primary = ready ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      loading={oauthFlowActive[p.provider]}
                      onClick={() => {
                        setSheetProvider(p.provider);
                        void connectOAuth(p.provider);
                      }}
                    >
                      {t("credentials.oauth.connect")}
                    </Button>
                  ) : undefined;

                  return (
                    <SettingsRow
                      key={p.provider}
                      icon={<ProviderIcon icon={p.icon} label={p.provider} />}
                      title={p.provider}
                      subtitle={subtitle}
                      status={statusBadge(p)}
                      primary={primary}
                      menu={menu}
                      onClick={() => setSheetProvider(p.provider)}
                    />
                  );
                })}
              </SettingsList>
            )}
          </SettingsSection>
        )}

        {!personalSurface && (
          <SettingsSection
            icon={<Plug className="size-4" />}
            title={t("admin.resources.credentials.oauthApps")}
            count={systemOAuthProviders.length}
          >
            {systemOAuthProviders.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("credentials.oauth.noProviders")}</p>
            ) : (
              <SettingsList>
                {systemOAuthProviders.map((provider) => (
                  <SettingsRow
                    key={provider.provider}
                    icon={<Plug className="size-4" />}
                    title={provider.provider}
                    subtitle={
                      provider.configured
                        ? t("credentials.oauth.configured")
                        : t("credentials.oauth.appNotConfigured")
                    }
                    primary={
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => setSheetProvider(provider.provider)}
                      >
                        {t(
                          provider.configured
                            ? "credentials.oauth.editApp"
                            : "credentials.oauth.setUp",
                        )}
                      </Button>
                    }
                    onClick={() => setSheetProvider(provider.provider)}
                  />
                ))}
              </SettingsList>
            )}
          </SettingsSection>
        )}

        {personalSurface && (
          <EmailAccountsPanel
            showToast={showToast}
            hasStoredConfig={hasStoredEmailConfig}
            vaultLoaded={vaultLoaded}
            onConfigChanged={reloadEmailConfigMetadata}
          />
        )}

        <SettingsSection
          icon={<KeyRound className="size-4" />}
          title={t(personalSurface ? "credentials.tab.vault" : "admin.resources.credentials.vault")}
          count={filteredVaultEntries.length}
          action={
            <Button variant="ghost" size="xs" onClick={openAddSheet} className="cursor-pointer">
              <Plus className="size-3.5" />
              {t("credentials.addSecret")}
            </Button>
          }
        >
          {vaultLoading && <p className="text-sm text-muted-foreground">{t("common.loading")}</p>}

          <div className="space-y-5">
            {vaultGroups.map((group) => {
              const color = SCOPE_COLOR[group.scope];
              return (
                <div key={group.scope} className="space-y-2">
                  <div className="flex items-center gap-2 px-1">
                    <span className={`size-2 shrink-0 rounded-full ${color.dot}`} />
                    <span className="text-xs font-semibold text-foreground">
                      {t(SCOPE_LABEL_KEY[group.scope])}
                    </span>
                    <span className="text-xs text-muted-foreground">{group.entries.length}</span>
                  </div>
                  <SettingsList>
                    {group.entries.map((entry) => {
                      const reserved = isReservedVaultKey(entry.name);
                      return (
                        <SettingsRow
                          key={`${entry.scope}:${entry.agent_id ?? ""}:${entry.name}`}
                          icon={
                            reserved ? <Lock className="size-4" /> : <KeyRound className="size-4" />
                          }
                          title={<span className="font-mono">{entry.name}</span>}
                          chip={
                            entry.agent_id ? (
                              <Badge variant="outline" size="sm">
                                {agentName(entry.agent_id)}
                              </Badge>
                            ) : reserved ? (
                              <Badge variant="secondary" size="sm">
                                {t("credentials.scope.reserved")}
                              </Badge>
                            ) : undefined
                          }
                          subtitle={t("credentials.updatedCreated", {
                            updated: formatTime(entry.updated_at),
                            created: formatTime(entry.created_at),
                          })}
                          menu={
                            reserved
                              ? []
                              : [
                                  {
                                    label: t("common.edit"),
                                    onClick: () => void openEditSheet(entry),
                                  },
                                  {
                                    label: t("common.delete"),
                                    destructive: true,
                                    onClick: () => confirmDeleteVaultEntry(entry),
                                  },
                                ]
                          }
                          onClick={reserved ? undefined : () => void openEditSheet(entry)}
                        />
                      );
                    })}
                  </SettingsList>
                </div>
              );
            })}
          </div>

          {vaultGroups.length === 0 && !vaultLoading && (
            <p className="py-4 text-center text-sm text-muted-foreground">
              {t("credentials.noSecrets")}
            </p>
          )}
        </SettingsSection>
      </SettingsGridPage>

      <SettingsDetailSheet open={!!sheetProvider} onClose={() => setSheetProvider(null)}>
        {personalSurface ? providerSheet : systemProviderSheet}
      </SettingsDetailSheet>

      <SettingsDetailSheet
        open={addSheetOpen}
        onClose={() => {
          setAddSheetOpen(false);
          resetVaultForm();
        }}
      >
        {vaultAddPanel}
      </SettingsDetailSheet>

      <ConfirmDialog
        open={confirm !== null}
        onOpenChange={(open) => {
          if (!open) setConfirm(null);
        }}
        title={confirm?.title ?? ""}
        message={confirm?.message ?? ""}
        confirmLabel={confirm?.confirmLabel}
        onConfirm={() => confirm?.onConfirm()}
      />
    </>
  );
}

export function PersonalCredentialsPage() {
  return <CredentialsPage scopeBand="personal" />;
}

export function SystemCredentialsPage() {
  return <CredentialsPage scopeBand="system" />;
}
