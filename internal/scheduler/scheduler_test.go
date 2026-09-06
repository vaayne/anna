package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/goal"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.New(t)
}

// newTestService wraps New and explicitly replaces the unexported durable-fire
// boundary for scheduler mechanics tests. Access tests use the real direct rules.
func newTestService(t *testing.T, db *pgxpool.Pool) *Service {
	t.Helper()
	svc, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc.authorizeFire = func(context.Context, Job) (authz.Authority, error) {
		return agentaccess.SystemAgentAuthority("scheduler")
	}
	// These mechanics tests predate the public plugin gate and exercise River
	// dispatch itself. C2-specific tests cover the fail-closed missing-gate path.
	svc.capabilityGate = func(context.Context, authz.Authority, string, ...string) error { return nil }
	return svc
}

func testService(t *testing.T) *Service {
	t.Helper()
	db := testDB(t)
	svc := newTestService(t, db)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })
	return svc
}

// addTestJob is a convenience wrapper around AddJobWithOwner for tests.
func addTestJob(t *testing.T, svc *Service, name, message string, sched Schedule, sessionMode string) Job {
	t.Helper()
	job, err := svc.AddJobWithOwner(name, message, sched, sessionMode, "agent-a", "user-1")
	if err != nil {
		t.Fatalf("AddJobWithOwner: %v", err)
	}
	return job
}

// TestStartExternalRiverRequiresClient pins CR-004: in external-river mode the
// shared working client must be injected via SetRiverClient before start, else
// start() nil-derefs s.river. The guard fails fast with a clear error instead.
func TestStartExternalRiverRequiresClient(t *testing.T) {
	db := testDB(t)
	svc, err := New(db, WithExternalRiver())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("Start in external-river mode without SetRiverClient must error, got nil")
	}
}

type fakeWorkflowRunner struct {
	wf               ScheduledWorkflow
	latest           WorkflowRunState
	result           WorkflowInstantiateResult
	instantiateReq   WorkflowInstantiateRequest
	instantiateCalls int
	validateErr      error
	authorizeErr     error
	authorizeCalls   int
}

func (f *fakeWorkflowRunner) ValidateScheduledWorkflow(context.Context, WorkflowValidateRequest) (ScheduledWorkflow, error) {
	if f.validateErr != nil {
		return ScheduledWorkflow{}, f.validateErr
	}
	if f.wf.ID == "" {
		return ScheduledWorkflow{}, fmt.Errorf("not found")
	}
	return f.wf, nil
}

func (f *fakeWorkflowRunner) LatestWorkflowRun(context.Context, WorkflowLatestRunRequest) (WorkflowRunState, error) {
	return f.latest, nil
}

func (f *fakeWorkflowRunner) InstantiateWorkflow(_ context.Context, _ authz.Authority, req WorkflowInstantiateRequest) (WorkflowInstantiateResult, error) {
	f.instantiateReq = req
	f.instantiateCalls++
	return f.result, nil
}

func (f *fakeWorkflowRunner) AuthorizeWorkflow(context.Context, authz.Authority, string, authz.Action) error {
	f.authorizeCalls++
	return f.authorizeErr
}

func TestAddWorkflowJobMapsRunnerDomainNotFound(t *testing.T) {
	svc := testService(t)
	svc.SetWorkflowRunner(&fakeWorkflowRunner{validateErr: ErrWorkflowJobNotFound})
	_, err := svc.AddWorkflowJobWithOwner(context.Background(), "wf", Schedule{Every: "1h"}, "reuse", "", "user-1", "missing", nil, false)
	if !errors.Is(err, ErrWorkflowJobNotFound) {
		t.Fatalf("AddWorkflowJobWithOwner error = %v, want ErrWorkflowJobNotFound", err)
	}
}

