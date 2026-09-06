import { useMemo, useRef, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { ChevronLeft, Blocks, PackagePlus, Store, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Sheet, SheetPopup } from "@/components/ui/sheet";
import {
  InstallScopeStep,
  type InstallRequest,
  type WritableScope,
} from "@/features/marketplace/InstallScopeStep";
import { MarketGrid } from "@/features/marketplace/MarketGrid";
import { MarketSearch } from "@/features/marketplace/MarketSearch";
import {
  BearerSecretStep,
  RegistryCard,
  RegistryDetailBody,
  RegistryRepositoryLink,
} from "@/features/mcp/McpRegistryCard";
import {
  buildInstallRequest,
  registryPluginNamespace,
  useMcpMarketInstall,
} from "./useMcpMarketInstall";
import { mcpRegistryInfiniteQueryOptions } from "@/lib/queries/mcp";
import { useI18n } from "@/lib/i18n";
import { cn } from "@/lib/utils";

type Notify = (message: string, kind?: "success" | "error") => void;
type Mode = "market" | "manual";

const MODE_META = {
  market: { icon: Store, key: "sessions.skillsList.market" },
  manual: { icon: PackagePlus, key: "sessions.skillsList.manualTab" },
} as const;

const tabPillCls = (active: boolean) =>
  cn(
    "inline-flex h-8 shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-3 text-sm font-medium transition-colors",
    active
      ? "bg-accent text-accent-foreground"
      : "text-muted-foreground hover:bg-muted/50 hover:text-foreground",
  );

/**
 * Add-an-MCP-server surface: a Marketplace tab (registry search, detail, scoped
 * install with bearer secret capture, Connect offer for OAuth-protected
 * servers) and a Manual tab rendered from the `manual` slot so the host page
 * keeps its own scope-band form logic. `onRequestManual` hands an
 * `unsupported` entry to the host's manual form prefilled with its identity.
 */
export function McpInstallSheet({
  open,
  onOpenChange,
  notify,
  defaultScope = "user_agent",
  agentId,
  isAdmin = false,
  manual,
  onRequestManual,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  notify: Notify;
  defaultScope?: WritableScope;
  /** Agent the agent scopes apply to; absent on the global MCP page. */
  agentId?: string;
  isAdmin?: boolean;
  manual?: React.ReactNode;
  onRequestManual?: (prefill: { name: string; namespace: string; url: string }) => void;
}) {
  const { t } = useI18n();
  const [mode, setMode] = useState<Mode>("market");
  const [query, setQuery] = useState("");
  const [debounced, setDebounced] = useState("");
  const [detailId, setDetailId] = useState<string | null>(null);
  const [pending, setPending] = useState<InstallRequest<WritableScope> | null>(null);
  const [secretFor, setSecretFor] = useState<string | null>(null);
  const [secret, setSecret] = useState("");
  const contentRef = useRef<HTMLDivElement>(null);
  const sentinelRef = useRef<HTMLDivElement>(null);

  // Destinations this surface can write: agent scopes need an agent in
  // context, system scopes need an admin.
  const scopes = useMemo<WritableScope[]>(
    () =>
      (["user", "user_agent", "system", "system_agent"] as const).filter(
        (s) => (agentId || !s.endsWith("_agent")) && (isAdmin || !s.startsWith("system")),
      ),
    [agentId, isAdmin],
  );

  const marketActive = open && mode === "market";
  const market = useInfiniteQuery({
    ...mcpRegistryInfiniteQueryOptions(debounced),
    enabled: marketActive,
  });
  const rows = useMemo(
    () => (market.data?.pages ?? []).flatMap((page) => page.servers ?? []),
    [market.data?.pages],
  );
  const detail = detailId ? rows.find((r) => r.id === detailId) : undefined;
  const { mutation, created, setCreated } = useMcpMarketInstall(notify, t);
  function requestInstall() {
    if (!detail) return;
    if (detail.auth === "unsupported") {
      onRequestManual?.({
        name: detail.name,
        namespace: registryPluginNamespace(detail.id),
        url: detail.url,
      });
      onOpenChange(false);
      return;
    }
    if (detail.auth === "bearer" && !secret.trim()) {
      setSecretFor(detail.id);
      return;
    }
    setPending(
      buildInstallRequest(detail, mutation.mutateAsync, t("common.install"), agentId, secret),
    );
  }

  function close() {
    setPending(null);
    setDetailId(null);
    setSecretFor(null);
    setCreated(null);
    onOpenChange(false);
  }

  return (
    <Sheet open={open} onOpenChange={(next) => (next ? onOpenChange(true) : close())}>
      <SheetPopup
        side="right"
        showCloseButton={false}
        className="w-full sm:w-[560px] sm:max-w-[560px]"
      >
        <div className="relative flex h-full min-h-0 flex-col">
          {created ? (
            <div className="flex h-full flex-col items-start gap-4 p-6">
              <h2 className="text-base font-semibold">{t("mcp.market.installed")}</h2>
              <p className="text-sm text-muted-foreground">
                {created.backend_summary.backend === "mcp" ? created.backend_summary.auth_type : ""}
              </p>
              <div className="mt-auto flex w-full items-center justify-end gap-2">
                <Button variant="ghost" onClick={close}>
                  {t("common.cancel")}
                </Button>
              </div>
            </div>
          ) : detail ? (
            <>
              <div className="flex items-start gap-3 border-b p-5">
                <Button
                  size="icon-sm"
                  variant="ghost"
                  aria-label={t("common.back")}
                  onClick={() => setDetailId(null)}
                >
                  <ChevronLeft size={16} />
                </Button>
                <div className="min-w-0 flex-1">
                  <h2 className="truncate font-mono text-base font-semibold">{detail.name}</h2>
                  <div className="mt-2">
                    <RegistryDetailBody server={detail} />
                  </div>
                </div>
              </div>
              <div className="mt-auto flex items-center justify-between gap-2 border-t p-4">
                <RegistryRepositoryLink server={detail} />
                <Button onClick={requestInstall}>{t("common.install")}</Button>
              </div>
            </>
          ) : (
            <>
              <div className="flex items-center gap-3 border-b p-5">
                <h2 className="min-w-0 flex-1 truncate text-base font-semibold">
                  {t("mcp.market.title")}
                </h2>
                <Button
                  size="icon-sm"
                  variant="ghost"
                  aria-label={t("common.close")}
                  onClick={close}
                >
                  <X size={16} />
                </Button>
              </div>
              <div className="flex flex-col gap-2.5 border-b p-4">
                <div className="flex flex-wrap items-center gap-1">
                  {(["market", "manual"] as const).map((m) => {
                    const Icon = MODE_META[m].icon;
                    return (
                      <button
                        key={m}
                        type="button"
                        aria-pressed={mode === m}
                        onClick={() => setMode(m)}
                        className={tabPillCls(mode === m)}
                      >
                        <Icon className="size-4" />
                        {t(MODE_META[m].key)}
                      </button>
                    );
                  })}
                </div>
                {mode === "market" && (
                  <MarketSearch
                    value={query}
                    onValueChange={setQuery}
                    onDebounce={setDebounced}
                    placeholder={t("mcp.market.searchPlaceholder")}
                  />
                )}
              </div>
              <div ref={contentRef} className="min-h-0 flex-1 overflow-y-auto p-4">
                {mode === "market" ? (
                  <MarketGrid
                    isLoading={market.isLoading}
                    isError={market.isError}
                    isFetchingNextPage={market.isFetchingNextPage}
                    isFetchNextPageError={market.isFetchNextPageError}
                    hasNextPage={market.hasNextPage}
                    rows={rows}
                    sentinelRef={sentinelRef}
                    renderItem={(srv) => (
                      <RegistryCard
                        key={`${srv.source}:${srv.id}:${srv.version ?? ""}`}
                        server={srv}
                        installing={false}
                        installDisabled={false}
                        onOpen={() => setDetailId(srv.id)}
                        onInstall={() => {
                          setDetailId(srv.id);
                        }}
                      />
                    )}
                    onRetry={() =>
                      void (market.isFetchNextPageError ? market.fetchNextPage() : market.refetch())
                    }
                    emptyIcon={<Blocks />}
                    emptyTitleKey="mcp.market.emptyTitle"
                    emptyDescriptionKey="mcp.market.empty"
                  />
                ) : (
                  <div className="space-y-4">{manual}</div>
                )}
              </div>
            </>
          )}
          {secretFor && (
            <div className="absolute inset-0 flex flex-col justify-center gap-4 bg-background p-6">
              <BearerSecretStep
                server={rows.find((r) => r.id === secretFor) ?? detail!}
                value={secret}
                onChange={setSecret}
              />
              <div className="flex items-center justify-end gap-2">
                <Button variant="ghost" onClick={() => setSecretFor(null)}>
                  {t("common.cancel")}
                </Button>
                <Button
                  onClick={() => {
                    const target = rows.find((r) => r.id === secretFor);
                    setSecretFor(null);
                    if (target)
                      setPending(
                        buildInstallRequest(
                          target,
                          mutation.mutateAsync,
                          t("common.install"),
                          agentId,
                          secret,
                        ),
                      );
                  }}
                >
                  {t("common.next")}
                </Button>
              </div>
            </div>
          )}
          {pending && (
            <InstallScopeStep
              request={pending}
              defaultScope={defaultScope}
              showAgentScope={isAdmin}
              scopes={scopes}
              onConfirmed={() => {
                setPending(null);
                setCreated(null);
                onOpenChange(false);
              }}
              onCancel={() => setPending(null)}
            />
          )}
        </div>
      </SheetPopup>
    </Sheet>
  );
}
