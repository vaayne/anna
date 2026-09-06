package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"filippo.io/age"
	"go.opentelemetry.io/otel"

	ucli "github.com/urfave/cli/v2"

	"golang.org/x/sync/errgroup"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/agent/prompt"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/auth/account"
	"github.com/CherryHQ/stella/internal/auth/oidc"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/channel"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/credential"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/eventlog"
	"github.com/CherryHQ/stella/internal/inbox"
	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/memorywrite"
	memprofile "github.com/CherryHQ/stella/internal/memory/profile"
	oauthserver "github.com/CherryHQ/stella/internal/oidc"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/observability"
	"github.com/CherryHQ/stella/internal/provisioning"
	"github.com/CherryHQ/stella/internal/scheduler"
	"github.com/CherryHQ/stella/internal/server"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
	"github.com/CherryHQ/stella/pkg/providers"
	"github.com/CherryHQ/stella/plugins/email"
)

const defaultAdminPort = 25678

var nativeServerGOOS = runtime.GOOS

func checkNativeServerPlatform(goos string) error {
	if goos != "linux" && goos != "darwin" {
		return errors.New("native Stella server and durable Skill Home are supported only on Linux and macOS")
	}
	return nil
}

// userDirectory adapts the account user store to the Agent domain's UserDirectory
// port so agent-assignment views can show a target's email without the transport
// (or the Agent domain) depending on the account boundary's internals. Lookups
// are per-id: the assignment set of one agent is small and admin-only, and this
// keeps the composition root free of the raw query layer.
type userDirectory struct {
	users interface {
		GetUser(ctx context.Context, id string) (auth.User, error)
	}
}

func (d userDirectory) LookupUser(ctx context.Context, id string) (agentaccess.UserRef, error) {
	u, err := d.users.GetUser(ctx, id)
	if err != nil {
		return agentaccess.UserRef{}, err
	}
	return agentaccess.UserRef{ID: u.ID, Email: u.Email}, nil
}

// settingsAdminLookup resolves the durable active-admin state at catalog-build
// and Settings-execution time. A missing or unreadable user is an error so
// settingspolicy can fail closed.
type settingsAdminLookup struct {
	users interface {
		GetUser(context.Context, string) (auth.User, error)
	}
}

func (l settingsAdminLookup) IsAdmin(ctx context.Context, userID string) (bool, error) {
	u, err := l.users.GetUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.Role == auth.RoleAdmin && u.IsActive, nil
}

func (d userDirectory) LookupUsers(ctx context.Context, ids []string) ([]agentaccess.UserRef, error) {
	out := make([]agentaccess.UserRef, 0, len(ids))
	for _, id := range ids {
		// A stale assignment link whose user no longer resolves is skipped rather
		// than failing the admin listing — the historical batch lookup did the same.
		u, err := d.users.GetUser(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, agentaccess.UserRef{ID: u.ID, Email: u.Email})
	}
	return out, nil
}

func serverCommand() *ucli.Command {
	return &ucli.Command{
		Name:     "server",
		Aliases:  []string{"serve"},
		Usage:    "Start the stella server",
		Category: "System",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:    "host",
				Usage:   "Host/interface for Web UI",
				Value:   "127.0.0.1",
				EnvVars: []string{"HOST"},
			},
			&ucli.IntFlag{
				Name:    "port",
				Usage:   "Port for Web UI",
				Value:   defaultAdminPort,
				EnvVars: []string{"PORT"},
			},
		},
		Action: serverAction,
	}
}

