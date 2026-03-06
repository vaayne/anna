package types

import "time"

// StreamOptions configures provider-level streaming behavior.
type StreamOptions struct {
	Temperature     *float64
	MaxTokens       *int
	APIKey          string
	BaseURL         string
	SessionID       string
	CacheRetention  string
	MaxRetryDelayMS *int
	Timeout         time.Duration
}

// SimpleStreamOptions extends StreamOptions with reasoning controls.
type SimpleStreamOptions struct {
	StreamOptions
	Reasoning       string
	ThinkingBudgets map[string]int
}

// CompleteOptions configures non-streaming requests.
type CompleteOptions struct {
	StreamOptions
}
