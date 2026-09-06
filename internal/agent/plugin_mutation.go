package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/CherryHQ/stella/internal/plugin"
)

// ApplyPluginMutation keeps the database commit and runner retirement within
// one admission boundary. The callback must contain only a short transaction.
// Global admission fence; split by scope when plugin-write contention or
// tenant-wide runner churn becomes material.
func (pm *PoolManager) ApplyPluginMutation(ctx context.Context, mutate func() error) error {
	if pm == nil || pm.lifecycle == nil || mutate == nil {
		return errors.New("plugin mutation fence unavailable")
	}
	if err := pm.lifecycle.lockExclusive(ctx); err != nil {
		return err
	}
	defer pm.lifecycle.unlockExclusive()

	mutationErr := mutate()
	if mutationErr != nil && !errors.Is(mutationErr, plugin.ErrCommitOutcomeUnknown) {
		return mutationErr
	}
	if err := pm.resetPluginRunnersLocked(); err != nil {
		// Reset has already detached idle runners and marked active ones stale.
		// Reporting a failed write here would misrepresent a committed change.
		pm.log.Error("close retired plugin runners", "error", err)
	}
	return mutationErr
}

// The caller holds lifecycle exclusively, so admissions and service publication
// cannot interleave. Reset detaches idle runners before closing them and marks
// admitted runners stale; a Close error cannot restore an old capability set.
func (pm *PoolManager) resetPluginRunnersLocked() error {
	pm.mu.RLock()
	services := slices.Collect(maps.Values(pm.services))
	pm.mu.RUnlock()
	var failures []error
	for _, svc := range services {
		if err := svc.Runtime.ResetRunners(); err != nil {
			failures = append(failures, fmt.Errorf("retire plugin runners for %s: %w", svc.AgentID, err))
		}
	}
	return errors.Join(failures...)
}
