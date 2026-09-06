package scheduler

import (
	"context"
	"fmt"

	"github.com/CherryHQ/stella/internal/authz"
)

// BackgroundCapabilityGate resolves the current plugin capabilities for a
// trusted owner/executor tuple. The resolver must read a fresh snapshot at the
// admission boundary; scheduler never accepts a plugin/config identity from a
// persisted job row.
type BackgroundCapabilityGate func(ctx context.Context, authority authz.Authority, agentID string, pluginIDs ...string) error

const (
	SchedulerPluginID = "system/scheduler"
	RecallyPluginID   = "system/recally"
)

// AuthorizeAgentStart performs the final fresh capability check for a
// user-owned scheduler chat immediately before the agent runtime starts it.
// The caller supplies the executor selected by composition, while the durable
// job remains the source of truth for the intended target.
func (s *Service) AuthorizeAgentStart(ctx context.Context, job Job, authority authz.Authority, agentID string) error {
	if job.OwnerKind != JobOwnerUser {
		return fmt.Errorf("scheduler: agent start requires a user-owned job %s", job.ID)
	}
	if job.AgentID == "" || agentID == "" || job.AgentID != agentID {
		return fmt.Errorf("scheduler: agent start target mismatch for job %s", job.ID)
	}
	return s.authorizeBackgroundForAgent(ctx, job, authority, agentID, nil)
}

func (s *Service) authorizeBackground(ctx context.Context, job Job, authority authz.Authority, handler OnJobFunc) error {
	return s.authorizeBackgroundForAgent(ctx, job, authority, job.AgentID, handler)
}

func (s *Service) authorizeBackgroundForAgent(ctx context.Context, job Job, authority authz.Authority, agentID string, handler OnJobFunc) error {
	if job.OwnerKind == JobOwnerSystem {
		// System jobs are only safe when this process has a live native handler.
		// A persisted message is not a fallback: dispatching it would turn an
		// orphaned maintenance row into an agent prompt.
		if handler == nil {
			return fmt.Errorf("scheduler: system job %q has no live handler registered", job.Name)
		}
		return nil
	}
	if job.OwnerKind != JobOwnerUser {
		return fmt.Errorf("scheduler: unsupported durable owner kind %q for job %s", job.OwnerKind, job.ID)
	}
	if agentID == "" {
		return fmt.Errorf("scheduler: user job %s has no agent to authorize", job.ID)
	}

	s.mu.Lock()
	gate := s.capabilityGate
	s.mu.Unlock()
	if gate == nil {
		return fmt.Errorf("scheduler: background capability gate is not configured")
	}
	pluginIDs := requiredBackgroundPluginIDs(job)
	if err := gate(ctx, authority, agentID, pluginIDs...); err != nil {
		return fmt.Errorf("scheduler: background capability denied for job %s: %w", job.ID, err)
	}
	return nil
}

func requiredBackgroundPluginIDs(job Job) []string {
	pluginIDs := []string{SchedulerPluginID}
	if job.JobKey == RecallyRSSTemplate.Key || job.JobKey == RecallyDigestTemplate.Key {
		pluginIDs = append(pluginIDs, RecallyPluginID)
	}
	return pluginIDs
}
