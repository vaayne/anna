package types

import "time"

// ContentBlock is the normalized assistant content unit.
type ContentBlock interface {
	contentBlockKind() string
}

// TextContent represents plain text output.
type TextContent struct {
	Text string
}

func (TextContent) contentBlockKind() string { return "text" }

// ThinkingContent stores provider reasoning metadata.
type ThinkingContent struct {
	Thinking  string
	Signature string
	Redacted  bool
}

func (ThinkingContent) contentBlockKind() string { return "thinking" }

// ToolCall represents a tool invocation emitted by an assistant.
type ToolCall struct {
	ID               string
	Name             string
	Arguments        map[string]any
	ThoughtSignature string
}

func (ToolCall) contentBlockKind() string { return "toolCall" }

// ToolResultContent represents output from a tool execution.
type ToolResultContent struct {
	Text string
	JSON map[string]any
}

// Message is the base conversation entry.
type Message interface {
	messageRole() string
}

// UserMessage contains user-provided content.
type UserMessage struct {
	Content   any
	Timestamp time.Time
}

func (UserMessage) messageRole() string { return "user" }

// AssistantMessage contains assistant output and metadata.
type AssistantMessage struct {
	Content      []ContentBlock
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
	Content    []ToolResultContent
	IsError    bool
	Timestamp  time.Time
}

func (ToolResultMessage) messageRole() string { return "tool" }

// SystemMessage represents system-level instructions in the transcript.
type SystemMessage struct {
	Content string
}

func (SystemMessage) messageRole() string { return "system" }
