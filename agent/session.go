package agent

import (
	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/agent/store"
)

// SessionInfo is an alias for store.SessionInfo.
type SessionInfo = store.SessionInfo

// Session holds the state of a single conversation: metadata, the full event
// log (source of truth) and the currently assigned runner.
type Session struct {
	Info   SessionInfo
	Events []runner.RPCEvent
	Runner runner.Runner
}
