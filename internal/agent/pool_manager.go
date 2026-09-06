package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/agent/prompt"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/sandbox"
	"github.com/CherryHQ/stella/internal/agent/session"
	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	skillstool "github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/skill/policy"
	coreagent "github.com/CherryHQ/stella/pkg/agent"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/hooks"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	"github.com/CherryHQ/stella/pkg/tools"
	core "github.com/CherryHQ/stella/plugins/core"
)

// PromptSectionsBuilder builds prompt sections from the runner's authority-bound
// plugin snapshot.
type PromptSectionsBuilder func(ctx context.Context, build pkgplugins.SystemPromptContext, snapshot plugin.Snapshot) ([]pkgplugins.SystemPromptSection, error)

// PluginHooksBuilder creates hooks from the runner's authority-bound plugin
// snapshot. Hooks belong to the runner generation that built them.
type PluginHooksBuilder func(ctx context.Context, snapshot plugin.Snapshot) ([]hooks.HookPlugin, error)

// ToolLifecycleBuilder creates the per-run core tool lifecycle from the same
// authority-bound snapshot as tools and hooks.
type ToolLifecycleBuilder func(ctx context.Context, snapshot plugin.Snapshot) (*coreagent.ToolLifecycle, error)

// PluginToolsBuilder creates tools from the runner's authority-bound plugin
// snapshot. The snapshot is the final argument so callers cannot accidentally
// use an ambient or independently resolved plugin state.
type PluginToolsBuilder func(ctx context.Context, build pkgplugins.ToolBuildContext, snapshot plugin.Snapshot) ([]tools.Tool, error)

type (
	BeforeRunBuilder      func(ctx context.Context, build pkgplugins.BeforeRunContext, snapshot plugin.Snapshot) (pkgplugins.BeforeRunResult, error)
	ProviderStreamBuilder func(api, apiKey, baseURL string) (providers.StreamFunc, error)
)

// PoolManagerOption configures a PoolManager.
type PoolManagerOption func(*PoolManager)

// WithCodeToolSurface selects the Code Mode provider-visible treatment. The
// production default remains the established hot-tool surface.
func WithCodeToolSurface(surface coreagent.CodeToolSurface) PoolManagerOption {
	return func(pm *PoolManager) {
		if surface == "" {
			surface = coreagent.CodeToolSurfaceHot
		}
		pm.codeToolSurface = surface
	}
}

// WithCoreRuntimePlan supplies the startup-prepared release runtime selection
// to every runner factory. Docker leaves this unset because its Linux runtime
// artifacts are owned by the container preparation path.
func WithCoreRuntimePlan(plan *core.RuntimePlan) PoolManagerOption {
	return func(pm *PoolManager) { pm.coreRuntimePlan = plan }
}

// WithSnapshotLoader overrides the loader used for per-agent Snapshots. The
// composition root passes the credential-aware loader so every runner factory
// resolves per-Agent Provider key overrides. A nil loader leaves the base store.
func WithSnapshotLoader(loader config.SnapshotLoader) PoolManagerOption {
	return func(pm *PoolManager) {
		if loader != nil {
			pm.snapshots = loader
		}
	}
}

func WithCompactionPM(cfg CompactionConfig) PoolManagerOption {
	return func(pm *PoolManager) { pm.compaction = cfg }
}

func WithBuiltinTools(tools []BuiltinTool) PoolManagerOption {
	return func(pm *PoolManager) { pm.builtinTools = tools }
}

// WithToolMetaRegistry supplies the declarations of the generated tools this
// build registers. Family and legacy selectors resolve through it, so a runner
// built without one falls back to exact-name matching rather than guessing a
// family from the underscores in a name.
func WithToolMetaRegistry(reg *toolmeta.Registry) PoolManagerOption {
	return func(pm *PoolManager) { pm.toolMetaRegistry = reg }
}

func WithPluginToolsBuilder(b PluginToolsBuilder) PoolManagerOption {
	return func(pm *PoolManager) { pm.pluginToolsBuilder = b }
}

func WithPluginHooksBuilder(b PluginHooksBuilder) PoolManagerOption {
	return func(pm *PoolManager) { pm.pluginHooksBuilder = b }
}

// WithCoreHooks registers server-level hooks (e.g. the OTel trace hook) that
// live for the whole PoolManager lifetime. Unlike plugin hooks they are never
// rebuilt or closed on reload, so in-flight runners can keep calling them; they
// are closed exactly once in Close.
func WithCoreHooks(h []hooks.HookPlugin) PoolManagerOption {
	return func(pm *PoolManager) { pm.coreHooks = h }
}

func WithPromptSectionsBuilder(b PromptSectionsBuilder) PoolManagerOption {
	return func(pm *PoolManager) { pm.promptSectionsBuilder = b }
}

func WithPluginContextBuilder(b PluginContextBuilder) PoolManagerOption {
	return func(pm *PoolManager) { pm.pluginContextBuilder = b }
}

func WithBeforeRunBuilderPM(b BeforeRunBuilder) PoolManagerOption {
	return func(pm *PoolManager) { pm.beforeRunBuilder = b }
}

func WithToolLifecyclePM(tl *coreagent.ToolLifecycle) PoolManagerOption {
	return func(pm *PoolManager) { pm.toolLifecycle = tl }
}

func WithToolLifecycleBuilder(b ToolLifecycleBuilder) PoolManagerOption {
	return func(pm *PoolManager) { pm.toolLifecycleBuilder = b }
}

func WithProviderStreamBuilder(b ProviderStreamBuilder) PoolManagerOption {
	return func(pm *PoolManager) { pm.providerStreamBuilder = b }
}

// WithSandboxBackends supplies the compiled-in sandbox backend registry.
func WithSandboxBackends(backends *sandbox.BackendRegistry) PoolManagerOption {
	return func(pm *PoolManager) { pm.sandboxBackends = backends }
}

func WithSkillRevisionReader(r skillstool.RuntimeReader) PoolManagerOption {
	return func(pm *PoolManager) { pm.skillRevisionReader = r }
}

