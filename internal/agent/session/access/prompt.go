package access

import (
	"context"
	"fmt"
	"slices"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/agent/prompt"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	agentsession "github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/skill"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/plugins/core"
)

// SystemPromptInput is the transport-owned identity and route tuple for the
// session system-prompt read use case.
type SystemPromptInput struct {
	Authority authz.Authority
	AgentID   string
	SessionID string
}

// AgentSystemPrompt reads one agent's configured system prompt. It is a func
// rather than a port because the builder's other config-shaped deps (Projects,
// Skills) already are, and because a failure here is non-fatal: the prompt is
// simply built without the agent's own instructions.
type AgentSystemPrompt func(ctx context.Context, agentID string) (string, error)

// ConfigAgentSystemPrompt reads the system prompt from the deployment config store.
func ConfigAgentSystemPrompt(store config.Store) AgentSystemPrompt {
	return func(ctx context.Context, agentID string) (string, error) {
		agentCfg, err := store.GetAgent(ctx, agentID)
		if err != nil {
			return "", err
		}
		return agentCfg.SystemPrompt, nil
	}
}

type PromptSkillSectionBuilder func(context.Context, pkgplugins.SystemPromptContext, *skill.ProjectSnapshot) (pkgplugins.SystemPromptSection, error)

type PromptSectionsBuilder func(context.Context, pkgplugins.SystemPromptContext, plugin.Snapshot) ([]pkgplugins.SystemPromptSection, error)

type SystemPromptBuildInput struct {
	Info agentsession.Info
}

type SystemPromptDeps struct {
	Memory                memory.Provider
	Agents                AgentSystemPrompt
	Projects              agent.ProjectResolverFunc
	Workspace             home.RootOpener
	PluginContextBuilder  agentruntime.PluginContextBuilder
	PromptSectionsBuilder PromptSectionsBuilder
	Skills                PromptSkillSectionBuilder
	// SandboxBackendFn lets prompt construction preserve bundled declarations
	// for Docker, whose Linux image is the authority on runtime availability.
	// Other backends filter against the current host's trusted assets.
	SandboxBackendFn func(context.Context) string
}

// SystemPromptBuilder assembles a session's effective system prompt from the
// deps it was constructed with. Its deps are the seams; the builder itself has
// exactly one behaviour.
type SystemPromptBuilder struct {
	deps SystemPromptDeps
}

func NewSystemPromptBuilder(deps SystemPromptDeps) (*SystemPromptBuilder, error) {
	missing := ""
	if deps.Memory == nil {
		missing = appendMissing(missing, "Memory")
	}
	if deps.Agents == nil {
		missing = appendMissing(missing, "Agents")
	}
	if deps.Projects == nil {
		missing = appendMissing(missing, "Projects")
	}
	if deps.Workspace == nil {
		missing = appendMissing(missing, "Workspace")
	}
	if deps.PluginContextBuilder == nil {
		missing = appendMissing(missing, "PluginContextBuilder")
	}
	if deps.PromptSectionsBuilder == nil {
		missing = appendMissing(missing, "PromptSectionsBuilder")
	}
	if deps.Skills == nil {
		missing = appendMissing(missing, "Skills")
	}
	if missing != "" {
		return nil, fmt.Errorf("session prompt builder: missing %s", missing)
	}
	return &SystemPromptBuilder{deps: deps}, nil
}

func appendMissing(current, next string) string {
	if current == "" {
		return next
	}
	return current + ", " + next
}

func promptPluginAuthority(info agentsession.Info) (authz.Authority, error) {
	switch {
	case info.GuestID != "":
		return authz.Authority{}, nil
	case info.GroupID != "":
		return agentaccess.GroupAgentAuthority(info.GroupID, info.AgentID)
	case info.UserID != "" && info.AgentID != "":
		return agentaccess.WorkerAgentAuthority(info.UserID, info.AgentID)
	default:
		return authz.Authority{}, nil
	}
}

func (b *SystemPromptBuilder) BuildSessionSystemPrompt(ctx context.Context, in SystemPromptBuildInput) (string, error) {
	info := in.Info
	ctx = authz.WithAgentID(ctx, info.AgentID)
	if info.GroupID != "" {
		ctx = authz.WithGroupID(ctx, info.GroupID)
	} else if info.UserID != "" {
		ctx = authz.WithUserID(ctx, info.UserID)
	}
	var agentPrompt string
	if info.AgentID != "" {
		agentPrompt, _ = b.deps.Agents(ctx, info.AgentID)
	}

	var projectContext prompt.ProjectContext
	var projectSkills *skill.ProjectSnapshot
	if info.UserID != "" && info.ProjectID != "" {
		projectSnapshot, err := agent.SnapshotAuthorizedProject(ctx, b.deps.Projects, b.deps.Workspace, info.ProjectID, info.UserID, info.AgentID)
		if err != nil {
			return "", fmt.Errorf("%w: project context: %w", ErrUnavailable, err)
		}
		projectContext, projectSkills = projectSnapshot.Context, projectSnapshot.Skills
	}

	pluginContext := agentruntime.PluginContext{}
	hasPluginAuthority := false
	if info.GuestID == "" {
		authority, err := promptPluginAuthority(info)
		if err != nil {
			return "", fmt.Errorf("%w: plugin authority: %w", ErrUnavailable, err)
		}
		if authority.Valid() {
			pluginContext, err = b.deps.PluginContextBuilder(ctx, authority, info.AgentID)
			if err != nil {
				return "", fmt.Errorf("%w: plugin context: %w", ErrUnavailable, err)
			}
			hasPluginAuthority = true
		}
	}
	pluginView := pluginContext.SessionPluginView()
	backendName := config.SandboxBackendLocal
	if b.deps.SandboxBackendFn != nil {
		if selected := b.deps.SandboxBackendFn(ctx); selected != "" {
			backendName = selected
		}
	}
	var disabledSkillRefs []string
	if backendName != config.SandboxBackendDocker {
		disabledSkillRefs = core.UnavailableSkillRefs()
	}
	promptBuild := pkgplugins.SystemPromptContext{
		UserID:              info.UserID,
		AgentID:             info.AgentID,
		RegisteredPluginIDs: slices.Clone(pluginView.RegisteredPluginIDs),
		EnabledPluginIDs:    slices.Clone(pluginView.ExposedPluginIDs),
		DisabledSkillRefs:   disabledSkillRefs,
	}
	var promptSections []pkgplugins.SystemPromptSection
	if hasPluginAuthority {
		var err error
		promptSections, err = b.deps.PromptSectionsBuilder(ctx, promptBuild, pluginContext.Snapshot())
		if err != nil {
			return "", fmt.Errorf("%w: system prompt sections: %w", ErrUnavailable, err)
		}
		if skillsSection, err := b.deps.Skills(ctx, promptBuild, projectSkills); err != nil {
			return "", fmt.Errorf("%w: skills prompt section: %w", ErrUnavailable, err)
		} else if skillsSection.Title != "" && skillsSection.Content != "" {
			promptSections = append(promptSections, skillsSection)
		}
	}

	promptUserID := info.UserID
	if info.GroupID != "" {
		promptUserID = ""
	}
	system := prompt.BuildSystemPromptFromDB(ctx, prompt.DBPromptParams{
		SystemPrompt:   agentPrompt,
		Memory:         b.deps.Memory,
		UserID:         promptUserID,
		AgentID:        info.AgentID,
		GroupID:        info.GroupID,
		ProjectContext: projectContext,
		Sections:       promptSections,
	})
	return system, nil
}
