package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	ucli "github.com/urfave/cli/v2"

	"github.com/CherryHQ/stella/internal/agent"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/settingspolicy"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/core/providercred"
	"github.com/CherryHQ/stella/pkg/toolmeta"

	cfgstore "github.com/CherryHQ/stella/cmd/stellad/store"
	sessionaccess "github.com/CherryHQ/stella/internal/agent/session/access"
	sessioninbox "github.com/CherryHQ/stella/internal/agent/session/inbox"
	"github.com/CherryHQ/stella/internal/agent/tracehook"
	"github.com/CherryHQ/stella/internal/asset"
	"github.com/CherryHQ/stella/internal/channel"
	"github.com/CherryHQ/stella/internal/connections"
	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/controlplane"
	"github.com/CherryHQ/stella/internal/credential"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/goal"
	"github.com/CherryHQ/stella/internal/library"
	"github.com/CherryHQ/stella/internal/library/recally"
	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/model/embedding"
	"github.com/CherryHQ/stella/internal/model/usage"
	"github.com/CherryHQ/stella/internal/notify"
	"github.com/CherryHQ/stella/internal/platform/blob"
	"github.com/CherryHQ/stella/internal/platform/cli"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/platform/observability"
	"github.com/CherryHQ/stella/internal/platform/observability/metrichook"
	"github.com/CherryHQ/stella/internal/platform/version"
	"github.com/CherryHQ/stella/internal/plugin"
	pluginhost "github.com/CherryHQ/stella/internal/plugin/host"
	"github.com/CherryHQ/stella/internal/reflect"
	"github.com/CherryHQ/stella/internal/scheduler"
	"github.com/CherryHQ/stella/internal/sessionmedia"
	sharepkg "github.com/CherryHQ/stella/internal/share"
	"github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/skill/access"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/internal/vision"
	"github.com/CherryHQ/stella/internal/webhook"
	workflowpkg "github.com/CherryHQ/stella/internal/workflow"
	coreagent "github.com/CherryHQ/stella/pkg/agent"
	"github.com/CherryHQ/stella/pkg/hooks"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
	"github.com/CherryHQ/stella/plugins/core"
	"github.com/CherryHQ/stella/plugins/email"
	"github.com/CherryHQ/stella/resources"
	"github.com/CherryHQ/stella/resources/binaries"
)

func newApp() *ucli.App {
	return &ucli.App{
		Name:  "stellad",
		Usage: "Stella daemon — server, services, and system management",
		Description: `Stellad is the server component of Stella. Run "stellad server" to start
the server, or use "stellad service" to manage it as a background service.`,
		Version: version.DisplayVersion(),
		Commands: []*ucli.Command{
			serverCommand(),
			versionCommand(),
			upgradeCommand(),
			postgresCommand(),
			vaultCommand(),
			systemBundleCommand(),
			serviceCommand(),
		},
	}
}

type setupResult struct {
	ctx context.Context
	// cfg is the parsed boot-time server config, carried so runServer reads the
	// injected values (base URL, vault key, lifecycle, OIDC) instead of the
	// environment. A secret it holds (Vault.Key) must never be logged.
	cfg      config.ServerConfig
	db       *pgxpool.Pool
	embedded *appdb.Embedded
	mem      memory.Provider
	store    config.Store
	// snapshotLoader is the credential-aware Snapshot loader wrapping store: every
	// runtime consumer that resolves per-Agent Provider credentials reads through
	// it so Agent key overrides apply, while writes/other reads use store directly.
	snapshotLoader config.SnapshotLoader
	// credentialSvc is the Agent Provider credential write/encryption boundary.
	credentialSvc *providercred.Service
	// credentialProviders exposes canonical Provider IDs only; unlike the general
	// config Store it cannot reveal deployment-global Provider keys.
	credentialProviders    agentaccess.ProviderReader
	authStore              *appdb.AuthStore
	agentAccess            *agentaccess.Service
	agentManagement        *agentaccess.Management
	projectStore           *agent.ProjectStore
	sessionAccess          *sessionaccess.Service
	skillAccess            *access.Service
	pluginHost             *pluginhost.Host
	pluginService          *plugin.Service
	providerRegistry       *providers.Registry
	channelRuntimeServices *pluginhost.ChannelPlatform
	poolManager            *agent.PoolManager
	schedulerSvc           *scheduler.Service
	goalSvc                *goal.Service
	vaultSvc               *vault.Service
	mcpSvc                 *mcp.Service
	controlPlane           *controlplane.Service
	webhooks               *webhook.Service
	credSvc                *connections.Service
	emailSvc               *email.Service
	shareSvc               *sharepkg.Service
	recallySvc             *recally.Service
	assetStore             *asset.Store
	workspaceManager       *home.WorkspaceManager
	homeDeletion           *home.OwnerDeletion
	workflowSvc            *workflowpkg.Service
	embeddingSvc           *embedding.Service
	librarySvc             *library.Service
	groupNudgeWorker       *channel.GroupNudgeWorker
	riverClient            *river.Client[pgx.Tx]
	builtinTools           []agent.BuiltinTool
	toolMeta               *toolmeta.Registry
	notifier               *notify.Dispatcher
	skillStore             *skill.POSIXStore
	sessionImages          *sessionmedia.Pipeline
	cliUserID              int64
	oauthRegistry          *oauth.ProviderRegistry
	backgroundTasks        *sync.WaitGroup
	metricHook             *metrichook.Hook
}

