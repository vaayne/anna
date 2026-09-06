package reflect

import (
	"context"
	"log/slog"
	"time"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/scheduler"
	"github.com/CherryHQ/stella/internal/skill"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
)

const (
	defaultMaxReviewTargetsPerAgent = 30
	defaultReflectRunSoftBudget     = 15 * time.Minute
)

// skillWriteAuthorizer authorizes reflect's staged skill writes (create/patch/
// delete) under a fresh trusted WorkerAgentAuthority per operation. It is
// satisfied by *skillaccess.Service; when unset, reflect skill writes fail closed.
type skillWriteAuthorizer interface {
	AuthorizeWorkerWrite(ctx context.Context, userID, agentID, skillID string, create bool) error
}

// reflectSkillStore is the exact managed-Skill boundary Reflect requires. It is
// deliberately one mandatory contract: reconciliation and curation cannot fall
// back to mutable current-file reads or discover optional write capabilities at
// runtime.
type reflectSkillStore interface {
	ListActiveReflectOwnedUserAgentSkills(context.Context, string, string) ([]skill.Skill, error)
	LoadExactRevision(context.Context, skill.Skill, string) (skill.ManagedRevision, error)
	CreateReflectOwnedUserAgentSkill(context.Context, skill.ReflectSkillCreate) (skill.Skill, error)
	PatchReflectOwnedUserAgentSkill(context.Context, skill.ReflectSkillPatch) (skill.Skill, error)
	DeleteReflectOwnedUserAgentSkill(context.Context, skill.ReflectSkillDelete) (skill.Skill, error)
}

// Config holds dependencies for the reflect service.
type Config struct {
	StateStore pkgplugins.StateStore
	Memory     memory.Provider
	Store      Store
	// Snapshots loads credential-aware per-agent snapshots for review.
	Snapshots  config.SnapshotLoader
	SkillStore reflectSkillStore
	// SkillAuthorizer applies Skill domain rules to Reflect's staged writes.
	SkillAuthorizer   skillWriteAuthorizer
	UsageCuratorStore UsageCuratorStore
	Log               *slog.Logger
	Providers         func(api, apiKey, baseURL string) (providers.StreamFunc, error)
	CandidateGates    CandidateGateSettings
	// UsageCuratorSettings defaults to armed. Operators may switch to shadow to
	// stop lifecycle writes while keeping scans and telemetry active.
	UsageCuratorSettings UsageCuratorSettings
	// Services provides the per-agent session registry used for review target
	// listing and owner-scoped memory coordinates.
	Services agent.ServiceManager
	// CapabilityGate resolves system/reflect against the target's trusted
	// user/agent tuple at the review boundary.
	CapabilityGate scheduler.BackgroundCapabilityGate
}

// watermarker abstracts watermark storage for testability.
type watermarker interface {
	getLine(ctx context.Context, sessionID string, line reflectLine) (reviewWatermark, error)
	setLine(ctx context.Context, sessionID string, line reflectLine, mark reviewWatermark) error
}

// Service runs background conversation review.
type Service struct {
	memory                   memory.Provider
	store                    Store
	snapshots                config.SnapshotLoader
	skillStore               reflectSkillStore
	skillAuthorizer          skillWriteAuthorizer
	stateStore               pkgplugins.StateStore
	usageCuratorStore        UsageCuratorStore
	wm                       watermarker
	maxReviewTargetsPerAgent int
	runSoftBudget            time.Duration
	now                      func() time.Time
	log                      *slog.Logger
	providers                func(api, apiKey, baseURL string) (providers.StreamFunc, error)
	candidateGates           CandidateGateSettings
	usageCuratorSettings     UsageCuratorSettings
	services                 agent.ServiceManager
	capabilityGate           scheduler.BackgroundCapabilityGate
}

// New creates a new reflect service.
func New(cfg Config) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Service{
		memory:                   cfg.Memory,
		store:                    cfg.Store,
		snapshots:                cfg.Snapshots,
		skillStore:               cfg.SkillStore,
		skillAuthorizer:          cfg.SkillAuthorizer,
		stateStore:               cfg.StateStore,
		usageCuratorStore:        cfg.UsageCuratorStore,
		wm:                       newWatermarkStore(cfg.StateStore),
		maxReviewTargetsPerAgent: defaultMaxReviewTargetsPerAgent,
		runSoftBudget:            defaultReflectRunSoftBudget,
		now:                      time.Now,
		log:                      cfg.Log,
		providers:                cfg.Providers,
		candidateGates:           cfg.CandidateGates.withDefaults(),
		usageCuratorSettings:     cfg.UsageCuratorSettings.withDefaults(),
		services:                 cfg.Services,
		capabilityGate:           cfg.CapabilityGate,
	}
}
