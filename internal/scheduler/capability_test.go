package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CherryHQ/stella/internal/authz"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
)

func TestAuthorizeBackgroundRequiresSchedulerAndRecallyForSubscriptions(t *testing.T) {
	authority, err := agentaccess.WorkerAgentAuthority("user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	svc := &Service{capabilityGate: func(_ context.Context, gotAuthority authz.Authority, agentID string, pluginIDs ...string) error {
		if gotAuthority != authority {
			t.Fatalf("authority = %#v, want %#v", gotAuthority, authority)
		}
		if agentID != "agent-1" {
			t.Fatalf("agentID = %q, want agent-1", agentID)
		}
		got = append(got, pluginIDs...)
		return nil
	}}

	job := Job{ID: "job-1", OwnerKind: JobOwnerUser, AgentID: "agent-1", JobKey: RecallyDigestTemplate.Key}
	if err := svc.authorizeBackground(context.Background(), job, authority, nil); err != nil {
		t.Fatalf("authorizeBackground: %v", err)
	}
	want := []string{SchedulerPluginID, RecallyPluginID}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("plugin IDs = %v, want %v", got, want)
	}
}

func TestAuthorizeBackgroundFailsClosedWithoutGateForUserJob(t *testing.T) {
	svc := &Service{}
	authority, err := authz.NewAgentAuthority("user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.authorizeBackground(context.Background(), Job{ID: "job-1", OwnerKind: JobOwnerUser}, authority, nil); err == nil {
		t.Fatal("user background job without capability gate was accepted")
	}
}

func TestAuthorizeAgentStartUsesActualAgentAndSameRequiredPluginIDs(t *testing.T) {
	authority, err := agentaccess.WorkerAgentAuthority("user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	var gotAgent string
	var gotPlugins []string
	svc := &Service{capabilityGate: func(_ context.Context, gotAuthority authz.Authority, agentID string, pluginIDs ...string) error {
		if gotAuthority != authority {
			t.Fatalf("authority = %#v, want %#v", gotAuthority, authority)
		}
		gotAgent = agentID
		gotPlugins = append(gotPlugins, pluginIDs...)
		return nil
	}}
	job := Job{ID: "job-1", OwnerKind: JobOwnerUser, AgentID: "agent-1", JobKey: RecallyRSSTemplate.Key}

	if err := svc.AuthorizeAgentStart(context.Background(), job, authority, "agent-1"); err != nil {
		t.Fatalf("AuthorizeAgentStart: %v", err)
	}
	if gotAgent != "agent-1" {
		t.Fatalf("gate agentID = %q, want agent-1", gotAgent)
	}
	want := []string{SchedulerPluginID, RecallyPluginID}
	if len(gotPlugins) != len(want) || gotPlugins[0] != want[0] || gotPlugins[1] != want[1] {
		t.Fatalf("plugin IDs = %v, want %v", gotPlugins, want)
	}
}

func TestAuthorizeAgentStartRejectsNonUserAndTargetMismatch(t *testing.T) {
	called := false
	svc := &Service{capabilityGate: func(context.Context, authz.Authority, string, ...string) error {
		called = true
		return nil
	}}
	authority, err := agentaccess.WorkerAgentAuthority("user-1", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AuthorizeAgentStart(context.Background(), Job{ID: "system-job", OwnerKind: JobOwnerSystem, AgentID: "agent-1"}, authority, "agent-1"); err == nil {
		t.Fatal("system job was accepted for an agent start")
	}
	if err := svc.AuthorizeAgentStart(context.Background(), Job{ID: "job-1", OwnerKind: JobOwnerUser, AgentID: "agent-1"}, authority, "agent-2"); err == nil {
		t.Fatal("mismatched agent target was accepted")
	}
	if called {
		t.Fatal("rejected agent start consulted the capability gate")
	}
}

func TestAuthorizeBackgroundLeavesSystemMaintenanceOnCorePath(t *testing.T) {
	called := false
	svc := &Service{capabilityGate: func(context.Context, authz.Authority, string, ...string) error {
		called = true
		return errors.New("must not be called")
	}}
	authority, err := agentaccess.SystemAgentAuthority("maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.authorizeBackground(context.Background(), Job{ID: "job-1", OwnerKind: JobOwnerSystem}, authority, func(context.Context, Job) error { return nil }); err != nil {
		t.Fatalf("system maintenance authorization: %v", err)
	}
	if called {
		t.Fatal("system maintenance unexpectedly consulted public capability gate")
	}
}

func TestDispatchJobSystemMessageCannotBypassMissingBuiltin(t *testing.T) {
	called := false
	svc := &Service{
		authorizeFire: func(context.Context, Job) (authz.Authority, error) {
			return agentaccess.SystemAgentAuthority("scheduler")
		},
		onJob: func(context.Context, Job, authz.Authority) error {
			called = true
			return nil
		},
	}
	err := svc.dispatchJob(context.Background(), Job{ID: "job-1", Name: "removed-builtin", OwnerKind: JobOwnerSystem, Message: "must not run"})
	if err == nil || !strings.Contains(err.Error(), "no live handler") {
		t.Fatalf("dispatchJob error = %v, want missing live handler", err)
	}
	if called {
		t.Fatal("system message fallback invoked onJob")
	}
}

func TestDispatchJobSystemUsesRegisteredBuiltinHandler(t *testing.T) {
	called := false
	svc := &Service{
		authorizeFire: func(context.Context, Job) (authz.Authority, error) {
			return agentaccess.SystemAgentAuthority("scheduler")
		},
	}
	if err := svc.RegisterBuiltin(BuiltinJob{
		Name: "live-builtin",
		Handler: func(context.Context, Job) error {
			called = true
			return nil
		},
	}); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}
	if err := svc.dispatchJob(context.Background(), Job{ID: "job-1", Name: "live-builtin", OwnerKind: JobOwnerSystem, Message: "ignored"}); err != nil {
		t.Fatalf("dispatchJob: %v", err)
	}
	if !called {
		t.Fatal("registered builtin handler was not invoked")
	}
}

func TestDispatchJobRejectsUserJobWhenSchedulerCapabilityIsDisabled(t *testing.T) {
	called := false
	denied := errors.New("system/scheduler disabled")
	svc := &Service{
		authorizeFire: func(context.Context, Job) (authz.Authority, error) {
			return agentaccess.WorkerAgentAuthority("user-1", "agent-1")
		},
		capabilityGate: func(context.Context, authz.Authority, string, ...string) error {
			return denied
		},
		onJob: func(context.Context, Job, authz.Authority) error {
			called = true
			return nil
		},
	}
	err := svc.dispatchJob(context.Background(), Job{ID: "job-1", OwnerKind: JobOwnerUser, UserID: "user-1", AgentID: "agent-1", Message: "hello"})
	if err == nil {
		t.Fatal("dispatchJob accepted a user job with scheduler capability disabled")
	}
	if !errors.Is(err, denied) {
		t.Fatalf("dispatchJob error = %v, want capability denial", err)
	}
	if called {
		t.Fatal("user callback ran after scheduler capability denial")
	}
}
