package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/CherryHQ/stella/internal/agent/prompt"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/sandbox"
	"github.com/CherryHQ/stella/internal/authz"
	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/sessionmedia"
	skillstool "github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/vault"
	"github.com/CherryHQ/stella/internal/vision"
	coreagent "github.com/CherryHQ/stella/pkg/agent"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/hooks"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	"github.com/CherryHQ/stella/pkg/tools"
	"github.com/CherryHQ/stella/plugins/core"
)

type (
	PluginContext        = agentruntime.PluginContext
	PluginContextBuilder = agentruntime.PluginContextBuilder
)

// MCPToolProvider surfaces external MCP-server tools from the runner's
// authority-bound plugin snapshot. Implemented by *mcp.ToolProvider; kept as
// an interface here so the agent package need not depend on MCP internals.
type MCPToolProvider interface {
	ToolsForSnapshot(ctx context.Context, snapshot plugin.Snapshot) ([]tools.Tool, error)
}

type ToolUnavailableReason string

const (
	// ToolUnavailableReasonEmailConfigRequired is emitted only after the email
	// availability predicate confirms the signed-in user has no EMAIL_CONFIG.
	ToolUnavailableReasonEmailConfigRequired ToolUnavailableReason = "email_config_required"
)

type BuiltinTool struct {
	Tool  tools.Tool
	Build func(pkgplugins.ToolBuildContext) (tools.Tool, error)
	Spec  tools.Definition
	// UnavailableReason is backend-owned metadata for a known unavailable
	// state. It is exposed only after Available returns false, never inferred
	// from a tool name by a client.
	UnavailableReason ToolUnavailableReason
	// Available reports whether this tool belongs in the runner's registry for
	// the given run. An error means the answer is unknown; callers must fail the
	// build instead of guessing, so a transient dependency outage can never be
	// cached as either a missing capability or a wrongly granted one.
	Available func(context.Context, RunnerParams) (bool, error)
}

func (b BuiltinTool) Definition() (tools.Definition, bool) {
	if b.Tool != nil {
		return b.Tool.Definition(), true
	}
	if b.Build != nil && b.Spec.Name != "" {
		return b.Spec, true
	}
	return tools.Definition{}, false
}

// SessionImagePipeline is the complete session image boundary. Both operations
// are owner-scoped: a group session's media belongs to the group, a direct
// session's to its user.
type SessionImagePipeline interface {
	Enrich(context.Context, sessionmedia.Owner, string, []ai.ContentBlock) ([]ai.ContentBlock, error)
	Load(context.Context, sessionmedia.Owner, string) (ai.ImageContent, error)
}

const runnerScratchDir = "runner-scratch"

// newRunnerScratch creates a disposable runner-owned child outside Home
// authority. Its structural parent is trusted host-owned state. Close and
// construction failure clean best-effort; crashes may leave operator-cleaned
// children. Isolating providers mount only the exact returned child.
func newRunnerScratch(stellaHome string) (string, func() error, error) {
	homeRoot, err := os.OpenRoot(stellaHome)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = homeRoot.Close() }()
	if err := homeRoot.Mkdir(runnerScratchDir, 0o700); err != nil && !os.IsExist(err) {
		return "", nil, err
	}

	root, err := homeRoot.OpenRoot(runnerScratchDir)
	if err != nil {
		return "", nil, err
	}
	info, lstatErr := homeRoot.Lstat(runnerScratchDir)
	openedInfo, statErr := root.Stat(".")
	if lstatErr != nil || statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return "", nil, fmt.Errorf("scratch root %q is not a directory", filepath.Join(stellaHome, runnerScratchDir))
	}
	if err := root.Chmod(".", 0o700); err != nil {
		_ = root.Close()
		return "", nil, err
	}

	var name string
	for range 100 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			_ = root.Close()
			return "", nil, err
		}
		name = "runner-" + hex.EncodeToString(random[:])
		if err := root.Mkdir(name, 0o700); err == nil {
			break
		} else if !os.IsExist(err) {
			_ = root.Close()
			return "", nil, err
		}
		name = ""
	}
	if name == "" {
		_ = root.Close()
		return "", nil, fmt.Errorf("create runner scratch: too many collisions")
	}
	dir := filepath.Join(stellaHome, runnerScratchDir, name)
	var once sync.Once
	var cleanupErr error
	cleanup := func() error {
		once.Do(func() {
			cleanupErr = errors.Join(root.RemoveAll(name), root.Close())
		})
		return cleanupErr
	}
	return dir, cleanup, nil
}

