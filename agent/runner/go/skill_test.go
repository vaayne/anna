package gorunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vaayne/anna/memory"
)


func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    skillFrontmatter
		wantErr bool
	}{
		{
			name:    "valid frontmatter",
			content: "---\nname: my-skill\ndescription: Does things\n---\n# Body",
			want:    skillFrontmatter{Name: "my-skill", Description: "Does things"},
		},
		{
			name:    "with disable-model-invocation",
			content: "---\nname: hidden\ndescription: Secret\ndisable-model-invocation: true\n---\n",
			want:    skillFrontmatter{Name: "hidden", Description: "Secret", DisableModelInvocation: true},
		},
		{
			name:    "no frontmatter",
			content: "# Just markdown",
			wantErr: true,
		},
		{
			name:    "unclosed frontmatter",
			content: "---\nname: broken\n# no closing",
			wantErr: true,
		},
		{
			name:    "windows line endings",
			content: "---\r\nname: win\r\ndescription: Windows skill\r\n---\r\nBody",
			want:    skillFrontmatter{Name: "win", Description: "Windows skill"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFrontmatter(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Name != tt.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, tt.want.Name)
			}
			if got.Description != tt.want.Description {
				t.Errorf("Description = %q, want %q", got.Description, tt.want.Description)
			}
			if got.DisableModelInvocation != tt.want.DisableModelInvocation {
				t.Errorf("DisableModelInvocation = %v, want %v", got.DisableModelInvocation, tt.want.DisableModelInvocation)
			}
		})
	}
}

func TestLoadSkillsFromDir(t *testing.T) {
	// Create temp skill directory structure:
	// skills/
	//   root-skill.md          (root .md file, should be picked up)
	//   my-skill/
	//     SKILL.md             (subdirectory with SKILL.md)
	//   no-desc/
	//     SKILL.md             (missing description, should be skipped)
	//   .hidden/
	//     SKILL.md             (hidden dir, should be skipped)
	//   nested/
	//     deep-skill/
	//       SKILL.md           (deeply nested)

	dir := t.TempDir()

	writeSkill := func(relPath, content string) {
		t.Helper()
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeSkill("root-skill.md", "---\nname: root-skill\ndescription: A root skill\n---\n# Root")
	writeSkill("my-skill/SKILL.md", "---\nname: my-skill\ndescription: My cool skill\n---\n# Cool")
	writeSkill("no-desc/SKILL.md", "---\nname: no-desc\n---\n# Missing description")
	writeSkill(".hidden/SKILL.md", "---\nname: hidden\ndescription: Should be hidden\n---\n")
	writeSkill("nested/deep-skill/SKILL.md", "---\nname: deep-skill\ndescription: Deep one\n---\n")

	skills := loadSkillsFromDir(dir, "test")

	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
	}

	if !names["root-skill"] {
		t.Error("expected root-skill to be loaded")
	}
	if !names["my-skill"] {
		t.Error("expected my-skill to be loaded")
	}
	if !names["deep-skill"] {
		t.Error("expected deep-skill to be loaded")
	}
	if names["no-desc"] {
		t.Error("no-desc should be skipped (missing description)")
	}
	if names["hidden"] {
		t.Error(".hidden dir should be skipped")
	}
}

func TestLoadSkillsFallbackName(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "fallback-name")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\ndescription: No name field\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills := loadSkillsFromDir(dir, "test")
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Name != "fallback-name" {
		t.Errorf("expected name fallback to dir name 'fallback-name', got %q", skills[0].Name)
	}
}

