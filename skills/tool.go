package skills

import (
	"context"
	"encoding/json"
	"fmt"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

var skillsInputSchema = func() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["search", "install", "list", "remove"],
      "description": "Action to perform: 'search' finds skills from the ecosystem, 'install' adds a skill to the project, 'list' shows installed skills, 'remove' deletes an installed skill"
    },
    "query": {
      "type": "string",
      "description": "Search query (required for search)"
    },
    "limit": {
      "type": "integer",
      "description": "Max results to return (default 10, for search)"
    },
    "source": {
      "type": "string",
      "description": "Package source to install, e.g. 'owner/repo@skill-name' (required for install)"
    },
    "name": {
      "type": "string",
      "description": "Name of the installed skill (required for remove)"
    }
  },
  "required": ["action"]
}`), &m)
	return m
}()

// SkillsTool exposes skill management as an agent tool.
type SkillsTool struct {
	agentsDir string
	cwd       string
	searchURL string // override for testing; empty uses default
}

// NewTool creates a SkillsTool for the given project directories.
func NewTool(agentsDir, cwd string) *SkillsTool {
	return &SkillsTool{agentsDir: agentsDir, cwd: cwd}
}

// Definition returns the tool definition for the LLM.
func (t *SkillsTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "skills",
		Description: "Manage agent skills. Use 'search' to find skills from the ecosystem, 'install' to add a skill (e.g. owner/repo@skill-name), 'list' to see installed skills, 'remove' to delete one.",
		InputSchema: skillsInputSchema,
	}
}

// Execute runs the skills tool action.
func (t *SkillsTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	action, _ := args["action"].(string)
	switch action {
	case "search":
		return t.search(ctx, args)
	case "install":
		return t.install(ctx, args)
	case "list":
		return t.list()
	case "remove":
		return t.remove(args)
	default:
		return "", fmt.Errorf("unknown action %q, expected search/install/list/remove", action)
	}
}
