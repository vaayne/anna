package gorunner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill represents a discovered skill with its metadata and location.
type Skill struct {
	Name                   string
	Description            string
	FilePath               string // absolute path to the SKILL.md or .md file
	BaseDir                string // directory containing the skill file
	Source                 string // "user", "project", or "path"
	DisableModelInvocation bool
}

// skillFrontmatter is the YAML frontmatter parsed from a SKILL.md file.
type skillFrontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

const (
	maxNameLength        = 64
	maxDescriptionLength = 1024
)

var validNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// LoadSkills discovers skills from project, workspace, and common directories.
// workspace is the workspace dir (e.g. ~/.anna/workspace), cwd is the working directory.
// Priority order: cwd/.agents/skills/ > workspace/skills/ > ~/.agents/skills/
func LoadSkills(workspace, cwd string) []Skill {
	home, _ := os.UserHomeDir()
	return loadSkills(home, workspace, cwd)
}

func loadSkills(homeDir, workspace, cwd string) []Skill {
	seen := map[string]bool{} // name → already loaded
	var skills []Skill

	add := func(s Skill) {
		if seen[s.Name] {
			return
		}
		seen[s.Name] = true
		skills = append(skills, s)
	}

	dedupPaths := map[string]bool{}
	addDir := func(dir, source string) {
		abs, _ := filepath.Abs(dir)
		if dedupPaths[abs] {
			return
		}
		dedupPaths[abs] = true
		for _, s := range loadSkillsFromDir(dir, source) {
			add(s)
		}
	}

	// 1. Project-local skills: cwd/.agents/skills/ (highest priority)
	if cwd != "" {
		addDir(filepath.Join(cwd, ".agents", "skills"), "project")
	}

	// 2. Workspace skills: workspace/skills/ (e.g. ~/.anna/workspace/skills/)
	if workspace != "" {
		addDir(filepath.Join(workspace, "skills"), "user")
	}

	// 3. Common skills: ~/.agents/skills/ (legacy/shared)
	if homeDir != "" {
		addDir(filepath.Join(homeDir, ".agents", "skills"), "common")
	}

	return skills
}

// loadSkillsFromDir scans a directory for skills.
// Discovery rules (matching Pi):
//   - Direct .md files in the root directory
//   - Recursive SKILL.md files under subdirectories
func loadSkillsFromDir(dir, source string) []Skill {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	return scanDir(dir, source, true)
}

func scanDir(dir, source string, isRoot bool) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var skills []Skill

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}

		fullPath := filepath.Join(dir, name)

		if entry.IsDir() {
			// Recurse into subdirectories looking for SKILL.md
			skills = append(skills, scanDir(fullPath, source, false)...)
			continue
		}

		if !entry.Type().IsRegular() {
			continue
		}

		// In root: any .md file. In subdirs: only SKILL.md.
		isRootMd := isRoot && strings.HasSuffix(name, ".md")
		isSkillMd := !isRoot && name == "SKILL.md"

		if !isRootMd && !isSkillMd {
			continue
		}

		if s, ok := loadSkillFromFile(fullPath, source); ok {
			skills = append(skills, s)
		}
	}

	return skills
}

func loadSkillFromFile(filePath, source string) (Skill, bool) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return Skill{}, false
	}

	fm, err := parseFrontmatter(string(data))
	if err != nil {
		return Skill{}, false
	}

	// Description is required — skip skills without one.
	if strings.TrimSpace(fm.Description) == "" {
		return Skill{}, false
	}

	skillDir := filepath.Dir(filePath)
	parentDirName := filepath.Base(skillDir)

	// Name: from frontmatter, or fall back to parent directory name.
	name := fm.Name
	if name == "" {
		name = parentDirName
	}

	return Skill{
		Name:                   name,
		Description:            fm.Description,
		FilePath:               filePath,
		BaseDir:                skillDir,
		Source:                 source,
		DisableModelInvocation: fm.DisableModelInvocation,
	}, true
}

// parseFrontmatter extracts YAML frontmatter delimited by "---" from content.
func parseFrontmatter(content string) (skillFrontmatter, error) {
	// Normalize line endings.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	if !strings.HasPrefix(content, "---") {
		return skillFrontmatter{}, fmt.Errorf("no frontmatter")
	}

	endIdx := strings.Index(content[3:], "\n---")
	if endIdx == -1 {
		return skillFrontmatter{}, fmt.Errorf("no closing frontmatter delimiter")
	}

	yamlStr := content[4 : 3+endIdx] // skip "---\n", end before "\n---"

	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(yamlStr), &fm); err != nil {
		return skillFrontmatter{}, fmt.Errorf("invalid yaml: %w", err)
	}

	return fm, nil
}

// FormatSkillsForPrompt renders the available skills as XML for the system prompt.
// Skills with DisableModelInvocation=true are excluded.
func FormatSkillsForPrompt(skills []Skill) string {
	var visible []Skill
	for _, s := range skills {
		if !s.DisableModelInvocation {
			visible = append(visible, s)
		}
	}

	if len(visible) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nThe following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use the read tool to load a skill's file when the task matches its description.\n")
	b.WriteString("When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n")
	b.WriteString("\n<available_skills>\n")

	for _, s := range visible {
		b.WriteString("  <skill>\n")
		b.WriteString("    <name>" + escapeXML(s.Name) + "</name>\n")
		b.WriteString("    <description>" + escapeXML(s.Description) + "</description>\n")
		b.WriteString("    <location>" + escapeXML(s.FilePath) + "</location>\n")
		b.WriteString("  </skill>\n")
	}

	b.WriteString("</available_skills>")
	return b.String()
}

// ValidateSkillName checks a skill name against the Agent Skills spec.
// Returns validation errors (empty slice if valid).
func ValidateSkillName(name, parentDirName string) []string {
	var errs []string
	if name != parentDirName {
		errs = append(errs, fmt.Sprintf("name %q does not match parent directory %q", name, parentDirName))
	}
	if len(name) > maxNameLength {
		errs = append(errs, fmt.Sprintf("name exceeds %d characters (%d)", maxNameLength, len(name)))
	}
	if !validNameRe.MatchString(name) {
		errs = append(errs, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errs = append(errs, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errs = append(errs, "name must not contain consecutive hyphens")
	}
	return errs
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}
