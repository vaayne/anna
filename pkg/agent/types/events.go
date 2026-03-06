package types

import aitypes "github.com/vaayne/anna/pkg/ai/types"

// Event is the runtime event contract emitted by the agent.
type Event interface {
	kind() string
}

// AgentStarted is emitted when a loop begins.
type AgentStarted struct{}

func (AgentStarted) kind() string { return "agentStarted" }

// AssistantEmitted is emitted when assistant output is available.
type AssistantEmitted struct {
	Message aitypes.AssistantMessage
}

func (AssistantEmitted) kind() string { return "assistantEmitted" }

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
