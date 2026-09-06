package reflect

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/cmd/stellad/store"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/authz"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/lcm"
	"github.com/CherryHQ/stella/internal/memory/memorywrite"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	skills "github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/pkg/ai"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
)

func newReflectPOSIXSkillStore(t *testing.T, db *pgxpool.Pool) *skills.POSIXStore {
	t.Helper()
	manager, err := home.NewWorkspaceManager(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	store, err := skills.NewPOSIXStore(db, manager)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func currentReflectSkill(t *testing.T, store *skills.POSIXStore, id string) *skills.Skill {
	t.Helper()
	identity, err := store.GetIdentity(t.Context(), id)
	if err != nil || identity == nil {
		return nil
	}
	revision, err := store.LoadCurrentRevision(t.Context(), *identity)
	if err != nil {
		t.Fatal(err)
	}
	return &revision.Skill
}

func TestUsageCuratorSettingsDefaultArmed(t *testing.T) {
	settings := (UsageCuratorSettings{}).withDefaults()
	if settings.Mode != UsageCuratorModeArmed {
		t.Fatalf("default curator mode = %q, want armed", settings.Mode)
	}
}

func TestUsageCuratorShadowReportsWithoutWriting(t *testing.T) {
	store := fakeUsageCuratorStore{
		knowledge: []usageCuratorKnowledgeCandidate{{
			FactID: "fact-1", UserID: "user-1", AgentID: "agent-1",
		}},
		skill: []usageCuratorSkillCandidate{{
			SkillID: "skill-1", UserID: "user-1", AgentID: "agent-1", Rule: usageCuratorSkillRuleLowUse,
		}},
	}
	factWriter := &fakeUsageCuratorFactWriter{}
	skillWriter := &fakeUsageCuratorSkillWriter{}

	report, err := runUsageCurator(context.Background(), usageCuratorRunConfig{
		SkillAuthorizer: &stubSkillAuthorizer{},
		Store:           store,
		FactWriter:      factWriter,
		SkillWriter:     skillWriter,
		Pair:            usageCuratorPair{UserID: "user-1", AgentID: "agent-1"},
		Settings: usageCuratorSettings{
			Mode: usageCuratorModeShadow,
			Now:  fixedUsageCuratorNow,
		},
	})
	if err != nil {
		t.Fatalf("runUsageCurator: %v", err)
	}
	if report.KnowledgeCandidates != 1 || report.SkillCandidates != 1 {
		t.Fatalf("report = %#v, want both candidate counts", report)
	}
	if len(report.Evidence) != 2 {
		t.Fatalf("shadow evidence = %#v, want one knowledge and one skill item", report.Evidence)
	}
	if report.RuleCounts["knowledge:idle"] != 1 || report.RuleCounts["skill:low_use"] != 1 {
		t.Fatalf("shadow rule counts = %#v, want one candidate of each rule", report.RuleCounts)
	}
	if report.Errors != 0 || report.Duration < 0 {
		t.Fatalf("shadow runtime evidence = duration %v errors %d", report.Duration, report.Errors)
	}
	if report.Evidence[0].UserID != "user-1" || report.Evidence[0].AgentID != "agent-1" || report.Evidence[0].RecordID == "" {
		t.Fatalf("shadow evidence missing pair or record identity: %#v", report.Evidence)
	}
	if len(factWriter.calls) != 0 {
		t.Fatalf("shadow fact writes = %#v, want none", factWriter.calls)
	}
	if len(skillWriter.inputs) != 0 {
		t.Fatalf("shadow skill writes = %#v, want none", skillWriter.inputs)
	}
}

func TestUsageCuratorReportCountsIndependentScanErrors(t *testing.T) {
	store := fakeUsageCuratorStore{
		knowledgeErrors: map[string]error{"user-1\x00agent-1": errors.New("knowledge scan")},
		skillErrors:     map[string]error{"user-1\x00agent-1": errors.New("skill scan")},
	}
	report, err := runUsageCurator(context.Background(), usageCuratorRunConfig{
		Store: store,
		Pair:  usageCuratorPair{UserID: "user-1", AgentID: "agent-1"},
		Settings: usageCuratorSettings{
			Mode: usageCuratorModeShadow,
			Now:  fixedUsageCuratorNow,
		},
	})
	if err == nil {
		t.Fatal("expected joined scan errors")
	}
	if report.Errors != 2 {
		t.Fatalf("report.Errors = %d, want 2", report.Errors)
	}
}

func TestUsageCuratorArmedDeprecatesKnowledgeAndDeletesSkills(t *testing.T) {
	store := fakeUsageCuratorStore{
		knowledge: []usageCuratorKnowledgeCandidate{
			{FactID: "fact-1", UserID: "user-1", AgentID: "agent-1"},
			{FactID: "fact-2", UserID: "user-1", AgentID: "agent-1"},
		},
		skill: []usageCuratorSkillCandidate{{
			SkillID: "skill-1", UserID: "user-1", AgentID: "agent-1", UseCount: 2, Rule: usageCuratorSkillRuleLowUse,
		}},
	}
	factWriter := &fakeUsageCuratorFactWriter{}
	skillWriter := &fakeUsageCuratorSkillWriter{}

	report, err := runUsageCurator(context.Background(), usageCuratorRunConfig{
		SkillAuthorizer: &stubSkillAuthorizer{},
		Store:           store,
		FactWriter:      factWriter,
		SkillWriter:     skillWriter,
		Pair:            usageCuratorPair{UserID: "user-1", AgentID: "agent-1"},
		Settings: usageCuratorSettings{
			Mode: usageCuratorModeArmed,
			Now:  fixedUsageCuratorNow,
		},
	})
	if err != nil {
		t.Fatalf("runUsageCurator: %v", err)
	}
	if report.KnowledgeDeprecated != 2 || report.SkillDeleted != 1 {
		t.Fatalf("report = %#v, want knowledge deprecated and skill deleted counts", report)
	}
	if len(factWriter.calls) != 2 {
		t.Fatalf("fact writes = %#v, want one batch per candidate", factWriter.calls)
	}
	call := factWriter.calls[0]
	if call.userID != "user-1" || call.agentID != "agent-1" || len(call.ops) != 1 {
		t.Fatalf("fact call = %#v, want one deprecate_many batch", call)
	}
	op := call.ops[0]
	if op.Action != memorywrite.FactBatchDeprecateMany || op.Subject != memory.FactSubjectWorld {
		t.Fatalf("fact op = %#v, want world deprecate_many", op)
	}
	if !slices.Equal(op.TargetFactIDs, []string{"fact-1"}) {
		t.Fatalf("target facts = %#v, want fact-1", op.TargetFactIDs)
	}
	if len(skillWriter.inputs) != 1 {
		t.Fatalf("skill writes = %#v, want one", skillWriter.inputs)
	}
	if got := skillWriter.inputs[0]; got.ID != "skill-1" {
		t.Fatalf("skill delete input = %#v, want skill-1", got)
	}
}

func TestMaybeRunUsageCuratorIsolatesPairFailures(t *testing.T) {
	ctx := context.Background()
	state := newMapStateStore()
	store := fakeUsageCuratorStore{
		pairs: []usageCuratorPair{
			{UserID: "user-failing", AgentID: "agent-a"},
			{UserID: "user-success", AgentID: "agent-b"},
		},
		knowledgeErrors: map[string]error{
			"user-failing\x00agent-a": errors.New("pair scan failed"),
		},
	}
	svc := &Service{
		stateStore: state, usageCuratorStore: store, log: testLogger(),
		usageCuratorSettings: UsageCuratorSettings{Mode: UsageCuratorModeShadow, Now: fixedUsageCuratorNow},
		capabilityGate:       func(context.Context, authz.Authority, string, ...string) error { return nil },
	}

	svc.maybeRunUsageCurator(ctx)

	failingScope := pkgplugins.StateScope{Kind: pkgplugins.StateScopeAgent, ID: "agent-a"}
	if _, ok, err := state.Get(ctx, failingScope, usageCuratorPairStateKey("user-failing")); err != nil || ok {
		t.Fatalf("failing pair state = ok:%v err:%v, want no success state", ok, err)
	}
	successScope := pkgplugins.StateScope{Kind: pkgplugins.StateScopeAgent, ID: "agent-b"}
	value, ok, err := state.Get(ctx, successScope, usageCuratorPairStateKey("user-success"))
	if err != nil {
		t.Fatalf("get successful pair state: %v", err)
	}
	if !ok || value["last_success_at"] == "" {
		t.Fatalf("successful pair state = %#v, want last_success_at", value)
	}
}

func TestMaybeRunUsageCuratorGatesArmedPairBeforeWrites(t *testing.T) {
	ctx := context.Background()
	store := fakeUsageCuratorStore{
		pairs: []usageCuratorPair{{UserID: "user-1", AgentID: "agent-1"}},
		knowledge: []usageCuratorKnowledgeCandidate{{
			FactID: "fact-1", UserID: "user-1", AgentID: "agent-1",
		}},
	}

	for _, tt := range []struct {
		name       string
		gateErr    error
		wantWrites int
		wantState  bool
	}{
		{name: "disabled", gateErr: errors.New("system/reflect disabled")},
		{name: "enabled", wantWrites: 1, wantState: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := newMapStateStore()
			memoryProvider := &fakeUsageCuratorMemoryProvider{}
			var gotAuthority authz.Authority
			var gotPlugins []string
			svc := &Service{
				memory:            memoryProvider,
				stateStore:        state,
				usageCuratorStore: store,
				skillStore:        stubStructuredSkillStore{},
				skillAuthorizer:   &stubSkillAuthorizer{},
				usageCuratorSettings: UsageCuratorSettings{
					Mode: UsageCuratorModeArmed,
					Now:  fixedUsageCuratorNow,
				},
				capabilityGate: func(_ context.Context, authority authz.Authority, agentID string, pluginIDs ...string) error {
					gotAuthority = authority
					if agentID != "agent-1" {
						t.Fatalf("agentID = %q, want agent-1", agentID)
					}
					gotPlugins = append([]string(nil), pluginIDs...)
					return tt.gateErr
				},
				log: testLogger(),
			}

			svc.maybeRunUsageCurator(ctx)

			if len(memoryProvider.calls) != tt.wantWrites {
				t.Fatalf("fact writes = %d, want %d", len(memoryProvider.calls), tt.wantWrites)
			}
			if len(gotPlugins) != 1 || gotPlugins[0] != PluginID {
				t.Fatalf("plugin IDs = %v, want [%s]", gotPlugins, PluginID)
			}
			if gotAuthority.UserID() != "user-1" || gotAuthority.AgentID() != "agent-1" {
				t.Fatalf("authority = %#v, want user-1/agent-1", gotAuthority)
			}
			_, ok, err := state.Get(ctx, usageCuratorPairScope(store.pairs[0]), usageCuratorPairStateKey("user-1"))
			if err != nil {
				t.Fatalf("get pair state: %v", err)
			}
			if ok != tt.wantState {
				t.Fatalf("pair success state present = %v, want %v", ok, tt.wantState)
			}
		})
	}
}

func TestMaybeRunUsageCuratorSkipsPairWithoutTrustedOwner(t *testing.T) {
	ctx := context.Background()
	state := newMapStateStore()
	memoryProvider := &fakeUsageCuratorMemoryProvider{}
	svc := &Service{
		memory:            memoryProvider,
		stateStore:        state,
		usageCuratorStore: fakeUsageCuratorStore{pairs: []usageCuratorPair{{AgentID: "agent-1"}}},
		usageCuratorSettings: UsageCuratorSettings{
			Mode: UsageCuratorModeArmed,
			Now:  fixedUsageCuratorNow,
		},
		capabilityGate: func(context.Context, authz.Authority, string, ...string) error {
			t.Fatal("capability gate called for pair without a trusted owner")
			return nil
		},
		log: testLogger(),
	}

	svc.maybeRunUsageCurator(ctx)

	if len(memoryProvider.calls) != 0 {
		t.Fatalf("fact writes = %d, want none", len(memoryProvider.calls))
	}
	_, ok, err := state.Get(ctx, pkgplugins.StateScope{Kind: pkgplugins.StateScopeAgent, ID: "agent-1"}, usageCuratorPairStateKey(""))
	if err != nil {
		t.Fatalf("get pair state: %v", err)
	}
	if ok {
		t.Fatal("pair success state written without a trusted owner")
	}
}

func TestUsageCuratorArmedUsesFactWriterBehindTracing(t *testing.T) {
	inner := &fakeUsageCuratorMemoryProvider{}
	svc := &Service{
		memory: memory.WithTracing(inner, nil),
		usageCuratorStore: fakeUsageCuratorStore{
			knowledge: []usageCuratorKnowledgeCandidate{{
				FactID:     "fact-1",
				UserID:     "user-1",
				AgentID:    "agent-1",
				LastUsedAt: fixedUsageCuratorNow().Add(-30 * 24 * time.Hour),
			}},
		},
	}

	report, err := svc.runUsageCuratorOnce(context.Background(), usageCuratorPair{UserID: "user-1", AgentID: "agent-1"}, UsageCuratorSettings{
		Mode: UsageCuratorModeArmed,
		Now:  fixedUsageCuratorNow,
	})
	if err != nil {
		t.Fatalf("runUsageCuratorOnce: %v", err)
	}
	if report.KnowledgeDeprecated != 1 {
		t.Fatalf("KnowledgeDeprecated = %d, want 1", report.KnowledgeDeprecated)
	}
	if len(inner.calls) != 1 {
		t.Fatalf("fact writer calls = %d, want 1", len(inner.calls))
	}
}

func TestUsageCuratorArmedSkipsKnowledgeWhenUsageChangedAfterSelection(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	q := sqlc.New(db)
	userID, agentID := seedUsageCuratorDB(t, ctx, db)
	provider, err := lcm.New(db, nil, nil)
	if err != nil {
		t.Fatalf("new lcm provider: %v", err)
	}
	fact, err := memorywrite.CreateFact(ctx, db, q, memory.FactWrite{
		UserID:  userID,
		AgentID: agentID,
		Subject: memory.FactSubjectWorld,
		Content: "Reflect world fact.",
		Source:  memory.SourceReflect,
	})
	if err != nil {
		t.Fatalf("create reflect fact: %v", err)
	}
	selectedLastUsed := fixedUsageCuratorNow().Add(-30 * 24 * time.Hour)
	if _, err := db.Exec(ctx, `
		UPDATE knowledge_usage
		SET last_used_at = $1
		WHERE fact_id = $2
	`, selectedLastUsed, fact.ID); err != nil {
		t.Fatalf("seed stale knowledge usage: %v", err)
	}
	if _, err := db.Exec(ctx, `
		UPDATE knowledge_usage
		SET last_used_at = $1
		WHERE fact_id = $2
	`, fixedUsageCuratorNow(), fact.ID); err != nil {
		t.Fatalf("simulate knowledge usage after selection: %v", err)
	}

	deprecated, err := deprecateCuratorKnowledge(ctx, provider, []usageCuratorKnowledgeCandidate{{
		FactID:     fact.ID,
		UserID:     userID,
		AgentID:    agentID,
		LastUsedAt: selectedLastUsed,
	}})
	if err != nil {
		t.Fatalf("deprecateCuratorKnowledge: %v", err)
	}
	if deprecated != 0 {
		t.Fatalf("deprecated = %d, want 0", deprecated)
	}
	row, err := q.GetFact(ctx, sqlc.GetFactParams{ID: fact.ID, UserID: userID, AgentID: agentID})
	if err != nil {
		t.Fatalf("read fact after curator: %v", err)
	}
	if row.Status != string(memory.FactStatusActive) {
		t.Fatalf("fact status = %s, want active", row.Status)
	}
}

func TestUsageCuratorArmedDoesNotRollBackSiblingKnowledgeWhenOneCandidateChanged(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	q := sqlc.New(db)
	userID, agentID := seedUsageCuratorDB(t, ctx, db)
	seedEligibleCuratorActivity(t, ctx, db, userID, agentID, fixedUsageCuratorNow())
	provider, err := lcm.New(db, nil, nil)
	if err != nil {
		t.Fatalf("new lcm provider: %v", err)
	}
	selectedLastUsed := fixedUsageCuratorNow().Add(-30 * 24 * time.Hour)
	changedFact, err := memorywrite.CreateFact(ctx, db, q, memory.FactWrite{
		UserID:  userID,
		AgentID: agentID,
		Subject: memory.FactSubjectWorld,
		Content: "Changed Reflect fact.",
		Source:  memory.SourceReflect,
	})
	if err != nil {
		t.Fatalf("create changed fact: %v", err)
	}
	staleFact, err := memorywrite.CreateFact(ctx, db, q, memory.FactWrite{
		UserID:  userID,
		AgentID: agentID,
		Subject: memory.FactSubjectWorld,
		Content: "Still stale Reflect fact.",
		Source:  memory.SourceReflect,
	})
	if err != nil {
		t.Fatalf("create stale fact: %v", err)
	}
	for _, factID := range []string{changedFact.ID, staleFact.ID} {
		if _, err := db.Exec(ctx, `
			UPDATE knowledge_usage
			SET last_used_at = $1
			WHERE fact_id = $2
		`, selectedLastUsed, factID); err != nil {
			t.Fatalf("seed stale knowledge usage: %v", err)
		}
	}
	if _, err := db.Exec(ctx, `
		UPDATE knowledge_usage
		SET last_used_at = $1
		WHERE fact_id = $2
	`, fixedUsageCuratorNow(), changedFact.ID); err != nil {
		t.Fatalf("simulate changed knowledge usage: %v", err)
	}

	deprecated, err := deprecateCuratorKnowledge(ctx, provider, []usageCuratorKnowledgeCandidate{
		{FactID: changedFact.ID, UserID: userID, AgentID: agentID, LastUsedAt: selectedLastUsed},
		{FactID: staleFact.ID, UserID: userID, AgentID: agentID, LastUsedAt: selectedLastUsed},
	})
	if err != nil {
		t.Fatalf("deprecateCuratorKnowledge: %v", err)
	}
	if deprecated != 1 {
		t.Fatalf("deprecated = %d, want 1", deprecated)
	}
	changedRow, err := q.GetFact(ctx, sqlc.GetFactParams{ID: changedFact.ID, UserID: userID, AgentID: agentID})
	if err != nil {
		t.Fatalf("read changed fact: %v", err)
	}
	staleRow, err := q.GetFact(ctx, sqlc.GetFactParams{ID: staleFact.ID, UserID: userID, AgentID: agentID})
	if err != nil {
		t.Fatalf("read stale fact: %v", err)
	}
	if changedRow.Status != string(memory.FactStatusActive) {
		t.Fatalf("changed fact status = %s, want active", changedRow.Status)
	}
	if staleRow.Status != string(memory.FactStatusDeprecated) {
		t.Fatalf("stale fact status = %s, want deprecated", staleRow.Status)
	}
}

func TestUsageCuratorArmedRecordsKnowledgeDeprecateMetadata(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	q := sqlc.New(db)
	userID, agentID := seedUsageCuratorDB(t, ctx, db)
	seedEligibleCuratorActivity(t, ctx, db, userID, agentID, fixedUsageCuratorNow())
	provider, err := lcm.New(db, nil, nil)
	if err != nil {
		t.Fatalf("new lcm provider: %v", err)
	}
	fact, err := memorywrite.CreateFact(ctx, db, q, memory.FactWrite{
		UserID:  userID,
		AgentID: agentID,
		Subject: memory.FactSubjectWorld,
		Content: "Stale Reflect knowledge.",
		Source:  memory.SourceReflect,
	})
	if err != nil {
		t.Fatalf("create reflect fact: %v", err)
	}
	lastUsed := fixedUsageCuratorNow().Add(-30 * 24 * time.Hour)
	pairLatestActivity := fixedUsageCuratorNow().Add(-24 * time.Hour)
	if _, err := db.Exec(ctx, `
		UPDATE knowledge_usage
		SET last_used_at = $1
		WHERE fact_id = $2
	`, lastUsed, fact.ID); err != nil {
		t.Fatalf("seed stale knowledge usage: %v", err)
	}

	deprecated, err := deprecateCuratorKnowledge(ctx, provider, []usageCuratorKnowledgeCandidate{{
		FactID:               fact.ID,
		UserID:               userID,
		AgentID:              agentID,
		LastUsedAt:           lastUsed,
		PairLatestActivityAt: pairLatestActivity,
	}})
	if err != nil {
		t.Fatalf("deprecateCuratorKnowledge: %v", err)
	}
	if deprecated != 1 {
		t.Fatalf("deprecated = %d, want 1", deprecated)
	}

	logs, err := q.ListMemoryChangelog(ctx, sqlc.ListMemoryChangelogParams{
		UserID:  userID,
		AgentID: agentID,
		Scope:   "fact",
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("list memory changelog: %v", err)
	}
	if len(logs) == 0 || logs[0].Action != "deprecate" || logs[0].EntityID.String != fact.ID {
		t.Fatalf("latest fact changelog = %#v, want deprecate for %s", logs, fact.ID)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(logs[0].Metadata.String), &metadata); err != nil {
		t.Fatalf("unmarshal deprecate metadata: %v", err)
	}
	if metadata["curator"] != "usage" || metadata["rule"] != "idle" {
		t.Fatalf("metadata = %#v, want curator usage idle", metadata)
	}
	if metadata["last_used_at"] == "" || metadata["pair_latest_activity_at"] == "" {
		t.Fatalf("metadata missing usage timestamps: %#v", metadata)
	}
}

func TestUsageCuratorArmedSkipsKnowledgeWhenEligibleActivityDisappearsAfterSelection(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	q := sqlc.New(db)
	userID, agentID := seedUsageCuratorDB(t, ctx, db)
	provider, err := lcm.New(db, nil, nil)
	if err != nil {
		t.Fatalf("new lcm provider: %v", err)
	}
	fact, err := memorywrite.CreateFact(ctx, db, q, memory.FactWrite{
		UserID: userID, AgentID: agentID, Subject: memory.FactSubjectWorld,
		Content: "Fact selected before its conversation was archived.", Source: memory.SourceReflect,
	})
	if err != nil {
		t.Fatalf("create reflect fact: %v", err)
	}
	lastUsed := fixedUsageCuratorNow().Add(-30 * 24 * time.Hour)
	activityAt := fixedUsageCuratorNow().Add(-24 * time.Hour)
	conversationID := uuid.NewString()
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, last_active)
		VALUES ($1, 'archived-after-selection', 'test', 'chat', $2, $3, $4)
	`, conversationID, agentID, userID, activityAt); err != nil {
		t.Fatalf("insert eligible conversation: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE knowledge_usage SET last_used_at = $1 WHERE fact_id = $2`, lastUsed, fact.ID); err != nil {
		t.Fatalf("seed stale knowledge usage: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE ctx_conversation SET archived = true WHERE id = $1`, conversationID); err != nil {
		t.Fatalf("archive conversation: %v", err)
	}

	deprecated, err := deprecateCuratorKnowledge(ctx, provider, []usageCuratorKnowledgeCandidate{{
		FactID: fact.ID, UserID: userID, AgentID: agentID,
		LastUsedAt: lastUsed, PairLatestActivityAt: activityAt,
	}})
	if err != nil {
		t.Fatalf("deprecateCuratorKnowledge: %v", err)
	}
	if deprecated != 0 {
		t.Fatalf("deprecated = %d, want 0 after activity gate disappeared", deprecated)
	}
	row, err := q.GetFact(ctx, sqlc.GetFactParams{ID: fact.ID, UserID: userID, AgentID: agentID})
	if err != nil {
		t.Fatalf("read fact after curator: %v", err)
	}
	if row.Status != string(memory.FactStatusActive) {
		t.Fatalf("fact status = %s, want active", row.Status)
	}
}

func TestUsageCuratorArmedSkipsSkillWhenUsageChangedAfterSelection(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	userID, agentID := seedUsageCuratorDB(t, ctx, db)
	skillStore := newReflectPOSIXSkillStore(t, db)
	created, err := skillStore.CreateReflectOwnedUserAgentSkill(ctx, skills.ReflectSkillCreate{
		UserID:          userID,
		AgentID:         agentID,
		Name:            "curator-skill-used-after-selection",
		Description:     "selected then used",
		MainFileContent: "# Selected Then Used\n",
	})
	if err != nil {
		t.Fatalf("CreateReflectOwnedUserAgentSkill: %v", err)
	}
	selectedLastUsed := fixedUsageCuratorNow().Add(-30 * 24 * time.Hour)
	if _, err := db.Exec(ctx, `
		UPDATE skill_usage
		SET last_used_at = $1, use_count = 2
		WHERE skill_id = $2
	`, selectedLastUsed, created.ID); err != nil {
		t.Fatalf("seed stale skill usage: %v", err)
	}
	if err := skillStore.TouchReflectSkillRuntimeUseDigest(ctx, created.ID, userID, agentID, created.ContentDigest); err != nil {
		t.Fatalf("TouchReflectSkillRuntimeUseDigest: %v", err)
	}

	deleted, err := deleteCuratorSkills(ctx, skillStore, &stubSkillAuthorizer{}, []usageCuratorSkillCandidate{{
		SkillID:       created.ID,
		UserID:        userID,
		AgentID:       agentID,
		ContentDigest: created.ContentDigest,
		UseCount:      2,
		LastUsedAt:    selectedLastUsed,
		Rule:          usageCuratorSkillRuleLowUse,
	}})
	if err != nil {
		t.Fatalf("deleteCuratorSkills: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	skill := currentReflectSkill(t, skillStore, created.ID)
	if skill == nil || skill.Status != skills.SkillStatusActive {
		t.Fatalf("skill after curator = %#v, want active", skill)
	}
}

func TestUsageCuratorArmedSkipsSkillWhenEligibleActivityDisappearsAfterSelection(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	userID, agentID := seedUsageCuratorDB(t, ctx, db)
	skillStore := newReflectPOSIXSkillStore(t, db)
	created, err := skillStore.CreateReflectOwnedUserAgentSkill(ctx, skills.ReflectSkillCreate{
		UserID: userID, AgentID: agentID, Name: "curator-skill-activity-disappeared",
		Description: "selected before archive", MainFileContent: "# Selected Before Archive\n",
	})
	if err != nil {
		t.Fatalf("CreateReflectOwnedUserAgentSkill: %v", err)
	}
	lastUsed := fixedUsageCuratorNow().Add(-30 * 24 * time.Hour)
	activityAt := fixedUsageCuratorNow().Add(-24 * time.Hour)
	conversationID := uuid.NewString()
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, last_active)
		VALUES ($1, 'skill-archived-after-selection', 'test', 'chat', $2, $3, $4)
	`, conversationID, agentID, userID, activityAt); err != nil {
		t.Fatalf("insert eligible conversation: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE skill_usage SET last_used_at = $1, use_count = 2 WHERE skill_id = $2`, lastUsed, created.ID); err != nil {
		t.Fatalf("seed stale skill usage: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE ctx_conversation SET archived = true WHERE id = $1`, conversationID); err != nil {
		t.Fatalf("archive conversation: %v", err)
	}

	deleted, err := deleteCuratorSkills(ctx, skillStore, &stubSkillAuthorizer{}, []usageCuratorSkillCandidate{{
		SkillID: created.ID, UserID: userID, AgentID: agentID,
		ContentDigest: created.ContentDigest,
		UseCount:      2, LastUsedAt: lastUsed, PairLatestActivityAt: activityAt,
		Rule: usageCuratorSkillRuleLowUse,
	}})
	if err != nil {
		t.Fatalf("deleteCuratorSkills: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 after activity gate disappeared", deleted)
	}
	resolved := currentReflectSkill(t, skillStore, created.ID)
	if resolved == nil || resolved.Status != skills.SkillStatusActive {
		t.Fatalf("skill after curator = %#v, want active", resolved)
	}
}

func TestSQLUsageCuratorStoreListsOnlyStaleReflectRecordsWithActivity(t *testing.T) {
	ctx := context.Background()
	db := dbtest.New(t)
	q := sqlc.New(db)
	userID, agentID := seedUsageCuratorDB(t, ctx, db)
	now := fixedUsageCuratorNow()
	lastUsed := now.Add(-45 * 24 * time.Hour)
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, last_active)
		VALUES ($1, 'curator-session-1', 'test', 'chat', $2, $3, $4)
	`, uuid.NewString(), agentID, userID, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	reflectFact, err := memorywrite.CreateFact(ctx, db, q, memory.FactWrite{
		UserID:  userID,
		AgentID: agentID,
		Subject: memory.FactSubjectWorld,
		Content: "Reflect world fact.",
		Source:  memory.SourceReflect,
	})
	if err != nil {
		t.Fatalf("create reflect fact: %v", err)
	}
	manualFact, err := memorywrite.CreateFact(ctx, db, q, memory.FactWrite{
		UserID:  userID,
		AgentID: agentID,
		Subject: memory.FactSubjectWorld,
		Content: "Manual world fact.",
		Source:  memory.SourceManual,
	})
	if err != nil {
		t.Fatalf("create manual fact: %v", err)
	}
	if _, err := db.Exec(ctx, `
		UPDATE knowledge_usage
		SET last_used_at = $1
		WHERE fact_id = $2
	`, lastUsed, reflectFact.ID); err != nil {
		t.Fatalf("seed reflect knowledge usage: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO knowledge_usage (fact_id, user_id, agent_id, last_used_at)
		VALUES ($1, $2, $3, $4)
	`, manualFact.ID, userID, agentID, lastUsed); err != nil {
		t.Fatalf("seed manual knowledge usage: %v", err)
	}

	skillStore := newReflectPOSIXSkillStore(t, db)
	staleSkill, err := skillStore.CreateReflectOwnedUserAgentSkill(ctx, skills.ReflectSkillCreate{
		UserID: userID, AgentID: agentID, Name: "curator-stale-skill", Description: "stale", MainFileContent: "# Stale\n",
	})
	if err != nil {
		t.Fatalf("create stale skill: %v", err)
	}
	highUseSkill, err := skillStore.CreateReflectOwnedUserAgentSkill(ctx, skills.ReflectSkillCreate{
		UserID: userID, AgentID: agentID, Name: "curator-high-use-skill", Description: "high use", MainFileContent: "# High\n",
	})
	if err != nil {
		t.Fatalf("create high-use skill: %v", err)
	}
	if _, err := db.Exec(ctx, `
		UPDATE skill_usage
		SET last_used_at = $1, use_count = 2, content_digest = $3
		WHERE skill_id = $2
	`, now.Add(-25*24*time.Hour), staleSkill.ID, testSkillContentDigest); err != nil {
		t.Fatalf("seed stale skill usage: %v", err)
	}
	if _, err := db.Exec(ctx, `
		UPDATE skill_usage
		SET last_used_at = $1, use_count = 8, content_digest = $3
		WHERE skill_id = $2
	`, now.Add(-25*24*time.Hour), highUseSkill.ID, testSkillContentDigest); err != nil {
		t.Fatalf("seed high-use skill usage: %v", err)
	}

	curatorStore := NewSQLUsageCuratorStore(q)
	knowledge, err := curatorStore.ListStaleReflectKnowledge(ctx, usageCuratorKnowledgeQuery{
		UserID:      userID,
		AgentID:     agentID,
		StaleBefore: now.Add(-20 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ListStaleReflectKnowledge: %v", err)
	}
	if len(knowledge) != 1 || knowledge[0].FactID != reflectFact.ID {
		t.Fatalf("knowledge candidates = %#v, want only reflect fact", knowledge)
	}
	skillCandidates, err := curatorStore.ListStaleReflectSkills(ctx, usageCuratorSkillQuery{
		UserID:            userID,
		AgentID:           agentID,
		StaleBefore:       now.Add(-60 * 24 * time.Hour),
		LowUseBefore:      now.Add(-20 * 24 * time.Hour),
		LowUseMaxUseCount: 5,
	})
	if err != nil {
		t.Fatalf("ListStaleReflectSkills: %v", err)
	}
	if len(skillCandidates) != 1 || skillCandidates[0].SkillID != staleSkill.ID || skillCandidates[0].Rule != usageCuratorSkillRuleLowUse {
		t.Fatalf("skill candidates = %#v, want only low-use stale skill", skillCandidates)
	}

	if _, err := db.Exec(ctx, `UPDATE ctx_conversation SET archived = true WHERE session_id = 'curator-session-1'`); err != nil {
		t.Fatalf("archive eligible conversation: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, last_active)
		VALUES ($1, 'curator-internal-session', 'test', 'scheduler', $2, $3, $4)
	`, uuid.NewString(), agentID, userID, now); err != nil {
		t.Fatalf("insert internal conversation: %v", err)
	}
	knowledge, err = curatorStore.ListStaleReflectKnowledge(ctx, usageCuratorKnowledgeQuery{UserID: userID, AgentID: agentID, StaleBefore: now.Add(-20 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("ListStaleReflectKnowledge with internal activity: %v", err)
	}
	if len(knowledge) != 0 {
		t.Fatalf("knowledge candidates = %#v, internal activity must not satisfy gate", knowledge)
	}
	skillCandidates, err = curatorStore.ListStaleReflectSkills(ctx, usageCuratorSkillQuery{
		UserID: userID, AgentID: agentID, StaleBefore: now.Add(-60 * 24 * time.Hour), LowUseBefore: now.Add(-20 * 24 * time.Hour), LowUseMaxUseCount: 5,
	})
	if err != nil {
		t.Fatalf("ListStaleReflectSkills with internal activity: %v", err)
	}
	if len(skillCandidates) != 0 {
		t.Fatalf("skill candidates = %#v, internal activity must not satisfy gate", skillCandidates)
	}
}

func fixedUsageCuratorNow() time.Time {
	return time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
}

func seedUsageCuratorDB(t *testing.T, ctx context.Context, db *pgxpool.Pool) (string, string) {
	t.Helper()
	user, err := appdb.NewOIDCStore(db).CreateUser(ctx, auth.User{
		ID:    uuid.NewString(),
		Email: "curator@test.local",
		Name:  "curator",
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	agentID := "curator-agent"
	if err := store.NewDBStore(db).CreateAgent(ctx, config.Agent{
		ID: agentID, Name: agentID, Model: "p/m", Workspace: "/tmp/curator-agent", Enabled: true,
	}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return user.ID, agentID
}

func seedEligibleCuratorActivity(t *testing.T, ctx context.Context, db *pgxpool.Pool, userID string, agentID string, lastActive time.Time) {
	t.Helper()
	if _, err := db.Exec(ctx, `
		INSERT INTO ctx_conversation (id, session_id, channel, kind, agent_id, user_id, last_active)
		VALUES ($1, $2, 'test', 'chat', $3, $4, $5)
	`, uuid.NewString(), "curator-eligible-"+uuid.NewString(), agentID, userID, lastActive); err != nil {
		t.Fatalf("insert eligible curator activity: %v", err)
	}
}

type fakeUsageCuratorStore struct {
	pairs           []usageCuratorPair
	knowledge       []usageCuratorKnowledgeCandidate
	skill           []usageCuratorSkillCandidate
	knowledgeErrors map[string]error
	skillErrors     map[string]error
}

func (s fakeUsageCuratorStore) ListReflectUsagePairs(context.Context) ([]usageCuratorPair, error) {
	return append([]usageCuratorPair(nil), s.pairs...), nil
}

func (s fakeUsageCuratorStore) ListStaleReflectKnowledge(_ context.Context, query usageCuratorKnowledgeQuery) ([]usageCuratorKnowledgeCandidate, error) {
	if err := s.knowledgeErrors[usageCuratorPairMapKey(query.UserID, query.AgentID)]; err != nil {
		return nil, err
	}
	out := make([]usageCuratorKnowledgeCandidate, 0, len(s.knowledge))
	for _, candidate := range s.knowledge {
		if query.UserID != "" && (candidate.UserID != query.UserID || candidate.AgentID != query.AgentID) {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

func (s fakeUsageCuratorStore) ListStaleReflectSkills(_ context.Context, query usageCuratorSkillQuery) ([]usageCuratorSkillCandidate, error) {
	if err := s.skillErrors[usageCuratorPairMapKey(query.UserID, query.AgentID)]; err != nil {
		return nil, err
	}
	out := make([]usageCuratorSkillCandidate, 0, len(s.skill))
	for _, candidate := range s.skill {
		if query.UserID != "" && (candidate.UserID != query.UserID || candidate.AgentID != query.AgentID) {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

func usageCuratorPairMapKey(userID string, agentID string) string {
	return userID + "\x00" + agentID
}

type fakeUsageCuratorFactWriter struct {
	calls []fakeUsageCuratorFactCall
}

type fakeUsageCuratorFactCall struct {
	userID  string
	agentID string
	ops     []memorywrite.FactBatchOperation
}

func (w *fakeUsageCuratorFactWriter) ApplyFactBatch(_ context.Context, userID string, agentID string, ops []memorywrite.FactBatchOperation) ([]memory.Fact, error) {
	w.calls = append(w.calls, fakeUsageCuratorFactCall{
		userID:  userID,
		agentID: agentID,
		ops:     append([]memorywrite.FactBatchOperation(nil), ops...),
	})
	facts := make([]memory.Fact, 0, len(ops))
	for _, op := range ops {
		for _, id := range op.TargetFactIDs {
			facts = append(facts, memory.Fact{ID: id, Status: memory.FactStatusDeprecated})
		}
	}
	return facts, nil
}

type fakeUsageCuratorMemoryProvider struct {
	fakeUsageCuratorFactWriter
}

func (p *fakeUsageCuratorMemoryProvider) Name() string { return "fake-usage-curator-memory" }

func (p *fakeUsageCuratorMemoryProvider) Bootstrap(context.Context, memory.Session) error { return nil }

func (p *fakeUsageCuratorMemoryProvider) Append(context.Context, memory.Session, ...ai.Message) error {
	return nil
}

func (p *fakeUsageCuratorMemoryProvider) Assemble(context.Context, memory.Session, int, int) ([]ai.Message, error) {
	return nil, nil
}

func (p *fakeUsageCuratorMemoryProvider) Stats(context.Context, memory.Session) (memory.SessionStats, error) {
	return memory.SessionStats{}, nil
}

func (p *fakeUsageCuratorMemoryProvider) Close() error { return nil }

type fakeUsageCuratorSkillWriter struct {
	inputs []skills.ReflectSkillDelete
}

func (w *fakeUsageCuratorSkillWriter) DeleteReflectOwnedUserAgentSkill(_ context.Context, in skills.ReflectSkillDelete) (skills.Skill, error) {
	w.inputs = append(w.inputs, in)
	return skills.Skill{ID: in.ID, Status: skills.SkillStatusActive}, nil
}
