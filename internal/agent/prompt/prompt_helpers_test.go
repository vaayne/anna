package prompt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/agent/prompt"
	"github.com/CherryHQ/stella/resources"
)

func TestDefaultSystemPrompt(t *testing.T) {
	got := prompt.DefaultSystemPrompt()
	if got == "" {
		t.Error("expected non-empty default system prompt")
	}
}

func TestDefaultAgentSoul(t *testing.T) {
	got := prompt.DefaultAgentSoul()
	if got == "" {
		t.Error("expected non-empty default agent soul")
	}
}

func TestFilesystemPromptOperationalContract(t *testing.T) {
	got := prompt.BuildSystemPromptFromDB(context.Background(), prompt.DBPromptParams{SystemPrompt: "You are Stella."})
	for _, want := range []string{
		"If `$STELLA_ASSETS_DIR` is available, put user uploads and final durable deliverables there; otherwise keep them under `$HOME`.",
		"XDG, mise, and Lark directories are tool-managed; do not choose them for files.",
		"`$HOME`, `$TMPDIR`, and `$STELLA_ASSETS_DIR` are expanded in tool paths for you.",
		"Never hardcode `/workspace`, `/user`, or `/tmp`.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("filesystem guidance missing %q:\n%s", want, got)
		}
	}
}

func TestRecallGuidanceMatchesUnifiedAgentActions(t *testing.T) {
	systemPrompt := prompt.BuildSystemPromptFromDB(context.Background(), prompt.DBPromptParams{SystemPrompt: "You are Stella."})
	registry, err := resources.Default()
	if err != nil {
		t.Fatal(err)
	}
	stellaSkill, _, err := registry.ReadBuiltinSkillFile("stella", "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for surface, content := range map[string]string{
		"system prompt": systemPrompt,
		"Stella skill":  string(stellaSkill),
	} {
		for _, action := range []string{"memory_search", "memory_read", "library_search", "session_list", "session_get", "session_create", "session_send"} {
			if !strings.Contains(content, action) {
				t.Errorf("%s omitted model-facing action %q", surface, action)
			}
		}
		for _, stale := range []string{"session.find", "session.list", "session.send", "memory.search", "memory.read", "memory.search_knowledge", "memory.profile_get"} {
			if strings.Contains(content, stale) {
				t.Errorf("%s still teaches removed action %q", surface, stale)
			}
		}
	}
}

// Every agent used to be seeded with the product default persona, so a group of
// two fresh agents contained two Stellas.
func TestDefaultPersonaUsesAgentName(t *testing.T) {
	got := prompt.DefaultSystemPromptFor("Anna")
	if !strings.Contains(got, "You are Anna") {
		t.Fatalf("default persona = %q, want it named after the agent", got)
	}
	if strings.Contains(got, "Stella") {
		t.Fatalf("default persona still claims another name: %q", got)
	}
	if unnamed := prompt.DefaultSystemPromptFor("  "); !strings.Contains(unnamed, "You are Stella") {
		t.Fatalf("unnamed default = %q, want the product default", unnamed)
	}
}
