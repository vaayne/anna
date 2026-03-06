package types

import "time"

// AssistantEvent is the normalized event emitted by providers during generation.
type AssistantEvent interface {
	eventType() string
}

// EventStart signals the beginning of an assistant turn.
type EventStart struct {
	Time time.Time
}

func (EventStart) eventType() string { return "start" }

// EventTextDelta carries incremental text output.
type EventTextDelta struct {
	Text string
}

func (EventTextDelta) eventType() string { return "textDelta" }

// EventThinkingDelta carries incremental reasoning output.
type EventThinkingDelta struct {
	Thinking string
}

func (EventThinkingDelta) eventType() string { return "thinkingDelta" }

// EventToolCallDelta carries incremental tool-call updates.
type EventToolCallDelta struct {
	ID        string
	Name      string
	Arguments string
}

func (EventToolCallDelta) eventType() string { return "toolCallDelta" }

// EventUsage reports token usage.
type EventUsage struct {
	Usage Usage
}

func (EventUsage) eventType() string { return "usage" }

// EventStop signals end of generation with reason.
type EventStop struct {
	Reason StopReason
}

func (EventStop) eventType() string { return "stop" }

// EventError reports terminal provider errors.
type EventError struct {
	Err error
}

func (EventError) eventType() string { return "error" }

// EventDone signals stream completion.
type EventDone struct{}

func (EventDone) eventType() string { return "done" }