func TestLoadSkillsDedup(t *testing.T) {
	// Same skill name in both agentsDir (project) and cwd/.agents (project alt) — first wins.
	agentsDir := t.TempDir()
	projectDir := t.TempDir()

	// agentsDir/skills/dupe-skill (loaded as "project" source)
	if err := os.MkdirAll(filepath.Join(agentsDir, "skills", "dupe-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "skills", "dupe-skill", "SKILL.md"),
		[]byte("---\nname: dupe-skill\ndescription: AgentsDir version\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// cwd/.agents/skills/dupe-skill (would also be "project" source)
	projAgents := filepath.Join(projectDir, ".agents")
	if err := os.MkdirAll(filepath.Join(projAgents, "skills", "dupe-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projAgents, "skills", "dupe-skill", "SKILL.md"),
		[]byte("---\nname: dupe-skill\ndescription: CWD version\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills := loadSkills("/nonexistent/home", agentsDir, projectDir)

	count := 0
	for _, s := range skills {
		if s.Name == "dupe-skill" {
			count++
			// agentsDir is scanned before cwd, so it wins
			if s.Description != "AgentsDir version" {
				t.Errorf("expected agentsDir version to win, got description %q", s.Description)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 dupe-skill, got %d", count)
	}
}

func TestFormatSkillsForPrompt(t *testing.T) {
	skills := []Skill{
		{Name: "web-search", Description: "Search the web", FilePath: "/skills/web-search/SKILL.md"},
		{Name: "hidden", Description: "Secret skill", FilePath: "/skills/hidden/SKILL.md", DisableModelInvocation: true},
		{Name: "code-review", Description: "Review code & suggest <improvements>", FilePath: "/skills/code-review/SKILL.md"},
	}

	result := FormatSkillsForPrompt(skills)

	if !strings.Contains(result, "<available_skills>") {
		t.Error("expected <available_skills> tag")
	}
	if !strings.Contains(result, "web-search") {
		t.Error("expected web-search in output")
	}
	if strings.Contains(result, "hidden") {
		t.Error("hidden skill should be excluded (DisableModelInvocation)")
	}
	// Check XML escaping
	if !strings.Contains(result, "&amp;") {
		t.Error("expected & to be escaped")
	}
	if !strings.Contains(result, "&lt;improvements&gt;") {
		t.Error("expected < > to be escaped")
	}
}

func TestFormatSkillsForPromptEmpty(t *testing.T) {
	result := FormatSkillsForPrompt(nil)
	if result != "" {
		t.Errorf("expected empty string for nil skills, got %q", result)
	}

	result = FormatSkillsForPrompt([]Skill{{Name: "x", Description: "y", DisableModelInvocation: true}})
	if result != "" {
		t.Errorf("expected empty string when all skills are hidden, got %q", result)
	}
}

func TestValidateSkillName(t *testing.T) {
	// Valid
	errs := ValidateSkillName("web-search", "web-search")
	if len(errs) != 0 {
		t.Errorf("expected no errors for valid name, got %v", errs)
	}

	// Name mismatch
	errs = ValidateSkillName("foo", "bar")
	if len(errs) == 0 {
		t.Error("expected error for name/dir mismatch")
	}

	// Invalid chars
	errs = ValidateSkillName("My_Skill", "My_Skill")
	found := false
	for _, e := range errs {
		if strings.Contains(e, "invalid characters") {
			found = true
		}
	}
	if !found {
		t.Error("expected invalid characters error")
	}

	// Leading hyphen
	errs = ValidateSkillName("-bad", "-bad")
	found = false
	for _, e := range errs {
		if strings.Contains(e, "start or end") {
			found = true
		}
	}
	if !found {
		t.Error("expected hyphen error")
	}

	// Consecutive hyphens
	errs = ValidateSkillName("bad--name", "bad--name")
	found = false
	for _, e := range errs {
		if strings.Contains(e, "consecutive") {
			found = true
		}
	}
	if !found {
		t.Error("expected consecutive hyphens error")
	}
}

func TestLoadSkillsUserLevelPriority(t *testing.T) {
	homeDir := t.TempDir()
	agentsDir := t.TempDir()

	// User-level skill: homeDir/.agents/skills/my-skill
	userSkill := filepath.Join(homeDir, ".agents", "skills", "my-skill")
	if err := os.MkdirAll(userSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userSkill, "SKILL.md"),
		[]byte("---\nname: my-skill\ndescription: User version\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project-level skill with same name: agentsDir/skills/my-skill
	projSkill := filepath.Join(agentsDir, "skills", "my-skill")
	if err := os.MkdirAll(projSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projSkill, "SKILL.md"),
		[]byte("---\nname: my-skill\ndescription: Project version\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills := loadSkills(homeDir, agentsDir, "")

	count := 0
	for _, s := range skills {
		if s.Name == "my-skill" {
			count++
			if s.Source != "user" {
				t.Errorf("expected user-level to win, got source %q", s.Source)
			}
			if s.Description != "User version" {
				t.Errorf("expected user description, got %q", s.Description)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 my-skill, got %d", count)
	}
}

func TestLoadSkillsNonexistentDir(t *testing.T) {
	skills := loadSkills("/nonexistent/home", "/nonexistent/agents", "/nonexistent/cwd")
	if len(skills) != 0 {
		t.Errorf("expected no skills for nonexistent dirs, got %d", len(skills))
	}
}

func TestBuildSystemPromptIncludesSkills(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	projectDir := filepath.Join(dir, "project")

	// Create a skill in the agents dir
	skillDir := filepath.Join(agentsDir, "skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: test-skill\ndescription: A test skill for prompt integration\n---\n# Test"),
		0o644); err != nil {
		t.Fatal(err)
	}

	memStore := memory.NewStore(filepath.Join(agentsDir, "memory"))
	prompt := BuildSystemPrompt(memStore, agentsDir, projectDir)
	if !strings.Contains(prompt, "<available_skills>") {
		t.Error("expected skills section in system prompt")
	}
	if !strings.Contains(prompt, "test-skill") {
		t.Error("expected test-skill in system prompt")
	}
}

func TestLoadProjectContextFiles(t *testing.T) {
	t.Run("finds AGENTS.md in cwd", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Project Rules"), 0o644); err != nil {
			t.Fatal(err)
		}

		files := loadProjectContextFiles(dir)
		if len(files) == 0 {
			t.Fatal("expected at least one context file")
		}
		found := false
		for _, f := range files {
			if strings.Contains(f.Content, "# Project Rules") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected to find AGENTS.md content")
		}
	})

	t.Run("walks ancestors in root-to-leaf order", func(t *testing.T) {
		root := t.TempDir()
		child := filepath.Join(root, "sub", "project")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(child, "AGENTS.md"), []byte("project rules"), 0o644); err != nil {
			t.Fatal(err)
		}

		files := loadProjectContextFiles(child)
		if len(files) < 2 {
			t.Fatalf("expected at least 2 context files, got %d", len(files))
		}
		// Root should come before child.
		rootIdx, childIdx := -1, -1
		for i, f := range files {
			if strings.Contains(f.Content, "root rules") {
				rootIdx = i
			}
			if strings.Contains(f.Content, "project rules") {
				childIdx = i
			}
		}
		if rootIdx == -1 || childIdx == -1 {
			t.Fatal("expected both root and child AGENTS.md files")
		}
		if rootIdx >= childIdx {
			t.Errorf("expected root (%d) before child (%d)", rootIdx, childIdx)
		}
	})

	t.Run("case insensitive match", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "agents.md"), []byte("lowercase agents"), 0o644); err != nil {
			t.Fatal(err)
		}

		files := loadProjectContextFiles(dir)
		found := false
		for _, f := range files {
			if strings.Contains(f.Content, "lowercase agents") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected case-insensitive AGENTS.md match")
		}
	})

	t.Run("empty cwd returns nil", func(t *testing.T) {
		files := loadProjectContextFiles("")
		if files != nil {
			t.Errorf("expected nil, got %v", files)
		}
	})
}

func TestBuildSystemPromptIncludesContextFiles(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	projectDir := filepath.Join(dir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(projectDir, "AGENTS.md"),
		[]byte("Always use snake_case."), 0o644); err != nil {
		t.Fatal(err)
	}

	memStore := memory.NewStore(filepath.Join(agentsDir, "memory"))
	prompt := BuildSystemPrompt(memStore, agentsDir, projectDir)

	if !strings.Contains(prompt, "# Project Context") {
		t.Error("expected Project Context section in system prompt")
	}
	if !strings.Contains(prompt, "Always use snake_case.") {
		t.Error("expected AGENTS.md content in system prompt")
	}
}
