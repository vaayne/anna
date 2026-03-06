package types

import aitypes "github.com/vaayne/anna/pkg/ai/types"

// Event is the runtime event contract emitted by the agent.
type Event interface {
	kind() string
}

// AgentStarted is emitted when a loop begins.
type AgentStarted struct{}

func (AgentStarted) kind() string { return "agentStarted" }

// AssistantStarted is emitted when assistant streaming begins.
type AssistantStarted struct {
	Message aitypes.AssistantMessage
}

func (AssistantStarted) kind() string { return "assistantStarted" }

// AssistantDelta forwards an incremental provider event with the current partial message.
type AssistantDelta struct {
	Event   aitypes.AssistantEvent
	Message aitypes.AssistantMessage
}

func (AssistantDelta) kind() string { return "assistantDelta" }

// AssistantFinished is emitted when the final assistant message is assembled.
type AssistantFinished struct {
	Message aitypes.AssistantMessage
}

func (AssistantFinished) kind() string { return "assistantFinished" }

// TurnStarted is emitted at the start of each loop turn.
type TurnStarted struct {
	Turn int
}

func (TurnStarted) kind() string { return "turnStarted" }

// TurnFinished is emitted at the end of each loop turn.
type TurnFinished struct {
	Turn int
}

func (TurnFinished) kind() string { return "turnFinished" }

// ToolStarted is emitted for each tool invocation.
type ToolStarted struct {
	ToolCall aitypes.ToolCall
}

func (ToolStarted) kind() string { return "toolStarted" }

// ToolFinished is emitted when tool returns.
type ToolFinished struct {
	Result aitypes.ToolResultMessage
}

func (ToolFinished) kind() string { return "toolFinished" }

// AgentFinished is emitted when loop completes.
type AgentFinished struct{}

func (AgentFinished) kind() string { return "agentFinished" }

// AgentErrored is emitted for terminal loop errors.
type AgentErrored struct {
	Err error
}

func (AgentErrored) kind() string { return "agentErrored" }
