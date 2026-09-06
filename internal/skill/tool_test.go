package skill

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/CherryHQ/stella/internal/authz"
)

func TestDispatchRejectsUnknownAction(t *testing.T) {
	tool := newProjectionTool(t, &projectionReader{}, projectionSession{tempVisible: "/tmp", tempHost: t.TempDir()}, allowAllSkillReads{})
	// Management actions were removed from the runtime tool, and after the split
	// they are not names either — nothing can route to one.
	if _, err := SkillDispatch(context.Background(), tool, "install", map[string]any{"name": "owned"}); err == nil {
		t.Fatal("runtime Skill tool accepted a removed management action")
	}
}

// skillAction builds the generated tool for one action over the runner's Tool.
// A test names the action the way the model does: by calling a different tool.
func skillAction(tool *Tool, action string) *Action {
	for _, spec := range RuntimeActionTools() {
		if spec.Action == action {
			return NewAction(tool, spec)
		}
	}
	panic("no skill tool for action " + action)
}

// loadSkill is skill_load's result as the model receives it: text, not a value.
func loadSkill(t *testing.T, tool *Tool, name string) (string, error) {
	t.Helper()
	return skillAction(tool, "load").Execute(t.Context(), map[string]any{"name": name})
}

func TestSearchInstalledRanksExactManagedSnapshotsAndHonorsAuthorization(t *testing.T) {
	deploy := Skill{ID: "deploy", Scope: "user", UserID: "user-1", Name: "deploy-runbook", Description: "Production release checklist", Status: SkillStatusActive}
	secret := Skill{ID: "secret", Scope: "user", UserID: "user-1", Name: "secret-runbook", Description: "Confidential incident procedure", Status: SkillStatusActive}
	reader := &projectionReader{
		identities: []Skill{deploy, secret},
		revisions: map[string]ManagedRevision{
			deploy.ID: promptRevision(deploy, strings.Repeat("a", 64), "# Deploy"),
			secret.ID: promptRevision(secret, strings.Repeat("b", 64), "# Secret"),
		},
	}
	tool := newProjectionTool(t, reader, projectionSession{tempVisible: "/tmp", tempHost: t.TempDir()}, selectedSkillReads{denied: map[string]bool{secret.ID: true}})

	out, err := skillAction(tool, "search").Execute(context.Background(), map[string]any{"q": "release checklist", "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	var results []installedSkillSearchResult
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("decode search results: %v\n%s", err, out)
	}
	if len(results) != 1 || results[0].Name != deploy.Name {
		t.Fatalf("results = %#v, want only %q", results, deploy.Name)
	}

	out, err = skillAction(tool, "search").Execute(context.Background(), map[string]any{"q": "confidential incident"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "No installed skills found." {
		t.Fatalf("denied managed Skill leaked into search: %s", out)
	}
}

func TestPluginSkillVisibilityAppliesToSearchAndLoad(t *testing.T) {
	tool := newProjectionTool(t, &projectionReader{}, projectionSession{tempVisible: "/tmp", tempHost: t.TempDir()}, allowAllSkillReads{}).
		WithPluginVisibility([]string{"tool/lark-cli"}, nil)

	out, err := skillAction(tool, "search").Execute(t.Context(), map[string]any{"q": "lark cli"})
	if err != nil {
		t.Fatal(err)
	}
	// Core skills may match "cli" independently; only the disabled owner is hidden.
	if out != noInstalledSkills {
		var results []installedSkillSearchResult
		if err := json.Unmarshal([]byte(out), &results); err != nil {
			t.Fatal(err)
		}
		for _, result := range results {
			if result.Name == "lark-cli" {
				t.Fatalf("disabled plugin skill leaked into search: %s", out)
			}
		}
	}
	if out, err := loadSkill(t, tool, "lark-cli"); !errors.Is(err, errSkillNotFound) || out != "" {
		t.Fatalf("disabled plugin skill load = %q, %v; want hidden", out, err)
	}
}

func TestLoadProjectsImmutableProjectSnapshotThroughSessionFiles(t *testing.T) {
	snapshot, err := SnapshotProjectSkills(t.Context(), snapshotRoot{fstest.MapFS{
		".agents/skills/deploy/SKILL.md":       {Data: []byte("---\nname: deploy\ndescription: deploy app\n---\n# Deploy")},
		".agents/skills/deploy/scripts/run.sh": {Data: []byte("#!/bin/sh\nprintf deploy"), Mode: 0o755},
	}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	session := projectionSession{tempVisible: "/tmp", tempHost: t.TempDir()}
	tool := newProjectionTool(t, &projectionReader{}, session, allowAllSkillReads{}).WithProjectSnapshot(snapshot)

	out, err := skillAction(tool, "load").Execute(context.Background(), map[string]any{"name": "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	projection, err := snapshot.immutableProjection("deploy")
	if err != nil {
		t.Fatal(err)
	}
	wantVisible := filepath.ToSlash(filepath.Join(session.tempVisible, "stella-skills", "project", "deploy", projection.digest))
	if !strings.Contains(out, "<skill_dir>"+wantVisible+"</skill_dir>") || !strings.Contains(out, "# Deploy") {
		t.Fatalf("load output = %q", out)
	}
	hostScript := filepath.Join(session.tempHost, "stella-skills", "project", "deploy", projection.digest, "scripts", "run.sh")
	content, err := os.ReadFile(hostScript)
	if err != nil || string(content) != "#!/bin/sh\nprintf deploy" {
		t.Fatalf("projected script = %q, %v", content, err)
	}
	info, err := os.Stat(hostScript)
	if err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("projected mode = %v, %v; want 0555", info, err)
	}
}

func TestLoadProjectSkillWhileManagedSkillsUnavailable(t *testing.T) {
	snapshot, err := SnapshotProjectSkills(t.Context(), snapshotRoot{fsys: fstest.MapFS{
		".agents/skills/local/SKILL.md": {Data: []byte("---\nname: local\ndescription: local project skill\n---\n# Local")},
	}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	reader := unavailableManagedReader{projectionReader: &projectionReader{}}
	tool := newProjectionTool(t, reader, projectionSession{tempVisible: "/tmp", tempHost: t.TempDir()}, allowAllSkillReads{}).WithProjectSnapshot(snapshot)
	out, err := skillAction(tool, "load").Execute(t.Context(), map[string]any{"name": "local"})
	if err != nil || !strings.Contains(out, "# Local") {
		t.Fatalf("project Skill with managed authority unavailable = %q, %v", out, err)
	}
}

type failingRuntimeTouchReader struct {
	*projectionReader
	err error
}

func (r failingRuntimeTouchReader) TouchReflectSkillRuntimeUseDigest(context.Context, string, string, string, string) error {
	return r.err
}

func TestLoadReflectSkillFailsBeforeProjectionWhenUsageClaimFails(t *testing.T) {
	metadata, err := MarkReflectOwnedMetadata(nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := Skill{
		ID: "reflect", Scope: "user_agent", UserID: "user-1", AgentID: "agent-1",
		Name: "reflect", Status: SkillStatusActive, Metadata: metadata,
	}
	base := &projectionReader{
		identities: []Skill{identity},
		revisions: map[string]ManagedRevision{
			identity.ID: promptRevision(identity, strings.Repeat("c", 64), "# Reflect"),
		},
	}
	wantErr := errors.New("usage claim changed")
	session := projectionSession{tempVisible: "/tmp", tempHost: t.TempDir()}
	tool := newProjectionTool(t, failingRuntimeTouchReader{projectionReader: base, err: wantErr}, session, allowAllSkillReads{})
	ctx := authz.WithAgentID(authz.WithUserID(context.Background(), identity.UserID), identity.AgentID)

	if out, err := skillAction(tool, "load").Execute(ctx, map[string]any{"name": identity.Name}); !errors.Is(err, wantErr) || out != "" {
		t.Fatalf("load = %q, %v; want usage claim failure", out, err)
	}
	entries, err := os.ReadDir(session.tempHost)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("usage claim failure projected files: %#v", entries)
	}
}
