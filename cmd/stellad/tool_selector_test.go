package main

import (
	"slices"
	"strings"
	"testing"

	agentpkg "github.com/CherryHQ/stella/internal/agent"
	sessionaccess "github.com/CherryHQ/stella/internal/agent/session/access"
	"github.com/CherryHQ/stella/internal/connections"
	"github.com/CherryHQ/stella/internal/controlplane"
	"github.com/CherryHQ/stella/internal/goal"
	"github.com/CherryHQ/stella/internal/library"
	"github.com/CherryHQ/stella/internal/library/recally"
	"github.com/CherryHQ/stella/internal/mcp"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/scheduler"
	sharepkg "github.com/CherryHQ/stella/internal/share"
	skillstool "github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/vault"
	workflowpkg "github.com/CherryHQ/stella/internal/workflow"
	"github.com/CherryHQ/stella/pkg/toolmeta"
	pkgtools "github.com/CherryHQ/stella/pkg/tools"
	"github.com/CherryHQ/stella/plugins/email"
)

// The consumer surfaces — a delegate preset's tools: list and a runner's
// excluded_tools — both resolve a selector against this registry by name.
// internal/agent cannot import the generated families (they import it back), so
// the real names are asserted here, against the registry the daemon builds.
func TestSelectorsResolveAgainstTheRealGeneratedNames(t *testing.T) {
	reg := newToolMetaRegistry(generatedFamilies()...)
	names := reg.Names()
	// A plugin is free to pick a name that looks like a family member. It has no
	// declaration, so the family selector must not sweep it in.
	names = append(names, "goal_helper", "bash")

	matched := func(selector string) []string {
		var out []string
		for _, name := range names {
			if reg.MatchName(selector, name) {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return out
	}

	for _, tc := range []struct {
		name     string
		selector string
		want     []string
	}{
		{
			name:     "family selector grants every action in it",
			selector: "goal",
			want:     []string{"goal_cancel", "goal_create", "goal_get", "goal_list"},
		},
		{
			name:     "retired union name still means its family",
			selector: "scheduler",
			want: []string{
				"scheduler__job_create", "scheduler__job_delete", "scheduler__job_get",
				"scheduler__job_list", "scheduler__job_pause", "scheduler__job_resume",
				"scheduler__job_update",
			},
		},
		{
			// The clean break deletes the override rows naming `memory`, but the
			// selector keeps working for the same reason `scheduler` does: the
			// union's name was already its family's name, so a preset that lists
			// it still grants the capability it always granted. There is no
			// legacy-name entry behind this and none is wanted.
			name:     "the retired memory union name still means its family",
			selector: "memory",
			want:     []string{"memory_read", "memory_search"},
		},
		{
			// A retired name is deleted, not redirected: the migration removes
			// the override rows that named it, so nothing resolves it.
			name:     "a retired action name selects nothing",
			selector: "oauth_status",
			want:     nil,
		},
		{
			name:     "a retired recally name selects nothing",
			selector: "recally_digest",
			want:     nil,
		},
		{
			name:     "an undeclared lookalike is never swept into a family",
			selector: "goal_helper",
			want:     []string{"goal_helper"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matched(tc.selector); !slices.Equal(got, tc.want) {
				t.Fatalf("selector %q matched %v, want %v", tc.selector, got, tc.want)
			}
		})
	}

	if reg.SelectsNothing("goal", names) {
		t.Fatal("a live family selector must not be reported as stale")
	}
	if !reg.SelectsNothing("recally_save_article", []string{"goal_create"}) {
		t.Fatal("a selector matching none of the given tools must be reported as stale")
	}
}

// The goal worker's exclusion list is derived from the families rather than
// written by hand. Pinning the exact set is what catches a family silently
// dropping out of the derivation.
func TestGoalWorkerExclusionListCoversTheWholeOrchestrationSurface(t *testing.T) {
	got := splitFamilyNames(goal.ActionTools(), scheduler.ActionTools(), workflowpkg.ActionTools())
	slices.Sort(got)
	want := []string{
		"goal_cancel", "goal_create", "goal_get", "goal_list",
		"scheduler__job_create", "scheduler__job_delete", "scheduler__job_get",
		"scheduler__job_list", "scheduler__job_pause", "scheduler__job_resume",
		"scheduler__job_update",
		"workflow_get", "workflow_list", "workflow_run", "workflow_save",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("goal worker exclusion list = %v (%d names), want %v (%d names)", got, len(got), want, len(want))
	}
}

// Every generated tool name must be reachable by its own name and carry the
// action its handler dispatches on; the trace hook reads that action from here
// now that the union's `action` argument is gone.
func TestEveryGeneratedToolDeclaresItsActionForTracing(t *testing.T) {
	reg := newToolMetaRegistry(generatedFamilies()...)
	// The two "create X from Y" tools carry a name_override, so their name
	// reads as a sentence instead of ending in the action (rules §4).
	renamed := map[string]bool{"share_create_article": true, "share_create_artifact": true}
	for _, name := range reg.Names() {
		action := reg.Action(name)
		if action == "" {
			t.Errorf("%s declares no action", name)
			continue
		}
		if !strings.HasSuffix(name, action) && !renamed[name] {
			t.Errorf("%s carries action %q, which its name does not end with", name, action)
		}
	}
	if reg.Action("bash") != "" {
		t.Error("an undeclared tool must report no action rather than a guess")
	}
}

// A description is the model's only prose about a tool, and it is paid for on
// every turn of every session. The exact schema already says what the fields
// are, so 60 words is the budget the rule sets (rules/agent-tools.md §6).
func TestGeneratedToolDescriptionsStayWithinTheWordBudget(t *testing.T) {
	const maxWords = 60
	// The services are nil on purpose: Definition() is static prose plus a
	// generated schema, and a tool that needed a live service to describe
	// itself could not appear in the catalog either.
	definitions := map[string][]pkgtools.Definition{}
	collect := func(specs []toolmeta.ActionTool, newTool func(toolmeta.ActionTool) pkgtools.Tool) {
		for _, spec := range specs {
			definitions[spec.Family] = append(definitions[spec.Family], newTool(spec).Definition())
		}
	}
	collect(goal.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return goal.NewTool(nil, s) })
	collect(workflowpkg.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return workflowpkg.NewTool(nil, s) })
	collect(email.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return email.NewTool(nil, s, email.ToolDeps{}) })
	collect(scheduler.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return scheduler.NewTool(nil, s) })
	collect(sharepkg.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return sharepkg.NewTool(nil, s) })
	collect(connections.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return connections.NewTool(nil, s) })
	collect(vault.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return vault.NewTool(nil, nil, s) })
	collect(recally.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return recally.NewTool(nil, s) })
	collect(sessionaccess.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return sessionaccess.NewTool(nil, s) })
	collect(agentpkg.SettingsAgentActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return agentpkg.NewManagementTool(s, nil) })
	collect(agentpkg.SettingsAgentToolActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return agentpkg.NewToolOverrideManagementTool(s, nil, nil, nil, nil)
	})
	collect(skillstool.RuntimeActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return skillstool.NewAction(nil, s) })
	collect(skillstool.SettingsSkillActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return skillstool.NewRuntimeManagementTool(nil, nil, s)
	})
	collect(memory.ActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return memory.NewTool(nil, s) })
	collect(library.RuntimeActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool { return library.NewTool(nil, s) })
	collect(library.SettingsLibraryActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return library.NewRuntimeManagementTool(nil, nil, s)
	})
	collect(controlplane.SettingsProviderActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return controlplane.NewProviderManagementTool(s, nil)
	})
	collect(controlplane.SettingsDefaultModelActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return controlplane.NewDefaultModelManagementTool(s, nil)
	})
	collect(controlplane.SettingsEmbeddingSettingActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return controlplane.NewEmbeddingSettingManagementTool(s, nil)
	})
	collect(controlplane.SettingsPluginActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return controlplane.NewPluginManagementTool(s, nil)
	})
	collect(mcp.SettingsMcpActionTools(), func(s toolmeta.ActionTool) pkgtools.Tool {
		return mcp.NewManagementTool(s, nil)
	})

	var seen int
	for _, family := range definitions {
		for _, def := range family {
			seen++
			words := len(strings.Fields(def.Description))
			if words == 0 {
				t.Errorf("%s has no description", def.Name)
			}
			if words > maxWords {
				t.Errorf("%s description is %d words, want at most %d", def.Name, words, maxWords)
			}
		}
	}
	if want := len(newToolMetaRegistry(generatedFamilies()...).Names()); seen != want {
		t.Fatalf("checked %d descriptions, want %d: a family is missing from this test", seen, want)
	}
}
