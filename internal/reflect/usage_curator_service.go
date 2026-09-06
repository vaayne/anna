package reflect

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/memory"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

const usageCuratorStateKeyPrefix = "curator_last_success:"

func (s *Service) maybeRunUsageCurator(ctx context.Context) {
	if s.usageCuratorStore == nil {
		return
	}
	if s.stateStore == nil {
		s.log.Warn("reflect usage curator: state store unavailable; skipping scheduled run")
		return
	}
	settings := s.usageCuratorSettings.withDefaults()
	now := settings.Now().UTC()
	pairs, err := s.usageCuratorStore.ListReflectUsagePairs(ctx)
	if err != nil {
		s.log.Error("reflect usage curator: list managed pairs", "error", err)
		return
	}
	for _, pair := range pairs {
		if err := ctx.Err(); err != nil {
			return
		}
		due, err := s.usageCuratorDue(ctx, pair, settings, now)
		if err != nil {
			s.log.Error("reflect usage curator: read pair schedule state", "user_id", pair.UserID, "agent_id", pair.AgentID, "error", err)
			continue
		}
		if !due {
			continue
		}
		if err := s.authorizeUsageCuratorPair(ctx, pair); err != nil {
			s.log.Error("reflect usage curator: pair capability denied; skipping", "user_id", pair.UserID, "agent_id", pair.AgentID, "error", err)
			continue
		}

		pairSettings := settings
		pairSettings.Now = func() time.Time { return now }
		pairCtx, span := startUsageCuratorSpan(ctx, pair, settings.Mode)
		report, err := s.runUsageCuratorOnce(pairCtx, pair, pairSettings)
		span.SetAttributes(
			attribute.Int("stella.reflect.curator.knowledge_candidates", report.KnowledgeCandidates),
			attribute.Int("stella.reflect.curator.knowledge_deprecated", report.KnowledgeDeprecated),
			attribute.Int("stella.reflect.curator.skill_candidates", report.SkillCandidates),
			attribute.Int("stella.reflect.curator.skill_deleted", report.SkillDeleted),
			attribute.Int64("stella.reflect.curator.duration_ms", report.Duration.Milliseconds()),
			attribute.Int("stella.reflect.curator.errors", report.Errors),
		)
		if report.Mode == UsageCuratorModeShadow {
			s.logUsageCuratorShadowEvidence(report)
		}
		if err != nil {
			recordError(span, err)
			span.End()
			s.log.Error("reflect usage curator: pair run failed", "user_id", pair.UserID, "agent_id", pair.AgentID, "error", err, "report", report)
			continue
		}
		span.End()
		if err := s.recordUsageCuratorSuccess(ctx, pair, now, report); err != nil {
			s.log.Error("reflect usage curator: record pair success", "user_id", pair.UserID, "agent_id", pair.AgentID, "error", err)
			continue
		}
		s.log.Info("reflect usage curator: pair run complete", "report", report)
	}
}

func (s *Service) authorizeUsageCuratorPair(ctx context.Context, pair usageCuratorPair) error {
	if s.capabilityGate == nil {
		return fmt.Errorf("reflect usage curator: background capability gate is not configured")
	}
	if pair.UserID == "" || pair.AgentID == "" {
		return fmt.Errorf("reflect usage curator: pair has no trusted user/agent owner")
	}
	authority, err := agentaccess.WorkerAgentAuthority(pair.UserID, pair.AgentID)
	if err != nil {
		return fmt.Errorf("reflect usage curator: pair authority: %w", err)
	}
	if err := s.capabilityGate(ctx, authority, pair.AgentID, PluginID); err != nil {
		return fmt.Errorf("reflect usage curator: %w", err)
	}
	return nil
}

func (s *Service) runUsageCuratorOnce(ctx context.Context, pair usageCuratorPair, settings usageCuratorSettings) (usageCuratorReport, error) {
	// Reflect may receive a traced memory provider in production; the write
	// capability belongs to the wrapped provider.
	factWriter, _ := memory.Unwrap(s.memory).(factBatchWriter)
	return runUsageCurator(ctx, usageCuratorRunConfig{
		Store:           s.usageCuratorStore,
		FactWriter:      factWriter,
		SkillWriter:     s.skillStore,
		SkillAuthorizer: s.skillAuthorizer,
		Pair:            pair,
		Settings:        settings,
	})
}

func (s *Service) usageCuratorDue(ctx context.Context, pair usageCuratorPair, settings usageCuratorSettings, now time.Time) (bool, error) {
	value, ok, err := s.stateStore.Get(ctx, usageCuratorPairScope(pair), usageCuratorPairStateKey(pair.UserID))
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	raw, _ := value["last_success_at"].(string)
	if raw == "" {
		return true, nil
	}
	lastSuccess, parsed := parseUsageCuratorLastSuccess(raw)
	if !parsed {
		return true, nil
	}
	return !now.Before(lastSuccess.Add(settings.RunInterval)), nil
}

func parseUsageCuratorLastSuccess(raw string) (time.Time, bool) {
	lastSuccess, err := time.Parse(time.RFC3339, raw)
	return lastSuccess, err == nil
}

func (s *Service) recordUsageCuratorSuccess(ctx context.Context, pair usageCuratorPair, at time.Time, report usageCuratorReport) error {
	return s.stateStore.Set(ctx, usageCuratorPairScope(pair), usageCuratorPairStateKey(pair.UserID), map[string]any{
		"last_success_at":      at.UTC().Format(time.RFC3339),
		"mode":                 string(report.Mode),
		"knowledge_candidates": report.KnowledgeCandidates,
		"knowledge_deprecated": report.KnowledgeDeprecated,
		"skill_candidates":     report.SkillCandidates,
		"skill_deleted":        report.SkillDeleted,
		"rule_counts":          report.RuleCounts,
		"duration_ms":          report.Duration.Milliseconds(),
		"errors":               report.Errors,
	})
}

func usageCuratorPairScope(pair usageCuratorPair) pkgplugins.StateScope {
	return pkgplugins.StateScope{Kind: pkgplugins.StateScopeAgent, ID: pair.AgentID}
}

func usageCuratorPairStateKey(userID string) string {
	return usageCuratorStateKeyPrefix + userID
}

func (s *Service) logUsageCuratorShadowEvidence(report usageCuratorReport) {
	for _, evidence := range report.Evidence {
		attributes := []any{
			"record_type", evidence.RecordType,
			"record_id", evidence.RecordID,
			"user_id", evidence.UserID,
			"agent_id", evidence.AgentID,
			"rule", evidence.Rule,
			"last_used_at", evidence.LastUsedAt,
			"cutoff", evidence.Cutoff,
			"pair_latest_activity_at", evidence.PairLatestActivityAt,
		}
		if evidence.RecordType == "skill" {
			attributes = append(attributes, "use_count", evidence.UseCount)
		}
		s.log.Info("reflect usage curator: shadow candidate", attributes...)
	}
}
