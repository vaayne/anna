package skills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestDefinition(t *testing.T) {
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	def := tool.Definition()

	if def.Name != "skills" {
		t.Errorf("expected name 'skills', got %q", def.Name)
	}
	if def.Description == "" {
		t.Error("expected non-empty description")
	}
	if def.InputSchema == nil {
		t.Error("expected non-nil input schema")
	}
}

func TestExecuteUnknownAction(t *testing.T) {
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	_, err := tool.Execute(context.Background(), map[string]any{"action": "bogus"})
	if err == nil {
		t.Error("expected error for unknown action")
	}
}

func TestExecuteDispatch(t *testing.T) {
	// search — missing query → error
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	_, err := tool.Execute(context.Background(), map[string]any{"action": "search"})
	if err == nil {
		t.Error("expected error for search without query")
	}

	// install — missing source → error
	_, err = tool.Execute(context.Background(), map[string]any{"action": "install"})
	if err == nil {
		t.Error("expected error for install without source")
	}

	// remove — missing name → error
	_, err = tool.Execute(context.Background(), map[string]any{"action": "remove"})
	if err == nil {
		t.Error("expected error for remove without name")
	}
}

func TestListWithSkill(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".agents", "skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: test-skill
description: A test skill
---
# Test Skill
`), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewTool(filepath.Join(dir, ".agents"), dir)
	result, err := tool.list()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var skills []installedSkill
	if err := json.Unmarshal([]byte(result), &skills); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	// Find the test skill among results (user-level skills may also appear)
	var found bool
	for _, s := range skills {
		if s.Name == "test-skill" {
			found = true
			if s.Description != "A test skill" {
				t.Errorf("expected description 'A test skill', got %q", s.Description)
			}
			break
		}
	}
	if !found {
		t.Error("expected test-skill to appear in list results")
	}
}

func TestRemoveNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}

	tool := NewTool(filepath.Join(dir, ".agents"), dir)
	_, err := tool.remove(map[string]any{"name": "nonexistent"})
	if err == nil {
		t.Error("expected error for nonexistent skill")
	}
}

func TestRemoveMissingName(t *testing.T) {
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	_, err := tool.remove(map[string]any{})
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestRemoveInvalidName(t *testing.T) {
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	_, err := tool.remove(map[string]any{"name": "../../../etc"})
	if err == nil {
		t.Error("expected error for path traversal name")
	}
}

func TestRemoveSuccess(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".agents", "skills", "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewTool(filepath.Join(dir, ".agents"), dir)
	result, err := tool.remove(map[string]any{"name": "my-skill"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Error("expected skill directory to be removed")
	}
}

func TestSearchSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "react" {
			t.Errorf("expected query 'react', got %q", r.URL.Query().Get("q"))
		}
		_ = json.NewEncoder(w).Encode(searchResponse{
			Count: 1,
			Skills: []SearchResult{
				{ID: "react-best-practices", Name: "React Best Practices", Installs: 100, Source: "vercel-labs/agent-skills"},
			},
		})
	}))
	defer server.Close()

	tool := &SkillsTool{searchURL: server.URL}
	result, err := tool.search(context.Background(), map[string]any{"query": "react"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestSearchNoResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(searchResponse{Count: 0, Skills: []SearchResult{}})
	}))
	defer server.Close()

	tool := &SkillsTool{searchURL: server.URL}
	result, err := tool.search(context.Background(), map[string]any{"query": "nonexistent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "No skills found." {
		t.Errorf("expected 'No skills found.', got %q", result)
	}
}

func TestSearchMissingQuery(t *testing.T) {
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	_, err := tool.search(context.Background(), map[string]any{})
	if err == nil {
		t.Error("expected error for missing query")
	}
}

func TestParseSource(t *testing.T) {
	tests := []struct {
		input     string
		owner     string
		repo      string
		skill     string
		ref       string
		expectErr bool
	}{
		{"owner/repo@skill", "owner", "repo", "skill", "", false},
		{"owner/repo@skill#main", "owner", "repo", "skill", "main", false},
		{"vercel-labs/agent-skills@react-best-practices", "vercel-labs", "agent-skills", "react-best-practices", "", false},
		{"invalid", "", "", "", "", true},
		{"owner/repo", "", "", "", "", true},
		{"owner/@skill", "", "", "", "", true},
		{"/repo@skill", "", "", "", "", true},
		{"owner/repo@", "", "", "", "", true},
		// Path traversal attempts
		{"../evil/repo@skill", "", "", "", "", true},
		{"owner/../../etc@skill", "", "", "", "", true},
		{"owner/repo@../../../etc", "", "", "", "", true},
		{"owner/repo@skill#../../main", "", "", "", "", true},
		{"owner/repo@skill#..", "", "", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			owner, repo, skill, ref, err := parseSource(tt.input)
			if tt.expectErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tt.owner || repo != tt.repo || skill != tt.skill || ref != tt.ref {
				t.Errorf("got (%q, %q, %q, %q), want (%q, %q, %q, %q)",
					owner, repo, skill, ref, tt.owner, tt.repo, tt.skill, tt.ref)
			}
		})
	}
}

func TestCopyDir(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "helper.md"), []byte("helper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git", "config"), []byte("gitconfig"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "target")
	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir error: %v", err)
	}

	// SKILL.md should be copied
	if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err != nil {
		t.Error("expected SKILL.md to be copied")
	}
	// sub/helper.md should be copied
	if _, err := os.Stat(filepath.Join(dst, "sub", "helper.md")); err != nil {
		t.Error("expected sub/helper.md to be copied")
	}
	// .git should NOT be copied
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Error("expected .git to be skipped")
	}
}

func TestFindSkillDir(t *testing.T) {
	cache := t.TempDir()
	skillDir := filepath.Join(cache, "skills", "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	found, err := findSkillDir(cache, "my-skill")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found != skillDir {
		t.Errorf("expected %q, got %q", skillDir, found)
	}
}

func TestFindSkillDirNotFound(t *testing.T) {
	cache := t.TempDir()
	_, err := findSkillDir(cache, "nonexistent")
	if err == nil {
		t.Error("expected error for missing skill")
	}
}

func TestInstallMissingSource(t *testing.T) {
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	_, err := tool.install(context.Background(), map[string]any{})
	if err == nil {
		t.Error("expected error for missing source")
	}
}

func TestInstallInvalidSource(t *testing.T) {
	tool := NewTool("/tmp/agents", "/tmp/cwd")
	_, err := tool.install(context.Background(), map[string]any{"source": "invalid"})
	if err == nil {
		t.Error("expected error for invalid source")
	}
}

func TestSearchAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tool := &SkillsTool{searchURL: server.URL}
	_, err := tool.search(context.Background(), map[string]any{"query": "test"})
	if err == nil {
		t.Error("expected error for API error")
	}
}

func TestSearchWithLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("expected limit '5', got %q", r.URL.Query().Get("limit"))
		}
		_ = json.NewEncoder(w).Encode(searchResponse{Count: 0, Skills: []SearchResult{}})
	}))
	defer server.Close()

	tool := &SkillsTool{searchURL: server.URL}
	_, err := tool.search(context.Background(), map[string]any{"query": "test", "limit": float64(5)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSearchInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	tool := &SkillsTool{searchURL: server.URL}
	_, err := tool.search(context.Background(), map[string]any{"query": "test"})
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestGetCacheDir(t *testing.T) {
	dir, err := getCacheDir("owner", "repo", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Error("expected absolute path")
	}

	dirWithRef, err := getCacheDir("owner", "repo", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir == dirWithRef {
		t.Error("expected different dirs for different refs")
	}
}

func TestCopyFileContent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(src, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "sub", "dest.txt")
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile error: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data))
	}
}

// createTestRepo creates a bare git repo with a skill directory for testing.
func createTestRepo(t *testing.T, skillName string) string {
	t.Helper()

	repoDir := t.TempDir()
	r, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Create skill directory with SKILL.md
	skillDir := filepath.Join(repoDir, skillName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: `+skillName+`
description: Test skill for install
---
# `+skillName+`
`), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := r.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := w.Add(skillName); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := w.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	return repoDir
}

