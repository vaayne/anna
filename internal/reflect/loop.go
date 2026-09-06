package reflect

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/platform/config"
)

// RunOnce executes the scheduler-owned review cycle across all enabled agents.
// Production and immediate operator runs must go through the registered
// scheduler builtin (using scheduler.RunJobNow for the latter) so concurrency
// control and run records are preserved. It returns the first cycle-level error;
// per-agent failures are logged inside runCycle and do not surface here.
func (s *Service) RunOnce(ctx context.Context) error {
	return s.runCycle(ctx)
}

func (s *Service) runCycle(ctx context.Context) error {
	return s.runCycleWithReviewer(ctx, s.reviewConversationStructured)
}

type reviewTargetFunc func(context.Context, *config.Snapshot, reviewTarget) error

func (s *Service) runCycleWithReviewer(ctx context.Context, review reviewTargetFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := s.reflectNow().Add(s.reflectRunSoftBudget())

	agents, err := s.store.ListEnabledAgents(ctx)
	if err != nil {
		s.log.Error("reflect: list agents", "error", err)
		return fmt.Errorf("list enabled agents: %w", err)
	}

	ctx, span := startCycleSpan(ctx, len(agents))
	defer span.End()

	agents = s.orderAgentsForReview(ctx, agents)
	totalReviewed := 0
	processedAgents := 0
	softStopped := false
	for i, agent := range agents {
		if err := ctx.Err(); err != nil {
			s.recordNextReviewAgentCursor(ctx, agents, processedAgents)
			return err
		}
		snap, err := s.snapshots.Snapshot(ctx, agent.ID)
		if err != nil {
			s.log.Error("reflect: snapshot", "agent", agent.ID, "error", err)
			processedAgents = i + 1
			continue
		}
		n, exhausted, err := s.reviewAgentWithReviewer(ctx, snap, deadline, review)
		if err != nil {
			s.log.Error("reflect: review agent", "agent", agent.ID, "error", err)
			if ctx.Err() != nil {
				s.recordNextReviewAgentCursor(ctx, agents, i)
				return ctx.Err()
			}
		}
		totalReviewed += n
		if exhausted {
			// This agent may still have unreviewed targets. Keep the cursor here;
			// watermark ordering will omit targets that completed in this run.
			s.recordNextReviewAgentCursor(ctx, agents, i)
			softStopped = true
			break
		}
		processedAgents = i + 1
	}
	if !softStopped {
		s.recordNextReviewAgentCursor(ctx, agents, processedAgents)
	}

	span.SetAttributes(attribute.Int("stella.reflect.sessions_reviewed", totalReviewed))
	s.maybeRunUsageCurator(ctx)
	return nil
}

func (s *Service) reviewAgentWithReviewer(ctx context.Context, snap *config.Snapshot, deadline time.Time, review reviewTargetFunc) (int, bool, error) {
	ctx, span := startAgentSpan(ctx, snap.AgentID)
	defer span.End()

	svc := s.services.GetService(snap.AgentID)
	if svc == nil || svc.Sessions == nil {
		return 0, false, fmt.Errorf("agent %s has no active session registry", snap.AgentID)
	}
	targets, err := s.listUnreviewedFromRegistry(ctx, svc.Sessions, snap.AgentID)
	if err != nil {
		recordError(span, err)
		return 0, false, fmt.Errorf("list unreviewed: %w", err)
	}

	span.SetAttributes(attribute.Int("stella.reflect.review_target_count", len(targets)))

	reviewed := 0
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return reviewed, false, err
		}
		if !deadline.IsZero() && !s.reflectNow().Before(deadline) {
			span.SetAttributes(attribute.Int("stella.reflect.sessions_reviewed", reviewed))
			return reviewed, true, nil
		}
		if err := s.authorizeTarget(ctx, target); err != nil {
			return reviewed, false, err
		}
		if err := review(ctx, snap, target); err != nil {
			s.log.Error("reflect: review conversation", "session", target.session.ID, "error", err)
			continue
		}
		reviewed++
	}

	span.SetAttributes(attribute.Int("stella.reflect.sessions_reviewed", reviewed))
	return reviewed, false, nil
}

func (s *Service) authorizeTarget(ctx context.Context, target reviewTarget) error {
	if s.capabilityGate == nil {
		return fmt.Errorf("reflect: background capability gate is not configured")
	}
	if target.session.UserID == "" || target.session.AgentID == "" {
		return fmt.Errorf("reflect: review target %q has no trusted user/agent owner", target.session.ID)
	}
	authority, err := agentaccess.WorkerAgentAuthority(target.session.UserID, target.session.AgentID)
	if err != nil {
		return fmt.Errorf("reflect: review target %q authority: %w", target.session.ID, err)
	}
	if err := s.capabilityGate(ctx, authority, target.session.AgentID, PluginID); err != nil {
		return fmt.Errorf("reflect: background capability denied for target %s: %w", target.session.ID, err)
	}
	return nil
}

func (s *Service) reflectNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Service) reflectRunSoftBudget() time.Duration {
	if s.runSoftBudget > 0 {
		return s.runSoftBudget
	}
	return defaultReflectRunSoftBudget
}
