package agent

import "github.com/vaayne/anna/agent/runner"

// Session holds the state of a single conversation: the full event log
// (source of truth) and the currently assigned runner.
type Session struct {
	Events []runner.RPCEvent
	Runner runner.Runner
}