func serverAction(c *ucli.Context) error {
	// Reject unsupported hosts before configuration, database startup, schema
	// migration, or any durable filesystem mutation.
	if err := checkNativeServerPlatform(nativeServerGOOS); err != nil {
		return err
	}
	// Parse the full server environment once, up front, so a misconfigured
	// value (bad duration, non-boolean guard) fails fast before any subsystem
	// starts. This is the single startup boundary that reads ServerConfig;
	// operator commands that never call setup (version, vault keygen, service,
	// mise) must not reach it, so an unrelated bad variable cannot block them.
	cfg, err := config.LoadServerConfig(os.LookupEnv)
	if err != nil {
		return err
	}

	// Parse the dynamic login-provider config (AUTH_OAUTH_*, LOCAL_*) at this same
	// startup boundary, alongside ServerConfig, so the oidc package reads no
	// environment of its own and a misconfigured provider fails fast here. The
	// resolved base URL supplies OAuth redirect defaults.
	adminHost := c.String("host")
	adminPort := c.Int("port")
	baseURL := resolveBaseURL(cfg.BaseURL, adminHost, adminPort)
	loginCfg, err := oidc.LoadLoginConfig(os.LookupEnv, baseURL)
	if err != nil {
		return fmt.Errorf("load login config: %w", err)
	}

	// The vault key is required to run the server. Check it from the parsed
	// config so there is a single reader; the key is a secret and never appears
	// in this error text.
	if cfg.Vault.Key == "" {
		return errors.New(
			"STELLA_VAULT_KEY is not set\n\n" +
				"stella requires a vault key to encrypt credentials and secrets.\n" +
				"Generate one and add it to $STELLA_HOME/.env:\n\n" +
				"  stellad vault keygen\n" +
				"  echo 'STELLA_VAULT_KEY=AGE-SECRET-KEY-1...' >> ~/.stella/.env\n\n" +
				"Back up the key — if it is lost, all stored secrets become unrecoverable.\n" +
				"See the vault documentation for details",
		)
	}

	// Clean up stale upgrade artifacts (.tmp/.bak/.old) from interrupted upgrades.
	if installDir, err := resolveUpgradeDir(""); err == nil {
		warnStaleUpgradeArtifacts(installDir)
		cleanStaleUpgradeArtifacts(installDir)
	}

	// Signals are handled manually, not via signal.NotifyContext: once serving,
	// the first SIGINT/SIGTERM must start a graceful drain without cancelling
	// work contexts, and only a second signal hard-stops. This base context is
	// cancelled by the cleanup defers below — or by a signal DURING STARTUP,
	// where "abort setup" (stopping the embedded PostgreSQL via the defers, not
	// killing the process with a live postmaster) is the old NotifyContext
	// behavior we must keep. runServer hands the channel over to the drain
	// supervisor once subsystems are up.
	ctx, cancel := context.WithCancel(c.Context)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	startupDone := make(chan struct{})
	var handoff sync.Once
	endStartupWatch := func() { handoff.Do(func() { close(startupDone) }) }
	// Release the watcher even when startup fails before runServer hands off.
	defer endStartupWatch()
	go watchStartupSignal(sigCh, startupDone, func() {
		slog.Info("shutdown signal during startup; aborting")
		cancel()
	})

	startDiagnostics(ctx, cfg.Diagnostics.PprofAddr)

	// Register provider shutdown before setup cleanup so defer LIFO closes pools
	// and trace hooks first, then flushes the provider with all final spans/logs.
	// Init is deliberately before setup: setup captures component loggers, and
	// those loggers must already point at the tee handler to reach OTLP.
	obs, err := observability.Init(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("init observability: %w", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := obs.Shutdown(shutCtx); err != nil {
			slog.Warn("otel shutdown failed", "error.type", fmt.Sprintf("%T", err), "error.class", "provider_shutdown_failed")
			observability.ConsoleOnlyLogger().Warn("otel shutdown detail", "error", err)
		}
	}()

	s, err := setup(ctx, cfg, baseURL)
	if err != nil {
		cancel()
		return err
	}
	if s.metricHook != nil && obs.MetricsEnabled() {
		if err := s.metricHook.Bind(otel.Meter("stella")); err != nil {
			cancel()
			return fmt.Errorf("bind observability metrics: %w", err)
		}
	}

	defer func() {
		cancel()
		s.waitBackgroundTasks()
		_ = s.poolManager.Close()
		_ = s.workspaceManager.Close()
		// Stop the managed PostgreSQL last, once every DB user is done: close the
		// pool first so the server shuts down without active connections. Only set
		// in zero-config mode; an external DSN leaves s.embedded nil.
		if s.embedded != nil {
			s.db.Close()
			_ = s.embedded.Stop()
		}
	}()

	listFn := func() []pkgchannel.ModelOption {
		return collectModelsFromStore(s.ctx, s.store)
	}
	switchFn := func(_, _ string) error { return nil }

	return runServer(s.ctx, s, loginCfg, baseURL, listFn, switchFn, adminHost, adminPort, sigCh, endStartupWatch)
}

// watchStartupSignal bridges signal ownership from startup to the drain
// supervisor. Until startupDone is closed, the first SIGINT/SIGTERM aborts
// startup via abort (cancelling the base context so partially-started subsystems
// unwind). Once startup completes, the drain supervisor owns signal handling;
// but this watcher and the supervisor can both be selecting on sigCh at the
// instant of handoff, and Go's select picks a ready case at random. If this
// watcher wins that race AFTER startupDone is closed, aborting would be wrong and
// would lose the drain, so it hands the signal back to sigCh (buffered, and this
// receive just freed a slot, so the nonblocking send succeeds) for the drain
// supervisor to consume. Returns as soon as it consumes a signal or startup ends,
// so it never leaks.
func watchStartupSignal(sigCh chan os.Signal, startupDone <-chan struct{}, abort func()) {
	select {
	case sig := <-sigCh:
		select {
		case <-startupDone:
			// Startup already finished; the drain supervisor owns this signal.
			select {
			case sigCh <- sig:
			default:
			}
		default:
			abort()
		}
	case <-startupDone:
	}
}

// runServer starts every subsystem and blocks until shutdown. sigCh delivers
// SIGINT/SIGTERM (registered by the caller, who owns signal.Stop); onServing is
// called exactly once, when startup is complete and the two-phase drain
// supervisor has taken over signal handling from the caller's startup watcher.
func runServer(ctx context.Context, s *setupResult, loginConfig oidc.LoginConfig, baseURL string, listFn func() []pkgchannel.ModelOption, switchFn func(string, string) error, adminHost string, adminPort int, sigCh <-chan os.Signal, onServing func()) error {
	// workCtx is the errgroup parent. Graceful drain cancels it only AFTER the
	// HTTP server has drained, so the LIFO defer chain (goal tick/dispatcher,
	// scheduler, riverClient.Stop) then tears subsystems down in order. A
	// subsystem crash still cancels gctx via the errgroup, exactly as before.
	// River is started below on a context decoupled from workCtx, so in-flight
	// jobs survive this cancellation and drain under the soft-stop budget.
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	g, gctx := errgroup.WithContext(workCtx)

	warnDeploymentBaseURL(baseURL, s.cfg.OIDC.IssuerURL, len(loginConfig.OAuth) > 0)

	// Remove legacy configuration and seed the default Agent if no Agent exists.
	if err := s.store.Seed(gctx); err != nil {
		return fmt.Errorf("seed default data: %w", err)
	}

	// The one authoritative Agent PEP was built before PoolManager.StartAll.
	// HTTP and channel ingress reuse the exact worker/runtime instance.
	as := s.authStore
	agentAccess := s.agentAccess

	// Link codes are shared between Web UI and channel bots.
	linkCodes, err := auth.NewSharedLinkCodeStore(gctx, s.db)
	if err != nil {
		return fmt.Errorf("create link code store: %w", err)
	}

	// Unified bearer credential front door (PAT/OAuth storage) + OAuth2
	// authorization server, built once by the composition root and injected into
	// the admin server via Deps. The PAT/user-lookup adapter is owned by
	// internal/credential; the OAuth access + client/code/refresh adapter is owned
	// by internal/oidc and satisfies both credential.OAuthAccessStore and
	// oidc.Store. The authorization server mints access tokens through the front
	// door (never its own JWT), so the credential surface has a single owner.
	credLog := slog.With("component", "admin")
	credPATStore := credential.NewPostgresStore(s.db)
	oauthStore := oauthserver.NewPostgresStore(s.db)
	credFrontDoor := credential.NewService(credential.Config{
		PATs:   credPATStore,
		OAuth:  oauthStore,
		Users:  credPATStore,
		Logger: credLog,
	})
	oauthAuthServer := oauthserver.NewService(oauthserver.Config{
		Store:  oauthStore,
		Issuer: credFrontDoor,
		Logger: credLog,
	})

	// Vault recipient for the admin server (new-user age keygen) and the channel
	// coordinator. The MCP service and the pool-side vault env loader / MCP tool
	// provider were bound in setup, before StartAll.
	var coordOpts []channel.CoordinatorOption
	var vaultRecipient *age.X25519Recipient
	coordOpts = append(coordOpts, channel.WithCoordinatorAuth(as, agentAccess, linkCodes))
	coordOpts = append(coordOpts, channel.WithGuestPolicyDecoder(s.pluginHost.GuestPolicyResolver), channel.WithSnapshotResolver(s.pluginService.ResolveSnapshot), channel.WithListenerCap(s.pluginService.AdministrativeCap))
	coordOpts = append(coordOpts, channel.WithRootOpener(s.workspaceManager))
	if s.vaultSvc != nil {
		vaultRecipient = s.vaultSvc.MasterRecipient()
		coordOpts = append(coordOpts, channel.WithVaultRecipient(vaultRecipient))
		coordOpts = append(coordOpts, channel.WithVaultService(s.vaultSvc))
	}

	// Login authentication (external OIDC/OAuth providers and local password
	// auth). The identity stores it produces back the auth handlers.
	oidcStore := appdb.NewOIDCStore(s.db)
	oidcResult, err := oidc.Setup(gctx, oidc.SetupParams{
		DB:         s.db,
		VaultKey:   s.cfg.Vault.Key,
		OIDC:       s.cfg.OIDC,
		Login:      loginConfig,
		AuthStores: oidcStore,
	})
	if err != nil {
		return fmt.Errorf("oidc: setup: %w", err)
	}
	slog.Info("oidc: authentication configured")

	intentClassifier := newIntentClassifier(s.snapshotLoader, s.providerRegistry)
	coordOpts = append(coordOpts, channel.WithIntentClassifier(intentClassifier))

	elStore := eventlog.NewStore(s.db)
	groupEvents := channel.NewGroupEventHub()
	elStore.OnCommitted(groupEvents.Announce)
	botRegistry := channel.NewBotIdentityRegistry()
	publisherRegistry := channel.NewPublisherRegistry()
	coordOpts = append(coordOpts, channel.WithDB(s.db))
	coordOpts = append(coordOpts, channel.WithGuestStore(channel.NewGuestStore(s.db)))
	// Group event ingestion canonicalizes its images through the very pipeline
	// ordinary sessions use, with the group as the media owner.
	coordOpts = append(coordOpts, channel.WithSessionImages(s.sessionImages))
	coordOpts = append(coordOpts, channel.WithEventLog(elStore))
	coordOpts = append(coordOpts, channel.WithBotRegistry(botRegistry))
	coordOpts = append(coordOpts, channel.WithPublisherRegistry(publisherRegistry))

	// The channel domain builds the coordinator and its durable group dispatcher
	// together and closes the coordinator<->dispatcher cycle; the HTTP server
	// receives only the narrow group-dispatch port (Deps.GroupDispatcher).
	coordination := channel.NewCoordination(s.db, s.poolManager, s.store, listFn, switchFn, coordOpts...)
	coordinator := coordination.Coordinator
	groupDispatcher := coordination.GroupDispatcher
	groupTurnCommitter, ok := s.mem.(memory.TxGroupCommitter)
	if !ok {
		return errors.New("group dispatch requires memory.TxGroupCommitter")
	}
	groupDispatcher.SetGroupTurnCommitter(groupTurnCommitter)
	if s.groupNudgeWorker == nil {
		return errors.New("group nudge worker is unavailable")
	}
	nudger := channel.NewGroupNudger(s.db, groupDispatcher)
	nudger.SetClassifier(channel.NewLLMGroupNudgeClassifier(s.db,
		func(ctx context.Context, agentID string) (*config.Snapshot, error) {
			return s.snapshotLoader.Snapshot(ctx, agentID)
		},
		intentClassifierStreamFuncBuilder(s.providerRegistry),
	))
	nudger.SetGroupEventHub(groupEvents)
	// River may immediately run persisted periodic work on Start. Bind only after
	// the channel coordination exists, then fail closed rather than let a nudge
	// worker observe a half-built dispatcher.
	if err := s.groupNudgeWorker.Bind(nudger); err != nil {
		return fmt.Errorf("bind group nudger: %w", err)
	}
	groupDispatcher.SetGroupEventHub(groupEvents)
	if err := groupDispatcher.ValidateStartup(); err != nil {
		return fmt.Errorf("configure group dispatcher: %w", err)
	}
	changelogPageReader, ok := s.mem.(memory.ChangelogPageReader)
	if !ok {
		return fmt.Errorf("build memory management service: changelog page reader unavailable")
	}
	memoryManagement := memorywrite.NewManagementService(s.db, changelogPageReader)

	// Agent management deepens the Agent PEP with the write use cases: it owns the
	// durable agent create/update/delete, admin assignment, and conversation
	// activity read model plus the runtime-reload port, so the HTTP transport no
	// longer reaches config.Store / auth.AuthStore / the query layer for them. The
	// user directory backs assignment views with the account user store (per-id
	// lookups; the assignment set per agent is small and admin-only).
	toolOverrides := agent.NewToolOverrideStore(s.db)
	agentSkillPolicy, ok := s.store.(server.AgentSkillPolicyStore)
	if !ok {
		return fmt.Errorf("agent Skill policy store is unavailable")
	}
	agentManagement := s.agentManagement
	if agentManagement == nil {
		return errors.New("agent management service is unavailable")
	}

	// The Account service owns the user-account application boundary. It composes
	// the single OIDC store (user/channel/login/session/credential) with the auth
	// assignment store and the credential front door as the PAT revoker, so the
	// HTTP transport no longer holds any auth store directly.
	accountSvc := account.NewService(
		oidcStore, oidcStore, oidcStore, oidcStore, oidcStore,
		as, credFrontDoor,
		slog.With("component", "account"),
	)
	provisioningSvc := provisioning.New(s.db, accountSvc, vaultRecipient, slog.With("component", "provisioning"))

	// The Profile service owns the per-(user, agent) memory boundary. The Provider
	// is viewed through its ProfileStore/ChangelogReader capabilities (nil when the
	// Provider lacks them, degrading those endpoints to 503), and the Agent PEP
	// backs the read gate. The transport no longer touches memory.Provider,
	// memorywrite, or the query layer for profile/soul/constraints/changelog.
	memProfiles, _ := s.mem.(memory.ProfileStore)
	memChangelog, _ := s.mem.(memory.ChangelogReader)
	// memoryManagement (built above from the pool + the Provider's keyset changelog
	// reader) is injected into Profile as its knowledge/changelog-page adapter, so
	// the HTTP transport reaches it only through the Agent-gated Profile boundary.
	profileSvc := memprofile.NewService(
		s.db, memProfiles, memChangelog, memoryManagement, agentAccess,
		prompt.DefaultAgentSoul, slog.With("component", "profile"),
	)

	// Assemble the immutable, validated admin-server dependency set and construct
	// the server exactly once. Every shared instance above is passed in; the
	// server creates no shadow service, reads no environment, and has no setters.
	// The Group service owns the Web group/channel application boundary. It holds
	// the pool for group/member/message/outbox persistence, the Agent PEP for
	// per-agent use authorization, the runtime resolver for agent-name projection,
	// and the event log + group dispatcher for the send path (nil-tolerant: the
	// send path degrades to 503 while CRUD stays available).
	groupSvc := channel.NewGroupService(s.db, agentAccess, channel.NewRuntimeResolver(s.store), elStore, groupDispatcher, channel.WithOwnerDeletion(s.homeDeletion), channel.WithGroupEventHub(groupEvents))

	// Accepted Web turns outlive their initiating HTTP connections and must also
	// survive the errgroup cancellation caused by HTTP Shutdown. workCtx is
	// canceled only after the graceful-drain accepted-work wait completes.
	adminSrv, err := server.New(workCtx, server.Deps{
		Pinger:               s.db,
		Account:              accountSvc,
		Profile:              profileSvc,
		ProjectStore:         s.projectStore,
		Inbox:                inbox.NewService(s.db),
		AgentAccess:          agentAccess,
		AgentManagement:      agentManagement,
		AgentSkillPolicy:     agentSkillPolicy,
		ToolOverrides:        toolOverrides,
		SessionAccess:        s.sessionAccess,
		SkillAccess:          s.skillAccess,
		Skills:               s.skillStore,
		LinkCodes:            linkCodes,
		PoolManager:          s.poolManager,
		PluginHost:           s.pluginHost,
		PluginService:        s.pluginService,
		WeixinRegistrar:      newWeixinRegistrar(),
		BuiltinTools:         s.builtinTools,
		ToolMeta:             s.toolMeta,
		BaseURL:              baseURL,
		Credentials:          s.credSvc,
		ControlPlane:         s.controlPlane,
		Webhooks:             s.webhooks,
		Email:                s.emailSvc,
		EmailConfigValidator: email.ValidateConfigValue,
		Share:                s.shareSvc,
		Recally:              s.recallySvc,
		Assets:               s.assetStore,
		CredentialFrontDoor:  credFrontDoor,
		OAuthAuthServer:      oauthAuthServer,
		Group:                groupSvc,
		Vault:                s.vaultSvc,
		VaultRecipient:       vaultRecipient,
		MCP:                  s.mcpSvc,
		MCPCatalog:           mcp.NewOfficialCatalog(),
		MCPAccess:            mcp.NewAccess(s.mcpSvc, agentAccess, s.poolManager),
		Scheduler:            s.schedulerSvc,
		Goal:                 s.goalSvc,
		Workflow:             s.workflowSvc,
		Provisioning:         provisioningSvc,
		Library:              s.librarySvc,
		OIDC: server.OIDCDeps{
			Providers:  oidcResult.Providers,
			AuthSvc:    oidcResult.AuthSvc,
			SessionMgr: oidcResult.SessionMgr,
			StateMgr:   oidcResult.StateMgr,
			LocalAuth:  oidcResult.LocalAuth,
		},
	})
	if err != nil {
		return fmt.Errorf("build admin server: %w", err)
	}

	// ---- Static callbacks + backend startup (BEFORE any ingress) ------------
	// Everything an inbound request or a River job might touch is wired and
	// started here, so ingress (HTTP Serve, channel runtimes, group dispatch)
	// never observes a half-wired backend — the #708 no-late-bind/ingress-window
	// contract. The three setters below are mutex-guarded one-time writes that
	// run before any concurrent reader exists.

	// Notification auth directory: River scheduler/goal jobs route per-user
	// notifications through it, so it must be set before River starts.
	s.notifier.SetAuthService(s.pluginHost.Auth())

	// Channel runtime back-edge: the coordinator + notifier the managed channel
	// runtimes reach. Set before applyManagedChannelPlugins starts any bot.
	if s.channelRuntimeServices != nil {
		s.channelRuntimeServices.Set(gctx, coordinator, s.notifier, channel.WrapOperationHandler)
	}

	// Scheduler OnJob handler MUST be wired before River starts: River may pick up
	// a persisted scheduler job the instant it starts, and this handler is what
	// runs it.
	if s.schedulerSvc != nil {
		wireSchedulerCallbacks(s.schedulerSvc, s.poolManager, s.notifier)
	}

	// Final channel teardown is registered before River's stop defer so LIFO
	// drains River first while notifier senders are still registered. It is
	// separate from drain-time Quiesce, which stops polling but preserves accepted
	// operations and outbound delivery.
	defer func() { _ = s.pluginHost.Stop(context.Background()) }()

	// Start the single shared River client (composition root: buildSharedRiverClient
	// assembled it from scheduler, goal, and group-nudge workers).
	if s.riverClient != nil {
		if s.groupNudgeWorker == nil {
			return errors.New("start river client: group nudge worker is unavailable")
		}
		if err := s.groupNudgeWorker.ValidateStartup(); err != nil {
			return fmt.Errorf("start river client: %w", err)
		}
		nudgePeriodic, err := s.groupNudgeWorker.StartPeriodic(s.riverClient)
		if err != nil {
			return fmt.Errorf("start group nudge periodic: %w", err)
		}
		defer s.groupNudgeWorker.StopPeriodic(s.riverClient, nudgePeriodic)
		// Decouple River from workCtx: graceful drain cancels workCtx/gctx, but
		// in-flight goal/scheduler agent runs must keep executing until Stop drains
		// them within the soft-stop budget. WithoutCancel preserves values (tracing)
		// while dropping cancellation.
		if err := s.riverClient.Start(context.WithoutCancel(workCtx)); err != nil {
			return fmt.Errorf("start river client: %w", err)
		}
		// Stop waits for in-flight jobs, then cancels their contexts after
		// SoftStopTimeout (STELLA_RIVER_SOFT_STOP_TIMEOUT). Drain-time quiesce has
		// already stopped new dispatch when this defer runs; the channel teardown
		// defer was registered earlier, so notifier senders remain alive until River
		// has finished draining.
		defer func() { _ = s.riverClient.Stop(context.Background()) }()
	}

	// Idempotent stop-once ingress closures. Each halts a source of NEW work; they
	// are invoked by stopIngress at the start of a graceful drain AND deferred for
	// the crash / startup-error teardown path, so double invocation is safe.
	var quiesceChanOnce, stopSchedOnce, stopGoalOnce, stopEmbedOnce, stopLibraryOnce, stopMediaSweepOnce sync.Once
	// quiesceChannelIngress stops channel polling but preserves work already
	// accepted and the notifier senders that deliver it; the SEPARATE final Stop
	// defer below (not sharing this once) tears the runtimes down fully.
	quiesceChannelIngress := func() {
		quiesceChanOnce.Do(func() { s.pluginHost.Quiesce(context.Background()) })
	}

	stopSchedulerDispatch := func() {}
	if s.schedulerSvc != nil {
		if err := s.schedulerSvc.Start(ctx); err != nil {
			return fmt.Errorf("start scheduler: %w", err)
		}
		s.schedulerSvc.EnsureBuiltinJobs()
		// Drain stops NEW scheduled dispatch (periodic removal + late-schedule
		// rejection) while durable one-time jobs and in-flight runs keep draining
		// on the shared River client. The final Stop below is the lifecycle
		// teardown; it must not share stopSchedOnce with the drain-time Quiesce.
		stopSchedulerDispatch = func() { stopSchedOnce.Do(func() { s.schedulerSvc.Quiesce() }) }
		// Registered before stopSchedulerDispatch so it runs AFTER it (LIFO):
		// quiesce first, then final teardown.
		defer func() { _ = s.schedulerSvc.Stop() }()
		defer stopSchedulerDispatch()
	}

	// Goal execution substrate (River Phase 2a + 2b). Its convergence tick is a
	// single-leader River periodic (StartDispatchTick). Stop order quiets the
	// dispatcher BEFORE removing the periodic, so a tick already queued that a
	// worker picks up during shutdown finds the dispatcher stopped and no-ops.
	stopGoalDispatch := func() {}
	if s.goalSvc != nil && s.riverClient != nil {
		tick, err := s.goalSvc.StartDispatchTick()
		if err != nil {
			return fmt.Errorf("start goal dispatcher tick: %w", err)
		}
		stopGoalDispatch = func() {
			stopGoalOnce.Do(func() {
				s.goalSvc.Dispatcher.Stop()
				s.goalSvc.StopDispatchTick(tick)
			})
		}
		defer stopGoalDispatch()
	}

	// Embedding backfill periodic (single-leader River job). Present only when the
	// semantic lane is configured.
	stopEmbeddingBackfill := func() {}
	if s.embeddingSvc != nil && s.riverClient != nil {
		handle, err := s.embeddingSvc.StartBackfill()
		if err != nil {
			return fmt.Errorf("start embedding backfill: %w", err)
		}
		stopEmbeddingBackfill = func() { stopEmbedOnce.Do(func() { s.embeddingSvc.StopBackfill(handle) }) }
		defer stopEmbeddingBackfill()
	}

	// Session media orphan sweep (single-leader River periodic). It reclaims
	// blobs whose rows are gone, so it starts with the backends rather than with
	// ingress: no request path waits on it.
	stopMediaSweep := func() {}
	if s.sessionImages != nil && s.riverClient != nil {
		handle, err := s.sessionImages.StartOrphanSweep()
		if err != nil {
			return fmt.Errorf("start session media orphan sweep: %w", err)
		}
		stopMediaSweep = func() {
			stopMediaSweepOnce.Do(func() { s.sessionImages.StopOrphanSweep(handle) })
		}
		defer stopMediaSweep()
	}

	// Library reconciliation is an internal single-leader periodic. It is
	// started only after all workers and the shared River client are live.
	stopLibraryReconciliation := func() {}
	if s.librarySvc != nil && s.riverClient != nil {
		handle, err := s.librarySvc.StartReconciliation()
		if err != nil {
			return fmt.Errorf("start library reconciliation: %w", err)
		}
		stopLibraryReconciliation = func() {
			stopLibraryOnce.Do(func() { s.librarySvc.StopReconciliation(handle) })
		}
		defer stopLibraryReconciliation()
	}

	// ---- Ingress (starts only now, with every backend + callback ready) -----
	// ingressCtx is a child of the errgroup context: stopIngress cancels it to
	// halt the in-process group-dispatch loop WITHOUT cancelling workCtx, so River
	// keeps draining in-flight jobs with outbound deps alive. A peer crash cancels
	// gctx -> ingressCtx too, so unexpected-error teardown still holds.
	ingressCtx, stopGroupDispatch := context.WithCancel(gctx)
	defer stopGroupDispatch()

	// The listener is bound now but not served until every backend is up.
	listenAddr := adminListenAddress(adminHost, adminPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("admin listen: %w", err)
	}
	addr := ln.Addr().String()
	slog.Info("starting Web UI", "addr", addr)
	fmt.Printf("Web UI running at %s\n", adminURLForDisplay(adminHost, adminPort, addr))
	// The capability reservation owns the entire /webhooks/ namespace ahead of
	// all instrumentation. It sanitizes a disclosed capability into private
	// context and dispatches only canonical POSTs to the ingress handler (itself
	// OTel-wrapped with a sanitized URL); every other /webhooks/ shape gets an
	// opaque 404. Everything else falls through to the admin handler.
	capabilityIngress := observability.Handler(adminSrv.WebhookIngressHandler())
	httpSrv := &http.Server{Handler: server.WebhookCapabilityReservation(capabilityIngress, adminSrv.Handler())}

	// Group-dispatch acceptance loop.
	g.Go(func() error { return normalizeRunErr(groupDispatcher.Run(ingressCtx)) })
	// Helm enforces one replica with a Recreate rollout, so managed channel
	// pollers start unconditionally after their dependencies are wired. Drain-time
	// Quiesce stops new polling; the final Stop remains after River drains.
	if _, err := applyManagedChannelPlugins(ingressCtx, s.pluginHost); err != nil {
		_ = ln.Close()
		return fmt.Errorf("start managed channel runtimes: %w", err)
	}
	// HTTP serve — the final ingress source to come up.
	g.Go(func() error { return normalizeServeErr(httpSrv.Serve(ln)) })

	// Two-phase shutdown supervisor (runs OUTSIDE the errgroup). The first
	// SIGINT/SIGTERM starts a graceful drain; a second collapses to a hard stop.
	// A subsystem crash cancels gctx and is torn down without a readiness drain.
	// Started only now, with every subsystem up: a signal at any earlier point is
	// consumed by serverAction's startup watcher, which cancels the base context
	// so partially-started subsystems unwind through the error path — the
	// pre-existing abort-startup semantics. onServing hands signal ownership over.
	abortCtx, abortDrain := context.WithCancel(context.Background())
	defer abortDrain()
	drainer := &drainSequence{
		beginDrain: adminSrv.BeginDrain,
		// Stop ALL new ingress before HTTP drains and before the work context is
		// cancelled: group-dispatch acceptance, channel bot pollers, and every
		// River-periodic/new-dispatch source (scheduler, goal, embedding). River
		// workers then drain in-flight jobs with outbound deps still alive; no
		// periodic or new dispatch runs after this point.
		stopIngress: func() {
			stopGroupDispatch()         // group-dispatch acceptance
			quiesceChannelIngress()     // channel / plugin runtimes (polling only)
			stopSchedulerDispatch()     // scheduler periodic + one-time dispatch
			stopGoalDispatch()          // goal tick + dispatcher claims
			stopEmbeddingBackfill()     // embedding backfill periodic
			stopLibraryReconciliation() // Library recovery periodic
			stopMediaSweep()            // session media orphan sweep periodic
		},
		httpTimeout:  s.cfg.Lifecycle.HTTPShutdownTimeout,
		shutdownHTTP: httpSrv.Shutdown,
		forceClose:   func() { _ = httpSrv.Close() },
		// Accepted turns with no HTTP connection (channel messages, webhook
		// runs, scheduler run-now) finish inside the drain budget; expiry is
		// logged, not fatal — the hard stop below still bounds the process.
		waitAccepted: func(ctx context.Context) {
			if err := s.poolManager.WaitInFlight(ctx); err != nil {
				slog.Warn("graceful drain: accepted agent turns still in flight when the budget expired", "error.type", fmt.Sprintf("%T", err), "error.class", "drain_wait_failed")
			}
		},
		cancelWork: workCancel,
		abort:      abortCtx,
	}
	onServing()
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		superviseShutdown(gctx, sigCh, httpSrv, drainer, abortDrain)
	}()

	// All subsystems are started; /readyz may now report ready. Do this last,
	// immediately before blocking on the errgroup, so a probe never sees ready
	// while wiring is still in progress.
	adminSrv.MarkStartupComplete()

	waitErr := g.Wait()
	// The errgroup empties at the START of a graceful drain, not the end:
	// Shutdown closes the listener immediately (Serve returns ErrServerClosed)
	// and stopIngress cancels the group-dispatch loop, both long before
	// Shutdown's active-connection wait finishes. Returning here would race the
	// LIFO teardown defers (River, pools, embedded PostgreSQL, process exit)
	// against the in-flight HTTP work the drain budget exists to finish — so
	// join the supervisor first. Every supervisor path is bounded: the drain by
	// httpTimeout + forceClose + cancelWork, the crash path by its 2s Shutdown,
	// and a second signal collapses the wait via Close.
	<-drainDone
	slog.Info("gateway stopped")
	return waitErr
}