func TestAddWorkflowJobValidation(t *testing.T) {
	svc := testService(t)
	svc.SetWorkflowRunner(&fakeWorkflowRunner{wf: ScheduledWorkflow{ID: "wf-1", FullyFrozen: false}})
	if _, err := svc.AddWorkflowJobWithOwner(context.Background(), "wf", Schedule{Every: "1h"}, "reuse", "", "user-1", "", nil, false); !errors.Is(err, ErrWorkflowJobValidation) {
		t.Fatalf("workflow job without workflow_id err=%v want validation", err)
	}
	if _, err := svc.addJobInternal(addJobSpec{Name: "wf", Message: "message", Schedule: Schedule{Every: "1h"}, SessionMode: "reuse", UserID: "user-1", OwnerKind: JobOwnerUser, ExecScope: ExecScopeUser, DispatchKind: DispatchKindWorkflow, Payload: map[string]any{"workflow_id": "wf-1"}, Enabled: true}); !errors.Is(err, ErrWorkflowJobValidation) {
		t.Fatalf("workflow job with message err=%v want validation", err)
	}
	if _, err := svc.AddWorkflowJobWithOwner(context.Background(), "wf", Schedule{Every: "1h"}, "reuse", "", "user-1", "wf-1", nil, false); !errors.Is(err, ErrWorkflowJobValidation) {
		t.Fatalf("partially frozen workflow err=%v want validation", err)
	}
	if _, err := svc.AddWorkflowJobWithOwner(context.Background(), "wf", Schedule{Every: "1h"}, "reuse", "", "user-1", "wf-1", nil, true); err != nil {
		t.Fatalf("allow_replan workflow job: %v", err)
	}
}