// setup builds every subsystem. baseURL is the final public URL resolved once at
// the startup boundary; the shared credentials/share services are constructed
// with it directly, so no service is built with a localhost placeholder and
// mutated later.
func setup(parent context.Context, cfg config.ServerConfig, baseURL string) (*setupResult, error) {
	dsn := cfg.Database.URL
	var embedded *appdb.Embedded
	if dsn == "" {
		if cfg.Database.RequireExternalDB {
			// Set by the Docker image (and k8s manifests): in a container the
			// embedded cluster lands on an ephemeral filesystem, and with multiple
			// replicas each process would create its own database — refuse it
			// rather than silently starting one.
			return nil, errors.New("STELLA_REQUIRE_EXTERNAL_DB=1 but STELLA_DATABASE_URL is not set: embedded PostgreSQL is for single-node local use; point STELLA_DATABASE_URL at an external PostgreSQL with pgvector + pg_search (or set STELLA_REQUIRE_EXTERNAL_DB=0 to deliberately run embedded PostgreSQL on a persistent volume)")
		}
		// Zero-config default: no external DSN, so run a managed PostgreSQL whose
		// cluster lives under the stella home and persists across restarts.
		emb, err := appdb.StartEmbedded(filepath.Join(config.StellaHome(), "postgres"), 0)
		if err != nil {
			return nil, fmt.Errorf("start embedded postgres: %w", err)
		}
		embedded = emb
		dsn = emb.DSN()
	}
	// Stop the embedded server if setup fails partway. Ownership transfers to the
	// returned setupResult on the success path (which clears this local), where
	// shutdown stops it instead.
	defer func() {
		if embedded != nil {
			_ = embedded.Stop()
		}
	}()

	db, err := appdb.OpenDB(dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	store := cfgstore.NewDBStore(db)
	dispatcher := notify.NewDispatcher()
	dispatcher.SetChannelStore(store)
	ps, err := setupPlugins(parent, db, store, dispatcher)
	if err != nil {
		return nil, err
	}
	phost := ps.host
	// Construct the Agent PEP at the composition root before any agent Service is
	// built. HTTP, channels, and durable workers all share its direct decisions.
	authStore := appdb.NewAuthStore(db)
	// Every authorization domain owns its own static rules and loads durable facts
	// before deciding; the Agent domain is the shared read gate the others fold in.
	agentAccess := agentaccess.NewService(store, authStore, agentaccess.WithGuestPolicyDecoder(phost.GuestPolicyResolver))
	var poolMgr *agent.PoolManager
	pluginSvc := plugin.NewService(db, agentAccess, ps.catalog, pluginBackendPolicy(cfg.MCP.AllowPrivateEndpoints), func(ctx context.Context, mutate func() error) error {
		// Startup has no admitted runners or listeners yet.
		if poolMgr == nil {
			return mutate()
		}
		err := poolMgr.ApplyPluginMutation(ctx, mutate)
		if err == nil || errors.Is(err, plugin.ErrCommitOutcomeUnknown) {
			// Event admission already reads the committed cap. Reconcile listeners
			// after releasing the admission fence, without holding a database tx.
			reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if reconcileErr := phost.ReconcileChannels(reconcileCtx); reconcileErr != nil {
				slog.Error("reconcile channels after committed plugin change", "error", reconcileErr)
			}
		}
		return err
	})
	if err := plugin.ImportLegacyState(parent, db, ps.catalog, newToolMetaRegistry(generatedFamilies()...)); err != nil && !errors.Is(err, plugin.ErrImportComplete) {
		return nil, fmt.Errorf("import plugin configuration: %w", err)
	}
	if err := pluginSvc.SyncBuiltinDefaults(parent); err != nil {
		return nil, fmt.Errorf("sync builtin plugins: %w", err)
	}
	phost.SetListenerCap(pluginSvc.AdministrativeCap)
	backgroundCapabilityGate := pluginBackgroundGate(pluginSvc)
	pluginContextBuilder := func(ctx context.Context, authority authz.Authority, agentID string) (agent.PluginContext, error) {
		snapshot, err := pluginSvc.ResolveSnapshot(ctx, authority, agentID)
		if err != nil {
			return agent.PluginContext{}, err
		}
		view, err := phost.SessionPluginView(snapshot)
		if err != nil {
			return agent.PluginContext{}, err
		}
		return agentruntime.NewPluginContext(snapshot, view), nil
	}

	// One process-wide manager is the sole materializer beneath STELLA_HOME.
	homeRegistry, err := home.NewWorkspaceManager(db, config.StellaHome())
	if err != nil {
		return nil, fmt.Errorf("build workspace manager: %w", err)
	}
	workspaceManagerOwned := true
	defer func() {
		if workspaceManagerOwned {
			_ = homeRegistry.Close()
		}
	}()
	skillStore, err := setupSkillStore(db, homeRegistry)
	if err != nil {
		return nil, fmt.Errorf("build Skill store: %w", err)
	}
	skillMigrator, err := skill.NewSkillHomeMigratorFromStore(db, skillStore)
	if err != nil {
		return nil, fmt.Errorf("build Skill migration reconciler: %w", err)
	}
	if err := ensureEmbeddedAssets(); err != nil {
		return nil, err
	}
	var coreRuntimePlan *core.RuntimePlan
	if config.ActiveSandboxBackend() != config.SandboxBackendDocker {
		plan, err := core.Prepare(parent, config.StellaHome())
		if err != nil {
			return nil, fmt.Errorf("prepare core runtimes: %w", err)
		}
		coreRuntimePlan = &plan
	}
	blobStore, err := blob.NewStoreFromConfig(cfg.Blob)
	if err != nil {
		return nil, err
	}
	assetStore, err := asset.NewStore(config.StellaHome(), blobStore, slog.Default())
	if err != nil {
		return nil, err
	}

	// The Skill domain shares the Agent read gate with the other execution
	// domains and reads the same authoritative PostgreSQL rows as the transports.
	skillAccess := access.NewService(skillStore, agentAccess)
	// Managed Skill CRUD is shared by HTTP and the Stella-only tool adapter;
	// both resolve scope and owner through the same PEP.
	skillManagement := skill.NewManagement(skillStore, skillAccess)

	// Bind account enrollment after Vault initialization and before host Seal.
	// Catalog construction needs no runtime backing services.
	// It depends only on db + key + the Agent PEP, all ready at this point.
	var vaultSvc *vault.Service
	if vaultKey := cfg.Vault.Key; vaultKey != "" {
		vaultSvc, err = vault.NewServiceForPool(db, vaultKey, agentAccess)
		if err != nil {
			slog.Warn("vault service init failed; vault endpoints and vault tool will be unavailable", "error", err)
			vaultSvc = nil
		}
	}
	enrollment := auth.NewAccountEnrollmentService(appdb.NewOIDCStore(db), func() *age.X25519Recipient {
		if vaultSvc == nil {
			return nil
		}
		return vaultSvc.MasterRecipient()
	}())

	phost.SetAccountEnrollment(enrollment)
	providerRegistry, err := setupProviderRegistry()
	if err != nil {
		return nil, fmt.Errorf("build provider registry: %w", err)
	}
	sandboxBackends, err := setupSandboxBackends()
	if err != nil {
		return nil, fmt.Errorf("build sandbox backend registry: %w", err)
	}

	schedulerSvc, err := setupScheduler(db, agentAccess, backgroundCapabilityGate)
	if err != nil {
		return nil, err
	}
	for _, tmpl := range []scheduler.JobTemplate{scheduler.RecallyDigestTemplate, scheduler.RecallyRSSTemplate} {
		if err := schedulerSvc.RegisterTemplate(tmpl); err != nil {
			return nil, fmt.Errorf("register template %q: %w", tmpl.Key, err)
		}
	}

	providerStreamBuilder := func(api, apiKey, baseURL string) (providers.StreamFunc, error) {
		return providerRegistry.BuildStream(api, providers.Config{APIKey: apiKey, BaseURL: baseURL})
	}

	// The credential cipher must be a true nil interface when the vault is absent —
	// a typed-nil *vault.Service would read as a non-nil SecretCipher and defeat the
	// loader's nil-cipher fail-closed guard.
	var credentialCipher providercred.SecretCipher
	if vaultSvc != nil {
		credentialCipher = vaultSvc
	}
	credentialSvc := providercred.NewService(store, credentialCipher)
	// The loader is always wired (even without a cipher) so a referenced override
	// after a key drop fails closed instead of silently serving the global key;
	// with no overrides present it returns the base Snapshot unchanged.
	snapshotLoader := config.SnapshotLoader(providercred.NewCredentialLoader(store, store, credentialCipher))

	// Semantic-search lane (config-driven via the web settings page). Always built:
	// it reads its config from the DB at runtime and idles when disabled. Built
	// before the memory provider so its query embedder can be injected, and before
	// the shared River client so its backfill worker joins the single electable client.
	embeddingSvc := setupEmbedding(db, store, slog.With("component", "embedding"))

	memProvider, err := setupMemoryProvider(parent, db, snapshotLoader, providerStreamBuilder, embeddingSvc)
	if err != nil {
		return nil, fmt.Errorf("memory provider: %w", err)
	}

	memProvider = wrapMemoryWithTracing(memProvider, &poolMgr)
	if _, ok := memory.Unwrap(memProvider).(memory.InboxAppender); !ok {
		return nil, errors.New("memory provider does not support durable Session inbox")
	}
	inboxAppender, ok := memProvider.(memory.InboxAppender)
	if !ok {
		return nil, errors.New("memory tracing wrapper does not forward durable Session inbox")
	}
	sessionInbox := sessioninbox.New(db)

	pluginToolsBuilder := phost.BuildEnabledTools
	// Immutable library raw content may use the configured BlobStore independently
	// of mutable Home files.
	libraryRaw, err := library.NewRawStoreFromConfig(config.StellaHome(), cfg.Blob, library.RawStoreOptions{
		TempDir:        os.TempDir(),
		FSMinFreeBytes: library.DefaultFSMinFreeBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("build library RawStore: %w", err)
	}
	textParser := library.NewTextParser()
	parserRoutes := map[string]library.Parser{
		library.MediaTypeText: textParser, library.MediaTypeMarkdown: textParser,
	}
	if xbergBinary := binaries.ToolPath(config.StellaHome(), "xberg"); xbergBinary != "" {
		xbergParser, probeErr := library.NewXbergCLIParser(parent, xbergBinary)
		if probeErr != nil {
			return nil, fmt.Errorf("start embedded Xberg parser: %w", probeErr)
		}
		for _, mediaType := range library.XbergMediaTypes() {
			parserRoutes[mediaType] = xbergParser
		}
		slog.Info("document extraction enabled", "runtime", xbergBinary,
			"media_types", len(library.XbergMediaTypes()))
	} else {
		// Not fatal: Windows ships no Xberg by design. Uploads of these formats
		// already fail closed at the API boundary, but nothing in the logs said
		// why, so an operator had no way to tell a deliberate build from a broken
		// install.
		slog.Warn("document extraction unavailable: no Xberg runtime installed",
			"bin_dir", binaries.BinDir(config.StellaHome()),
			"affected_media_types", library.XbergMediaTypes(),
			"remedy", "run `stellad system-bundle install`")
	}
	libraryParser, err := library.NewRoutingParser(parserRoutes)
	if err != nil {
		return nil, fmt.Errorf("build Library parser routes: %w", err)
	}
	librarySvc, err := library.NewService(library.ServiceConfig{
		DB:                   db,
		RawStore:             libraryRaw,
		Parser:               libraryParser,
		Logger:               slog.With("component", "library"),
		TempDir:              os.TempDir(),
		MaxConcurrentUploads: 4,
		MaxSpoolBytes:        4 * library.MaxFileBytes,
		AgentAccess:          agentAccess,
	})
	if err != nil {
		return nil, fmt.Errorf("build library service: %w", err)
	}
	sessionImages, err := sessionmedia.NewPipeline(assetStore.SessionMedia(), db, snapshotLoader, vision.StreamBuilder(providerStreamBuilder), sessionmedia.PipelineOptions{})
	if err != nil {
		return nil, fmt.Errorf("build session image pipeline: %w", err)
	}
	projectStore := agent.NewProjectStore(db, agentAccess, agent.WithProjectHomeWorkspace(homeRegistry))
	systemPromptBuilder, err := sessionaccess.NewSystemPromptBuilder(sessionaccess.SystemPromptDeps{
		Memory:                memProvider,
		Agents:                sessionaccess.ConfigAgentSystemPrompt(store),
		Projects:              projectStore.Resolve,
		Workspace:             homeRegistry,
		PluginContextBuilder:  pluginContextBuilder,
		PromptSectionsBuilder: phost.SystemPromptSections,
		SandboxBackendFn:      func(context.Context) string { return config.ActiveSandboxBackend() },
		Skills: func(ctx context.Context, build pkgplugins.SystemPromptContext, project *skill.ProjectSnapshot) (pkgplugins.SystemPromptSection, error) {
			return skill.BuildAuthorizedPromptSection(ctx, build, project, skillStore, skillAccess)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build session prompt service: %w", err)
	}
	usageHook := usage.New(db)
	sessionAccess, err := sessionaccess.NewService(memProvider, db, store, assetStore, agentAccess, sessionaccess.WithSystemPromptBuilder(systemPromptBuilder), sessionaccess.WithHomeWorkspace(homeRegistry), sessionaccess.WithUsageProgress(usageHook))
	if err != nil {
		return nil, fmt.Errorf("build session/workspace service: %w", err)
	}
	groupRecall, ok := memory.Unwrap(memProvider).(memory.GroupRecallSource)
	if !ok {
		return nil, fmt.Errorf("memory provider does not implement group recall")
	}
	if err := registerReflectBuiltin(schedulerSvc, reflect.Config{
		CapabilityGate:    backgroundCapabilityGate,
		Memory:            memProvider,
		Store:             store,
		Snapshots:         snapshotLoader,
		SkillStore:        skillStore,
		SkillAuthorizer:   skillAccess,
		UsageCuratorStore: reflect.NewSQLUsageCuratorStoreForPool(db),
		StateStore:        pluginhost.NewScopedStateStore(phost.StateStore(), "reflect"),
		Providers:         providerStreamBuilder,
		Services:          &lazyServiceManager{get: func() agent.ServiceManager { return poolMgr }},
	}, cfg.Reflect.Interval, cfg.Reflect.CuratorMode); err != nil {
		return nil, err
	}
	if err := registerChannelGuestRetentionBuiltin(schedulerSvc, channel.NewGuestRetention(db)); err != nil {
		return nil, err
	}

	pluginHooksBuilder := func(ctx context.Context, snapshot plugin.Snapshot) ([]hooks.HookPlugin, error) {
		return phost.BuildEnabledHooks(ctx, binaries.BinDir(config.StellaHome()), snapshot)
	}

	// The trace hook is server-level infrastructure, not a user-managed plugin:
	// it always runs and shares its enabled flag with the global tracer provider
	// (both derive from observability.LoadConfig) so there is a single source of
	// truth for whether OTel export is active. It is registered as a core hook so
	// plugin reloads never rebuild or close it out from under in-flight runners.
	// Its constructor starts no goroutine (#708 D); the composition root starts
	// its idle-session reaper here, bound to the daemon lifecycle context.
	toolMetaRegistry := newToolMetaRegistry(generatedFamilies()...)
	traceHook := tracehook.New(observability.LoadConfig().Enabled, cfg.Observability.RecordToolIO,
		tracehook.WithToolMeta(toolMetaRegistry))
	traceHook.Start(parent)
	metricHook := metrichook.New(usageHook, traceHook.ActiveSessions, func(name string) bool {
		_, ok := toolMetaRegistry.Lookup(name)
		return ok
	})
	usageHook.SetDropObserver(metricHook.RecordQueueDrop)
	usageHook.Start()
	coreHooks := []hooks.HookPlugin{traceHook, usageHook, metricHook}

	toolLifecycleBuilder := func(ctx context.Context, snapshot plugin.Snapshot) (*coreagent.ToolLifecycle, error) {
		return buildToolLifecycle(phost, snapshot), nil
	}

	// A goal worker must not reach the orchestration surface that scheduled it:
	// the list is derived from the families themselves, so a new action is
	// excluded the moment toolgen emits it.
	workerExcludedTools := append(
		splitFamilyNames(goal.ActionTools(), scheduler.ActionTools(), workflowpkg.ActionTools()),
		settingspolicy.ToolNames()...,
	)
	goalSvc, err := goal.Boot(goal.BootConfig{
		DB:            db,
		Services:      &lazyServiceManager{get: func() agent.ServiceManager { return poolMgr }},
		ExcludedTools: workerExcludedTools,
		AgentAccess:   agentAccess,
		Capabilities: goal.CapabilityProbeFunc(func() bool {
			return config.ActiveSandboxBackend() != config.SandboxBackendNone
		}),
		Chat: func(ctx context.Context, p goal.TaskChatParams) <-chan agent.Event {
			var svc *agent.Service
			if poolMgr != nil {
				svc = poolMgr.GetService(p.AgentID)
				if svc == nil {
					svc = poolMgr.Default()
				}
			}
			if svc == nil {
				out := make(chan agent.Event, 1)
				out <- agent.Event{Err: fmt.Errorf("no agent service for %s", p.AgentID)}
				close(out)
				return out
			}
			chatReq := agent.TaskChatRequest{
				SessionID:        p.SessionID,
				UserID:           p.UserID,
				AgentID:          p.AgentID,
				ProjectID:        p.ProjectID,
				Message:          p.Prompt,
				ExtraTools:       p.ExtraTools,
				ExcludedTools:    p.ExcludedTools,
				OnSandboxSession: p.OnSandboxSession,
				Authority:        p.Authority,
			}
			// Decomposition runs on the goal's KindDelegate planning session;
			// execution on the KindTask worker session. They resolve differently.
			if p.Decompose {
				return svc.ChatForGoalDecomposition(ctx, chatReq)
			}
			return svc.ChatForTask(ctx, chatReq)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("boot goal service: %w", err)
	}

	workflowSvc := workflowpkg.New(db, goalSvc.WorkflowWriter(), agentAccess)
	schedulerSvc.SetWorkflowRunner(schedulerWorkflowAdapter{svc: workflowSvc})

	// Build the shared credentials/email/share services once, with the final
	// resolved base URL — no localhost placeholder to mutate later. Each domain
	// owns its own sqlc query set (the *ForPool constructors), so the composition
	// root passes only the pool. These same instances back both the agent tools
	// (below) and the HTTP endpoints (via server.Deps).
	credSvc := connections.NewServiceForPool(vaultSvc, db, oauth.NewFlowStore(), baseURL)
	emailSvc := email.NewServiceForPool(pluginhost.ResolveEmailUser, emailConfigReader(vaultSvc), db)
	if ps.oauthRegistry != nil {
		credSvc.SetRegistry(ps.oauthRegistry)
		if vaultSvc != nil {
			vaultSvc.AddSystemManagedNames(ps.oauthRegistry.VaultKeys()...)
		}
	}
	recallyStore := recally.NewStore(db)
	recallySvc := recally.NewService(recallyStore, config.StellaHome())
	shareSvc := sharepkg.NewServiceForPool(db, memProvider, recallyStore, config.StellaHome(), baseURL, sharepkg.WithHomeWorkspace(homeRegistry), sharepkg.WithAgentAccess(agentAccess))

	// MCP registration service: one instance shared by the HTTP API and the agent
	// runtime. Built here (before StartAll) so its tool provider can be bound into
	// the pool as a static capability rather than injected after agents start.
	var mcpVault mcp.Vault
	var bindMCPVault func(pgx.Tx) mcp.Vault
	if vaultSvc != nil {
		mcpVault = vaultSvc
		bindMCPVault = func(tx pgx.Tx) mcp.Vault { return vaultSvc.WithTx(tx) }
	}
	mcpSvc := mcp.NewServiceForPool(db, mcpVault, bindMCPVault)
	mcpSvc.SetEndpointPolicy(mcp.EndpointPolicy{AllowPrivate: cfg.MCP.AllowPrivateEndpoints})
	mcpSvc.SetPluginService(pluginSvc)

	// The tools are built before the PoolManager exists; the closure is resolved
	// only during a turn, after the shared Management service is fully wired.
	var agentManagement *agentaccess.Management
	var controlPlaneSvc *controlplane.Service
	var mcpAccess *mcp.Access
	var registeredToolMeta *toolmeta.Registry
	builtinTools := newBuiltinTools(builtinToolDeps{
		Notifier:    dispatcher,
		Memory:      memProvider,
		Recall:      sessionAccess,
		GroupRecall: groupRecall,
		Goal:        goalSvc,
		Session:     sessionAccess,
		Library:     librarySvc,
		Scheduler:   schedulerSvc,
		Workflow:    workflowSvc,
		Credentials: credSvc,
		Email:       emailSvc,
		EmailTool: email.ToolDeps{
			Authorize: func(ctx context.Context, tool string) (context.Context, error) {
				return pluginhost.AuthorizeEmailTool(ctx, tool, email.ListTool)
			},
			MapError: authz.MapToolError,
		},
		Share:           shareSvc,
		Recally:         recallySvc,
		Vault:           vaultSvc,
		AgentManagement: func() *agentaccess.Management { return agentManagement },
		ToolOverrides:   agent.NewToolOverrideStore(db),
		ToolMeta:        func() *toolmeta.Registry { return registeredToolMeta },
		SkillManagement: skillManagement,
		SettingsAdmin:   settingsAdminLookup{users: appdb.NewOIDCStore(db)},
		SettingsAgents:  store,
		ControlPlane:    func() *controlplane.Service { return controlPlaneSvc },
		PluginService:   func() *plugin.Service { return pluginSvc },
		MCPAccess:       func() *mcp.Access { return mcpAccess },
		MCPCatalog:      mcpCatalogFunc(mcpSvc),
	})
	registeredSpecs := make([]toolmeta.ActionTool, 0, len(builtinTools))
	for _, builtin := range builtinTools {
		if definition, ok := builtin.Definition(); ok {
			if spec, ok := toolMetaRegistry.Lookup(definition.Name); ok {
				registeredSpecs = append(registeredSpecs, spec)
			}
		}
	}
	registeredToolMeta = toolmeta.NewRegistry(registeredSpecs...)

	poolMgr = agent.NewPoolManager(store, memProvider,
		agent.WithSnapshotLoader(snapshotLoader),
		agent.WithCodeToolSurface(cfg.Agent.CodeToolSurface),
		agent.WithCompactionPM(agent.CompactionConfig{}.WithDefaults()),
		agent.WithSessionImagePipeline(sessionImages),
		agent.WithSessionInboxPM(sessionInbox),
		agent.WithGroupRosterLoader(channel.NewGroupRosterPromptLoader(db)),
		agent.WithBuiltinTools(builtinTools),
		agent.WithToolMetaRegistry(toolMetaRegistry),
		agent.WithPluginToolsBuilder(pluginToolsBuilder),
		agent.WithPluginHooksBuilder(pluginHooksBuilder),
		agent.WithCoreHooks(coreHooks),
		agent.WithProviderStreamBuilder(providerStreamBuilder),
		agent.WithSandboxBackends(sandboxBackends),
		agent.WithPromptSectionsBuilder(phost.SystemPromptSections),
		agent.WithPluginContextBuilder(pluginContextBuilder),
		agent.WithBeforeRunBuilderPM(phost.BeforeRun),
		agent.WithToolLifecycleBuilder(toolLifecycleBuilder),
		agent.WithToolOverrideFetcher(agent.NewToolOverrideStore(db).Fetch),
		agent.WithSkillRevisionReader(skillStore),
		agent.WithSkillReadAuthorizer(skillAccess),
		agent.WithProjectResolver(projectStore.Resolve),
		agent.WithHomeWorkspace(homeRegistry),
		agent.WithCoreRuntimePlan(coreRuntimePlan),
	)

	// Bind the static Vault/MCP/OAuth capabilities into the pool BEFORE StartAll,
	// as one-shot pre-start binds. Binding them up front means agents are built
	// once, with the full capability set, rather than rebuilt after a late setter.
	if vaultSvc != nil {
		if err := poolMgr.BindVaultEnvLoader(vaultSvc); err != nil {
			return nil, fmt.Errorf("bind vault env loader: %w", err)
		}
	}
	if err := poolMgr.BindSessionAccess(sessionaccess.NewAgentSessionAccess(sessionAccess)); err != nil {
		return nil, fmt.Errorf("bind session access: %w", err)
	}
	if err := sessionAccess.BindRuntimeManager(sessionaccess.NewRuntimeManager(poolMgr)); err != nil {
		return nil, fmt.Errorf("bind session runtime manager: %w", err)
	}
	if err := poolMgr.BindMCPToolProvider(mcp.NewToolProvider(mcpSvc)); err != nil {
		return nil, fmt.Errorf("bind mcp tool provider: %w", err)
	}
	if ps.oauthRegistry != nil {
		if err := poolMgr.BindOAuthRegistry(ps.oauthRegistry); err != nil {
			return nil, fmt.Errorf("bind oauth registry: %w", err)
		}
	}

	if err := poolMgr.StartAll(parent); err != nil {
		return nil, fmt.Errorf("start pool manager: %w", err)
	}
	// Drain durable inputs before any server/channel/River ingress can accept
	// newer work. Recovery appends transcripts only; it never enters Runtime.
	if err := sessionInbox.Recover(parent, sessionAccess, inboxAppender); err != nil {
		return nil, fmt.Errorf("recover Session inbox: %w", err)
	}

	// The runner invalidator lets credential/token refresh propagate to running
	// pools. It targets the pool but is not a pool capability, so it is wired
	// after StartAll; the shared credSvc is then fully configured before it is
	// handed to the admin server via Deps.
	credSvc.SetInvalidator(poolMgr)

	// Webhook resource domain. It owns the user→Agent binding, opaque capability
	// verifier, and lifecycle independently from deployment channel management.
	webhookSvc, err := webhook.NewService(webhook.Config{
		Store:  webhook.NewPostgresStore(db),
		Users:  webhook.NewUserState(credential.NewPostgresStore(db)),
		Access: webhook.NewUserAgentAccess(agentAccess),
	})
	if err != nil {
		return nil, fmt.Errorf("build webhook service: %w", err)
	}

	// Control-plane domain for the admin-only deployment resources
	// (providers/settings/plugins/channels). Authorization is the admin gate in
	// Begin, so the HTTP transport keeps only decode/shape. Built here, after the
	// pool and shared connections service are fully wired.
	controlPlaneSvc = controlplane.NewService(store, phost, providerRegistry, poolMgr, credSvc, slog.With("component", "controlplane"))
	mcpAccess = mcp.NewAccess(mcpSvc, agentAccess, poolMgr)

	// Composition root for River: both the scheduler and goal subsystems are now
	// built, so assemble the single shared working client from their queues and
	// inject it back into each. runServer owns its Start/Stop.
	homeDeletion, err := home.NewOwnerDeletion(db, homeRegistry, poolMgr, home.WithMediaPurger(sessionImages))
	if err != nil {
		return nil, fmt.Errorf("build Home deletion lifecycle: %w", err)
	}
	agentManagement = agentaccess.NewManagement(
		agentAccess, store, authStore, poolMgr, userDirectory{users: appdb.NewOIDCStore(db)},
		agent.NewAgentActivityStore(db), credentialSvc, store,
		slog.With("component", "agent-management"),
		agentaccess.WithOwnerDeletion(homeDeletion),
		agentaccess.WithAgentIDOccupancy(homeRegistry),
	)
	groupNudgeWorker := channel.NewGroupNudgeWorker()
	riverClient, err := buildSharedRiverClient(db, schedulerSvc, goalSvc, embeddingSvc, librarySvc, sessionImages, groupNudgeWorker, cfg.Lifecycle.RiverSoftStopTimeout, cfg.Observability.RiverLogLevel)
	if err != nil {
		return nil, err
	}

	// Seal the plugin host: all static plugin registrations and capability
	// bindings are complete. This validates them once and refuses any late
	// static registration; the dynamic desired-state surface (ApplyChannel /
	// RegisterManifestPlugins, used by the background reconcile below and by
	// runtime admin edits) stays available.
	if err := phost.Seal(); err != nil {
		return nil, fmt.Errorf("seal plugin host: %w", err)
	}

	backgroundTasks := &sync.WaitGroup{}

	reconcileProjectCoordinatesInBackground(parent, backgroundTasks, homeRegistry)
	// Close runtime entry points before setup returns and traffic can beat the
	// background reconciler to the legacy inventory.
	skillStore.BeginStartupReconciliation()
	reconcileSkillHomeInBackground(parent, backgroundTasks, skillMigrator)
	backfillRecallyContentInBackground(parent, backgroundTasks, recallySvc)

	result := &setupResult{
		ctx:                    parent,
		cfg:                    cfg,
		db:                     db,
		embedded:               embedded,
		mem:                    memProvider,
		store:                  store,
		snapshotLoader:         snapshotLoader,
		credentialSvc:          credentialSvc,
		credentialProviders:    store,
		authStore:              authStore,
		agentAccess:            agentAccess,
		agentManagement:        agentManagement,
		projectStore:           projectStore,
		sessionAccess:          sessionAccess,
		skillAccess:            skillAccess,
		pluginHost:             phost,
		pluginService:          pluginSvc,
		providerRegistry:       providerRegistry,
		channelRuntimeServices: ps.channelRuntimeServices,
		poolManager:            poolMgr,
		schedulerSvc:           schedulerSvc,
		goalSvc:                goalSvc,
		vaultSvc:               vaultSvc,
		mcpSvc:                 mcpSvc,
		controlPlane:           controlPlaneSvc,
		webhooks:               webhookSvc,
		credSvc:                credSvc,
		emailSvc:               emailSvc,
		shareSvc:               shareSvc,
		recallySvc:             recallySvc,
		assetStore:             assetStore,
		workspaceManager:       homeRegistry,
		homeDeletion:           homeDeletion,
		workflowSvc:            workflowSvc,
		embeddingSvc:           embeddingSvc,
		librarySvc:             librarySvc,
		groupNudgeWorker:       groupNudgeWorker,
		riverClient:            riverClient,
		builtinTools:           builtinTools,
		toolMeta:               registeredToolMeta,
		notifier:               dispatcher,
		skillStore:             skillStore,
		sessionImages:          sessionImages,
		cliUserID:              0,
		oauthRegistry:          ps.oauthRegistry,
		backgroundTasks:        backgroundTasks,
		metricHook:             metricHook,
	}
	// Ownership of the embedded server moves to result; clear the local so the
	// cleanup defer above becomes a no-op on this success path.
	workspaceManagerOwned = false
	embedded = nil
	return result, nil
}

func ensureEmbeddedAssets() error {
	registry, err := resources.Default()
	if err != nil {
		return fmt.Errorf("load builtin skill bundle: %w", err)
	}
	blockers, err := registry.InventoryLegacySkills(filepath.Join(config.StellaHome(), ".agents", "skills"))
	if err != nil {
		return fmt.Errorf("inventory legacy system skills: %w", err)
	}
	if len(blockers) != 0 {
		paths := make([]string, 0, len(blockers))
		for _, blocker := range blockers {
			paths = append(paths, blocker.Path)
		}
		return fmt.Errorf("cannot activate builtin skill bundle: legacy system skills remain at %s; back up the listed paths, run or roll back to the previous working Stella binary, import each custom root as a global/system Skill through Settings → Skills (older releases) or Admin Console → Deployment resources → Global Skills, verify each import, remove only migrated or residual legacy paths, then retry", strings.Join(paths, ", "))
	}
	// Remove assets retired or renamed by newer releases so stale copies do not
	// remain discoverable beside their replacements.
	_ = os.Remove(filepath.Join(config.StellaHome(), "bin", "stella"))
	_ = os.Remove(filepath.Join(config.StellaHome(), "bin", "stella.exe"))
	if err := binaries.EnsureTools(config.StellaHome()); err != nil {
		return fmt.Errorf("extract embedded tools: %w", err)
	}
	if err := binaries.VerifyTools(config.StellaHome()); err != nil {
		return err
	}
	if _, err := registry.InstallBuiltinBundle(config.StellaHome()); err != nil {
		return fmt.Errorf("install builtin skill bundle: %w", err)
	}
	return nil
}

func setupScheduler(db *pgxpool.Pool, agentAccess *agentaccess.Service, capabilityGate scheduler.BackgroundCapabilityGate) (*scheduler.Service, error) {
	// External-river mode: the scheduler does not build its own River client. The
	// composition root (buildSharedRiverClient) assembles the single process-wide
	// working client from both the scheduler and goal queues and injects it back
	// via BindRiverClient, so there is exactly one electable River client per
	// database (see db.NewWorkingRiverClient).
	//
	// WithAgentAccess wires Scheduler to Agent's direct access port. Scheduler
	// itself owns durable-job rules for HTTP and tool use cases.
	svc, err := scheduler.New(db, scheduler.WithExternalRiver(), scheduler.WithAgentAccess(agentAccess), scheduler.WithBackgroundCapabilityGate(capabilityGate))
	if err != nil {
		return nil, fmt.Errorf("create scheduler service: %w", err)
	}
	return svc, nil
}

// buildSharedRiverClient is the composition root for River: it assembles the
// single process-wide working client from every subsystem's queue + worker
// (scheduler and goal), then injects it back into each so they enqueue and
// register periodic jobs against the same client. There must be exactly one
// electable River client per database (see db.NewWorkingRiverClient); this is
// where that invariant is enforced. The caller owns the returned client's
// Start/Stop lifecycle (runServer); the subsystems only use it.
func buildSharedRiverClient(db *pgxpool.Pool, schedulerSvc *scheduler.Service, goalSvc *goal.Service, embeddingSvc *embedding.Service, librarySvc *library.Service, sessionImages *sessionmedia.Pipeline, groupNudgeWorker *channel.GroupNudgeWorker, softStopTimeout time.Duration, riverLogLevel string) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	scheduler.RegisterRiverWorker(workers, schedulerSvc)
	goalSvc.RegisterRiverWorker(workers)
	if groupNudgeWorker == nil {
		return nil, errors.New("build shared river client: group nudge worker is required")
	}
	channel.RegisterGroupNudgeWorker(workers, groupNudgeWorker)

	queues := map[string]river.QueueConfig{}
	sn, sc := scheduler.SchedulerQueueConfig()
	queues[sn] = sc
	gn, gc := goalSvc.GoalQueueConfig()
	queues[gn] = gc
	gtn, gtc := goalSvc.GoalTickQueueConfig()
	queues[gtn] = gtc
	nn, nc := channel.GroupNudgeQueueConfig()
	queues[nn] = nc

	// The embedding lane is opt-in: only contribute its backfill worker + queue
	// when an embedding provider is configured.
	if embeddingSvc != nil {
		embeddingSvc.RegisterRiverWorker(workers)
		en, ec := embeddingSvc.BackfillQueueConfig()
		queues[en] = ec
	}
	if librarySvc != nil {
		librarySvc.RegisterRiverWorkers(workers)
		kn, kc := librarySvc.QueueConfig()
		queues[kn] = kc
	}
	// Session media always has a sweep: media rows exist in every deployment,
	// and an orphan is storage nobody will ever reclaim by hand.
	if sessionImages != nil {
		sessionImages.RegisterRiverWorker(workers)
		mn, mc := sessionImages.OrphanSweepQueueConfig()
		queues[mn] = mc
	}

	// River heartbeats at DEBUG/INFO every few seconds (producer batches, job
	// stats, leader reelection), which drowns application logs. Cap its logger
	// at WARN unless LOG_LEVEL_RIVER explicitly opens it up for queue debugging.
	riverLevel := slog.LevelWarn
	if riverLogLevel != "" {
		riverLevel = cli.ParseLogLevel(riverLogLevel)
	}
	riverLog := slog.New(cli.NewMinLevelHandler(riverLevel, slog.Default().Handler())).With("component", "river")
	client, err := appdb.NewWorkingRiverClient(db, queues, workers, riverLog, softStopTimeout)
	if err != nil {
		return nil, fmt.Errorf("build shared river client: %w", err)
	}
	// One-shot pre-start binds: each subsystem rejects a nil/duplicate/late bind.
	if err := schedulerSvc.BindRiverClient(client); err != nil {
		return nil, err
	}
	if err := goalSvc.BindRiverClient(client); err != nil {
		return nil, err
	}
	if embeddingSvc != nil {
		if err := embeddingSvc.BindRiverClient(client); err != nil {
			return nil, err
		}
	}
	if librarySvc != nil {
		if err := librarySvc.BindRiverClient(client); err != nil {
			return nil, err
		}
	}
	if sessionImages != nil {
		if err := sessionImages.BindRiverClient(client); err != nil {
			return nil, err
		}
	}
	return client, nil
}

// schedulerWorkflowAdapter bridges the scheduler's WorkflowRunner port to the
// workflow service. It holds no queries of its own — every read goes through the
// workflow domain (svc), so the composition root carries no application SQL.
type schedulerWorkflowAdapter struct {
	svc *workflowpkg.Service
}

func (a schedulerWorkflowAdapter) ValidateScheduledWorkflow(ctx context.Context, req scheduler.WorkflowValidateRequest) (scheduler.ScheduledWorkflow, error) {
	wf, err := a.svc.ValidateScheduledWorkflow(ctx, req.UserID, req.AgentID, req.WorkflowID)
	if errors.Is(err, workflowpkg.ErrNotFound) {
		return scheduler.ScheduledWorkflow{}, scheduler.ErrWorkflowJobNotFound
	}
	if err != nil {
		return scheduler.ScheduledWorkflow{}, err
	}
	return scheduler.ScheduledWorkflow{ID: wf.ID, FullyFrozen: wf.FullyFrozen}, nil
}

// AuthorizeWorkflow delegates the target's durable access decision to Workflow.
func (a schedulerWorkflowAdapter) AuthorizeWorkflow(ctx context.Context, authority authz.Authority, workflowID string, action authz.Action) error {
	return a.svc.Authorize(ctx, authority, workflowID, action)
}

func (a schedulerWorkflowAdapter) LatestWorkflowRun(ctx context.Context, req scheduler.WorkflowLatestRunRequest) (scheduler.WorkflowRunState, error) {
	rs, err := a.svc.LatestRunState(ctx, req.WorkflowID)
	if err != nil {
		return scheduler.WorkflowRunState{}, err
	}
	return scheduler.WorkflowRunState{
		Found:            rs.Found,
		Status:           rs.Status,
		IdempotencyKey:   rs.IdempotencyKey,
		RootGoalID:       rs.RootGoalID,
		RootGoalTerminal: rs.RootGoalTerminal,
	}, nil
}

func (a schedulerWorkflowAdapter) InstantiateWorkflow(ctx context.Context, authority authz.Authority, req scheduler.WorkflowInstantiateRequest) (scheduler.WorkflowInstantiateResult, error) {
	run, _, err := a.svc.InstantiateAs(ctx, authority, req.WorkflowID, req.Inputs, req.IdempotencyKey)
	if err != nil {
		return scheduler.WorkflowInstantiateResult{}, err
	}
	rootID := ""
	if run.RootGoalID != nil {
		rootID = *run.RootGoalID
	}
	return scheduler.WorkflowInstantiateResult{RunID: run.ID, RootGoalID: rootID}, nil
}

func wireSchedulerCallbacks(svc *scheduler.Service, poolMgr *agent.PoolManager, dispatcher *notify.Dispatcher) {
	if svc == nil {
		return
	}
	svc.SetOnJob(func(ctx context.Context, job scheduler.Job, authority authz.Authority) error {
		// Scheduler dispatch already directly rechecked the durable Job and current
		// Agent. The composition root only selects the implementation and runs the
		// turn under that explicit authority.
		agentSvc := poolMgr.GetService(job.AgentID)
		if agentSvc == nil && job.AgentID == "" {
			agentSvc = poolMgr.Default()
		}
		if agentSvc == nil {
			slog.Warn("scheduler: no service available for job", "job_id", job.ID, "agent_id", job.AgentID)
			return fmt.Errorf("no agent service available for job %s (agent %q)", job.ID, job.AgentID)
		}
		agentID := agentSvc.AgentID
		sessionID := scheduler.RunSessionIDFromContext(ctx)
		if sessionID == "" {
			if job.UserID != "" {
				sessionID = job.UserSessionID(job.UserID)
			} else {
				sessionID = job.SessionID()
			}
		}
		ch := agentSvc.ChatForScheduler(schedulerJobContext(ctx, agentID, job), agent.SchedulerChatRequest{
			SessionID:   sessionID,
			UserID:      job.UserID,
			AgentID:     agentID,
			Message:     schedulerJobMessage(job),
			Authority:   authority,
			BeforeStart: func() error { return svc.AuthorizeAgentStart(ctx, job, authority, agentID) },
		})
		// Keep the last step's text — that's the final assistant answer;
		// earlier steps are tool-call narration.
		var runErr error
		var output, step strings.Builder
		for evt := range ch {
			if evt.Err != nil {
				runErr = evt.Err
				slog.Error("scheduler job error", "job_id", job.ID, "error", evt.Err)
			}
			if evt.Text != "" {
				step.WriteString(evt.Text)
			}
			if evt.Step != nil && evt.Step.Kind == "finish" && step.Len() > 0 {
				output.Reset()
				output.WriteString(step.String())
				step.Reset()
			}
		}
		if step.Len() > 0 {
			output.Reset()
			output.WriteString(step.String())
		}
		scheduler.RunOutputSinkFromContext(ctx).Set(strings.TrimSpace(output.String()))
		return runErr
	})
}

type projectCoordinateReconciler interface {
	ReconcileProjectCoordinates(context.Context) (home.ProjectCoordinateReconcileResult, error)
}

type skillHomeReconciler interface {
	ReconcileStartup(context.Context) (skill.SkillStartupReconcileResult, error)
}

func reconcileProjectCoordinatesInBackground(ctx context.Context, wg *sync.WaitGroup, manager projectCoordinateReconciler) {
	wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("project coordinate reconciliation panic", "panic", r)
			}
		}()
		result, err := manager.ReconcileProjectCoordinates(ctx)
		if err != nil {
			slog.Error("project coordinate reconciliation failed", "error", err)
			return
		}
		if len(result.UnresolvedIDs) != 0 {
			slog.Warn("legacy projects remain unavailable after background reconciliation",
				"updated", result.Updated,
				"unresolved_count", len(result.UnresolvedIDs),
				"project_ids", result.UnresolvedIDs,
			)
			return
		}
		if result.Updated != 0 {
			slog.Info("project coordinate reconciliation complete", "updated", result.Updated)
		}
	})
}

func reconcileSkillHomeInBackground(ctx context.Context, wg *sync.WaitGroup, migrator skillHomeReconciler) {
	wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Skill storage reconciliation panic",
					"managed_skills_available", false,
					"recovery", "inspect the panic, repair the reported storage problem, then restart stellad",
					"panic", r,
				)
			}
		}()
		result, err := migrator.ReconcileStartup(ctx)
		if err != nil {
			slog.Error("Skill storage reconciliation failed",
				"managed_skills_available", false,
				"recovery", "fix the reported Home or database error, then restart stellad",
				"error", err,
			)
			return
		}
		if result.Degraded != nil {
			slog.Error("managed Skills unavailable after background reconciliation",
				"managed_skills_available", false,
				"recovery", "fix the reported legacy Skill data, then restart stellad",
				"error", result.Degraded,
			)
			return
		}
		if result.Migration.SkillCount != 0 {
			slog.Info("Skill storage reconciliation complete",
				"skills", result.Migration.SkillCount,
				"files", result.Migration.FileCount,
				"bytes", result.Migration.ContentBytes,
			)
		}
	})
}

// backfillRecallyContentInBackground eager-copies legacy recally article bodies
// from their disk mirrors into PostgreSQL at startup, so bodies survive on hosts
// where the pod-local disk that held the mirror is gone. It runs off the daemon
// lifecycle context in a background goroutine so it never delays startup.
func backfillRecallyContentInBackground(ctx context.Context, wg *sync.WaitGroup, svc *recally.Service) {
	if svc == nil {
		return
	}
	wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recally content backfill panic", "panic", r)
			}
		}()
		scanned, backfilled, missing, err := svc.BackfillMissingContent(ctx)
		if err != nil {
			slog.Warn("recally content backfill failed", "scanned", scanned, "backfilled", backfilled, "missing", missing, "error", err)
			return
		}
		if scanned > 0 {
			slog.Info("recally content backfill complete", "scanned", scanned, "backfilled", backfilled, "missing", missing)
		}
	})
}

func (s *setupResult) waitBackgroundTasks() {
	if s != nil && s.backgroundTasks != nil {
		s.backgroundTasks.Wait()
	}
}