// normalizeRunErr maps a Stella-owned Run(ctx) component's shutdown error to
// nil: an orchestrated shutdown cancels the run context, so context.Canceled
// from the loop is expected, not a failure. Any other error propagates to the
// errgroup so it cancels peers and becomes the root error.
func normalizeRunErr(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// normalizeServeErr maps http.Server.Serve's expected close error to nil.
// http.ErrServerClosed means an orchestrated Shutdown/Close ran; anything else
// is a real serve failure that must cancel peers.
func normalizeServeErr(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("admin serve: %w", err)
}

// drainSequence runs the graceful shutdown steps in order. It is a struct of
// side-effect hooks so the ordering can be asserted in tests without a live
// server. The order is: flip not-ready + signal SSE (beginDrain) -> stop channel
// ingress (stopIngress) -> HTTP shutdown within the budget -> force-close
// leftovers -> cancel work contexts. Outbound dependencies (pools, notifier)
// stay alive until the final cancel, so work accepted before the drain can still
// complete and deliver.
//
// There is deliberately no in-process delay between not-ready and shutdown for
// load-balancer propagation: that window is the platform's job (Kubernetes
// preStop sleep), not the process's.
type drainSequence struct {
	beginDrain   func()
	stopIngress  func()
	httpTimeout  time.Duration
	shutdownHTTP func(context.Context) error
	forceClose   func()
	// waitAccepted, when non-nil, blocks until accepted agent turns that hold
	// no HTTP connection (channel messages, webhook runs, scheduler run-now)
	// have finished, bounded by the same budget as the HTTP drain (#744).
	waitAccepted func(context.Context)
	cancelWork   func()
	// abort, when non-nil, collapses the remaining drain budget when cancelled
	// (the second-signal hard stop), so both the HTTP drain and the
	// accepted-work wait unwind immediately.
	abort context.Context
}

func (d *drainSequence) run() {
	// 1. Flip readiness to not-ready and signal SSE streams to end. This is
	//    observable before the listener is touched (happens-before), so a probe
	//    can never see /readyz succeed once the drain has begun.
	d.beginDrain()
	// 2. Stop non-HTTP ingress (channel pollers) so no new inbound work starts,
	//    while outbound dependencies remain alive for work already accepted.
	if d.stopIngress != nil {
		d.stopIngress()
	}
	// 3. One budget covers the rest of the drain; a second signal collapses it.
	shutCtx, cancel := context.WithTimeout(context.Background(), d.httpTimeout)
	defer cancel()
	if d.abort != nil {
		stop := context.AfterFunc(d.abort, cancel)
		defer stop()
	}
	// 4. Stop accepting and drain in-flight HTTP within the budget; force-close
	//    anything still open when the budget is spent.
	if err := d.shutdownHTTP(shutCtx); err != nil {
		d.forceClose()
	}
	// 5. Wait for accepted non-HTTP turns within the remaining budget, while
	//    every outbound dependency (pools, notifier, channel runtimes) is still
	//    alive. Only after this may the work contexts be cancelled.
	if d.waitAccepted != nil {
		d.waitAccepted(shutCtx)
	}
	// 6. Cancel work contexts: gctx cancels, g.Wait returns, and the LIFO defer
	//    chain drains River and reverse-closes the subsystems.
	d.cancelWork()
}

// superviseShutdown blocks until the first signal (start graceful drain) or a
// subsystem crash cancelling gctx (hard teardown, no drain — prior semantics).
// During the drain a second signal aborts the remaining budget — abortDrain
// collapses the drain's shared budget context and Close unblocks the HTTP
// Shutdown wait — so it hard-stops immediately.
func superviseShutdown(gctx context.Context, sigCh <-chan os.Signal, httpSrv *http.Server, d *drainSequence, abortDrain func()) {
	select {
	case <-sigCh:
		// Graceful drain below.
		slog.Info("shutdown signal received; starting graceful drain")
	case <-gctx.Done():
		// A subsystem error cancelled the errgroup — not a drain. Mirror the prior
		// <-gctx.Done() -> Shutdown(2s) path: bounded HTTP shutdown, no readiness
		// drain and no drain budget, so Serve returns and g.Wait completes.
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return
	}
	// Watch for a second signal: force-closing the server aborts the in-flight
	// Shutdown wait, so the drain collapses to an immediate hard stop.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigCh:
			slog.Warn("second shutdown signal received; hard-stopping")
			abortDrain()
			_ = httpSrv.Close()
		case <-done:
		}
	}()
	d.run()
}

