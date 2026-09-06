package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/platform/config"
)

const canonicalPolicy = `{"version":1,"disabled":["builtin:alpha","system:beta"]}`

func rawAgentPolicy(t *testing.T, db *pgxpool.Pool, id string) []byte {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(context.Background(), `SELECT enabled_builtin_skills FROM agent WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read policy: %v", err)
	}
	return raw
}

func TestAgentSkillPolicyCreateAndSeedUseColumnDefault(t *testing.T) {
	s, db := setupDBStoreWithDB(t)
	ctx := context.Background()
	if err := s.CreateAgent(ctx, config.Agent{ID: "policy-default", Name: "Policy default", Enabled: true}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	assertCanonicalEmptyPolicy(t, rawAgentPolicy(t, db, "policy-default"))
	seedStore, seedDB := setupDBStoreWithDB(t)
	if err := seedStore.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	assertCanonicalEmptyPolicy(t, rawAgentPolicy(t, seedDB, "stella"))
}

func assertCanonicalEmptyPolicy(t *testing.T, raw []byte) {
	t.Helper()
	var policy struct {
		Version  int       `json:"version"`
		Disabled *[]string `json:"disabled"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("decode canonical empty policy %s: %v", raw, err)
	}
	if policy.Version != 1 || policy.Disabled == nil || len(*policy.Disabled) != 0 {
		t.Fatalf("canonical empty policy = %s; want version=1 and non-null disabled=[]", raw)
	}
}

func TestAgentSkillPolicyOrdinaryUpdatePreservesExactBytes(t *testing.T) {
	s, db := setupDBStoreWithDB(t)
	ctx := context.Background()
	a := config.Agent{
		ID: "policy-update", Name: "before", Model: "provider/model", ModelThinking: "low",
		ModelStrong: "provider/strong", ModelStrongThinking: "high", ModelFast: "provider/fast",
		ModelFastThinking: "none", SystemPrompt: "before", Soul: "before", Workspace: "/tmp/before",
		Sandbox: config.SandboxConfig{}, Scope: config.AgentScopeSystem, Enabled: true,
	}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE agent SET enabled_builtin_skills = $1::jsonb WHERE id = $2`, canonicalPolicy, a.ID); err != nil {
		t.Fatalf("seed canonical policy: %v", err)
	}
	before := rawAgentPolicy(t, db, a.ID)
	a.Name, a.Model, a.ModelThinking = "after", "other/model", "medium"
	a.ModelStrong, a.ModelStrongThinking = "other/strong", "off"
	a.ModelFast, a.ModelFastThinking = "other/fast", "high"
	a.SystemPrompt, a.Soul, a.Workspace = "after", "after", "/tmp/after"
	a.Scope, a.Enabled = config.AgentScopeRestricted, false
	if err := s.UpdateAgent(ctx, a); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if after := rawAgentPolicy(t, db, a.ID); !bytes.Equal(after, before) {
		t.Fatalf("ordinary UpdateAgent rewrote policy: got %s, want exact %s", after, before)
	}
}

func TestAgentSkillPolicyMutationsSerializeAndRollback(t *testing.T) {
	s, db := setupDBStoreWithDB(t)
	ctx := context.Background()
	if err := s.CreateAgent(ctx, config.Agent{ID: "policy-concurrent", Name: "Policy", Enabled: true}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Concurrent mutations use the row-locked query; neither committed ref may
	// erase the other.
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, ref := range []string{"system:alpha", "system:beta"} {
		wg.Go(func() {
			<-start
			_, err := s.SetAgentSkillPolicy(ctx, "policy-concurrent", ref, false)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent SetAgentSkillPolicy: %v", err)
		}
	}
	policy, err := s.ReadAgentSkillPolicy(ctx, "policy-concurrent")
	if err != nil {
		t.Fatalf("ReadAgentSkillPolicy: %v", err)
	}
	if got, want := policy.Disabled, []string{"system:alpha", "system:beta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent disabled = %#v, want %#v", got, want)
	}

	// Queue two same-ref writers behind an actual PostgreSQL row lock. PostgreSQL
	// grants the row lock in queue order, so the second committed transaction is
	// deterministically last and wins without a process-wide mutex.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // commit below makes this inert
	if _, err := tx.Exec(ctx, `SELECT enabled_builtin_skills FROM agent WHERE id = $1 FOR UPDATE`, "policy-concurrent"); err != nil {
		t.Fatalf("lock policy row: %v", err)
	}
	sameRefErrs := make(chan error, 2)
	go func() {
		_, err := s.SetAgentSkillPolicy(ctx, "policy-concurrent", "system:alpha", true)
		sameRefErrs <- err
	}()
	waitForPolicyRowLocks(t, db, 1)
	go func() {
		_, err := s.SetAgentSkillPolicy(ctx, "policy-concurrent", "system:alpha", false)
		sameRefErrs <- err
	}()
	waitForPolicyRowLocks(t, db, 2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release lock transaction: %v", err)
	}
	for range 2 {
		if err := <-sameRefErrs; err != nil {
			t.Fatalf("same-ref SetAgentSkillPolicy: %v", err)
		}
	}
	policy, err = s.ReadAgentSkillPolicy(ctx, "policy-concurrent")
	if err != nil || !policy.DisabledRef("system:alpha") {
		t.Fatalf("same-ref last committed update = %#v, %v; want disabled", policy, err)
	}

	// The database rejects noncanonical policy objects before application code
	// can observe or accidentally normalize them.
	for _, corrupt := range []string{
		`{"version":99,"disabled":[]}`,
		`{"version":1}`,
		`{"version":1,"disabled":null}`,
	} {
		before := rawAgentPolicy(t, db, "policy-concurrent")
		if _, err := db.Exec(ctx, `UPDATE agent SET enabled_builtin_skills = $1::jsonb WHERE id = $2`, corrupt, "policy-concurrent"); err == nil {
			t.Fatalf("database accepted corrupt policy %s", corrupt)
		}
		if after := rawAgentPolicy(t, db, "policy-concurrent"); !bytes.Equal(after, before) {
			t.Fatalf("rejected policy changed storage: got %s, want exact %s", after, before)
		}
	}
}

func waitForPolicyRowLocks(t *testing.T, db *pgxpool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.QueryRow(context.Background(), `
SELECT count(*)
FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock'
`).Scan(&waiting); err != nil {
			t.Fatalf("inspect policy lock wait: %v", err)
		}
		if waiting >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d row-locked policy mutations", want)
}