func BuiltinToolAvailable(_ context.Context, params RunnerParams) (bool, error) {
	return params.UserID != "" && params.AgentID != "", nil
}

func runnerPluginAuthority(params RunnerParams) (authz.Authority, error) {
	switch {
	case params.GroupID != "":
		return agentaccess.GroupAgentAuthority(params.GroupID, params.AgentID)
	case params.UserID != "" && params.AgentID != "":
		return agentaccess.WorkerAgentAuthority(params.UserID, params.AgentID)
	default:
		return authz.Authority{}, nil
	}
}

// runnerBuilderConfig holds all dependencies needed to assemble a NewRunnerFunc.
type runnerBuilderConfig struct {
	Snap                  *config.Snapshot
	BuiltinTools          []BuiltinTool
	ToolMetaRegistry      *toolmeta.Registry
	PluginToolsBuilder    PluginToolsBuilder
	ProviderStreamBuilder ProviderStreamBuilder
	SandboxBackends       *sandbox.BackendRegistry
	PromptSectionsBuilder PromptSectionsBuilder
	PluginContextBuilder  PluginContextBuilder
	PluginHooksBuilder    PluginHooksBuilder
	ToolLifecycleBuilder  ToolLifecycleBuilder
	SkillRevisionReader   skillstool.RuntimeReader
	SkillReadAuthorizer   skillstool.SkillReadAuthorizer
	MCPToolProvider       MCPToolProvider
	ToolOverrideFetcher   ToolOverrideFetcher
	ToolLifecycle         *coreagent.ToolLifecycle
	SandboxBackendFn      func(ctx context.Context) string
	CoreRuntimePlan       *core.RuntimePlan
	VaultEnvLoader        sandbox.VaultEnvLoader
	TokenManager          *oauth.TokenManager
	ProjectResolver       ProjectResolverFunc
	SessionImages         SessionImagePipeline
	GroupRosterLoader     func(context.Context, string, string) prompt.GroupRoster
	Home                  home.Workspace
	CodeToolSurface       coreagent.CodeToolSurface
}

// canonicalImageConfig is the session image policy every runner gets, group or
// direct. The owner is derived from the session identity at call time, so the
// one rule (a group owns its media, otherwise the user does) lives here and
// nowhere else, and a session with no UUID principal fails at the image rather
// than at runner construction.
func canonicalImageConfig(images SessionImagePipeline, params RunnerParams) *coreagent.CanonicalImageConfig {
	if images == nil {
		return &coreagent.CanonicalImageConfig{
			Load: func(context.Context, string) (ai.ImageContent, error) {
				return ai.ImageContent{}, fmt.Errorf("session image loader is not configured")
			},
			CanonicalizeToolResult: func(_ context.Context, result ai.ToolResultMessage) (ai.ToolResultMessage, error) {
				if ai.HasImage(result.Content) {
					return ai.ToolResultMessage{}, fmt.Errorf("session image enrichment is not configured")
				}
				return result, nil
			},
		}
	}
	return &coreagent.CanonicalImageConfig{
		Load: func(ctx context.Context, mediaID string) (ai.ImageContent, error) {
			owner, err := sessionmedia.SessionOwner(params.UserID, params.GroupID)
			if err != nil {
				return ai.ImageContent{}, err
			}
			return images.Load(ctx, owner, mediaID)
		},
		CanonicalizeToolResult: func(ctx context.Context, result ai.ToolResultMessage) (ai.ToolResultMessage, error) {
			if !ai.HasImage(result.Content) {
				return result, nil
			}
			owner, err := sessionmedia.SessionOwner(params.UserID, params.GroupID)
			if err != nil {
				return ai.ToolResultMessage{}, err
			}
			blocks, err := images.Enrich(ctx, owner, params.AgentID, result.Content)
			if err != nil {
				return ai.ToolResultMessage{}, err
			}
			result.Content = blocks
			return result, nil
		},
	}
}