// WithSkillReadAuthorizer injects Skill domain read access into every runner's
// skills tool, so DB-backed reads (load/search_installed) are authorized.
func WithSkillReadAuthorizer(a skillstool.SkillReadAuthorizer) PoolManagerOption {
	return func(pm *PoolManager) { pm.skillReadAuthz = a }
}

func WithToolOverrideFetcher(f ToolOverrideFetcher) PoolManagerOption {
	return func(pm *PoolManager) { pm.toolOverrideFetcher = f }
}

func WithProjectResolver(r ProjectResolverFunc) PoolManagerOption {
	return func(pm *PoolManager) { pm.projectResolver = r }
}

// WithHomeWorkspace supplies the sole persistent Home resolver for runners.
func WithHomeWorkspace(v home.Workspace) PoolManagerOption {
	return func(pm *PoolManager) { pm.homeWorkspace = v }
}

// WithSessionImagePipeline wires the ordinary-session canonical image boundary.
// Groups deliberately bypass it until their separate ownership model exists.
func WithSessionImagePipeline(images SessionImagePipeline) PoolManagerOption {
	return func(pm *PoolManager) { pm.sessionImages = images }
}

// WithSessionInboxPM injects durable Agent-to-Session input persistence into
// every service. The inbox does not own execution; turnqueue remains caller-driven.
func WithSessionInboxPM(inbox SessionInbox) PoolManagerOption {
	return func(pm *PoolManager) { pm.sessionInbox = inbox }
}

// WithGroupRosterLoader projects group membership into a group runner prompt so
// an agent knows which name in the transcript is its own.
func WithGroupRosterLoader(loader func(context.Context, string, string) prompt.GroupRoster) PoolManagerOption {
	return func(pm *PoolManager) { pm.groupRosterLoader = loader }
}

// PoolManager manages one Service per enabled agent. It reads enabled agents
// from the config Store and creates a Service (session.Registry + runtime.Runtime)
// per agent.
type PoolManager struct {
	services map[string]*Service
	store    config.Store
	// snapshots loads the per-agent config Snapshot. It is the credential-aware
	// loader when one is wired (overlaying per-Agent Provider key overrides),
	// otherwise it falls back to store. Kept separate from store so only the
	// Snapshot read is decorated; GetAgent and the rest stay on the base store.
	snapshots config.SnapshotLoader
	mem       memory.Provider
	// lifecycle serializes process-local service publication/removal and retained
	// Home owner fences with synchronous runner admission.
	lifecycle                    *lifecycleGate
	mu                           sync.RWMutex
	closing                      bool
	closed                       bool
	startAgentBuiltHook          func(*Service)
	runnerFuncRefreshedHook      func(*Service, *config.Snapshot)
	syncAgentBeforeLifecycleHook func()
	// started is set true when StartAll runs. The one-shot pre-start binds
	// (Bind* below) refuse to run once started, while the dynamic reconfigure
	// surface (ReloadPlugin*/SyncAgent/Invalidate*) stays available afterward.
	started               bool
	idleTimeout           time.Duration
	compaction            CompactionConfig
	builtinTools          []BuiltinTool
	toolMetaRegistry      *toolmeta.Registry
	pluginToolsBuilder    PluginToolsBuilder
	hookPlugins           []hooks.HookPlugin
	coreHooks             []hooks.HookPlugin
	pluginHooksBuilder    PluginHooksBuilder
	promptSectionsBuilder PromptSectionsBuilder
	pluginContextBuilder  PluginContextBuilder
	beforeRunBuilder      BeforeRunBuilder
	toolLifecycle         *coreagent.ToolLifecycle
	toolLifecycleBuilder  ToolLifecycleBuilder
	providerStreamBuilder ProviderStreamBuilder
	sandboxBackends       *sandbox.BackendRegistry
	skillRevisionReader   skillstool.RuntimeReader
	skillReadAuthz        skillstool.SkillReadAuthorizer
	mcpToolProvider       MCPToolProvider
	toolOverrideFetcher   ToolOverrideFetcher
	vaultEnvLoader        sandbox.VaultEnvLoader
	projectResolver       ProjectResolverFunc
	tokenManager          *oauth.TokenManager
	oauthRegistry         *oauth.ProviderRegistry
	sessionImages         SessionImagePipeline
	sessionAccess         SessionAccessService
	sessionInbox          SessionInbox
	groupRosterLoader     func(context.Context, string, string) prompt.GroupRoster
	codeToolSurface       coreagent.CodeToolSurface
	coreRuntimePlan       *core.RuntimePlan
	homeWorkspace         home.Workspace
	log                   *slog.Logger
}

func NewPoolManager(store config.Store, mem memory.Provider, opts ...PoolManagerOption) *PoolManager {
	pm := &PoolManager{
		services:        make(map[string]*Service),
		store:           store,
		snapshots:       store,
		mem:             mem,
		lifecycle:       newLifecycleGate(),
		idleTimeout:     10 * time.Minute,
		codeToolSurface: coreagent.CodeToolSurfaceHot,
		log:             slog.With("component", "pool_manager"),
	}
	for _, opt := range opts {
		opt(pm)
	}
	if pm.snapshots == nil {
		pm.snapshots = store
	}
	return pm
}

