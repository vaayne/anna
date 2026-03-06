package agent

// Session holds the state of a single conversation: the full event log
// (source of truth) and the currently assigned runner.
type Session struct {
	Events []RPCEvent
	Runner Runner
}