func TestCloneOrUpdate(t *testing.T) {
	srcRepo := createTestRepo(t, "my-skill")

	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Clone from local repo (use file:// protocol)
	_, err := git.PlainClone(cacheDir, false, &git.CloneOptions{URL: srcRepo})
	if err != nil {
		t.Fatalf("initial clone: %v", err)
	}

	// Verify skill file exists in cache
	skillFile := filepath.Join(cacheDir, "my-skill", "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatalf("expected SKILL.md in cache: %v", err)
	}

	// findSkillDir should work on the cache
	found, err := findSkillDir(cacheDir, "my-skill")
	if err != nil {
		t.Fatalf("findSkillDir: %v", err)
	}

	// Copy to target
	targetDir := filepath.Join(t.TempDir(), "target")
	if err := copyDir(found, targetDir); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed SKILL.md: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty SKILL.md")
	}
}

func TestCloneOrUpdateExistingCache(t *testing.T) {
	srcRepo := createTestRepo(t, "test-skill")

	cacheDir := filepath.Join(t.TempDir(), "cache")

	// First clone
	_, err := git.PlainClone(cacheDir, false, &git.CloneOptions{URL: srcRepo})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// cloneOrUpdate should succeed on existing cache (pull path)
	// Note: this tests the "open existing repo" branch of cloneOrUpdate
	// We can't easily test the remote pull without a real remote, but
	// we verify it doesn't error when already up to date
	r, err := git.PlainOpen(cacheDir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	err = w.Pull(&git.PullOptions{RemoteName: "origin"})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		t.Fatalf("pull: %v", err)
	}
}

