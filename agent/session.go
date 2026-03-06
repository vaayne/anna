package agent

import (
	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/agent/store"
)

// CompactionConfig controls automatic session compaction.
type CompactionConfig struct {
	// MaxEvents triggers compaction when the session event count exceeds this.
	// 0 disables automatic compaction. Default: 200.
	MaxEvents int `yaml:"max_events"`
	// KeepTail is the number of recent message entries to preserve verbatim
	// after compaction. Default: 20.
	KeepTail int `yaml:"keep_tail"`
}

// CompactionDefaults returns a CompactionConfig with sane defaults applied.
func (c CompactionConfig) WithDefaults() CompactionConfig {
	if c.MaxEvents == 0 {
		c.MaxEvents = 200
	}
	if c.KeepTail == 0 {
		c.KeepTail = 20
	}
	return c
}

// SessionInfo is an alias for store.SessionInfo.
type SessionInfo = store.SessionInfo

// Session holds the state of a single conversation: metadata, the full event
// log (source of truth) and the currently assigned runner.
type Session struct {
	Info   SessionInfo
	Events []runner.RPCEvent
	Runner runner.Runner
}
