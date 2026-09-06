package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CherryHQ/stella/internal/authz"
)

// ---------------------------------------------------------------------------
// Template registration
// ---------------------------------------------------------------------------

func TestRegisterTemplate_Basic(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	tmpl := JobTemplate{
		Key:             "rss-tmpl",
		Name:            "RSS Template",
		Description:     "A test template",
		Message:         "do something",
		DefaultSchedule: Schedule{Every: "1h"},
		SessionMode:     SessionNew,
	}
	if err := svc.RegisterTemplate(tmpl); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}

	templates := svc.Templates()
	if len(templates) != 1 {
		t.Fatalf("Templates() len = %d, want 1", len(templates))
	}
	if templates[0].Key != "rss-tmpl" {
		t.Errorf("Key = %q, want %q", templates[0].Key, "rss-tmpl")
	}
}

func TestRegisterTemplate_RejectsAfterStart(t *testing.T) {
	svc := testService(t)

	err := svc.RegisterTemplate(JobTemplate{
		Key:             "late",
		Name:            "Late",
		Message:         "hi",
		DefaultSchedule: Schedule{Every: "1h"},
	})
	if err == nil || !strings.Contains(err.Error(), "called after Start") {
		t.Fatalf("expected post-Start rejection, got: %v", err)
	}
}

func TestRegisterTemplate_RejectsDuplicateKey(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	tmpl := JobTemplate{
		Key:             "dup",
		Name:            "Dup",
		Message:         "hi",
		DefaultSchedule: Schedule{Every: "1h"},
	}
	if err := svc.RegisterTemplate(tmpl); err != nil {
		t.Fatalf("first RegisterTemplate: %v", err)
	}
	if err := svc.RegisterTemplate(tmpl); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected duplicate rejection, got: %v", err)
	}
}

// Mutual exclusion: template registered first, then builtin with same name must fail.
func TestMutualExclusion_TemplateThenBuiltin(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "conflict",
		Name:            "conflict",
		Message:         "msg",
		DefaultSchedule: Schedule{Every: "1h"},
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}

	err := svc.RegisterBuiltin(BuiltinJob{
		Name:     "conflict",
		Handler:  func(context.Context, Job) error { return nil },
		Schedule: Schedule{Every: "1h"},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicts with a registered template") {
		t.Fatalf("expected conflict rejection, got: %v", err)
	}
}

