package anthropic

import "github.com/vaayne/anna/pkg/ai/types"

// RequestOptions contains mapped request fields for Anthropic messages.
type RequestOptions struct {
	Model       string   `json:"model"`
	System      string   `json:"system,omitempty"`
	Messages    []any    `json:"messages"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens"`
	Stream      bool     `json:"stream"`
}

func mapOptions(model types.Model, ctx types.Context, opts types.StreamOptions, messages []any) RequestOptions {
	maxTokens := 1024
	if opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}

	return RequestOptions{
		Model:       model.Name,
		System:      ctx.System,
		Messages:    messages,
		Temperature: opts.Temperature,
		MaxTokens:   maxTokens,
		Stream:      true,
	}
}
