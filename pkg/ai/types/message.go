package types

import "time"

// TextSignatureV1 carries model-generated text signature metadata.
type TextSignatureV1 struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Phase string `json:"phase,omitempty"` // "commentary" | "final_answer"
}

// ContentBlock is the normalized assistant content unit.
type ContentBlock interface {
	contentBlockKind() string
}

// TextContent represents plain text output.
type TextContent struct {
	Text          string
	TextSignature string // legacy id string or TextSignatureV1 JSON
}

func (TextContent) contentBlockKind() string { return "text" }

// ThinkingContent stores provider reasoning metadata.
type ThinkingContent struct {
	Thinking  string
	Signature string
	Redacted  bool
}

func (ThinkingContent) contentBlockKind() string { return "thinking" }

// ImageContent represents base64-encoded image data.
type ImageContent struct {
	Data     string // base64 encoded
	MimeType string // e.g. "image/jpeg", "image/png"
}

func (ImageContent) contentBlockKind() string { return "image" }

// ToolCall represents a tool invocation emitted by an assistant.
type ToolCall struct {
	ID               string
	Name             string
	Arguments        map[string]any
	ThoughtSignature string
}

func (ToolCall) contentBlockKind() string { return "toolCall" }

// Message is the base conversation entry.
type Message interface {
	messageRole() string
}

// UserMessage contains user-provided content.
// Content is string or []ContentBlock (TextContent | ImageContent).
type UserMessage struct {
	Content   any
	Timestamp time.Time
}

func (UserMessage) messageRole() string { return "user" }

// AssistantMessage contains assistant output and metadata.
type AssistantMessage struct {
	Content      []ContentBlock
	Api          string
	Provider     string
	Model        string
	Usage        Usage
	StopReason   StopReason
	ErrorMessage string
	Timestamp    time.Time
}

func (AssistantMessage) messageRole() string { return "assistant" }

// ToolResultMessage links a tool response to a tool call.
type ToolResultMessage struct {
	ToolCallID string
	ToolName   string
	Content    []ContentBlock // TextContent | ImageContent
	Details    any
	IsError    bool
	Timestamp  time.Time
}

func (ToolResultMessage) messageRole() string { return "tool" }
