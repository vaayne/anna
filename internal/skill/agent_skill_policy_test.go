package skill

import (
	"context"
	"strings"
	"testing"
)

func TestSkillsToolIgnoresLegacyBuiltinPolicy(t *testing.T) {
	tool := newProjectionTool(t, &projectionReader{}, projectionSession{tempVisible: "/tmp", tempHost: t.TempDir()}, allowAllSkillReads{}).
		WithAgentSkillPolicy([]string{"builtin:stella"})
	ctx := context.Background()

	// Legacy builtin policy bytes remain readable but no longer affect immutable
	// builtin resources. The owning plugin visibility gate remains authoritative.
	for _, args := range []map[string]any{
		{"name": "stella"},
		{"name": "builtin-stella"},
		{"name": "builtin:stella"},
		{"name": "stella", "path": "SKILL.md"},
	} {
		out, err := skillAction(tool, "load").Execute(ctx, args)
		if err != nil || out == "" {
			t.Fatalf("skill_load(%#v) = %q, %v; legacy builtin deny must be ignored", args, out, err)
		}
	}

	for _, args := range []map[string]any{{"q": "stella"}} {
		out, err := skillAction(tool, "search").Execute(ctx, args)
		if err != nil {
			t.Fatalf("skill_installed_search(%#v): %v", args, err)
		}
		if !strings.Contains(out, `"name": "stella"`) || strings.Contains(out, "<skill_dir>") {
			t.Fatalf("skill_installed_search(%#v) did not retain builtin winner: %s", args, out)
		}
	}
}
