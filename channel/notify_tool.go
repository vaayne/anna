package channel

import (
	"context"
	"fmt"

	aitypes "github.com/vaayne/anna/pkg/ai/types"
)

// NotifyTool is an agent tool that sends notifications via a Notifier.
type NotifyTool struct {
	notifier    Notifier
	defaultChat string
}

// NewNotifyTool creates a notify tool. defaultChat is used when the agent
// does not specify a target chat ID.
func NewNotifyTool(notifier Notifier, defaultChat string) *NotifyTool {
	return &NotifyTool{notifier: notifier, defaultChat: defaultChat}
}

var notifyInputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "The notification message to send (supports markdown)",
		},
		"chat_id": map[string]any{
			"type":        "string",
			"description": "Target chat ID. Omit to use the default notification chat.",
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
		Description: "Send a notification message to the user via Telegram. Use this to proactively push messages, alerts, cron job summaries, or long-running task results.",
		InputSchema: notifyInputSchema,
	}
}

func (t *NotifyTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	message, _ := args["message"].(string)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	chatID, _ := args["chat_id"].(string)
	if chatID == "" {
		chatID = t.defaultChat
	}
	if chatID == "" {
		return "", fmt.Errorf("no chat_id provided and no default notification chat configured")
	}

	silent, _ := args["silent"].(bool)

	err := t.notifier.Notify(ctx, Notification{
		ChatID: chatID,
		Text:   message,
		Silent: silent,
	})
	if err != nil {
		return "", fmt.Errorf("send notification: %w", err)
	}
	return "Notification sent successfully.", nil
}
