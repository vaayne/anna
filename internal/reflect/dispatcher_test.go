package reflect

import (
	"context"
	"testing"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/scheduler"
	"github.com/CherryHQ/stella/internal/skill"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
)

type fakeReflectStore struct {
	called bool
}

func (f *fakeReflectStore) ListEnabledAgents(_ context.Context) ([]config.Agent, error) {
	f.called = true
	return nil, nil
}

func (f *fakeReflectStore) Snapshot(_ context.Context, _ string) (*config.Snapshot, error) {
	return nil, nil
}

// Stubs to satisfy NewBuiltinHandler's dep validation. None of these are
// invoked when ListEnabledAgents returns no agents.

type stubMemory struct{ memory.Provider }

type stubStateStore struct{}

func (stubStateStore) Get(context.Context, pkgplugins.StateScope, string) (map[string]any, bool, error) {
	return nil, false, nil
}

func (stubStateStore) Set(context.Context, pkgplugins.StateScope, string, map[string]any) error {
	return nil
}
func (stubStateStore) Delete(context.Context, pkgplugins.StateScope, string) error { return nil }

func stubProviders(string, string, string) (providers.StreamFunc, error) { return nil, nil }

type stubStructuredMemory struct {
	stubMemory
	memory.FactStore
	factBatchWriter
	memory.ReviewHistoryReader
}

type stubStructuredMemoryWithoutReview struct {
	stubMemory
	memory.FactStore
	factBatchWriter
}

type stubStructuredSkillStore struct{}

func (stubStructuredSkillStore) ListActiveReflectOwnedUserAgentSkills(context.Context, string, string) ([]skill.Skill, error) {
	return nil, nil
}

func (stubStructuredSkillStore) LoadExactRevision(context.Context, skill.Skill, string) (skill.ManagedRevision, error) {
	return skill.ManagedRevision{}, nil
}

func (stubStructuredSkillStore) CreateReflectOwnedUserAgentSkill(context.Context, skill.ReflectSkillCreate) (skill.Skill, error) {
	return skill.Skill{}, nil
}

func (stubStructuredSkillStore) PatchReflectOwnedUserAgentSkill(context.Context, skill.ReflectSkillPatch) (skill.Skill, error) {
	return skill.Skill{}, nil
}

func (stubStructuredSkillStore) DeleteReflectOwnedUserAgentSkill(context.Context, skill.ReflectSkillDelete) (skill.Skill, error) {
	return skill.Skill{}, nil
}

type dispatcherSkillAuthorizer struct{}

func (dispatcherSkillAuthorizer) AuthorizeWorkerWrite(context.Context, string, string, string, bool) error {
	return nil
}

type emptyReflectServiceManager struct{}

func (emptyReflectServiceManager) GetService(string) *agent.Service { return nil }
func (emptyReflectServiceManager) Default() *agent.Service          { return nil }

func validConfig(store Store) Config {
	return Config{
		Memory:            stubStructuredMemory{stubMemory: stubMemory{}},
		Store:             store,
		Snapshots:         &fakeReflectStore{},
		StateStore:        stubStateStore{},
		Providers:         stubProviders,
		SkillStore:        stubStructuredSkillStore{},
		SkillAuthorizer:   dispatcherSkillAuthorizer{},
		UsageCuratorStore: fakeUsageCuratorStore{},
		Services:          emptyReflectServiceManager{},
		CapabilityGate:    func(context.Context, authz.Authority, string, ...string) error { return nil },
	}
}

func TestBuiltinHandlerInvokesStore(t *testing.T) {
	store := &fakeReflectStore{}
	handler, err := NewBuiltinHandler(validConfig(store))
	if err != nil {
		t.Fatalf("NewBuiltinHandler: %v", err)
	}

	job := scheduler.Job{Name: "reflect-review"}
	if err := handler(context.Background(), job); err != nil {
		t.Fatalf("handler: %v", err)
	}

	if !store.called {
		t.Error("ListEnabledAgents was not called")
	}
}

func TestNewBuiltinHandlerRejectsMissingDeps(t *testing.T) {
	if _, err := NewBuiltinHandler(Config{}); err == nil {
		t.Fatal("expected error for missing deps, got nil")
	}
}

func TestNewBuiltinHandlerValidatesStructuredDependencies(t *testing.T) {
	cfg := validConfig(&fakeReflectStore{})
	cfg.Memory = stubMemory{}
	if _, err := NewBuiltinHandler(cfg); err == nil {
		t.Fatal("expected missing structured memory dependencies to fail")
	}

	cfg.Memory = stubStructuredMemoryWithoutReview{stubMemory: stubMemory{}}
	if _, err := NewBuiltinHandler(cfg); err == nil {
		t.Fatal("expected missing exact review history to fail")
	}

	cfg.Memory = stubStructuredMemory{stubMemory: stubMemory{}}
	if _, err := NewBuiltinHandler(cfg); err != nil {
		t.Fatalf("structured dependencies rejected: %v", err)
	}
}

func TestNewBuiltinHandlerRejectsCuratorWithoutStore(t *testing.T) {
	for _, mode := range []UsageCuratorMode{UsageCuratorModeArmed, UsageCuratorModeShadow} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := validConfig(&fakeReflectStore{})
			cfg.UsageCuratorSettings = UsageCuratorSettings{Mode: mode}
			cfg.UsageCuratorStore = nil

			if _, err := NewBuiltinHandler(cfg); err == nil {
				t.Fatalf("expected error for %s usage curator without store", mode)
			}
		})
	}
}