// BindOAuthRegistry binds the OAuth provider registry before StartAll. It is a
// one-shot pre-start bind: it rejects a nil registry (missing), a second bind
// (duplicate), and any bind after StartAll (late).
func (pm *PoolManager) BindOAuthRegistry(r *oauth.ProviderRegistry) error {
	if r == nil {
		return errors.New("agent: BindOAuthRegistry requires a non-nil registry")
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.started {
		return errors.New("agent: BindOAuthRegistry after StartAll")
	}
	if pm.oauthRegistry != nil {
		return errors.New("agent: OAuth registry already bound")
	}
	pm.oauthRegistry = r
	if pm.tokenManager != nil {
		pm.tokenManager.SetRegistry(r)
	}
	return nil
}

// BindVaultEnvLoader binds the sandbox vault env loader (and the derived OAuth
// token manager) before StartAll. One-shot pre-start bind: rejects nil
// (missing), a second bind (duplicate), and any bind after StartAll (late).
// Because it runs before agents start, no runner rebuild is needed.
func (pm *PoolManager) BindVaultEnvLoader(v sandbox.VaultEnvLoader) error {
	if v == nil {
		return errors.New("agent: BindVaultEnvLoader requires a non-nil loader")
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.started {
		return errors.New("agent: BindVaultEnvLoader after StartAll")
	}
	if pm.vaultEnvLoader != nil {
		return errors.New("agent: vault env loader already bound")
	}
	pm.vaultEnvLoader = v
	if vs, ok := v.(oauth.VaultStore); ok {
		pm.tokenManager = oauth.NewTokenManager(vs)
		if pm.oauthRegistry != nil {
			pm.tokenManager.SetRegistry(pm.oauthRegistry)
		}
	}
	return nil
}

// BindSessionAccess binds the shared Session PEP before StartAll. All non-HTTP
// entry session lifecycle in Service goes through this port.
func (pm *PoolManager) BindSessionAccess(access SessionAccessService) error {
	if access == nil {
		return errors.New("agent: BindSessionAccess requires a non-nil service")
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.started {
		return errors.New("agent: BindSessionAccess after StartAll")
	}
	if pm.sessionAccess != nil {
		return errors.New("agent: session access already bound")
	}
	pm.sessionAccess = access
	return nil
}

// BindMCPToolProvider binds the MCP tool provider before StartAll. One-shot
// pre-start bind: rejects nil (missing), a second bind (duplicate), and any bind
// after StartAll (late). No runner rebuild is needed because agents have not yet
// started.
func (pm *PoolManager) BindMCPToolProvider(p MCPToolProvider) error {
	if p == nil {
		return errors.New("agent: BindMCPToolProvider requires a non-nil provider")
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.started {
		return errors.New("agent: BindMCPToolProvider after StartAll")
	}
	if pm.mcpToolProvider != nil {
		return errors.New("agent: MCP tool provider already bound")
	}
	pm.mcpToolProvider = p
	return nil
}

// HookPlugins returns the stable process-level hooks. User plugin hooks are
// built per runner from that runner's authority-bound plugin snapshot.
func (pm *PoolManager) HookPlugins() []hooks.HookPlugin {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make([]hooks.HookPlugin, 0, len(pm.coreHooks))
	out = append(out, pm.coreHooks...)
	return out
}

// StartAll reads enabled agents from the store and creates a Service per agent.
func (pm *PoolManager) StartAll(ctx context.Context) error {
	// Seal the pre-start binds: after this point the static Vault/MCP/OAuth
	// capabilities and builtin tools are fixed. StartAll is one-shot.
	pm.mu.Lock()
	if pm.started {
		pm.mu.Unlock()
		return errors.New("agent: PoolManager.StartAll called more than once")
	}
	if pm.sessionAccess == nil {
		pm.mu.Unlock()
		return errors.New("agent: PoolManager.StartAll requires SessionAccess")
	}
	if pm.homeWorkspace == nil {
		pm.mu.Unlock()
		return errors.New("agent: PoolManager.StartAll requires Home workspace")
	}
	pm.started = true
	pm.mu.Unlock()

	agents, err := pm.store.ListEnabledAgents(ctx)
	if err != nil {
		pm.log.Warn("could not list agents at startup", "error", err)
		return nil
	}
	if len(agents) == 0 {
		pm.log.Info("no enabled agents found, pool manager started empty")
		return nil
	}

	for _, ag := range agents {
		if err := pm.startAgent(ctx, ag); err != nil {
			pm.log.Error("failed to start agent", "agent_id", ag.ID, "error", err)
			continue
		}
	}

	pm.mu.RLock()
	count := len(pm.services)
	pm.mu.RUnlock()

	if count == 0 {
		pm.log.Warn("agents found but none could be started")
		return nil
	}

	pm.log.Info("all agents started", "count", count)
	return nil
}

func (pm *PoolManager) startAgent(ctx context.Context, ag config.Agent) error {
	if err := pm.lifecycle.lockExclusive(ctx); err != nil {
		return err
	}
	defer pm.lifecycle.unlockExclusive()
	return pm.startAgentLocked(ctx, ag)
}

// startAgentLocked builds and publishes one Service while the caller owns the
// process lifecycle gate exclusively.
func (pm *PoolManager) startAgentLocked(ctx context.Context, ag config.Agent) error {
	snap, workspace, err := pm.loadAgentSnapshot(ctx, ag.ID)
	if err != nil {
		return err
	}

	factory := pm.buildRunnerFunc(ctx, snap)

	svc, err := pm.buildService(ctx, ag.ID, factory, snap)
	if err != nil {
		return fmt.Errorf("build service for agent %q: %w", ag.ID, err)
	}
	if pm.startAgentBuiltHook != nil {
		pm.startAgentBuiltHook(svc)
	}
	published := false
	defer func() {
		if !published {
			_ = svc.Runtime.Close()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := pm.store.GetAgent(ctx, ag.ID)
	if err != nil {
		return fmt.Errorf("revalidate agent before service publication: %w", err)
	}
	if !current.Enabled {
		return fmt.Errorf("agent %q became disabled before service publication", ag.ID)
	}
	if err := pm.rebuildRunnerFuncForServiceLocked(ctx, ag.ID, svc); err != nil {
		return fmt.Errorf("refresh service snapshot for agent %q: %w", ag.ID, err)
	}
	pm.mu.Lock()
	if pm.closing || pm.closed {
		pm.mu.Unlock()
		return errPoolManagerClosing
	}
	if pm.services[ag.ID] != nil {
		pm.mu.Unlock()
		return fmt.Errorf("agent: service %q is already published", ag.ID)
	}
	pm.services[ag.ID] = svc
	published = true
	pm.mu.Unlock()

	pm.log.Info("agent started", "agent_id", ag.ID, "workspace", workspace)
	return nil
}

var errPoolManagerClosing = errors.New("agent: PoolManager is closing")

func (pm *PoolManager) buildService(ctx context.Context, agentID string, factory NewRunnerFunc, snap *config.Snapshot) (*Service, error) {
	reg, err := session.NewRegistry(pm.mem, agentID)
	if err != nil {
		return nil, fmt.Errorf("build session registry for %q: %w", agentID, err)
	}

	cfg := agentruntime.Config{
		NewRunner:       factory,
		Memory:          pm.mem,
		IdleTimeout:     pm.idleTimeout,
		DefaultModel:    snap.ResolveModelID(config.ModelTierNormal),
		DefaultThinking: snap.ResolveThinkingLevel(config.ModelTierNormal),
		HooksFn:         pm.HookPlugins,
		BeforeRun:       pm.runtimeBeforeRunFunc(snap),
		SnapshotPrompt:  pm.buildSnapshotPromptFunc(snap),
		SessionImages:   pm.sessionImages,
		Compaction: agentruntime.CompactionConfig{
			MaxTokens: pm.compaction.WithDefaults().MaxTokens,
			KeepTail:  pm.compaction.WithDefaults().KeepTail,
		},
	}
	rt, err := agentruntime.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("build runtime for %q: %w", agentID, err)
	}
	go rt.StartReaper(ctx)

	pm.mu.RLock()
	sessionAccess := pm.sessionAccess
	pm.mu.RUnlock()
	if sessionAccess == nil {
		return nil, errors.New("session access is not bound")
	}
	svc := &Service{Sessions: reg, Runtime: rt, SessionAccess: sessionAccess, SessionInbox: pm.sessionInbox, AgentID: agentID, lifecycle: pm.lifecycle}
	rt.SetDelegateRunner(svc)
	return svc, nil
}

// promptScope computes only the logical profile subject. Group sessions blank
// the prompt UserID so a group identifier is never treated as a human profile.
func (pm *PoolManager) promptScope(info session.Info) (promptUserID, groupID string) {
	if info.GroupID != "" {
		return "", info.GroupID
	}
	return info.UserID, ""
}

func (pm *PoolManager) promptSections(ctx context.Context, snap *config.Snapshot, info session.Info, projectSkills *skillstool.ProjectSnapshot, pluginContext PluginContext) ([]pkgplugins.SystemPromptSection, error) {
	pluginView := pluginContext.SessionPluginView()
	promptBuild := pkgplugins.SystemPromptContext{
		UserID:              info.UserID,
		AgentID:             info.AgentID,
		RegisteredPluginIDs: slices.Clone(pluginView.RegisteredPluginIDs),
		EnabledPluginIDs:    slices.Clone(pluginView.ExposedPluginIDs),
		DisabledSkillRefs:   slices.Clone(snap.DisabledSkillRefs),
	}
	var sections []pkgplugins.SystemPromptSection
	if pm.promptSectionsBuilder != nil {
		var err error
		sections, err = pm.promptSectionsBuilder(ctx, promptBuild, pluginContext.Snapshot())
		if err != nil {
			return nil, fmt.Errorf("build prompt sections: %w", err)
		}
	}
	skillBuild := promptBuild
	if info.GroupID != "" {
		skillBuild.UserID = ""
	}
	skillsSection, err := skillstool.BuildAuthorizedPromptSection(ctx, skillBuild, projectSkills, pm.skillRevisionReader, pm.skillReadAuthz)
	if err != nil {
		return nil, fmt.Errorf("build skills prompt: %w", err)
	}
	if skillsSection.Title != "" && skillsSection.Content != "" {
		sections = append(sections, skillsSection)
	}
	return sections, nil
}

func (pm *PoolManager) buildSnapshotPromptFunc(snap *config.Snapshot) agentruntime.SnapshotPromptFunc {
	return func(ctx context.Context, info session.Info, ss memory.SessionSnapshot, pluginContext PluginContext) (string, error) {
		// Keep an addressable copy so version zero remains an explicit snapshot.
		version := ss.Version
		promptUserID, groupID := pm.promptScope(info)
		var projectContext prompt.ProjectContext
		var projectSkills *skillstool.ProjectSnapshot
		if info.ProjectID != "" {
			if pm.projectResolver == nil {
				return "", errors.New("project resolver is not configured")
			}
			projectSnapshot, err := SnapshotAuthorizedProject(ctx, pm.projectResolver, pm.homeWorkspace, info.ProjectID, info.UserID, info.AgentID)
			if err != nil {
				return "", err
			}
			projectContext, projectSkills = projectSnapshot.Context, projectSnapshot.Skills
		}
		sections, err := pm.promptSections(ctx, snap, info, projectSkills, pluginContext)
		if err != nil {
			return "", err
		}

		return prompt.BuildSystemPromptFromDB(ctx, prompt.DBPromptParams{
			SystemPrompt:    snap.SystemPrompt,
			AgentSoul:       snap.Soul,
			Memory:          pm.mem,
			UserID:          promptUserID,
			AgentID:         info.AgentID,
			GroupID:         groupID,
			Sections:        sections,
			ProjectContext:  projectContext,
			SnapshotVersion: &version,
		}), nil
	}
}

func (pm *PoolManager) runtimeBeforeRunFunc(_ *config.Snapshot) agentruntime.BeforeRunFunc {
	return func(ctx context.Context, info session.Info, model, msgText, system string, history []ai.Message, pluginContext PluginContext) (string, error) {
		if pm.beforeRunBuilder == nil {
			return system, nil
		}
		result, err := pm.beforeRunBuilder(ctx, pkgplugins.BeforeRunContext{
			SessionID:    info.ID,
			Channel:      info.Channel,
			UserID:       info.UserID,
			AgentID:      info.AgentID,
			Model:        model,
			MessageText:  msgText,
			SystemPrompt: system,
			History:      slices.Clone(history),
		}, pluginContext.Snapshot())
		if err != nil {
			return "", err
		}
		if result.SystemPrompt == "" {
			return system, nil
		}
		return result.SystemPrompt, nil
	}
}

// GetService returns the Service for the given agent ID, or nil if not found.
func (pm *PoolManager) GetService(agentID string) *Service {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.services[agentID]
}

// Default returns any service (first found).
func (pm *PoolManager) Default() *Service {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, svc := range pm.services {
		return svc
	}
	return nil
}

// ReloadPluginTools rebuilds the runner factory for every service.
func (pm *PoolManager) ReloadPluginTools(ctx context.Context) error {
	if pm.pluginToolsBuilder == nil {
		return nil
	}

	pm.mu.RLock()
	agentIDs := make([]string, 0, len(pm.services))
	for id := range pm.services {
		agentIDs = append(agentIDs, id)
	}
	pm.mu.RUnlock()

	for _, agentID := range agentIDs {
		if err := pm.rebuildRunnerFunc(ctx, agentID); err != nil {
			pm.log.Error("failed to rebuild factory after plugin reload", "agent_id", agentID, "error", err)
		}
	}

	pm.log.Info("plugin tools reloaded")
	return nil
}

// ReloadModelDefaults rebuilds every runner factory from a fresh snapshot so
// newly admitted runners see the current deployment-wide default models. Every
// agent is rebuilt, not just the ones with no override: an agent inherits field
// by field, so a default change can move a single tier under an otherwise
// fully-configured agent.
func (pm *PoolManager) ReloadModelDefaults(ctx context.Context) error {
	pm.mu.RLock()
	agentIDs := make([]string, 0, len(pm.services))
	for id := range pm.services {
		agentIDs = append(agentIDs, id)
	}
	pm.mu.RUnlock()

	for _, agentID := range agentIDs {
		if err := pm.rebuildRunnerFunc(ctx, agentID); err != nil {
			pm.log.Error("failed to rebuild factory after default model reload", "agent_id", agentID, "error", err)
		}
	}

	pm.log.Info("default models reloaded")
	return nil
}

// ReloadPluginHooks refreshes runner factories so future runners build hooks
// from their own frozen plugin snapshots. Existing admitted runners retain the
// hook generation captured at construction.
func (pm *PoolManager) ReloadPluginHooks(ctx context.Context) error {
	if pm.pluginHooksBuilder == nil {
		return nil
	}
	pm.mu.RLock()
	ids := make([]string, 0, len(pm.services))
	for id := range pm.services {
		ids = append(ids, id)
	}
	pm.mu.RUnlock()
	sort.Strings(ids)
	if len(ids) == 0 {
		// Keep the pre-start hook generation available for a manager that has no
		// services yet. It is never exposed to a runner; once services exist,
		// each runner receives hooks from its own snapshot.
		hookPlugins, err := pm.pluginHooksBuilder(ctx, plugin.Snapshot{})
		if err != nil {
			return fmt.Errorf("build plugin hooks: %w", err)
		}
		pm.mu.Lock()
		oldPlugins := pm.hookPlugins
		pm.hookPlugins = hookPlugins
		pm.mu.Unlock()
		closeHookPlugins(oldPlugins)
		return nil
	}
	for _, agentID := range ids {
		if err := pm.rebuildRunnerFunc(ctx, agentID); err != nil {
			pm.log.Error("failed to rebuild factory after plugin hook reload", "agent_id", agentID, "error", err)
		}
	}
	pm.log.Info("plugin hooks reloaded", "runner_count", len(ids))
	return nil
}

// ReloadProviders rebuilds the runner factory for every service.
func (pm *PoolManager) ReloadProviders(ctx context.Context) error {
	pm.mu.RLock()
	agentIDs := make([]string, 0, len(pm.services))
	for id := range pm.services {
		agentIDs = append(agentIDs, id)
	}
	pm.mu.RUnlock()

	for _, agentID := range agentIDs {
		if err := pm.rebuildRunnerFunc(ctx, agentID); err != nil {
			pm.log.Error("failed to rebuild factory after provider reload", "agent_id", agentID, "error", err)
		}
	}

	pm.log.Info("providers reloaded")
	return nil
}

// rebuildRunnerFunc serializes a configuration-triggered factory rebuild with
// policy commit/admission. Snapshot loading must happen inside the barrier: a
// pre-commit snapshot installed after policy invalidation would fail open.
func (pm *PoolManager) rebuildRunnerFunc(ctx context.Context, agentID string) error {
	if err := pm.lifecycle.lockShared(ctx); err != nil {
		return err
	}
	defer pm.lifecycle.unlockShared()
	pm.mu.RLock()
	svc := pm.services[agentID]
	pm.mu.RUnlock()
	if svc == nil {
		_, _, err := pm.loadAgentSnapshot(ctx, agentID)
		return err
	}
	if err := svc.admissionMu.Lock(ctx); err != nil {
		return err
	}
	defer svc.admissionMu.Unlock()
	if err := pm.rebuildRunnerFuncForServiceLocked(ctx, agentID, svc); err != nil {
		return err
	}
	// A rebuilt factory is a new configuration boundary. reset preserves
	// admitted/reserved runners as stale and retires only idle ones.
	return svc.Runtime.ResetRunners()
}

// rebuildRunnerFuncForServiceLocked installs a snapshot-derived factory only
// into svc. The caller owns svc's admission barrier; this intentionally does
// not re-read the service map after DB I/O, so a replacement cannot receive a
// stale factory by identity drift.
func (pm *PoolManager) rebuildRunnerFuncForServiceLocked(ctx context.Context, agentID string, svc *Service) error {
	snap, _, err := pm.loadAgentSnapshot(ctx, agentID)
	if err != nil {
		return err
	}
	factory := pm.buildRunnerFunc(ctx, snap)
	svc.Runtime.SetNewRunner(factory)
	svc.Runtime.SetDefaultModel(snap.ResolveModelID(config.ModelTierNormal), snap.ResolveThinkingLevel(config.ModelTierNormal))
	svc.Runtime.SetHooks(pm.HookPlugins)
	svc.Runtime.SetPromptBuilders(pm.runtimeBeforeRunFunc(snap), pm.buildSnapshotPromptFunc(snap))
	if pm.runnerFuncRefreshedHook != nil {
		pm.runnerFuncRefreshedHook(svc, snap)
	}
	return nil
}

// SyncAgent reloads one agent's configuration. If the agent was deleted or
// disabled, its service is closed and removed. Otherwise the factory and
// runners are rebuilt.
func (pm *PoolManager) SyncAgent(ctx context.Context, agentID string) error {
	if pm.homeWorkspace == nil {
		return errors.New("agent: PoolManager.SyncAgent requires Home workspace")
	}
	if pm.syncAgentBeforeLifecycleHook != nil {
		pm.syncAgentBeforeLifecycleHook()
	}
	if err := pm.lifecycle.lockExclusive(ctx); err != nil {
		return err
	}
	defer pm.lifecycle.unlockExclusive()

	pm.mu.RLock()
	closing := pm.closing || pm.closed
	pm.mu.RUnlock()
	if closing {
		return errPoolManagerClosing
	}
	ag, err := pm.store.GetAgent(ctx, agentID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ag.Enabled) {
		return pm.removeAgentLocked(agentID)
	}
	if err != nil {
		return fmt.Errorf("agent: refresh durable Agent %q: %w", agentID, err)
	}
	pm.mu.RLock()
	svc := pm.services[agentID]
	pm.mu.RUnlock()
	if svc == nil {
		return pm.startAgentLocked(ctx, ag)
	}
	if err := pm.rebuildRunnerFuncForServiceLocked(ctx, agentID, svc); err != nil {
		return err
	}
	if err := svc.Runtime.ResetRunners(); err != nil {
		pm.log.Warn("failed to reset runners after sync", "agent_id", agentID, "error", err)
	}
	pm.log.Info("agent reloaded", "agent_id", agentID)
	return nil
}

func (pm *PoolManager) removeAgent(agentID string) error {
	_ = pm.lifecycle.lockExclusive(context.Background())
	defer pm.lifecycle.unlockExclusive()
	return pm.removeAgentLocked(agentID)
}

func (pm *PoolManager) removeAgentLocked(agentID string) error {
	pm.mu.RLock()
	svc := pm.services[agentID]
	pm.mu.RUnlock()
	if svc == nil {
		return nil
	}
	pm.log.Info("removing agent service", "agent_id", agentID)
	err := svc.Runtime.Close()
	pm.mu.Lock()
	if pm.services[agentID] == svc {
		delete(pm.services, agentID)
	}
	pm.mu.Unlock()
	return err
}

func (pm *PoolManager) loadAgentSnapshot(ctx context.Context, agentID string) (*config.Snapshot, string, error) {
	if pm.homeWorkspace == nil {
		return nil, "", errors.New("agent: load snapshot requires Home workspace")
	}
	view, err := pm.homeWorkspace.WorkspaceView(ctx, home.WorkspaceRequest{AgentID: agentID})
	if err != nil {
		return nil, "", fmt.Errorf("setup Agent definition workspace for %q: %w", agentID, err)
	}
	workspace := view.AgentRoot
	snap, err := pm.snapshots.Snapshot(ctx, agentID)
	if err != nil {
		return nil, "", fmt.Errorf("load snapshot for agent %q: %w", agentID, err)
	}
	snap.Workspace = workspace
	return snap, workspace, nil
}

// buildRunnerFunc assembles a NewRunnerFunc with builtin tools and external plugin tools.
func (pm *PoolManager) buildRunnerFunc(_ context.Context, snap *config.Snapshot) NewRunnerFunc {
	pm.mu.RLock()
	builtinTools := append([]BuiltinTool{}, pm.builtinTools...)
	pm.mu.RUnlock()

	sandboxBackendFn := func(context.Context) string { return config.ActiveSandboxBackend() }
	return newRunnerFunc(runnerBuilderConfig{
		Snap:                  snap,
		BuiltinTools:          builtinTools,
		ToolMetaRegistry:      pm.toolMetaRegistry,
		PluginToolsBuilder:    pm.pluginToolsBuilder,
		ProviderStreamBuilder: pm.providerStreamBuilder,
		SandboxBackends:       pm.sandboxBackends,
		PromptSectionsBuilder: pm.promptSectionsBuilder,
		PluginContextBuilder:  pm.pluginContextBuilder,
		PluginHooksBuilder:    pm.pluginHooksBuilder,
		ToolLifecycleBuilder:  pm.toolLifecycleBuilder,
		SkillRevisionReader:   pm.skillRevisionReader,
		SkillReadAuthorizer:   pm.skillReadAuthz,
		MCPToolProvider:       pm.mcpToolProvider,
		ToolOverrideFetcher:   pm.toolOverrideFetcher,
		ToolLifecycle:         pm.toolLifecycle,
		SandboxBackendFn:      sandboxBackendFn,
		VaultEnvLoader:        pm.vaultEnvLoader,
		TokenManager:          pm.tokenManager,
		ProjectResolver:       pm.projectResolver,
		SessionImages:         pm.sessionImages,
		GroupRosterLoader:     pm.groupRosterLoader,
		Home:                  pm.homeWorkspace,
		CodeToolSurface:       pm.codeToolSurface,
		CoreRuntimePlan:       pm.coreRuntimePlan,
	})
}

// AddBuiltinTool appends a builtin tool before StartAll. It is a one-shot
// pre-start bind: it rejects a nil tool, a duplicate tool name, and any add
// after StartAll (post-start rejection), since the builtin-tool set is sealed
// once agents start. Runtime tool changes go through the plugin-tool path
// (pluginToolsBuilder / ReloadPluginTools), not here.
func (pm *PoolManager) AddBuiltinTool(_ context.Context, tool tools.Tool) error {
	if tool == nil {
		return errors.New("agent: AddBuiltinTool requires a non-nil tool")
	}
	name := tool.Definition().Name
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.started {
		return fmt.Errorf("agent: AddBuiltinTool(%q) after StartAll", name)
	}
	for _, bt := range pm.builtinTools {
		if definition, ok := bt.Definition(); ok && definition.Name == name {
			return fmt.Errorf("agent: builtin tool %q already registered", name)
		}
	}
	pm.builtinTools = append(pm.builtinTools, BuiltinTool{Tool: tool})
	return nil
}

// InvalidateUser closes all live runners for userID across all services.
func (pm *PoolManager) InvalidateUser(userID string) error {
	_ = pm.lifecycle.lockShared(context.Background())
	defer pm.lifecycle.unlockShared()
	pm.mu.RLock()
	services := make(map[string]*Service, len(pm.services))
	maps.Copy(services, pm.services)
	pm.mu.RUnlock()

	var lastErr error
	for _, svc := range services {
		_ = svc.admissionMu.Lock(context.Background())
		err := svc.Runtime.ResetRunnersForUser(userID)
		svc.admissionMu.Unlock()
		if err != nil {
			pm.log.Error("reset runners for user", "user_id", userID, "error", err)
			lastErr = err
		}
	}
	return lastErr
}

// InvalidateUserAgent closes live runners for one user on one agent.
func (pm *PoolManager) InvalidateUserAgent(userID, agentID string) error {
	_ = pm.lifecycle.lockShared(context.Background())
	defer pm.lifecycle.unlockShared()
	pm.mu.RLock()
	svc, ok := pm.services[agentID]
	pm.mu.RUnlock()
	if !ok {
		return nil
	}
	_ = svc.admissionMu.Lock(context.Background())
	defer svc.admissionMu.Unlock()
	if err := svc.Runtime.ResetRunnersForUser(userID); err != nil {
		pm.log.Error("reset runners for user agent", "user_id", userID, "agent_id", agentID, "error", err)
		return err
	}
	return nil
}

// InvalidateAgent closes all live runners for one agent across every user.
func (pm *PoolManager) InvalidateAgent(agentID string) error {
	_ = pm.lifecycle.lockShared(context.Background())
	defer pm.lifecycle.unlockShared()
	pm.mu.RLock()
	svc, ok := pm.services[agentID]
	pm.mu.RUnlock()
	if !ok {
		return nil
	}
	_ = svc.admissionMu.Lock(context.Background())
	defer svc.admissionMu.Unlock()
	if err := svc.Runtime.ResetRunners(); err != nil {
		pm.log.Error("reset runners for agent", "agent_id", agentID, "error", err)
		return err
	}
	return nil
}

// homeOwnerFenceLease retains process-wide lifecycle exclusion through the
// owner transaction.
type homeOwnerFenceLease struct {
	pm      *PoolManager
	kind    home.OwnerKind
	ownerID string
	service *Service
	once    sync.Once
}

// Commit applies the only post-commit structural effect: an Agent's closed
// Service is unpublished after, never before, its durable owner row is gone.
func (l *homeOwnerFenceLease) Commit() {
	if l.kind != home.OwnerAgent {
		return
	}
	if l.service == nil {
		return
	}
	svc := l.service
	if err := svc.Runtime.Close(); err != nil {
		l.pm.log.Error("close committed deleted Agent service", "agent_id", l.ownerID, "error", err)
	}
	l.pm.mu.Lock()
	if l.pm.services[l.ownerID] == svc {
		delete(l.pm.services, l.ownerID)
	}
	l.pm.mu.Unlock()
}

func (l *homeOwnerFenceLease) Release() {
	l.once.Do(func() {
		l.pm.lifecycle.unlockExclusive()
	})
}

// AcquireHomeOwnerFence closes matching cached execution while retaining
// process-local lifecycle exclusion through the caller's database commit. Home
// owner gates are acquired by the caller after this lease, preserving the fixed
// lifecycle -> Home process gate -> PostgreSQL lock order.
func (pm *PoolManager) AcquireHomeOwnerFence(ctx context.Context, kind home.OwnerKind, ownerID string) (home.OwnerFenceLease, error) {
	if ownerID == "" {
		return nil, errors.New("agent: Home owner ID is required")
	}
	if kind != home.OwnerUser && kind != home.OwnerGroup && kind != home.OwnerAgent {
		return nil, fmt.Errorf("agent: unsupported Home owner %q", kind)
	}
	if err := pm.lifecycle.lockExclusive(ctx); err != nil {
		return nil, err
	}
	pm.mu.RLock()
	services := make([]*Service, 0, len(pm.services))
	var agentService *Service
	if kind == home.OwnerAgent {
		agentService = pm.services[ownerID]
		if agentService != nil {
			services = append(services, agentService)
		}
	} else {
		for _, svc := range pm.services {
			services = append(services, svc)
		}
	}
	pm.mu.RUnlock()

	lease := &homeOwnerFenceLease{pm: pm, kind: kind, ownerID: ownerID, service: agentService}
	var fenceErr error
	for _, svc := range services {
		match := func(info session.Info) bool { return matchesHomeOwner(info, kind, ownerID) }
		if kind == home.OwnerAgent {
			match = func(session.Info) bool { return true }
		}
		if err := svc.Runtime.TerminalCloseWhere(match); err != nil {
			pm.log.Error("terminal close Home owner runners", "owner", ownerID, "agent_id", svc.AgentID, "error", err)
			fenceErr = err
		}
	}
	if fenceErr != nil {
		lease.Release()
		return nil, fenceErr
	}
	return lease, nil
}

// matchesHomeOwner identifies sessions owned by a deleted principal. Agent
// deletion removes the whole service through removeAgent, not this predicate.
func matchesHomeOwner(info session.Info, kind home.OwnerKind, id string) bool {
	switch kind {
	case home.OwnerUser:
		return info.UserID == id && info.GroupID == ""
	case home.OwnerGroup:
		return info.GroupID == id
	default:
		return false
	}
}

// ApplyAgentSkillPolicyMutation commits mutate and makes every locally
// published Service observe that committed policy before a subsequent turn can
// use its old factory. It owns the complete orchestration so callers cannot
// accidentally call a barrier-assuming invalidator without the barrier.
//
// A refresh failure is deliberately logged but not returned after mutate has
// committed: the database response remains truthful while the target Service is
// poisoned fail-closed until a later successful refresh.
func (pm *PoolManager) ApplyAgentSkillPolicyMutation(agentID string, mutate func() error) error {
	return pm.applyAgentSkillPolicyMutation(agentID, mutate, pm.refreshAgentSkillPolicyForServiceLocked)
}

func (pm *PoolManager) applyAgentSkillPolicyMutation(agentID string, mutate func() error, refresh func(string, *Service) error) error {
	_ = pm.lifecycle.lockShared(context.Background())
	defer pm.lifecycle.unlockShared()
	pm.mu.RLock()
	svc := pm.services[agentID]
	pm.mu.RUnlock()
	if svc == nil {
		// Publication needs lifecycle exclusion. A later start therefore loads
		// the committed policy after this shared lease is released.
		return mutate()
	}
	_ = svc.admissionMu.Lock(context.Background())
	defer svc.admissionMu.Unlock()
	if err := mutate(); err != nil {
		if errors.Is(err, policy.ErrCommitOutcomeUnknown) {
			if refreshErr := refresh(agentID, svc); refreshErr != nil {
				pm.log.Error("reconcile unknown Agent Skill policy commit", "agent_id", agentID, "error", refreshErr)
			}
		}
		return err
	}
	if err := refresh(agentID, svc); err != nil {
		pm.log.Error("refresh committed Agent Skill policy", "agent_id", agentID, "error", err)
	}
	return nil
}

// refreshAgentSkillPolicyForServiceLocked refreshes one exact Service while
// its admission barrier is held. It never consults the service map after its
// snapshot read, preventing a replacement from receiving stale policy bytes.
func (pm *PoolManager) refreshAgentSkillPolicyForServiceLocked(agentID string, svc *Service) error {
	if err := pm.rebuildRunnerFuncForServiceLocked(context.Background(), agentID, svc); err != nil {
		svc.Runtime.SetNewRunner(func(context.Context, agentruntime.RunnerParams) (agentruntime.Runner, error) {
			return nil, fmt.Errorf("reload Agent Skill policy: %w", err)
		})
		if invalidateErr := svc.Runtime.InvalidateSkillPolicy(); invalidateErr != nil {
			pm.log.Warn("invalidate runners after failed Agent Skill policy refresh", "agent_id", agentID, "error", invalidateErr)
		}
		return err
	}
	return svc.Runtime.InvalidateSkillPolicy()
}

// InvalidateAll closes every live runner across all services.
func (pm *PoolManager) InvalidateAll() error {
	_ = pm.lifecycle.lockShared(context.Background())
	defer pm.lifecycle.unlockShared()
	pm.mu.RLock()
	services := make(map[string]*Service, len(pm.services))
	maps.Copy(services, pm.services)
	pm.mu.RUnlock()

	var lastErr error
	for id, svc := range services {
		_ = svc.admissionMu.Lock(context.Background())
		err := svc.Runtime.ResetRunners()
		svc.admissionMu.Unlock()
		if err != nil {
			pm.log.Error("reset runners for service", "agent_id", id, "error", err)
			lastErr = err
		}
	}
	return lastErr
}

// WaitInFlight blocks until no runtime has an in-flight chat turn or ctx
// expires, returning ctx's error on expiry. Graceful shutdown calls it after
// ingress has stopped and HTTP has drained, so accepted turns that hold no
// HTTP connection (channel messages, webhook runs, scheduler run-now) finish
// before the work contexts are cancelled (#744). It snapshots the service set
// once: ingress is already stopped, so no new agent service can be minted
// while it waits.
func (pm *PoolManager) WaitInFlight(ctx context.Context) error {
	pm.mu.RLock()
	services := make([]*Service, 0, len(pm.services))
	for _, svc := range pm.services {
		services = append(services, svc)
	}
	pm.mu.RUnlock()

	for _, svc := range services {
		if err := svc.Runtime.WaitTurns(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Close shuts down all services and hook plugins.
func (pm *PoolManager) Close() error {
	_ = pm.lifecycle.lockExclusive(context.Background())
	defer pm.lifecycle.unlockExclusive()
	pm.mu.Lock()
	if pm.closed {
		pm.mu.Unlock()
		return nil
	}
	pm.closing = true
	services := make(map[string]*Service, len(pm.services))
	maps.Copy(services, pm.services)
	hookPlugins := pm.hookPlugins
	pm.hookPlugins = nil
	coreHooks := pm.coreHooks
	pm.coreHooks = nil
	pm.mu.Unlock()

	var lastErr error
	for id, svc := range services {
		pm.log.Info("closing agent service", "agent_id", id)
		if err := svc.Runtime.Close(); err != nil {
			pm.log.Error("failed to close service runtime", "agent_id", id, "error", err)
			lastErr = err
		}
	}
	pm.mu.Lock()
	for id, svc := range services {
		if pm.services[id] == svc {
			delete(pm.services, id)
		}
	}
	pm.closed = true
	pm.mu.Unlock()
	closeHookPlugins(hookPlugins)
	// Core hooks (trace) are closed last so their end-of-session spans flush
	// after every runtime has stopped producing new ones.
	closeHookPlugins(coreHooks)
	if pm.mem != nil {
		if err := pm.mem.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func closeHookPlugins(plugins []hooks.HookPlugin) {
	for _, plugin := range plugins {
		closer, ok := plugin.(io.Closer)
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil {
			slog.Warn("failed to close hook", "name", plugin.Name(), "error", err)
		}
	}
}