func TestDispatchWorkflowJobInstantiatesAndRecordsRoot(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := uuid.NewString()
	if _, err := db.Exec(ctx, `INSERT INTO auth_user (id, email) VALUES ($1, $2)`, userID, "sched-"+userID[:8]+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	agentID := uuid.NewString()
	if _, err := db.Exec(ctx, `INSERT INTO agent (id, name, workspace) VALUES ($1, 'scheduler-agent', '/tmp')`, agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	q := sqlc.New(db)
	goalSvc := goal.New(db, q)
	root, err := goalSvc.CreateRoot(ctx, goal.CreateInput{UserID: userID, AgentID: agentID, Title: "root", Intent: "root", Kind: goal.KindComposite, Required: true})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	svc := newTestService(t, db)
	svc.ctx = ctx
	runner := &fakeWorkflowRunner{wf: ScheduledWorkflow{ID: "wf-1", FullyFrozen: true}, result: WorkflowInstantiateResult{RunID: "wr-1", RootGoalID: root.ID}}
	svc.SetWorkflowRunner(runner)
	job, err := svc.AddWorkflowJobWithOwner(ctx, "wf", Schedule{Every: "1h"}, "reuse", agentID, userID, "wf-1", map[string]string{"topic": "launch"}, false)
	if err != nil {
		t.Fatalf("add workflow job: %v", err)
	}
	runID := "runhappy"
	if err := svc.tryStartJobRun(ctx, runID, job.ID, job.UserSessionID(userID), userID, time.Now().UTC()); err != nil {
		t.Fatalf("start run: %v", err)
	}
	sink := &RunOutputSink{}
	if err := svc.dispatchJob(withRunOutputSink(WithRunID(ctx, runID), sink), job); err != nil {
		t.Fatalf("dispatch workflow: %v", err)
	}
	if runner.instantiateReq.IdempotencyKey != runID {
		t.Fatalf("idempotency key = %q, want %q", runner.instantiateReq.IdempotencyKey, runID)
	}
	run, err := q.GetSchedJobRun(ctx, sqlc.GetSchedJobRunParams{ID: runID, JobID: job.ID})
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !run.RootGoalID.Valid || run.RootGoalID.String != root.ID {
		t.Fatalf("root_goal_id = %v, want %s", run.RootGoalID, root.ID)
	}
	if sink.get() != "workflow run wr-1 -> goal "+root.ID {
		t.Fatalf("output = %q", sink.get())
	}
}

func TestDispatchWorkflowJobContinuesAfterFailedPreviousRun(t *testing.T) {
	svc := testService(t)
	runner := &fakeWorkflowRunner{
		wf:     ScheduledWorkflow{ID: "wf-1", FullyFrozen: true},
		latest: WorkflowRunState{Found: true, Status: "failed", IdempotencyKey: "old-key", RootGoalID: "root-1", RootGoalTerminal: false},
		result: WorkflowInstantiateResult{RunID: "wr-new"},
	}
	svc.SetWorkflowRunner(runner)
	job, err := svc.AddWorkflowJobWithOwner(context.Background(), "wf", Schedule{Every: "1h"}, "reuse", "agent-a", "user-1", "wf-1", nil, false)
	if err != nil {
		t.Fatalf("add workflow job: %v", err)
	}
	sink := &RunOutputSink{}
	if err := svc.dispatchJob(withRunOutputSink(WithRunID(context.Background(), "runnew"), sink), job); err != nil {
		t.Fatalf("dispatch workflow: %v", err)
	}
	if runner.instantiateReq.IdempotencyKey != "runnew" {
		t.Fatalf("idempotency key = %q, want runnew", runner.instantiateReq.IdempotencyKey)
	}
	if sink.get() != "workflow run wr-new -> goal " {
		t.Fatalf("output = %q", sink.get())
	}
}

func TestDispatchWorkflowJobResumesStalledPreviousRun(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := newTestService(t, db)
	svc.ctx = ctx
	runner := &fakeWorkflowRunner{
		wf:     ScheduledWorkflow{ID: "wf-1", FullyFrozen: true},
		latest: WorkflowRunState{Found: true, Status: "materializing", IdempotencyKey: "old-key", RootGoalID: "root-1", RootGoalTerminal: false},
		result: WorkflowInstantiateResult{RunID: "wr-old", RootGoalID: "root-1"},
	}
	svc.SetWorkflowRunner(runner)
	job, err := svc.AddWorkflowJobWithOwner(ctx, "wf", Schedule{Every: "1h"}, "reuse", "agent-a", "user-1", "wf-1", nil, false)
	if err != nil {
		t.Fatalf("add workflow job: %v", err)
	}
	if err := svc.tryStartJobRun(ctx, "runresume", job.ID, job.UserSessionID("user-1"), "user-1", time.Now().UTC()); err != nil {
		t.Fatalf("start run: %v", err)
	}
	sink := &RunOutputSink{}
	if err := svc.dispatchJob(withRunOutputSink(WithRunID(ctx, "runresume"), sink), job); err != nil {
		t.Fatalf("dispatch resume: %v", err)
	}
	if runner.instantiateCalls != 1 {
		t.Fatalf("instantiate calls = %d, want 1", runner.instantiateCalls)
	}
	if runner.instantiateReq.IdempotencyKey != "old-key" {
		t.Fatalf("idempotency key = %q, want old-key", runner.instantiateReq.IdempotencyKey)
	}
	run, err := svc.q.GetSchedJobRun(ctx, sqlc.GetSchedJobRunParams{ID: "runresume", JobID: job.ID})
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !run.RootGoalID.Valid || run.RootGoalID.String != "root-1" {
		t.Fatalf("root_goal_id = %v, want root-1", run.RootGoalID)
	}
	if sink.get() != "resumed stalled workflow run wr-old -> goal root-1" {
		t.Fatalf("output = %q", sink.get())
	}
}

func TestDispatchWorkflowJobSkipsActivePreviousRun(t *testing.T) {
	svc := testService(t)
	runner := &fakeWorkflowRunner{wf: ScheduledWorkflow{ID: "wf-1", FullyFrozen: true}, latest: WorkflowRunState{Found: true, Status: "done", RootGoalID: "root-1", RootGoalTerminal: false}}
	svc.SetWorkflowRunner(runner)
	job, err := svc.AddWorkflowJobWithOwner(context.Background(), "wf", Schedule{Every: "1h"}, "reuse", "agent-a", "user-1", "wf-1", nil, false)
	if err != nil {
		t.Fatalf("add workflow job: %v", err)
	}
	sink := &RunOutputSink{}
	if err := svc.dispatchJob(withRunOutputSink(WithRunID(context.Background(), "runskip"), sink), job); err != nil {
		t.Fatalf("dispatch skip: %v", err)
	}
	if runner.instantiateReq.WorkflowID != "" {
		t.Fatalf("instantiate called during skip: %+v", runner.instantiateReq)
	}
	if sink.get() != "skipped: previous workflow run still active" {
		t.Fatalf("skip output = %q", sink.get())
	}
}

func TestDispatchChatJobStillUsesOnJob(t *testing.T) {
	svc := testService(t)
	called := false
	svc.SetOnJob(func(ctx context.Context, job Job, _ authz.Authority) error {
		called = true
		if job.DispatchKind != DispatchKindChat {
			t.Fatalf("dispatch_kind = %q", job.DispatchKind)
		}
		return nil
	})
	job := addTestJob(t, svc, "chat", "say hello", Schedule{Every: "1h"}, "")
	if err := svc.dispatchJob(context.Background(), job); err != nil {
		t.Fatalf("dispatch chat: %v", err)
	}
	if !called {
		t.Fatal("chat OnJob callback not called")
	}
}

func TestAddListRemoveJob(t *testing.T) {
	svc := testService(t)

	// Add a job.
	job := addTestJob(t, svc, "test", "say hello", Schedule{Every: "1h"}, "")
	if job.ID == "" {
		t.Fatal("expected non-empty job ID")
	}
	if job.Name != "test" {
		t.Errorf("Name = %q, want %q", job.Name, "test")
	}
	if !job.Enabled {
		t.Error("expected job to be enabled")
	}

	// List jobs.
	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("ListJobs: got %d, want 1", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("job ID = %q, want %q", jobs[0].ID, job.ID)
	}

	// Verify persistence in DB.
	row, err := svc.q.GetSchedulerJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetSchedulerJob: %v", err)
	}
	if row.Name != "test" {
		t.Errorf("DB name = %q, want %q", row.Name, "test")
	}

	// Remove job.
	if err := svc.RemoveJob(job.ID); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if jobs := svc.ListJobs(); len(jobs) != 0 {
		t.Errorf("ListJobs after remove: got %d, want 0", len(jobs))
	}
}

func TestAddJobValidation(t *testing.T) {
	svc := testService(t)

	pastTime := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	tests := []struct {
		name    string
		jName   string
		message string
		sched   Schedule
	}{
		{"empty name", "", "msg", Schedule{Every: "1h"}},
		{"empty message", "test", "", Schedule{Every: "1h"}},
		{"no schedule", "test", "msg", Schedule{}},
		{"both cron and every", "test", "msg", Schedule{Cron: "* * * * *", Every: "1h"}},
		{"both cron and at", "test", "msg", Schedule{Cron: "* * * * *", At: time.Now().Add(time.Hour).Format(time.RFC3339)}},
		{"both every and at", "test", "msg", Schedule{Every: "1h", At: time.Now().Add(time.Hour).Format(time.RFC3339)}},
		{"all three set", "test", "msg", Schedule{Cron: "* * * * *", Every: "1h", At: time.Now().Add(time.Hour).Format(time.RFC3339)}},
		{"invalid duration", "test", "msg", Schedule{Every: "bogus"}},
		{"invalid at format", "test", "msg", Schedule{At: "not-a-timestamp"}},
		{"past at timestamp", "test", "msg", Schedule{At: pastTime}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.AddJobWithOwner(tt.jName, tt.message, tt.sched, "", "", "")
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestRemoveJobNotFound(t *testing.T) {
	svc := testService(t)

	if err := svc.RemoveJob("nonexistent"); err == nil {
		t.Error("expected error for nonexistent job")
	}
}

func TestJobPersistenceAcrossRestart(t *testing.T) {
	db := dbtest.New(t)

	// Create and add a job.
	svc1 := newTestService(t, db)
	if err := svc1.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := addTestJob(t, svc1, "persist-test", "check weather", Schedule{Cron: "0 9 * * *"}, "")
	_ = svc1.Stop()

	// Create a new service from the same database.
	svc2 := newTestService(t, db)
	if err := svc2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = svc2.Stop()
	}()

	jobs := svc2.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("ListJobs after restart: got %d, want 1", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("job ID = %q, want %q", jobs[0].ID, job.ID)
	}
	if jobs[0].Name != "persist-test" {
		t.Errorf("job Name = %q, want %q", jobs[0].Name, "persist-test")
	}
}

func TestStartEphemeralSkipsPersistedJobs(t *testing.T) {
	db := dbtest.New(t)

	svc1 := newTestService(t, db)
	if err := svc1.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addTestJob(t, svc1, "persist-test", "check weather", Schedule{Every: "1h"}, "")
	_ = svc1.Stop()

	svc2 := newTestService(t, db)
	if err := svc2.StartEphemeral(context.Background()); err != nil {
		t.Fatalf("StartEphemeral: %v", err)
	}
	defer func() {
		_ = svc2.Stop()
	}()

	if jobs := svc2.ListJobs(); len(jobs) != 0 {
		t.Fatalf("ListJobs after StartEphemeral: got %d, want 0", len(jobs))
	}
}

func TestOnJobCallbackFires(t *testing.T) {
	svc := testService(t)

	var mu sync.Mutex
	var fired []string
	svc.SetOnJob(func(_ context.Context, job Job, _ authz.Authority) error {
		mu.Lock()
		fired = append(fired, job.ID)
		mu.Unlock()
		return nil
	})

	addTestJob(t, svc, "quick", "ping", Schedule{Every: "100ms"}, "")

	// Wait for the callback to fire. River promotes scheduled jobs to runnable
	// on its scheduler interval (5s default), so a freshly registered periodic
	// job's first fire can lag the nominal interval at cold start — allow well
	// past one scheduler tick rather than asserting sub-second latency.
	deadline := time.After(12 * time.Second)
	for {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("callback did not fire within 12s")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestAddJobWithOwner(t *testing.T) {
	svc := testService(t)

	job, err := svc.AddJobWithOwner("owned-job", "do work", Schedule{Every: "1h"}, "", "agent-x", "99")
	if err != nil {
		t.Fatalf("AddJobWithOwner: %v", err)
	}
	if job.AgentID != "agent-x" {
		t.Errorf("AgentID = %q, want %q", job.AgentID, "agent-x")
	}
	if job.UserID != "99" {
		t.Errorf("UserID = %q, want 99", job.UserID)
	}
	if !job.Enabled {
		t.Error("expected job to be enabled")
	}

	// Verify it appears in the list.
	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].ID != job.ID {
		t.Errorf("listed job ID = %q, want %q", jobs[0].ID, job.ID)
	}
}

func TestOneTimeJobCreation(t *testing.T) {
	svc := testService(t)

	futureTime := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	job := addTestJob(t, svc, "one-time-test", "do something once", Schedule{At: futureTime}, "")
	if job.Schedule.At != futureTime {
		t.Errorf("At = %q, want %q", job.Schedule.At, futureTime)
	}

	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("ListJobs: got %d, want 1", len(jobs))
	}
}

func TestOneTimeJobFiresAndRetires(t *testing.T) {
	svc := testService(t)

	var mu sync.Mutex
	var fired []string
	svc.SetOnJob(func(_ context.Context, job Job, _ authz.Authority) error {
		mu.Lock()
		fired = append(fired, job.ID)
		mu.Unlock()
		return nil
	})

	// Schedule 200ms from now.
	at := time.Now().Add(200 * time.Millisecond).Format(time.RFC3339Nano)
	job := addTestJob(t, svc, "fire-once", "ping once", Schedule{At: at}, "")

	// Wait for the callback to fire and cleanup to happen. River promotes a
	// scheduled (at) job to runnable on its scheduler interval (5s default), so
	// allow past one scheduler tick rather than asserting sub-second latency.
	deadline := time.After(12 * time.Second)
	for {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("callback did not fire within 12s")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Verify the callback fired with the right job.
	mu.Lock()
	if fired[0] != job.ID {
		t.Errorf("fired job ID = %q, want %q", fired[0], job.ID)
	}
	mu.Unlock()

	// Retirement runs after the callback returns; wait for the observable state
	// instead of sleeping and racing the retire goroutine. The job must survive
	// as a disabled row (deleting it would cascade away its run records) with
	// its River registration gone.
	deadline = time.After(2 * time.Second)
	for {
		jobs := svc.ListJobs()
		if len(jobs) == 1 && !jobs[0].Enabled {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("jobs after one-time fire: got %+v, want 1 disabled job", jobs)
		case <-time.After(20 * time.Millisecond):
		}
	}

	svc.mu.Lock()
	_, hasRef := svc.refs[job.ID]
	svc.mu.Unlock()
	if hasRef {
		t.Error("expected fired one-time job to have no live River registration")
	}

	// The disabled state must be persisted, not just in memory: a run record
	// referencing the job survives restarts only if the row itself does.
	row, err := svc.q.GetSchedulerJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetSchedulerJob after retire: %v", err)
	}
	if row.Enabled {
		t.Error("expected fired one-time job to be persisted as disabled")
	}
}

func TestOneTimeJobSkippedOnRestartIfPast(t *testing.T) {
	db := dbtest.New(t)

	// Create a service and add a one-time job in the future.
	svc1 := newTestService(t, db)
	if err := svc1.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	futureTime := time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	addTestJob(t, svc1, "restart-test", "do once", Schedule{At: futureTime}, "")
	_ = svc1.Stop()

	// Manually tamper the job to have a past timestamp to simulate missed window.
	pastTime := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	_, err := db.Exec(context.Background(), "UPDATE sched_job SET schedule_at = $1 WHERE name = $2", pastTime, "restart-test")
	if err != nil {
		t.Fatalf("update schedule_at: %v", err)
	}

	// Restart: the past one-time job should be loaded but not scheduled (silently skipped).
	svc2 := newTestService(t, db)
	if err := svc2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = svc2.Stop()
	}()

	// Job is still in the list (persisted) but not scheduled with River.
	listed := svc2.ListJobs()
	if len(listed) != 1 {
		t.Fatalf("expected 1 persisted job, got %d", len(listed))
	}

	svc2.mu.Lock()
	_, hasRef := svc2.refs[listed[0].ID]
	svc2.mu.Unlock()
	if hasRef {
		t.Error("expected past one-time job to not be scheduled with River")
	}
}

func TestSessionModeDefault(t *testing.T) {
	svc := testService(t)

	// Empty session_mode defaults to "reuse".
	job := addTestJob(t, svc, "default-mode", "msg", Schedule{Every: "1h"}, "")
	if job.SessionMode != SessionReuse {
		t.Errorf("SessionMode = %q, want %q", job.SessionMode, SessionReuse)
	}
}

func TestSessionModeReuse(t *testing.T) {
	svc := testService(t)

	job := addTestJob(t, svc, "reuse-mode", "msg", Schedule{Every: "1h"}, SessionReuse)

	// Reuse mode: SessionID is stable across calls.
	id1 := job.SessionID()
	id2 := job.SessionID()
	if id1 != id2 {
		t.Errorf("reuse mode: SessionID changed: %q vs %q", id1, id2)
	}
	if id1 != "agent-a:scheduler:"+job.ID {
		t.Errorf("reuse mode: SessionID = %q, want %q", id1, "agent-a:scheduler:"+job.ID)
	}
}

func TestSessionModeNew(t *testing.T) {
	svc := testService(t)

	job := addTestJob(t, svc, "new-mode", "msg", Schedule{Every: "1h"}, SessionNew)
	if job.SessionMode != SessionNew {
		t.Errorf("SessionMode = %q, want %q", job.SessionMode, SessionNew)
	}

	// New mode: SessionID differs across calls.
	id1 := job.SessionID()
	time.Sleep(1 * time.Millisecond) // ensure different nano timestamp
	id2 := job.SessionID()
	if id1 == id2 {
		t.Error("new mode: SessionID should differ between calls")
	}
}

func TestSessionModeInvalid(t *testing.T) {
	svc := testService(t)

	_, err := svc.AddJobWithOwner("bad-mode", "msg", Schedule{Every: "1h"}, "invalid", "", "")
	if err == nil {
		t.Error("expected error for invalid session_mode")
	}
}

func TestEnsureJobCreatesOnce(t *testing.T) {
	svc := testService(t)

	job1, err := svc.EnsureJob("rss-poll", "poll feeds", Schedule{Every: "1h"}, SessionReuse, "", ExecScopeSystem)
	if err != nil {
		t.Fatalf("first EnsureJob: %v", err)
	}

	job2, err := svc.EnsureJob("rss-poll", "poll feeds", Schedule{Every: "1h"}, SessionReuse, "", ExecScopeSystem)
	if err != nil {
		t.Fatalf("second EnsureJob: %v", err)
	}
	if job2.ID != job1.ID {
		t.Errorf("second call created a new job: got ID %q, want %q", job2.ID, job1.ID)
	}

	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Errorf("ListJobs: got %d, want 1", len(jobs))
	}
}

func TestEnsureJobUpdatesExisting(t *testing.T) {
	svc := testService(t)

	job1, err := svc.EnsureJob("rss-poll", "poll feeds v1", Schedule{Every: "1h"}, SessionReuse, "", ExecScopeSystem)
	if err != nil {
		t.Fatalf("first EnsureJob: %v", err)
	}

	job2, err := svc.EnsureJob("rss-poll", "poll feeds v2", Schedule{Every: "30m"}, SessionNew, "", ExecScopeSystem)
	if err != nil {
		t.Fatalf("second EnsureJob: %v", err)
	}
	if job2.ID != job1.ID {
		t.Errorf("update created new job: got ID %q, want %q", job2.ID, job1.ID)
	}
	if job2.Message != "poll feeds v2" {
		t.Errorf("Message = %q, want %q", job2.Message, "poll feeds v2")
	}
	if job2.Schedule.Every != "30m" {
		t.Errorf("Schedule.Every = %q, want %q", job2.Schedule.Every, "30m")
	}
	if job2.SessionMode != SessionNew {
		t.Errorf("SessionMode = %q, want %q", job2.SessionMode, SessionNew)
	}

	jobs := svc.ListJobs()
	if len(jobs) != 1 {
		t.Errorf("ListJobs: got %d, want 1", len(jobs))
	}
}

func TestEnsureJobPersistsAcrossRestart(t *testing.T) {
	db := dbtest.New(t)

	svc1 := newTestService(t, db)
	if err := svc1.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	job1, err := svc1.EnsureJob("rss-poll", "poll feeds", Schedule{Every: "1h"}, SessionReuse, "", ExecScopeSystem)
	if err != nil {
		t.Fatalf("EnsureJob: %v", err)
	}
	_ = svc1.Stop()

	svc2 := newTestService(t, db)
	if err := svc2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = svc2.Stop()
	}()

	job2, err := svc2.EnsureJob("rss-poll", "poll feeds", Schedule{Every: "1h"}, SessionReuse, "", ExecScopeSystem)
	if err != nil {
		t.Fatalf("EnsureJob after restart: %v", err)
	}
	if job2.ID != job1.ID {
		t.Errorf("EnsureJob after restart created new job: got ID %q, want %q", job2.ID, job1.ID)
	}

	jobs := svc2.ListJobs()
	if len(jobs) != 1 {
		t.Errorf("ListJobs: got %d, want 1", len(jobs))
	}
}