// Mutual exclusion: builtin registered first, then template with same name must fail.
func TestMutualExclusion_BuiltinThenTemplate(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterBuiltin(BuiltinJob{
		Name:     "conflict2",
		Handler:  func(context.Context, Job) error { return nil },
		Schedule: Schedule{Every: "1h"},
	}); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}

	err := svc.RegisterTemplate(JobTemplate{
		Key:             "conflict2",
		Name:            "conflict2",
		Message:         "msg",
		DefaultSchedule: Schedule{Every: "1h"},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicts with a registered builtin") {
		t.Fatalf("expected conflict rejection, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Template resolution
// ---------------------------------------------------------------------------

func TestResolveTemplateMessage(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "my-tmpl",
		Name:            "My Template",
		Message:         "the-prompt",
		DefaultSchedule: Schedule{Every: "1h"},
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}

	msg, ok := svc.ResolveTemplateMessage("my-tmpl")
	if !ok {
		t.Fatal("ResolveTemplateMessage returned false")
	}
	if msg != "the-prompt" {
		t.Errorf("message = %q, want %q", msg, "the-prompt")
	}

	_, ok = svc.ResolveTemplateMessage("missing-key")
	if ok {
		t.Error("expected false for missing key")
	}
}

// ---------------------------------------------------------------------------
// Subscribe
// ---------------------------------------------------------------------------

func TestSubscribe_CreatesUserJob(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "sub-tmpl",
		Name:            "Sub Tmpl",
		Message:         "do work",
		DefaultSchedule: Schedule{Every: "1h"},
		SessionMode:     SessionNew,
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	job, err := svc.Subscribe(context.Background(), "user-1", "agent-1", "sub-tmpl", Schedule{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if job.OwnerKind != JobOwnerUser {
		t.Errorf("OwnerKind = %q, want %q", job.OwnerKind, JobOwnerUser)
	}
	if job.JobKey != "sub-tmpl" {
		t.Errorf("JobKey = %q, want %q", job.JobKey, "sub-tmpl")
	}
	if job.UserID != "user-1" {
		t.Errorf("UserID = %q, want %q", job.UserID, "user-1")
	}
	if job.Message != "" {
		// Message must be empty on the row; prompt is resolved at fire time.
		t.Errorf("Message = %q, want empty", job.Message)
	}
	if job.Schedule.Every != "1h" {
		t.Errorf("Schedule.Every = %q, want %q", job.Schedule.Every, "1h")
	}
}

func TestSubscribe_UniquenessConflict(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "unique-tmpl",
		Name:            "Unique Tmpl",
		Message:         "do stuff",
		DefaultSchedule: Schedule{Every: "1h"},
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	if _, err := svc.Subscribe(context.Background(), "user-2", "agent-1", "unique-tmpl", Schedule{}); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}

	_, err := svc.Subscribe(context.Background(), "user-2", "agent-1", "unique-tmpl", Schedule{})
	if err == nil {
		t.Fatal("second Subscribe: expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "already subscribed") {
		t.Errorf("error = %v, want substring %q", err, "already subscribed")
	}
}

// TestSubscribe_Concurrent asserts that exactly one subscription row lands
// when two goroutines race to subscribe the same (user, template) pair.
func TestSubscribe_Concurrent(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "race-tmpl",
		Name:            "Race Tmpl",
		Message:         "race",
		DefaultSchedule: Schedule{Every: "24h"},
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	const n = 10
	var successCount int32
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, err := svc.Subscribe(context.Background(), "user-race", "agent-1", "race-tmpl", Schedule{})
			if err == nil {
				atomic.AddInt32(&successCount, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&successCount); got != 1 {
		t.Errorf("successful Subscribe count = %d, want exactly 1", got)
	}

	// Confirm exactly one row in the DB.
	jobs := svc.ListJobs()
	count := 0
	for _, j := range jobs {
		if j.UserID == "user-race" && j.JobKey == "race-tmpl" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("subscription rows in DB = %d, want 1", count)
	}
}

func TestSubscribe_MissingTemplate(t *testing.T) {
	svc := testService(t)

	_, err := svc.Subscribe(context.Background(), "user-1", "agent-1", "no-such-key", Schedule{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got: %v", err)
	}
}

func TestSubscribe_ScheduleOverride(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "override-tmpl",
		Name:            "Override Tmpl",
		Message:         "msg",
		DefaultSchedule: Schedule{Every: "24h"},
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	job, err := svc.Subscribe(context.Background(), "user-ov", "agent-1", "override-tmpl", Schedule{Every: "6h"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if job.Schedule.Every != "6h" {
		t.Errorf("Schedule.Every = %q, want %q", job.Schedule.Every, "6h")
	}
}

// ---------------------------------------------------------------------------
// dispatchJob: missing-template produces an error run
// ---------------------------------------------------------------------------

func TestDispatchJob_MissingTemplate_ErrorRun(t *testing.T) {
	svc := testService(t)

	var onJobCalled bool
	svc.SetOnJob(func(_ context.Context, _ Job, _ authz.Authority) error {
		onJobCalled = true
		return nil
	})

	// A subscription job whose template has been removed from the registry.
	orphan := Job{
		ID:        "orphan1",
		OwnerKind: JobOwnerUser,
		JobKey:    "removed-template",
		Name:      "something",
		UserID:    "user-1",
		AgentID:   "agent-a",
		Message:   "",
	}
	err := svc.dispatchJob(context.Background(), orphan)
	if err == nil {
		t.Fatal("expected error for missing template, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want substring %q", err, "not found")
	}
	if onJobCalled {
		t.Error("onJob should not have been called for a missing-template run")
	}
}

// ---------------------------------------------------------------------------
// executeSingleRun: re-entrancy skip
// ---------------------------------------------------------------------------

func TestExecuteSingleRun_SkipsWhenAlreadyRunning(t *testing.T) {
	svc := testService(t)

	// Buffered so the non-blocking send below never drops the signal when the
	// run goroutine reaches it before the test goroutine parks on <-started.
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var callCount int32
	svc.SetOnJob(func(_ context.Context, _ Job, _ authz.Authority) error {
		atomic.AddInt32(&callCount, 1)
		// Signal that we've started, then block until told to stop.
		select {
		case started <- struct{}{}:
		default:
		}
		<-unblock
		return nil
	})

	job, err := svc.AddJobWithOwner("reenter-test", "block", Schedule{Every: "24h"}, SessionReuse, "agent-a", "user-1")
	if err != nil {
		t.Fatalf("AddJobWithOwner: %v", err)
	}

	// First run via RunJobNow (uses tryStartJobRun — creates the running record).
	runID, err := svc.RunJobNow(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("RunJobNow: %v", err)
	}
	if runID == "" {
		t.Fatal("RunJobNow: expected non-empty run ID")
	}

	// Wait until the first run is actually executing.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("first run did not start")
	}

	// executeSingleRun while the first is still running should be skipped.
	svc.mu.Lock()
	j := svc.jobs[job.ID]
	svcCtx := svc.ctx
	svc.mu.Unlock()

	svc.executeSingleRun(svcCtx, j, "user-1", false)

	close(unblock)

	// Allow the first run's goroutine to finish.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&callCount) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&callCount); got != 1 {
		t.Errorf("onJob called %d times, want 1 (second run should have been skipped)", got)
	}
}

// ---------------------------------------------------------------------------
// UpdateUserJob
// ---------------------------------------------------------------------------

func TestUpdateUserJob_ReschedulesLiveGocronJob(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	job, err := svc.AddJobWithOwner("reschedule-me", "hello", Schedule{Every: "24h"}, SessionReuse, "", "user-1")
	if err != nil {
		t.Fatalf("AddJobWithOwner: %v", err)
	}

	svc.mu.Lock()
	oldRef, hadOld := svc.refs[job.ID]
	svc.mu.Unlock()
	if !hadOld {
		t.Fatal("expected River registration before update")
	}

	every30m := "30m"
	updated, err := svc.UpdateUserJob(context.Background(), job.ID, JobUpdate{
		Schedule: &Schedule{Every: every30m},
	})
	if err != nil {
		t.Fatalf("UpdateUserJob: %v", err)
	}
	if updated.Schedule.Every != every30m {
		t.Errorf("Schedule.Every = %q, want %q", updated.Schedule.Every, every30m)
	}

	svc.mu.Lock()
	newRef, hasNew := svc.refs[job.ID]
	svc.mu.Unlock()

	if !hasNew {
		t.Fatal("expected new River registration after update")
	}
	if newRef == oldRef {
		t.Error("River registration did not change after schedule update")
	}
}

func TestUpdateUserJob_DisabledJobDoesNotFire(t *testing.T) {
	svc := testService(t)

	var fired int32
	svc.SetOnJob(func(_ context.Context, _ Job, _ authz.Authority) error {
		atomic.AddInt32(&fired, 1)
		return nil
	})

	job, err := svc.AddJobWithOwner("disable-test", "ping", Schedule{Every: "100ms"}, SessionReuse, "agent-a", "user-1")
	if err != nil {
		t.Fatalf("AddJobWithOwner: %v", err)
	}

	// Let it fire at least once to confirm it was active.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fired) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&fired) == 0 {
		t.Fatal("job did not fire before being disabled")
	}

	enabled := false
	if _, err := svc.UpdateUserJob(context.Background(), job.ID, JobUpdate{Enabled: &enabled}); err != nil {
		t.Fatalf("UpdateUserJob disable: %v", err)
	}

	// Confirm the River registration was removed.
	svc.mu.Lock()
	_, stillScheduled := svc.refs[job.ID]
	svc.mu.Unlock()
	if stillScheduled {
		t.Error("disabled job should have no River registration")
	}

	// A firing enqueued just before the registration was removed may still be
	// executing, so its fire can land after UpdateUserJob returns — a fixed
	// post-disable window races that straggler on slow runners. Instead wait
	// for the fire stream to stay quiet for several intervals: in-flight
	// stragglers are absorbed, while a job still scheduled at 100ms can never
	// go quiet for 500ms.
	countAtDisable := atomic.LoadInt32(&fired)
	last := countAtDisable
	lastChange := time.Now()
	quietDeadline := time.Now().Add(10 * time.Second)
	for time.Since(lastChange) < 500*time.Millisecond {
		if time.Now().After(quietDeadline) {
			t.Fatalf("disabled job fired %d more times and never went quiet", atomic.LoadInt32(&fired)-countAtDisable)
		}
		time.Sleep(20 * time.Millisecond)
		if cur := atomic.LoadInt32(&fired); cur != last {
			last, lastChange = cur, time.Now()
		}
	}
}

func TestUpdateUserJob_RejectsMessageChangeOnSubscription(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "sub-update-tmpl",
		Name:            "Sub Update Tmpl",
		Message:         "template-msg",
		DefaultSchedule: Schedule{Every: "1h"},
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop() })

	job, err := svc.Subscribe(context.Background(), "user-3", "agent-1", "sub-update-tmpl", Schedule{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	newMsg := "custom prompt"
	_, err = svc.UpdateUserJob(context.Background(), job.ID, JobUpdate{Message: &newMsg})
	if err == nil {
		t.Fatal("expected error when changing message of a subscription, got nil")
	}
	if !strings.Contains(err.Error(), "message cannot be changed") {
		t.Errorf("error = %v, want substring %q", err, "message cannot be changed")
	}
}

func TestUpdateUserJob_NotFound(t *testing.T) {
	svc := testService(t)
	_, err := svc.UpdateUserJob(context.Background(), "no-such-id", JobUpdate{})
	if err == nil {
		t.Fatal("expected error for unknown job ID")
	}
}

// ---------------------------------------------------------------------------
// dispatchJob: template message injection on fire
// ---------------------------------------------------------------------------

func TestDispatchJob_InjectsTemplateMessage(t *testing.T) {
	db := testDB(t)
	svc := newTestService(t, db)

	const wantMsg = "the-real-prompt"
	if err := svc.RegisterTemplate(JobTemplate{
		Key:             "inject-tmpl",
		Name:            "Inject Tmpl",
		Message:         wantMsg,
		DefaultSchedule: Schedule{Every: "1h"},
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}

	var gotMsg string
	svc.SetOnJob(func(_ context.Context, job Job, _ authz.Authority) error {
		gotMsg = job.Message
		return nil
	})

	// Simulate a subscription job arriving at dispatch with empty message.
	subJob := Job{
		ID:        "inj1",
		OwnerKind: JobOwnerUser,
		JobKey:    "inject-tmpl",
		Name:      "Inject Tmpl",
		UserID:    "user-1",
		AgentID:   "agent-a",
		Message:   "", // intentionally empty — resolved at fire time
	}

	if err := svc.dispatchJob(context.Background(), subJob); err != nil {
		// dispatchJob without Start still calls onJob if wired; ok.
		if !errors.Is(err, context.Canceled) {
			// Not a fatal failure in unit-test context where svc.ctx may be nil.
			t.Logf("dispatchJob returned: %v (may be expected without Start)", err)
		}
	}

	if gotMsg != wantMsg {
		t.Errorf("dispatched message = %q, want %q", gotMsg, wantMsg)
	}
}