func schedulerJobContext(ctx context.Context, agentID string, job scheduler.Job) context.Context {
	if job.UserID != "" {
		ctx = authz.WithUserID(ctx, job.UserID)
	}
	if agentID != "" {
		ctx = authz.WithAgentID(ctx, agentID)
	}
	ctx = agent.WithExcludedTools(ctx, "scheduler")
	return ctx
}

func schedulerJobMessage(job scheduler.Job) string {
	return fmt.Sprintf("[Scheduled Task] %s\n\nInstruction: %s\n\nUse the notify tool to send results to the user only when you have something meaningful to communicate.", job.Name, job.Message)
}

func adminListenAddress(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

func adminBaseURL(host string, port int) string {
	h := host
	if h == "" {
		h = "localhost"
	}
	return "http://" + net.JoinHostPort(h, fmt.Sprintf("%d", port))
}

// resolveBaseURL returns the canonical base URL: the configured STELLA_BASE_URL
// (threaded in raw as baseURL, trailing slash trimmed here) when set, otherwise
// one derived from the admin bind host.
func resolveBaseURL(baseURL, adminHost string, adminPort int) string {
	if v := baseURL; v != "" {
		return strings.TrimRight(v, "/")
	}
	return adminBaseURL(adminHost, adminPort)
}

// warnDeploymentBaseURL warns when the canonical base URL used for OAuth
// callbacks and channel deep links cannot work off-box. When STELLA_BASE_URL is
// unset the URL is derived from the bind host, so a default (loopback) bind
// yields a base URL that points back at this machine. That is legitimate in
// every deployment shape (local browser, docker port publish, kubectl
// port-forward) and its failure mode is immediately visible in the login
// redirect — so this warns loudly instead of failing, and only when a feature
// that emits such links is actually configured. Kubernetes charts enforce
// STELLA_BASE_URL as a required value at the layer that knows it is behind an
// ingress.
func warnDeploymentBaseURL(baseURL, oidcIssuerURL string, oauthConfigured bool) {
	if !baseURLUnsafe(baseURL) {
		return
	}
	if linkDependentFeaturesConfigured(oidcIssuerURL, oauthConfigured) {
		slog.Warn("STELLA_BASE_URL is loopback/unspecified; OAuth callbacks and channel deep links will point back at this host and fail off-box. Set STELLA_BASE_URL to the public URL clients use", "base_url", baseURL)
	}
}

// baseURLUnsafe reports whether a base URL cannot serve as a public canonical
// address: it fails to parse, is not http(s), or resolves to a loopback,
// unspecified, or localhost host.
func baseURLUnsafe(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return true
	}
	host := u.Hostname()
	if host == "" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// linkDependentFeaturesConfigured reports whether a feature that emits absolute
// links back to this server (external OIDC or an OAuth login provider) is
// configured. The OIDC signal is the same OIDC_ISSUER_URL snapshot value that
// drives the setup mode decision; oauthConfigured is derived from the login
// config parsed once at the startup boundary, so both observe one generation.
func linkDependentFeaturesConfigured(oidcIssuerURL string, oauthConfigured bool) bool {
	return oidcIssuerURL != "" || oauthConfigured
}

func adminURLForDisplay(host string, port int, fallbackAddr string) string {
	displayHost := host
	if displayHost == "" {
		displayHost = hostFromAddr(fallbackAddr)
	}
	if displayHost == "" {
		displayHost = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(displayHost, fmt.Sprintf("%d", port))
}

func hostFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

func newIntentClassifier(snapshots config.SnapshotLoader, registry *providers.Registry) *channel.LLMIntentClassifier {
	if snapshots == nil || registry == nil {
		return nil
	}
	return channel.NewLLMIntentClassifier(
		func(ctx context.Context, agentID string) (*config.Snapshot, error) {
			return snapshots.Snapshot(ctx, agentID)
		},
		intentClassifierStreamFuncBuilder(registry),
	)
}

func intentClassifierStreamFuncBuilder(registry *providers.Registry) channel.StreamFuncBuilder {
	return func(_ context.Context, providerType string, creds config.ProviderCreds) (providers.StreamFunc, error) {
		return registry.BuildStream(providerType, providers.Config{APIKey: creds.APIKey, BaseURL: creds.BaseURL})
	}
}
