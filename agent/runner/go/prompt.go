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

// contextFile represents a discovered AGENTS.md file with its path and content.
type contextFile struct {
	Path    string
	Content string
}

// BuildSystemPrompt composes the full system prompt: basic + memories + skills + project context.
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

	if ctxFiles := loadProjectContextFiles(workDir); len(ctxFiles) > 0 {
		buf.WriteString("\n\n# Project Context\n\n")
		buf.WriteString("Project-specific instructions and guidelines:\n\n")
		for _, f := range ctxFiles {
			buf.WriteString("## " + f.Path + "\n\n")
			buf.WriteString(strings.TrimRight(f.Content, "\n"))
			buf.WriteString("\n\n")
		}
	}

	return buf.String()
}

// loadProjectContextFiles walks from cwd up to the filesystem root,
// collecting AGENTS.md files from each directory (case-insensitive).
// Files are returned in root-to-leaf order (ancestors first).
func loadProjectContextFiles(cwd string) []contextFile {
	if cwd == "" {
		return nil
	}

	absDir, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}

	var files []contextFile
	seen := map[string]bool{}

	for {
		if path := resolveFile(absDir, "AGENTS.md"); path != "" {
			if !seen[path] {
				seen[path] = true
				if data, err := os.ReadFile(path); err == nil {
					files = append(files, contextFile{Path: path, Content: string(data)})
				}
			}
		}

		parent := filepath.Dir(absDir)
		if parent == absDir {
			break
		}
		absDir = parent
	}

	// Reverse so ancestors come first (root → leaf).
	for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
		files[i], files[j] = files[j], files[i]
	}

	return files
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
