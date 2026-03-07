package channel

import (
	"context"
	"fmt"

	aitypes "github.com/vaayne/anna/ai/types"
)

// NotifyTool is an agent tool that sends notifications via a Dispatcher.
type NotifyTool struct {
	dispatcher *Dispatcher
}

// NewNotifyTool creates a notify tool backed by the given dispatcher.
func NewNotifyTool(dispatcher *Dispatcher) *NotifyTool {
	return &NotifyTool{dispatcher: dispatcher}
}

var notifyInputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "The notification message to send (supports markdown)",
		},
		"channel": map[string]any{
			"type":        "string",
			"description": "Target backend (e.g. \"telegram\", \"slack\"). Omit to broadcast to all configured backends.",
		},
		"chat_id": map[string]any{
			"type":        "string",
			"description": "Target chat/channel within the backend. Omit to use the default.",
		},
		"silent": map[string]any{
			"type":        "boolean",
			"description": "Send without notification sound",
		},
	},
	"required": []string{"message"},
}

func (t *NotifyTool) Definition() aitypes.ToolDefinition {
	return aitypes.ToolDefinition{
		Name:        "notify",
		Description: "Send a notification message to the user. Supports multiple backends (Telegram, Slack, etc.). Omit 'channel' to broadcast to all configured backends. Use this for proactive messages, alerts, cron summaries, or long-running task results.",
		InputSchema: notifyInputSchema,
	}
}

func (t *NotifyTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	message, _ := args["message"].(string)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	ch, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	silent, _ := args["silent"].(bool)

	err := t.dispatcher.Notify(ctx, Notification{
		Channel: ch,
		ChatID:  chatID,
		Text:    message,
		Silent:  silent,
	})
	if err != nil {
		return "", fmt.Errorf("send notification: %w", err)
	}

	backends := t.dispatcher.Backends()
	if ch != "" {
		return fmt.Sprintf("Notification sent to %s.", ch), nil
	}
	return fmt.Sprintf("Notification broadcast to %v.", backends), nil
}
