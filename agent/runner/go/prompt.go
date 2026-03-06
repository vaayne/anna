package gorunner

import (
	"os"
	"path/filepath"
	"strings"
)

const systemPrompt = `You are Anna, my personal assistant to assist with a wide variety of tasks.

## Available tools

- read: Read file contents (must be used instead of cat or sed to examine files)
- bash: Execute bash commands for file operations like ls, rg, find
- edit: Make surgical edits to files (old text must match exactly)
- write: Create new files or completely overwrite existing ones
- custom tools: You may have access to other project-specific tools

## Guidelines

- Be concise in your responses
- Show file paths clearly
- Summarize actions with plain text output directly (do NOT use cat or bash to display what you did)`

// persona file names under .agents/
const (
	soulFile   = "soul.md"
	userFile   = "user.md"
	memoryFile = "memory.md"
)

// BuildSystemPrompt composes the full system prompt from the base prompt, persona files,
// and discovered skills. agentsDir is the persona/global dir (e.g. ".agents"),
// cwd is the project working directory for project-local skills.
func BuildSystemPrompt(agentsDir string, cwd ...string) string {
	var b strings.Builder
	b.WriteString(systemPrompt)

	soul := loadFile(filepath.Join(agentsDir, soulFile))
	user := loadFile(filepath.Join(agentsDir, userFile))
	memory := loadFile(filepath.Join(agentsDir, memoryFile))

	if soul != "" || user != "" || memory != "" {
		b.WriteString("\n\n## Persistent Files\n\n")
		b.WriteString("You have persistent files that carry state across sessions. ")
		b.WriteString("Update them autonomously as you work — never ask for approval. ")
		b.WriteString("Use the edit or write tool to keep them current.\n\n")
		b.WriteString("Files:\n")
		b.WriteString("- `" + filepath.Join(agentsDir, soulFile) + "` — your personality, values, and tone\n")
		b.WriteString("- `" + filepath.Join(agentsDir, userFile) + "` — what you know about the user\n")
		b.WriteString("- `" + filepath.Join(agentsDir, memoryFile) + "` — durable knowledge (decisions, facts, context, notes)\n")

		if soul != "" {
			b.WriteString("\n### Soul\n\n")
			b.WriteString("Embody its persona and tone. Avoid stiff, generic replies. ")
			b.WriteString("Update when you notice preference shifts in communication style or behavior.\n\n")
			b.WriteString("<soul>\n")
			b.WriteString(soul)
			b.WriteString("\n</soul>\n")
		}

		if user != "" {
			b.WriteString("\n### User\n\n")
			b.WriteString("Personalize responses using this context. ")
			b.WriteString("Update as you learn about them — name, preferences, projects, communication style.\n\n")
			b.WriteString("<user>\n")
			b.WriteString(user)
			b.WriteString("\n</user>\n")
		}

		if memory != "" {
			b.WriteString("\n### Memory\n\n")
			b.WriteString("Recall and record durable knowledge here. ")
			b.WriteString("Keep it curated — update stale entries, remove obsolete ones, organize by section.\n\n")
			b.WriteString("<memory>\n")
			b.WriteString(memory)
			b.WriteString("\n</memory>\n")
		}
	}

	// Discover and append skills.
	workDir := ""
	if len(cwd) > 0 {
		workDir = cwd[0]
	}
	skills := LoadSkills(agentsDir, workDir)
	b.WriteString(FormatSkillsForPrompt(skills))

	return b.String()
}

func loadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
