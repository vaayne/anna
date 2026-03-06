package agent

import (
	"github.com/vaayne/anna/agent/runner"
	"github.com/vaayne/anna/agent/store"
)

// CompactionConfig controls automatic session compaction.
type CompactionConfig struct {
	// MaxTokens triggers compaction when the estimated token count exceeds this.
	// 0 disables automatic compaction. Default: 80000.
	MaxTokens int `yaml:"max_tokens"`
	// KeepTail is the number of recent message entries to preserve verbatim
	// after compaction. Default: 20.
	KeepTail int `yaml:"keep_tail"`
}

// CompactionDefaults returns a CompactionConfig with sane defaults applied.
func (c CompactionConfig) WithDefaults() CompactionConfig {
	if c.MaxTokens == 0 {
		c.MaxTokens = 80_000
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
