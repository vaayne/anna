package gorunner

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/vaayne/anna/memory"
)

//go:embed template/system.md
var defaultBasicPrompt string

//go:embed template/soul.md
var defaultSoul string

//go:embed template/user.md
var defaultUser string

//go:embed template/fact.md
var defaultFact string

//go:embed template/memories.md.tmpl
var memoriesTemplate string

var memoriesTmpl = template.Must(template.New("memories").Parse(memoriesTemplate))

type promptMemories struct {
	Dir   string
	Soul  promptFile
	User  promptFile
	Facts promptFile
}

type promptFile struct {
	Path    string
	Content string
}

// BuildSystemPrompt composes the full system prompt: basic + memories + skills.
// The basic prompt defaults to the embedded system.md but can be overridden
// by placing a system.md file in the agents directory.
func BuildSystemPrompt(store *memory.Store, agentsDir string, cwd ...string) string {
	workDir := ""
	if len(cwd) > 0 {
		workDir = cwd[0]
	}

	// Basic prompt: use agentsDir/system.md if present, otherwise embedded default.
	basic := defaultBasicPrompt
	if path := resolveFile(agentsDir, "system.md"); path != "" {
		if custom, err := os.ReadFile(path); err == nil {
			basic = string(custom)
		}
	}

	soul, _ := store.Read(memory.FileSoul)
	user, _ := store.Read(memory.FileUser)
	facts, _ := store.Read(memory.FileFact)

	memories := promptMemories{
		Dir:   store.Dir(),
		Soul:  promptFile{Path: store.Path(memory.FileSoul), Content: fallback(soul, defaultSoul)},
		User:  promptFile{Path: store.Path(memory.FileUser), Content: fallback(user, defaultUser)},
		Facts: promptFile{Path: store.Path(memory.FileFact), Content: fallback(facts, defaultFact)},
	}

	var buf bytes.Buffer
	buf.WriteString(strings.TrimRight(basic, "\n"))
	_ = memoriesTmpl.Execute(&buf, memories)

	if skills := FormatSkillsForPrompt(LoadSkills(agentsDir, workDir)); skills != "" {
		buf.WriteString("\n")
		buf.WriteString(skills)
	}

	return buf.String()
}

// resolveFile finds a file in dir with case-insensitive matching.
// Returns the full path if found, empty string otherwise.
func resolveFile(dir, name string) string {
	exact := filepath.Join(dir, name)
	if _, err := os.Stat(exact); err == nil {
		return exact
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	target := strings.ToLower(name)
	for _, e := range entries {
		if strings.ToLower(e.Name()) == target {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func fallback(value, def string) string {
	if value != "" {
		return value
	}
	return strings.TrimSpace(def)
}
