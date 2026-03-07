package cron

import (
	"context"
	"encoding/json"
	"fmt"

	aitypes "github.com/vaayne/anna/ai/types"
)

var cronInputSchema = func() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["add", "list", "remove"],
      "description": "The action to perform"
    },
    "name": {
      "type": "string",
      "description": "Name of the job (required for add)"
    },
    "message": {
      "type": "string",
      "description": "The prompt/instruction to execute on schedule (required for add)"
    },
    "cron": {
      "type": "string",
      "description": "Cron expression, e.g. '0 9 * * 1-5' for weekdays at 9am (use cron OR every, not both)"
    },
    "every": {
      "type": "string",
      "description": "Go duration, e.g. '30m', '2h', '24h' (use every OR cron, not both)"
    },
    "at": {
      "type": "string",
      "description": "RFC3339 timestamp for a one-time job, e.g. '2024-01-15T14:30:00+08:00' (use at OR cron OR every, not combined)"
    },
    "session_mode": {
      "type": "string",
      "enum": ["reuse", "new"],
      "description": "Session behavior: 'reuse' (default) keeps conversation history across executions, 'new' starts a fresh session each time"
    },
    "id": {
      "type": "string",
      "description": "Job ID (required for remove)"
    }
  },
  "required": ["action"]
}`), &m)
	return m
}()

// CronTool exposes cron management as an agent tool.
type CronTool struct {
	service *Service
}

// NewTool creates a CronTool backed by the given service.
func NewTool(service *Service) *CronTool {
	return &CronTool{service: service}
}

// Definition returns the tool definition for the LLM.
func (t *CronTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "cron",
		Description: "Manage scheduled tasks. Use action 'add' to create a recurring or one-time job, 'list' to see all jobs, or 'remove' to delete a job. For one-time jobs, use the 'at' field with an RFC3339 timestamp.",
		InputSchema: cronInputSchema,
	}
}

// Execute runs the cron tool action.
func (t *CronTool) Execute(_ context.Context, args map[string]any) (string, error) {
	action, _ := args["action"].(string)
	switch action {
	case "add":
		return t.add(args)
	case "list":
		return t.list()
	case "remove":
		return t.remove(args)
	default:
		return "", fmt.Errorf("unknown action %q, expected add/list/remove", action)
	}
}

func (t *CronTool) add(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	message, _ := args["message"].(string)
	cronExpr, _ := args["cron"].(string)
	every, _ := args["every"].(string)

	at, _ := args["at"].(string)
	sessionMode, _ := args["session_mode"].(string)
	sched := Schedule{Cron: cronExpr, Every: every, At: at}
	job, err := t.service.AddJob(name, message, sched, sessionMode)
	if err != nil {
		return "", err
	}

	out, _ := json.MarshalIndent(job, "", "  ")
	return fmt.Sprintf("Job created:\n%s", out), nil
}

func (t *CronTool) list() (string, error) {
	jobs := t.service.ListJobs()
	if len(jobs) == 0 {
		return "No scheduled jobs.", nil
	}

	out, _ := json.MarshalIndent(jobs, "", "  ")
	return string(out), nil
}

func (t *CronTool) remove(args map[string]any) (string, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return "", fmt.Errorf("id is required for remove action")
	}

	if err := t.service.RemoveJob(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("Job %q removed.", id), nil
}
