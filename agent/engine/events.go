package engine

import aitypes "github.com/vaayne/anna/ai/types"

// LoopEvent is the runtime event contract emitted by the agent loop.
type LoopEvent interface {
	Kind() string
}

// AgentStarted is emitted when a loop begins.
type AgentStarted struct{}

func (AgentStarted) Kind() string { return "agentStarted" }

// AssistantStarted is emitted when assistant streaming begins.
type AssistantStarted struct {
	Message aitypes.AssistantMessage
}

func (AssistantStarted) Kind() string { return "assistantStarted" }

// AssistantDelta forwards an incremental provider event with the current partial message.
type AssistantDelta struct {
	Event   aitypes.AssistantEvent
	Message aitypes.AssistantMessage
}

func (AssistantDelta) Kind() string { return "assistantDelta" }

// AssistantFinished is emitted when the final assistant message is assembled.
type AssistantFinished struct {
	Message aitypes.AssistantMessage
}

func (AssistantFinished) Kind() string { return "assistantFinished" }

// TurnStarted is emitted at the start of each loop turn.
type TurnStarted struct {
	Turn int
}

func (TurnStarted) Kind() string { return "turnStarted" }

// TurnFinished is emitted at the end of each loop turn.
type TurnFinished struct {
	Turn int
}

func (TurnFinished) Kind() string { return "turnFinished" }

// ToolStarted is emitted for each tool invocation.
type ToolStarted struct {
	ToolCall aitypes.ToolCall
}

func (ToolStarted) Kind() string { return "toolStarted" }

// ToolFinished is emitted when tool returns.
type ToolFinished struct {
	Result aitypes.ToolResultMessage
}

func (ToolFinished) Kind() string { return "toolFinished" }

// AgentFinished is emitted when loop completes.
type AgentFinished struct{}

func (AgentFinished) Kind() string { return "agentFinished" }

// AgentErrored is emitted for terminal loop errors.
type AgentErrored struct {
	Err error
}

func (AgentErrored) Kind() string { return "agentErrored" }