func TestRunJobNow_SingleRun(t *testing.T) {
	svc := testService(t)

	var fired []string
	var mu sync.Mutex
	svc.SetOnJob(func(_ context.Context, job Job, _ authz.Authority) error {
		mu.Lock()
		fired = append(fired, job.ID)
		mu.Unlock()
		return nil
	})

	job := addTestJob(t, svc, "run-now-test", "hello", Schedule{Every: "24h"}, SessionReuse)

	runID, err := svc.RunJobNow(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("RunJobNow: %v", err)
	}
	if runID == "" {
		t.Fatal("RunJobNow: expected non-empty run ID")
	}

	// Wait for the async goroutine to finish.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fired)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 || fired[0] != job.ID {
		t.Errorf("callback fired for %v, want [%s]", fired, job.ID)
	}

	var runs []JobRun
	// The callback returns before RunJobNow finalizes the run row. Under
	// race/coverage builds that bookkeeping can lag long enough to observe
	// the intermediate "running" state, so wait for the durable final status.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, err = svc.ListJobRuns(context.Background(), job.ID, 10)
		if err != nil {
			t.Fatalf("ListJobRuns: %v", err)
		}
		if len(runs) == 1 && runs[0].Status == RunStatusSuccess {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(runs) != 1 {
		t.Fatalf("ListJobRuns: got %d runs, want 1", len(runs))
	}
	if runs[0].Status != RunStatusSuccess {
		t.Errorf("run status = %q, want %q", runs[0].Status, RunStatusSuccess)
	}
}

