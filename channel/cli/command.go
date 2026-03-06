package cli

import (
	"fmt"
	"strings"
)

type slashCommand struct {
	name        string
	description string
}

var slashCommands = []slashCommand{
	{"/model", "Switch model"},
	{"/new", "Start new session"},
	{"/quit", "Quit"},
	{"/exit", "Quit"},
}

func filterCommands(prefix string) []slashCommand {
	if prefix == "/" {
		return slashCommands
	}
	prefix = strings.ToLower(prefix)
	var result []slashCommand
	for _, cmd := range slashCommands {
		if strings.HasPrefix(cmd.name, prefix) {
			result = append(result, cmd)
		}
	}
	return result
}

func renderCompletions(completions []slashCommand, cursor int) string {
	if len(completions) == 0 {
		return ""
	}

	var sb strings.Builder

	// Find max name length for alignment.
	maxNameLen := 0
	for _, cmd := range completions {
		if len(cmd.name) > maxNameLen {
			maxNameLen = len(cmd.name)
		}
	}

	for i, cmd := range completions {
		gap := strings.Repeat(" ", maxNameLen-len(cmd.name)+4)

		var nameStr, descStr string
		if i == cursor {
			nameStr = completionSelectedStyle.Render(cmd.name)
			descStr = completionSelectedDescStyle.Render(cmd.description)
		} else {
			nameStr = completionItemStyle.Render(cmd.name)
			descStr = completionDescStyle.Render(cmd.description)
		}

		sb.WriteString(fmt.Sprintf("    %s%s%s", nameStr, gap, descStr))
		if i < len(completions)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
