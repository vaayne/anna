package types

// Model identifies a concrete model and API family.
type Model struct {
	API  string
	Name string
}

// ToolDefinition describes a callable tool exposed to a model.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Context carries all conversation and tool state for a model request.
type Context struct {
	System   string
	Messages []Message
	Tools    []ToolDefinition
	Metadata map[string]string
}

// Usage tracks token accounting returned by providers.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// StopReason normalizes provider-specific stop signals.
type StopReason string

const (
	StopReasonStop    StopReason = "stop"
	StopReasonLength  StopReason = "length"
	StopReasonToolUse StopReason = "toolUse"
	StopReasonError   StopReason = "error"
	StopReasonAborted StopReason = "aborted"
)
