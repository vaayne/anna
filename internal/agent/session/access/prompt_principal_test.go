package access

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/agent"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/memory/memorytest"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/skill"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func promptTestAgents(context.Context, string) (string, error) { return "test", nil }

type promptTestProjects struct {
	descriptor agent.ProjectDescriptor
	second     agent.ProjectDescriptor
	calls      int
}

type promptTestWorkspace struct{ root string }

func (w promptTestWorkspace) WorkspaceView(_ context.Context, req home.WorkspaceRequest) (home.WorkspaceView, error) {
	if req.GroupID != "" {
		principal := filepath.Join(w.root, "users", "group-"+req.GroupID)
		return home.WorkspaceView{PrincipalRoot: principal, AgentRoot: filepath.Join(principal, "agents", req.AgentID)}, nil
	}
	if req.UserID != "" {
		principal := filepath.Join(w.root, "users", req.UserID)
		return home.WorkspaceView{PrincipalRoot: principal, AgentRoot: filepath.Join(principal, "agents", req.AgentID)}, nil
	}
	return home.WorkspaceView{}, nil
}

func (w promptTestWorkspace) OpenRoot(ctx context.Context, req home.WorkspaceRequest, scope home.RootScope, _ home.RootAccess) (home.RootOperations, error) {
	view, err := w.WorkspaceView(ctx, req)
	if err != nil {
		return nil, err
	}
	dir := view.AgentRoot
	if scope == home.RootPrincipalData {
		dir = view.DataRoot
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return testRootOperations{Root: root}, nil
}

func (p *promptTestProjects) ResolveProject(context.Context, string, string, string) (agent.ProjectDescriptor, error) {
	p.calls++
	if p.descriptor.ID == "" {
		return agent.ProjectDescriptor{}, agent.ErrProjectNotFound
	}
	if p.calls > 1 && p.second.ID != "" {
		return p.second, nil
	}
	return p.descriptor, nil
}

func TestPromptPreviewUsesAuthorizedRootToLeafProjectContextWithoutHostPath(t *testing.T) {
	stellaHome := t.TempDir()
	root := filepath.Join(stellaHome, "users", "u1", "agents", "a1")
	project := filepath.Join(root, "projects", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		filepath.Join(root, "AGENTS.md"):                                 "preview root instructions",
		filepath.Join(project, "AGENTS.md"):                              "preview project instructions",
		filepath.Join(project, ".agents", "skills", "first", "SKILL.md"): "---\nname: first\ndescription: first skill generation\n---\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	projects := &promptTestProjects{
		descriptor: agent.ProjectDescriptor{ID: "p1", UserID: "u1", AgentID: "a1", Path: "projects/app"},
		second:     agent.ProjectDescriptor{ID: "p1", UserID: "u1", AgentID: "a1", Path: "changed/generation"},
	}
	builder, err := NewSystemPromptBuilder(SystemPromptDeps{
		Memory:    memorytest.New(),
		Agents:    promptTestAgents,
		Projects:  projects.ResolveProject,
		Workspace: promptTestWorkspace{root: stellaHome},
		PluginContextBuilder: func(context.Context, authz.Authority, string) (agentruntime.PluginContext, error) {
			return agentruntime.PluginContext{}, nil
		},
		PromptSectionsBuilder: func(context.Context, pkgplugins.SystemPromptContext, plugin.Snapshot) ([]pkgplugins.SystemPromptSection, error) {
			return nil, nil
		},
		Skills: func(_ context.Context, _ pkgplugins.SystemPromptContext, project *skill.ProjectSnapshot) (pkgplugins.SystemPromptSection, error) {
			merged := skill.NewService().ListMerged(nil, project)
			for _, resolved := range merged {
				if resolved.Name == "first" {
					return pkgplugins.SystemPromptSection{Title: "Snapshot Proof", Content: resolved.Description}, nil
				}
			}
			return pkgplugins.SystemPromptSection{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := builder.BuildSessionSystemPrompt(context.Background(), SystemPromptBuildInput{Info: session.Info{UserID: "u1", AgentID: "a1", ProjectID: "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "preview root instructions") || !strings.Contains(got, "preview project instructions") || !strings.Contains(got, "first skill generation") || strings.Contains(got, stellaHome) {
		t.Fatalf("preview prompt lacks logical root-to-leaf context or leaks host path:\n%s", got)
	}
	if projects.calls != 1 {
		t.Fatalf("project resolved %d times, want exactly once", projects.calls)
	}
}

func TestAuthorizedPromptPassesLogicalIdentityWithoutPhysicalPaths(t *testing.T) {
	stellaHome := t.TempDir()
	for _, tt := range []struct {
		name string
		info session.Info
	}{
		{name: "personal", info: session.Info{UserID: "u1", AgentID: "a1"}},
		{name: "group", info: session.Info{UserID: "g1", GroupID: "g1", AgentID: "a1"}},
		{name: "user-less", info: session.Info{AgentID: "a1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var build pkgplugins.SystemPromptContext
			builder, err := NewSystemPromptBuilder(SystemPromptDeps{
				Memory:    memorytest.New(),
				Agents:    promptTestAgents,
				Projects:  (&promptTestProjects{}).ResolveProject,
				Workspace: promptTestWorkspace{root: stellaHome},
				PluginContextBuilder: func(context.Context, authz.Authority, string) (agentruntime.PluginContext, error) {
					return agentruntime.PluginContext{}, nil
				},
				PromptSectionsBuilder: func(_ context.Context, got pkgplugins.SystemPromptContext, _ plugin.Snapshot) ([]pkgplugins.SystemPromptSection, error) {
					build = got
					return nil, nil
				},
				Skills: func(context.Context, pkgplugins.SystemPromptContext, *skill.ProjectSnapshot) (pkgplugins.SystemPromptSection, error) {
					return pkgplugins.SystemPromptSection{}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := builder.BuildSessionSystemPrompt(context.Background(), SystemPromptBuildInput{Info: tt.info}); err != nil {
				t.Fatal(err)
			}
			if tt.info.UserID == "" {
				return
			}
			if build.UserID != tt.info.UserID || build.AgentID != tt.info.AgentID {
				t.Errorf("prompt identity = (%q, %q), want (%q, %q)", build.UserID, build.AgentID, tt.info.UserID, tt.info.AgentID)
			}
		})
	}
}
