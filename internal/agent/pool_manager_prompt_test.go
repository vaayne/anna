package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/agent/session"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func TestPoolSnapshotPromptPassesLogicalIdentityWithoutPhysicalPaths(t *testing.T) {
	stellaHome := t.TempDir()
	t.Setenv("STELLA_HOME", stellaHome)
	config.ResetStellaHome()
	t.Cleanup(config.ResetStellaHome)

	snap := &config.Snapshot{AgentID: "a1", Workspace: "/agent-definition/a1"}
	for _, tt := range []struct {
		name string
		info session.Info
	}{
		{name: "personal", info: session.Info{UserID: "u1", AgentID: "a1"}},
		{name: "group", info: session.Info{UserID: "g1", GroupID: "g1", AgentID: "a1"}},
		{name: "user-less", info: session.Info{AgentID: "a1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got pkgplugins.SystemPromptContext
			pm := &PoolManager{homeWorkspace: testWorkspaceViewer{root: stellaHome}, skillRevisionReader: emptySkillRuntime{}, skillReadAuthz: allowSkillReads{}, promptSectionsBuilder: func(_ context.Context, build pkgplugins.SystemPromptContext, _ plugin.Snapshot) ([]pkgplugins.SystemPromptSection, error) {
				got = build
				return nil, nil
			}}
			if _, err := pm.buildSnapshotPromptFunc(snap)(context.Background(), tt.info, memory.SessionSnapshot{}, PluginContext{}); err != nil {
				t.Fatal(err)
			}
			if got.UserID != tt.info.UserID || got.AgentID != tt.info.AgentID {
				t.Errorf("prompt identity = (%q, %q), want (%q, %q)", got.UserID, got.AgentID, tt.info.UserID, tt.info.AgentID)
			}
		})
	}
}

func TestPoolSnapshotPromptDoesNotResolvePhysicalWorkspaceWithoutProject(t *testing.T) {
	pm := &PoolManager{homeWorkspace: failingWorkspaceViewer{err: os.ErrPermission}, skillRevisionReader: emptySkillRuntime{}, skillReadAuthz: allowSkillReads{}}
	if _, err := pm.buildSnapshotPromptFunc(&config.Snapshot{AgentID: "a"})(context.Background(), session.Info{UserID: "u", AgentID: "a"}, memory.SessionSnapshot{}, PluginContext{}); err != nil {
		t.Fatalf("snapshot prompt consulted physical workspace: %v", err)
	}
}

func TestPoolSnapshotPromptUsesAuthorizedRootToLeafProjectContextWithoutHostPath(t *testing.T) {
	stellaHome := t.TempDir()
	root := filepath.Join(stellaHome, "users", "u1", "agents", "a1")
	project := filepath.Join(root, "projects", "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		filepath.Join(root, "AGENTS.md"):    "pool root instructions",
		filepath.Join(project, "AGENTS.md"): "pool project instructions",
	} {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolveCalls := 0
	pm := &PoolManager{
		homeWorkspace:       testWorkspaceViewer{root: stellaHome},
		skillRevisionReader: emptySkillRuntime{},
		skillReadAuthz:      allowSkillReads{},
		projectResolver: func(_ context.Context, projectID, userID, agentID string) (ProjectDescriptor, error) {
			resolveCalls++
			if resolveCalls > 1 {
				return ProjectDescriptor{ID: projectID, UserID: userID, AgentID: agentID, Path: "changed/generation"}, nil
			}
			return ProjectDescriptor{ID: projectID, UserID: userID, AgentID: agentID, Path: "projects/app"}, nil
		},
	}
	got, err := pm.buildSnapshotPromptFunc(&config.Snapshot{AgentID: "a1"})(context.Background(), session.Info{UserID: "u1", AgentID: "a1", ProjectID: "p1"}, memory.SessionSnapshot{}, PluginContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "pool root instructions") || !strings.Contains(got, "pool project instructions") || strings.Contains(got, stellaHome) {
		t.Fatalf("snapshot prompt lacks logical root-to-leaf context or leaks host path:\n%s", got)
	}
	if resolveCalls != 1 {
		t.Fatalf("project resolved %d times, want exactly once", resolveCalls)
	}
}

func TestMatchesHomeOwnerKeepsUserAndGroupScopesDisjoint(t *testing.T) {
	for _, tt := range []struct {
		name string
		info session.Info
		kind home.OwnerKind
		id   string
		want bool
	}{
		{name: "user private session", info: session.Info{UserID: "same"}, kind: home.OwnerUser, id: "same", want: true},
		{name: "user excludes group session sharing raw ID", info: session.Info{UserID: "same", GroupID: "same"}, kind: home.OwnerUser, id: "same", want: false},
		{name: "group matches exact GroupID", info: session.Info{UserID: "same", GroupID: "same"}, kind: home.OwnerGroup, id: "same", want: true},
		{name: "group ignores matching user ID", info: session.Info{UserID: "same"}, kind: home.OwnerGroup, id: "same", want: false},
		{name: "agent uses removeAgent path", info: session.Info{UserID: "same"}, kind: home.OwnerAgent, id: "same", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesHomeOwner(tt.info, tt.kind, tt.id); got != tt.want {
				t.Fatalf("matchesHomeOwner(%+v, %q, %q) = %t, want %t", tt.info, tt.kind, tt.id, got, tt.want)
			}
		})
	}
}
