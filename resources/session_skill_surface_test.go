package resources

import (
	"strings"
	"testing"
)

func TestBuiltinSkillsUseSequentialSessionSurface(t *testing.T) {
	for _, logicalPath := range []string{
		"skills/plugins/system/recally/recally/references/rss-workflow.md",
		"skills/core/skill-creator/SKILL.md",
	} {
		text := readBuiltinSkillPath(t, logicalPath)
		if !strings.Contains(text, "session_create") || !strings.Contains(strings.ToLower(text), "sequential") {
			t.Fatalf("%s must teach the synchronous sequential Session workflow", logicalPath)
		}
	}

	r, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, skill := range r.BuiltinSkills() {
		for _, file := range skill.Files {
			if !strings.HasSuffix(file.Path, ".md") {
				continue
			}
			content, _, readErr := r.ReadBuiltinSkillFile(skill.Name, file.Path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			lower := strings.ToLower(string(content))
			for _, removed := range []string{
				"`delegate` tool", "spawn one delegate", "spawn two delegates",
				"grader delegate", "parallel via delegates", "delegate task completes", "baseline delegate",
			} {
				if strings.Contains(lower, removed) {
					t.Fatalf("%s/%s still teaches removed delegate-tool behavior %q", skill.Name, file.Path, removed)
				}
			}
		}
	}
}
