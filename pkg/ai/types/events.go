package types

import "time"

// AssistantEvent is the normalized event emitted by providers during generation.
type AssistantEvent interface {
	eventType() string
}

// EventStart signals the beginning of an assistant turn.
type EventStart struct {
	Time    time.Time
	Partial *AssistantMessage
}

func (EventStart) eventType() string { return "start" }

// EventTextStart signals the start of a text content block.
type EventTextStart struct {
	ContentIndex int
	Partial      *AssistantMessage
}

func (EventTextStart) eventType() string { return "text_start" }

// EventTextDelta carries incremental text output.
type EventTextDelta struct {
	ContentIndex int
	Text         string
	Partial      *AssistantMessage
}

func (EventTextDelta) eventType() string { return "text_delta" }

// EventTextEnd signals the end of a text content block.
type EventTextEnd struct {
	ContentIndex int
	Content      string
	Partial      *AssistantMessage
}

func (EventTextEnd) eventType() string { return "text_end" }

// EventThinkingStart signals the start of a thinking content block.
type EventThinkingStart struct {
	ContentIndex int
	Partial      *AssistantMessage
}

func (EventThinkingStart) eventType() string { return "thinking_start" }

// EventThinkingDelta carries incremental reasoning output.
type EventThinkingDelta struct {
	ContentIndex int
	Thinking     string
	Partial      *AssistantMessage
}

func (EventThinkingDelta) eventType() string { return "thinking_delta" }

// EventThinkingEnd signals the end of a thinking content block.
type EventThinkingEnd struct {
	ContentIndex int
	Content      string
	Partial      *AssistantMessage
}

func (EventThinkingEnd) eventType() string { return "thinking_end" }

// EventToolCallStart signals the start of a tool call content block.
type EventToolCallStart struct {
	ContentIndex int
	Partial      *AssistantMessage
}

func (EventToolCallStart) eventType() string { return "toolcall_start" }

// EventToolCallDelta carries incremental tool-call updates.
type EventToolCallDelta struct {
	ContentIndex int
	ID           string
	Name         string
	Arguments    string
	Partial      *AssistantMessage
}

func (EventToolCallDelta) eventType() string { return "toolcall_delta" }

// EventToolCallEnd signals the end of a tool call content block.
type EventToolCallEnd struct {
	ContentIndex int
	ToolCall     ToolCall
	Partial      *AssistantMessage
}

func (EventToolCallEnd) eventType() string { return "toolcall_end" }

// EventUsage reports token usage.
type EventUsage struct {
	Usage Usage
}

func (EventUsage) eventType() string { return "usage" }

// EventStop signals end of generation with reason.
type EventStop struct {
	Reason  StopReason
	Message *AssistantMessage
}

func (EventStop) eventType() string { return "stop" }

// EventError reports terminal provider errors.
type EventError struct {
	Err   error
	Error *AssistantMessage
}

func (EventError) eventType() string { return "error" }

// EventDone signals stream completion.
type EventDone struct{}

func (EventDone) eventType() string { return "done" }
