package types

// Api identifies the wire protocol family.
type Api = string

// Common API constants.
const (
	ApiOpenAICompletions     Api = "openai-completions"
	ApiOpenAIResponses       Api = "openai-responses"
	ApiAnthropicMessages     Api = "anthropic-messages"
	ApiBedrockConverseStream Api = "bedrock-converse-stream"
	ApiGoogleGenerativeAI    Api = "google-generative-ai"
	ApiGoogleVertex          Api = "google-vertex"
)

// Provider identifies the upstream service.
type Provider = string

// ThinkingLevel controls provider reasoning depth.
type ThinkingLevel = string

const (
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
)

// ThinkingBudgets maps thinking levels to token budgets (token-based providers only).
type ThinkingBudgets struct {
	Minimal *int
	Low     *int
	Medium  *int
	High    *int
}

// CacheRetention controls prompt cache retention preference.
type CacheRetention = string

const (
	CacheNone  CacheRetention = "none"
	CacheShort CacheRetention = "short"
	CacheLong  CacheRetention = "long"
)

// Transport selects the streaming transport.
type Transport = string

const (
	TransportSSE       Transport = "sse"
	TransportWebSocket Transport = "websocket"
	TransportAuto      Transport = "auto"
)

// ModelCost describes per-million-token pricing.
type ModelCost struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Model identifies a concrete model and its capabilities.
type Model struct {
	ID            string
	Name          string
	API           string
	Provider      string
	BaseURL       string
	Reasoning     bool
	Input         []string // e.g. ["text", "image"]
	Cost          ModelCost
	ContextWindow int
	MaxTokens     int
	Headers       map[string]string
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

// UsageCost tracks monetary cost per token category.
type UsageCost struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	Total      float64
}

// Usage tracks token accounting returned by providers.
type Usage struct {
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	TotalTokens  int
	Cost         UsageCost
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