// newRunnerFunc assembles a NewRunnerFunc for a given config snapshot.
// The returned func creates runners scoped to one agent's provider, model,
// workspace, and system prompt. Memory provider, user ID, and agent ID are
// injected per-session from RunnerParams. Runner execution is always user-scoped,
// so per-user workspace directories are created for every runner instance.
//
// Plugin hooks are built per runner from its captured plugin snapshot. Stable
// core hooks are injected via RunnerParams.HooksFn by the Pool.
func newRunnerFunc(cfg runnerBuilderConfig) NewRunnerFunc {
	if cfg.CodeToolSurface == "" {
		cfg.CodeToolSurface = coreagent.CodeToolSurfaceHot
	}
	return func(ctx context.Context, params RunnerParams) (built Runner, err error) {
		var scratchCleanup func() error
		var pluginHooks []hooks.HookPlugin
		runnerOwnsPluginHooks := false
		defer func() {
			if err != nil && scratchCleanup != nil {
				_ = scratchCleanup()
			}
			if err != nil && !runnerOwnsPluginHooks {
				closeHookPlugins(pluginHooks)
			}
		}()
		modelRef := params.Model
		if modelRef == "" {
			modelRef = cfg.Snap.Model
		}

		// Parse provider/model from the ref string.
		provID, modelID := config.ParseModelRef(modelRef)
		if provID == "" {
			provID = cfg.Snap.Provider
		}
		creds := cfg.Snap.ResolveProviderCreds(provID)
		apiName := creds.Type
		if apiName == "" {
			apiName = provID
		}
		providerID := creds.ProviderID
		if providerID == "" {
			providerID = provID
		}

		if params.GuestID != "" {
			return newRunner(ctx, runnerConfig{
				NoCapabilities: true,
				Provider: providerConfig{
					ProviderID: providerID,
					API:        apiName,
					Model:      modelID,
					Input:      cfg.Snap.ModelInput(provID, modelID),
					Cost:       cfg.Snap.ModelCost(provID, modelID),
					APIKey:     creds.APIKey,
					BaseURL:    creds.BaseURL,
					Builder:    cfg.ProviderStreamBuilder,
				},
				Thinking: params.Thinking,
				System:   prompt.BuildGuestSystemPrompt(cfg.Snap.SystemPrompt),
			})
		}
		if cfg.Home == nil {
			return nil, fmt.Errorf("runner: Home workspace resolver is required")
		}

		view, err := cfg.Home.WorkspaceView(ctx, home.WorkspaceRequest{UserID: params.UserID, GroupID: params.GroupID, AgentID: params.AgentID})
		if err != nil {
			return nil, fmt.Errorf("resolve Home workspace: %w", err)
		}
		var (
			userRoot      string
			workspaceRoot string
			userDataDir   string
			// projectValidateRoot is the per-(principal, agent) dir a project must
			// live under: a project is owned by the agent (see #442), so it stays
			// scoped to the agent's subdir of the shared user/group home.
			projectValidateRoot string
		)
		if params.UserID != "" || params.GroupID != "" {
			userRoot = view.PrincipalRoot
			workspaceRoot, userDataDir = view.AgentRoot, view.DataRoot
			projectValidateRoot = workspaceRoot
		} else {
			if params.ProjectID != "" {
				return nil, fmt.Errorf("runner: user-less runs cannot use a project")
			}
			userRoot, scratchCleanup, err = newRunnerScratch(config.StellaHome())
			if err != nil {
				return nil, fmt.Errorf("create user-less scratch: %w", err)
			}
			workspaceRoot, projectValidateRoot = userRoot, userRoot
		}

		// Resolve project directory when session has a project.
		var projectRoot string
		var projectSkillSnapshot *skillstool.ProjectSnapshot
		var projectContext prompt.ProjectContext
		var descriptor ProjectDescriptor
		if params.ProjectID != "" {
			if cfg.ProjectResolver == nil {
				if scratchCleanup != nil {
					_ = scratchCleanup()
				}
				return nil, fmt.Errorf("runner: project resolver is required")
			}
			projectSnapshot, snapshotErr := SnapshotAuthorizedProject(ctx, cfg.ProjectResolver, cfg.Home, params.ProjectID, params.UserID, params.AgentID)
			err = snapshotErr
			if err != nil {
				if scratchCleanup != nil {
					_ = scratchCleanup()
				}
				return nil, fmt.Errorf("runner: resolve project %q: %w", params.ProjectID, err)
			}
			descriptor, projectContext, projectSkillSnapshot = projectSnapshot.Descriptor, projectSnapshot.Context, projectSnapshot.Skills
			if descriptor.ID != params.ProjectID || descriptor.UserID != params.UserID || descriptor.AgentID != params.AgentID {
				if scratchCleanup != nil {
					_ = scratchCleanup()
				}
				return nil, fmt.Errorf("runner: project %q is outside the agent workspace", params.ProjectID)
			}
			projectRoot = projectValidateRoot
			if descriptor.Path != "." {
				projectRoot = filepath.Join(projectValidateRoot, filepath.FromSlash(descriptor.Path))
			}
			if ValidateProjectDir(projectRoot, projectValidateRoot) != nil {
				return nil, fmt.Errorf("runner: invalid project descriptor")
			}
		}

		// Extract memory provider from params (typed as any to avoid circular imports).
		var memProvider memory.Provider
		if params.Memory != nil {
			memProvider, _ = params.Memory.(memory.Provider)
		}

		pluginContext := PluginContext{}
		hasPluginAuthority := false
		if cfg.PluginContextBuilder != nil {
			authority, authorityErr := runnerPluginAuthority(params)
			err = authorityErr
			if err != nil {
				return nil, fmt.Errorf("runner: build plugin authority: %w", err)
			}
			if authority.Valid() {
				pluginContext, err = cfg.PluginContextBuilder(ctx, authority, params.AgentID)
				if err != nil {
					return nil, fmt.Errorf("runner: build plugin context: %w", err)
				}
				hasPluginAuthority = true
			}
		}
		pluginView := pluginContext.SessionPluginView()
		backendName := config.SandboxBackendLocal
		if cfg.SandboxBackendFn != nil {
			if selected := cfg.SandboxBackendFn(ctx); selected != "" {
				backendName = selected
			}
		}
		disabledSkillRefs := slices.Clone(cfg.Snap.DisabledSkillRefs)
		if backendName != config.SandboxBackendDocker {
			disabledSkillRefs = append(disabledSkillRefs, core.UnavailableSkillRefs()...)
		}
		promptBuild := pkgplugins.SystemPromptContext{
			UserID:              params.UserID,
			AgentID:             params.AgentID,
			RegisteredPluginIDs: slices.Clone(pluginView.RegisteredPluginIDs),
			EnabledPluginIDs:    slices.Clone(pluginView.ExposedPluginIDs),
			DisabledSkillRefs:   slices.Clone(disabledSkillRefs),
		}
		var sections []pkgplugins.SystemPromptSection
		if hasPluginAuthority && cfg.PromptSectionsBuilder != nil {
			sections, err = cfg.PromptSectionsBuilder(ctx, promptBuild, pluginContext.Snapshot())
			if err != nil {
				return nil, fmt.Errorf("runner: build prompt sections: %w", err)
			}
		}
		skillPromptBuild := promptBuild
		if params.GroupID != "" {
			skillPromptBuild.UserID = ""
		}
		skillsSection, err := skillstool.BuildAuthorizedPromptSection(ctx, skillPromptBuild, projectSkillSnapshot, cfg.SkillRevisionReader, cfg.SkillReadAuthorizer)
		if err != nil {
			return nil, fmt.Errorf("runner: build skills prompt: %w", err)
		}
		if skillsSection.Title != "" && skillsSection.Content != "" {
			sections = append(sections, skillsSection)
		}
		if params.GroupID == "" && cfg.VaultEnvLoader != nil {
			metas, err := cfg.VaultEnvLoader.ListAmbientSecretMetas(ctx, params.UserID, params.AgentID)
			if err != nil {
				slog.Warn("vault secret metadata unavailable",
					"component", "runner_builder",
					"user_id", params.UserID,
					"agent_id", params.AgentID,
					"project_id", params.ProjectID,
					"error", err,
				)
			} else if len(metas) > 0 {
				sections = append(sections, pkgplugins.SystemPromptSection{
					Title:   "Available Secrets",
					Content: "These vault secret names are already available as environment variables in bash. Values are never shown; use the names exactly as the CLI or tool expects.\n\n" + formatAvailableSecretMetas(metas),
				})
			}
		}

		var groupRoster prompt.GroupRoster
		if params.GroupID != "" && cfg.GroupRosterLoader != nil {
			groupRoster = cfg.GroupRosterLoader(ctx, params.GroupID, params.AgentID)
		}

		// Build the full system prompt per-session with profile from memory provider.
		// Group sessions skip private profile injection (D9 isolation); group memory
		// is Phase 3 concern.
		promptUserID := params.UserID
		if params.GroupID != "" {
			promptUserID = ""
		}
		system := prompt.BuildSystemPromptFromDB(ctx, prompt.DBPromptParams{
			SystemPrompt:   cfg.Snap.SystemPrompt,
			AgentSoul:      cfg.Snap.Soul,
			Memory:         memProvider,
			UserID:         promptUserID,
			AgentID:        params.AgentID,
			GroupID:        params.GroupID,
			GroupRoster:    groupRoster,
			ProjectContext: projectContext,
			Sections:       sections,
		})

		// Resolve hooks from RunnerParams — injected by Pool, not the builder.
		if hasPluginAuthority && cfg.PluginHooksBuilder != nil {
			pluginHooks, err = cfg.PluginHooksBuilder(ctx, pluginContext.Snapshot())
			if err != nil {
				return nil, fmt.Errorf("runner: build plugin hooks: %w", err)
			}
		}
		hookPlugins := pluginHooks
		if params.HooksFn != nil {
			hookPlugins = append(hookPlugins, params.HooksFn()...)
		}
		pluginToolsBuilder := cfg.PluginToolsBuilder
		if !hasPluginAuthority {
			pluginToolsBuilder = nil
		}
		toolLifecycle := cfg.ToolLifecycle
		if !hasPluginAuthority {
			toolLifecycle = nil
		}
		if hasPluginAuthority && cfg.ToolLifecycleBuilder != nil {
			toolLifecycle, err = cfg.ToolLifecycleBuilder(ctx, pluginContext.Snapshot())
			if err != nil {
				return nil, fmt.Errorf("runner: build tool lifecycle: %w", err)
			}
		}

		sessionSecretValues := sandbox.NewSessionSecretValues()
		sandboxCfg := sandbox.Config{
			SandboxConfig:    cfg.Snap.Sandbox,
			SandboxBackendFn: cfg.SandboxBackendFn,
			CoreRuntimePlan:  cfg.CoreRuntimePlan,
			Backends:         cfg.SandboxBackends,
			Paths: sandbox.Paths{
				StellaHome:    config.StellaHome(),
				AgentRoot:     cfg.Snap.Workspace,
				UserRoot:      userRoot,
				WorkspaceRoot: workspaceRoot,
				UserDataDir:   userDataDir,
				ProjectRoot:   projectRoot,
			},
			UserID:              params.UserID,
			GroupID:             params.GroupID,
			AgentID:             params.AgentID,
			SessionID:           params.SessionID,
			ProjectID:           params.ProjectID,
			SessionEnvSpecs:     slices.Clone(pluginView.SessionEnvSpecs),
			BinarySpecs:         slices.Clone(pluginView.BinarySpecs),
			VaultEnvLoader:      cfg.VaultEnvLoader,
			SessionSecretValues: sessionSecretValues,
			TokenManager:        cfg.TokenManager,
			OAuthEnvBindings:    sandbox.NewOAuthEnvBindings(),
		}

		builtinTools := append([]BuiltinTool(nil), cfg.BuiltinTools...)
		perRunTools := append([]tools.Tool(nil), params.ExtraTools...)

		canonicalImages := canonicalImageConfig(cfg.SessionImages, params)

		// Ownership transfers to newRunner before the call. It closes the
		// hooks on every construction error, including provider setup failures;
		// this defer handles errors that occur before that handoff.
		runnerOwnsPluginHooks = true
		built, err = newRunner(ctx, runnerConfig{
			Provider: providerConfig{
				ProviderID: providerID,
				API:        apiName,
				Model:      modelID,
				Input:      cfg.Snap.ModelInput(provID, modelID),
				Cost:       cfg.Snap.ModelCost(provID, modelID),
				APIKey:     creds.APIKey,
				BaseURL:    creds.BaseURL,
				Builder:    cfg.ProviderStreamBuilder,
			},
			Thinking:             params.Thinking,
			Sandbox:              sandboxCfg,
			System:               system,
			Sections:             sections,
			BuiltinTools:         builtinTools,
			BuiltinParams:        params,
			DisabledSkillRefs:    slices.Clone(disabledSkillRefs),
			PerRunTools:          perRunTools,
			SkillRevisionReader:  cfg.SkillRevisionReader,
			ProjectSkillSnapshot: projectSkillSnapshot,
			SkillReadAuthorizer:  cfg.SkillReadAuthorizer,
			PluginContext:        pluginContext,
			ToolOverrideFetcher:  cfg.ToolOverrideFetcher,
			ToolMetaRegistry:     cfg.ToolMetaRegistry,
			PluginTools:          pluginToolsBuilder,
			HookPlugins:          hookPlugins,
			PluginHookPlugins:    pluginHooks,
			ToolLifecycle:        toolLifecycle,
			MCPToolProvider:      cfg.MCPToolProvider,
			CodeToolSurface:      cfg.CodeToolSurface,
			DelegateRunner:       params.DelegateRunner,
			DelegateTimeout:      cfg.Snap.Runner.DelegateTimeoutDuration(),
			CanonicalImages:      canonicalImages,
			// Resolved from the factory's snapshot. A vision-settings write rebuilds
			// pool factories, so future runners use the current auxiliary service
			// while already admitted runners finish against their captured configuration.
			Vision:  vision.NewFromSnapshot(cfg.Snap, vision.StreamBuilder(cfg.ProviderStreamBuilder)),
			Cleanup: scratchCleanup,
		})
		return built, err
	}
}

func formatAvailableSecretMetas(metas []vault.AmbientSecretMeta) string {
	lines := make([]string, 0, len(metas))
	for _, meta := range metas {
		line := meta.Name
		if meta.Description != "" {
			line += " — " + meta.Description
		}
		lines = append(lines, line)
	}
	return "- " + strings.Join(lines, "\n- ")
}
