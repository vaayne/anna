package types

import "time"

// StreamOptions configures provider-level streaming behavior.
type StreamOptions struct {
	Temperature     *float64
	MaxTokens       *int
	APIKey          string
	BaseURL         string
	Transport       Transport
	CacheRetention  CacheRetention
	SessionID       string
	Headers         map[string]string
	Metadata        map[string]any
	MaxRetryDelayMS *int
	Timeout         time.Duration
}

// SimpleStreamOptions extends StreamOptions with reasoning controls.
type SimpleStreamOptions struct {
	StreamOptions
	Reasoning       ThinkingLevel
	ThinkingBudgets *ThinkingBudgets
}

// CompleteOptions configures non-streaming requests.
type CompleteOptions struct {
	StreamOptions
}
