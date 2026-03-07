package skills

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Install fetches a skill from source and installs it into targetDir.
// Source format: "owner/repo@skill-name" or "owner/repo@skill-name#ref".
// Returns the installed skill name.
func Install(ctx context.Context, source, targetDir string) (string, error) {
	owner, repo, skillName, ref, err := parseSource(source)
	if err != nil {
		return "", err
	}

	cacheDir, err := getCacheDir(owner, repo, ref)
	if err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	if err := cloneOrUpdate(ctx, owner, repo, ref, cacheDir); err != nil {
		return "", fmt.Errorf("fetch repo: %w", err)
	}

	skillDir, err := findSkillDir(cacheDir, skillName)
	if err != nil {
		return "", err
	}

	dst := filepath.Join(targetDir, skillName)
	if err := copyDir(skillDir, dst); err != nil {
		return "", fmt.Errorf("install skill: %w", err)
	}

	return skillName, nil
}

func (t *SkillsTool) install(ctx context.Context, args map[string]any) (string, error) {
	source, _ := args["source"].(string)
	if source == "" {
		return "", fmt.Errorf("source is required for install action (e.g. owner/repo@skill-name)")
	}

	owner, repo, skillName, ref, err := parseSource(source)
	if err != nil {
		return "", err
	}

	cacheDir, err := getCacheDir(owner, repo, ref)
	if err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	clone := t.cloner
	if clone == nil {
		clone = cloneOrUpdate
	}
	if err := clone(ctx, owner, repo, ref, cacheDir); err != nil {
		return "", fmt.Errorf("fetch repo: %w", err)
	}

	skillDir, err := findSkillDir(cacheDir, skillName)
	if err != nil {
		return "", err
	}

	targetDir := filepath.Join(t.agentsDir, "skills", skillName)
	if err := copyDir(skillDir, targetDir); err != nil {
		return "", fmt.Errorf("install skill: %w", err)
	}

	return fmt.Sprintf("Skill %q installed to %s.", skillName, targetDir), nil
}

// parseSource parses "owner/repo@skill-name" or "owner/repo@skill-name#ref".
func parseSource(source string) (owner, repo, skillName, ref string, err error) {
	// Split off optional #ref
	if idx := strings.LastIndex(source, "#"); idx != -1 {
		ref = source[idx+1:]
		source = source[:idx]
	}

	// Split owner/repo@skill-name
	atIdx := strings.Index(source, "@")
	if atIdx == -1 {
		return "", "", "", "", fmt.Errorf("invalid source %q: expected format owner/repo@skill-name", source)
	}

	repoPath := source[:atIdx]
	skillName = source[atIdx+1:]

	parts := strings.SplitN(repoPath, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", fmt.Errorf("invalid source %q: expected format owner/repo@skill-name", source)
	}

	if skillName == "" {
		return "", "", "", "", fmt.Errorf("invalid source %q: skill name is empty", source)
	}

	// Validate all segments to prevent path traversal in cache/install paths.
	for _, seg := range []struct{ name, val string }{
		{"owner", parts[0]},
		{"repo", parts[1]},
		{"skill", skillName},
		{"ref", ref},
	} {
		if seg.val == "" {
			continue
		}
		if !safeSegmentRe.MatchString(seg.val) {
			return "", "", "", "", fmt.Errorf("invalid %s %q: must be alphanumeric, hyphens, dots, or underscores", seg.name, seg.val)
		}
	}

	return parts[0], parts[1], skillName, ref, nil
}

// safeSegmentRe matches path-safe segments: alphanumeric, hyphens, dots, underscores.
// Rejects "..", leading dots, slashes, and other path traversal patterns.
var safeSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func getCacheDir(owner, repo, ref string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	dirName := repo
	if ref != "" {
		dirName = repo + "@" + ref
	}

	cacheDir := filepath.Join(home, ".cache", "anna", "skills", owner, dirName)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	return cacheDir, nil
}

func cloneOrUpdate(ctx context.Context, owner, repo, ref, cacheDir string) error {
	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)

	// Try to open existing repo and pull
	r, err := git.PlainOpen(cacheDir)
	if err == nil {
		w, err := r.Worktree()
		if err != nil {
			return fmt.Errorf("open worktree: %w", err)
		}
		err = w.PullContext(ctx, &git.PullOptions{RemoteName: "origin"})
		if err != nil && err != git.NoErrAlreadyUpToDate {
			return fmt.Errorf("pull: %w", err)
		}
		return nil
	}

	// Fresh clone
	cloneOpts := &git.CloneOptions{
		URL:   repoURL,
		Depth: 1,
	}
	if ref != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(ref)
		cloneOpts.SingleBranch = true
	}

	_, err = git.PlainCloneContext(ctx, cacheDir, false, cloneOpts)
	if err != nil {
		// Clean up failed clone
		_ = os.RemoveAll(cacheDir)
		return fmt.Errorf("clone %s: %w", repoURL, err)
	}

	return nil
}

// findSkillDir walks the cache looking for a directory with the given name containing SKILL.md.
func findSkillDir(cacheDir, skillName string) (string, error) {
	var found string
	err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "SKILL.md" {
			dir := filepath.Dir(path)
			if filepath.Base(dir) == skillName {
				found = dir
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk cache: %w", err)
	}
	if found == "" {
		return "", fmt.Errorf("skill %q not found in repository", skillName)
	}
	return found, nil
}

// copyDir recursively copies src to dst, skipping .git directories.
func copyDir(src, dst string) error {
	// Remove existing target
	if err := os.RemoveAll(dst); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		if !d.Type().IsRegular() {
			return nil // skip symlinks etc.
		}

		return copyFile(path, target)
	})
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	_, err = io.Copy(out, in)
	return err
}