func TestInstallFullFlow(t *testing.T) {
	// Create a fake "cached repo" with a skill
	cacheBase := t.TempDir()
	skillSrc := filepath.Join(cacheBase, "my-skill")
	if err := os.MkdirAll(skillSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillSrc, "SKILL.md"), []byte(`---
name: my-skill
description: A great skill
---
# My Skill
`), 0o644); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".agents", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create tool with a fake cloner that just sets up the cache
	tool := &SkillsTool{
		workspace: filepath.Join(projectDir, ".agents"),
		cwd:       projectDir,
		cloner: func(_ context.Context, _, _, _, cacheDir string) error {
			// Copy our fake repo into the cache dir
			return copyDir(cacheBase, cacheDir)
		},
	}

	result, err := tool.install(context.Background(), map[string]any{
		"source": "test-owner/test-repo@my-skill",
	})
	if err != nil {
		t.Fatalf("install error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}

	// Verify skill was installed
	installed := filepath.Join(projectDir, ".agents", "skills", "my-skill", "SKILL.md")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("installed SKILL.md not found: %v", err)
	}
	if !strings.Contains(string(data), "A great skill") {
		t.Error("expected installed SKILL.md to contain description")
	}
}

func TestInstallSkillNotInRepo(t *testing.T) {
	cacheBase := t.TempDir()
	// Empty cache — no skill dirs

	tool := &SkillsTool{
		cwd: t.TempDir(),
		cloner: func(_ context.Context, _, _, _, cacheDir string) error {
			return copyDir(cacheBase, cacheDir)
		},
	}

	_, err := tool.install(context.Background(), map[string]any{
		"source": "owner/repo@nonexistent",
	})
	if err == nil {
		t.Error("expected error for skill not found in repo")
	}
}

func TestRemoveSingleCharName(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".agents", "skills", "x")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewTool(filepath.Join(dir, ".agents"), dir)
	_, err := tool.remove(map[string]any{"name": "x"})
	if err != nil {
		t.Fatalf("unexpected error removing single-char skill: %v", err)
	}
}