func TestRunJobNow_PreventsConcurrentRun(t *testing.T) {
	svc := testService(t)

	// Slow job so we can attempt a second trigger while it's still running.
	started := make(chan struct{})
	unblock := make(chan struct{})
	svc.SetOnJob(func(_ context.Context, _ Job, _ authz.Authority) error {
		close(started)
		<-unblock
		return nil
	})

	// RunJobNow applies background authorization before invoking the test hook.
	// Give the user-owned job a real agent owner so that this test exercises the
	// concurrent-run guard instead of waiting forever for a rejected dispatch.
	job := addTestJob(t, svc, "concurrent-test", "block", Schedule{Every: "24h"}, SessionReuse)

	runID, err := svc.RunJobNow(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("first RunJobNow: %v", err)
	}
	if runID == "" {
		t.Fatal("first RunJobNow: expected non-empty run ID")
	}

	// Wait until the job has actually started (run record is in DB).
	<-started

	_, err = svc.RunJobNow(context.Background(), job.ID)
	if err == nil {
		t.Fatal("second RunJobNow: expected error, got nil")
	}

	close(unblock)
}

func TestRunJobNow_NotFound(t *testing.T) {
	svc := testService(t)
	_, err := svc.RunJobNow(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown job ID")
	}
}

// A non-positive Every must be rejected up front: river.PeriodicInterval(0)
// hot-loops the enqueuer and time.NewTicker(0) panics.
func TestAddJobRejectsNonPositiveEvery(t *testing.T) {
	svc := testService(t)
	for _, every := range []string{"0s", "-5m"} {
		if _, err := svc.AddJobWithOwner("bad-every", "hi", Schedule{Every: every}, "", "", "user-1"); err == nil {
			t.Errorf("AddJobWithOwner(every=%q): expected error, got nil", every)
		}
	}
}

func TestScheduleEveryRejectsNonPositive(t *testing.T) {
	svc := testService(t)
	if err := svc.ScheduleEvery(context.Background(), "0s", func(context.Context) {}); err == nil {
		t.Error("ScheduleEvery(0s): expected error, got nil")
	}
}
