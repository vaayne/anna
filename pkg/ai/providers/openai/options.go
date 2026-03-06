package openai

import "github.com/vaayne/anna/pkg/ai/types"

// RequestOptions contains mapped request configuration for OpenAI completions.
type RequestOptions struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	Stream      bool     `json:"stream"`
}

func mapOptions(model types.Model, prompt string, opts types.StreamOptions, stream bool) RequestOptions {
	return RequestOptions{
		Model:       model.Name,
		Prompt:      prompt,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Stream:      stream,
	}
}
