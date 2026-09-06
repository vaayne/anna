package reflect

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/scheduler"
)

// BuiltinJobName is the scheduler builtin name reflect registers under.
// Exported so gateway wiring and operational tooling refer to one source.
const BuiltinJobName = "reflect-review"

// NewBuiltinHandler returns a scheduler.OnJobFunc that runs one reflect
// review cycle per fire. Cycle-level failures (store errors, ctx cancellation)
// surface as errors so the scheduler marks the run errored. Per-agent
// failures are still logged inside the service and do not fail the run.
func NewBuiltinHandler(cfg Config) (scheduler.OnJobFunc, error) {
	if cfg.Memory == nil {
		return nil, fmt.Errorf("reflect: Memory provider is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("reflect: Store is required")
	}
	if cfg.Snapshots == nil {
		return nil, fmt.Errorf("reflect: Snapshots loader is required")
	}
	if cfg.StateStore == nil {
		return nil, fmt.Errorf("reflect: StateStore is required")
	}
	if cfg.Providers == nil {
		return nil, fmt.Errorf("reflect: Providers builder is required")
	}
	if _, ok := cfg.Memory.(memory.FactStore); !ok {
		return nil, fmt.Errorf("reflect: Memory provider must support fact reads")
	}
	if _, ok := memory.Unwrap(cfg.Memory).(factBatchWriter); !ok {
		return nil, fmt.Errorf("reflect: Memory provider must support fact batch writes")
	}
	if _, ok := memory.Unwrap(cfg.Memory).(memory.ReviewHistoryReader); !ok {
		return nil, fmt.Errorf("reflect: Memory provider must support exact review history")
	}
	if _, ok := cfg.Memory.(memory.ReviewHistoryReader); !ok {
		return nil, fmt.Errorf("reflect: Memory provider wrapper must preserve exact review history")
	}
	if cfg.SkillStore == nil {
		return nil, fmt.Errorf("reflect: SkillStore is required")
	}
	if cfg.SkillAuthorizer == nil {
		return nil, fmt.Errorf("reflect: SkillAuthorizer is required")
	}
	if cfg.UsageCuratorStore == nil {
		return nil, fmt.Errorf("reflect: UsageCuratorStore is required")
	}
	if cfg.Services == nil {
		return nil, fmt.Errorf("reflect: Services is required")
	}
	if cfg.CapabilityGate == nil {
		return nil, fmt.Errorf("reflect: CapabilityGate is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return func(ctx context.Context, job scheduler.Job) error {
		perFire := cfg
		return New(perFire).RunOnce(ctx)
	}, nil
}
