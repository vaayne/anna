package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/pkg/tools"
)

// ListTool is the scheduler action that lists what this agent can reach. Error
// prose points at it, so a rename shows up here rather than in a string.
const ListTool = "scheduler__job_list"

// actionDescriptions is the model-facing description per generated tool. A
// split tool's schema is exact, so each description only says what the call
// does and what it costs.
var actionDescriptions = map[string]string{
	"create": "Schedule a prompt to run later or repeatedly, as a one-time at, an interval every, or a cron expression; template_key subscribes to a built-in job instead. Use goal_create for durable acceptance-tracked work, not this.",
	"list":   "List this agent's scheduled jobs with their schedule, next run, and enabled state.",
	"get":    "Read one scheduled job by id, including its message and its run history summary.",
	"update": "Change one scheduled job's name, message, or schedule. Missing fields keep their current values; use scheduler__job_pause to stop it instead.",
	"delete": "Delete one scheduled job by id. This is not reversible; pause it instead when it should come back.",
	"pause":  "Stop one scheduled job from running without deleting it. Its schedule and message are kept for scheduler__job_resume.",
	"resume": "Let a paused job run again on its existing schedule. Missed runs are not replayed.",
}

// Tool is one generated scheduler action. The tool name carries the action, so
// the provider validates arguments against an exact schema before dispatch.
type Tool struct {
	spec ActionTool
	svc  *Service
}

// NewTool builds one scheduler action tool.
func NewTool(svc *Service, spec ActionTool) *Tool { return &Tool{spec: spec, svc: svc} }

func (t *Tool) Definition() tools.Definition {
	return t.spec.Definition(actionDescriptions[t.spec.Action])
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t == nil || t.svc == nil {
		return "", fmt.Errorf("scheduler service is unavailable — try again later")
	}
	ident, err := authz.ToolIdentity(ctx, t.spec.Name)
	if err != nil {
		return "", err
	}
	// The runtime context identity is the trusted adapter: a delegated agent turn
	// becomes a confined AgentActor. Model-supplied arguments never form identity.
	authority, err := ident.ToAuthority()
	if err != nil {
		return "", authz.MapToolError(t.spec.Name, ListTool, err)
	}
	out, err := Dispatch(ctx, schedulerHandler{svc: t.svc, authority: authority, agentID: ident.AgentID}, t.spec.Action, args)
	if err != nil {
		return "", authz.MapToolError(t.spec.Name, ListTool, err)
	}
	return tools.MarshalResult(out)
}

type schedulerHandler struct {
	svc       *Service
	authority authz.Authority
	agentID   string
}

func (h schedulerHandler) begin(ctx context.Context) (*Access, error) {
	return h.svc.Begin(ctx, h.authority)
}

func (h schedulerHandler) Create(ctx context.Context, in CreateInput) (any, error) {
	acc, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	sched := Schedule{Cron: in.Cron, Every: in.Every, At: in.At}
	var job Job
	if in.TemplateKey != "" {
		job, err = acc.Subscribe(ctx, h.agentID, in.TemplateKey, sched)
	} else {
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		job, err = acc.CreateJobWithEnabled(ctx, in.Name, in.Message, sched, in.SessionMode, h.agentID, in.IdempotencyKey, enabled)
	}
	if err != nil {
		return nil, err
	}
	return schedulerSummary(job), nil
}

func (h schedulerHandler) Get(ctx context.Context, in GetInput) (any, error) {
	acc, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	job, err := acc.GetJob(ctx, h.agentID, in.Id)
	if err != nil {
		return nil, err
	}
	return schedulerSummary(job), nil
}

func (h schedulerHandler) List(ctx context.Context, _ ListInput) (any, error) {
	acc, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := acc.ListJobs(ctx, h.agentID)
	if err != nil {
		return nil, err
	}
	items := make([]schedulerResponse, 0, len(jobs))
	for _, job := range jobs {
		items = append(items, schedulerSummary(job))
	}
	return listResponse[schedulerResponse]{Items: items, HasMore: false}, nil
}

func (h schedulerHandler) Update(ctx context.Context, in UpdateInput) (any, error) {
	acc, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	update := JobUpdate{}
	if in.Name != "" {
		update.Name = &in.Name
	}
	if in.Message != "" {
		update.Message = &in.Message
	}
	if in.Cron != "" || in.Every != "" || in.At != "" {
		sched := Schedule{Cron: in.Cron, Every: in.Every, At: in.At}
		update.Schedule = &sched
	}
	if in.SessionMode != "" {
		update.SessionMode = &in.SessionMode
	}
	if in.Enabled != nil {
		update.Enabled = in.Enabled
	}
	job, err := acc.UpdateJob(ctx, h.agentID, in.Id, update)
	if err != nil {
		return nil, err
	}
	return schedulerSummary(job), nil
}

func (h schedulerHandler) Delete(ctx context.Context, in DeleteInput) (any, error) {
	acc, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := acc.DeleteJob(ctx, h.agentID, in.Id); err != nil {
		return nil, err
	}
	return map[string]any{"id": in.Id, "status": "deleted"}, nil
}

func (h schedulerHandler) Pause(ctx context.Context, in PauseInput) (any, error) {
	acc, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	job, err := acc.SetJobEnabled(ctx, h.agentID, in.Id, false)
	if err != nil {
		return nil, err
	}
	return schedulerSummary(job), nil
}

func (h schedulerHandler) Resume(ctx context.Context, in ResumeInput) (any, error) {
	acc, err := h.begin(ctx)
	if err != nil {
		return nil, err
	}
	job, err := acc.SetJobEnabled(ctx, h.agentID, in.Id, true)
	if err != nil {
		return nil, err
	}
	return schedulerSummary(job), nil
}

type schedulerResponse struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Enabled     bool          `json:"enabled"`
	Schedule    schedulerView `json:"schedule"`
	SessionMode string        `json:"session_mode"`
	UpdatedAt   string        `json:"updated_at"`
	TemplateKey string        `json:"template_key,omitempty"`
}
type schedulerView struct {
	Cron  string `json:"cron,omitempty"`
	Every string `json:"every,omitempty"`
	At    string `json:"at,omitempty"`
}
type listResponse[T any] struct {
	Items         []T    `json:"items"`
	HasMore       bool   `json:"has_more"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

func schedulerSummary(job Job) schedulerResponse {
	return schedulerResponse{ID: job.ID, Name: job.Name, Enabled: job.Enabled, Schedule: schedulerView{Cron: job.Schedule.Cron, Every: job.Schedule.Every, At: job.Schedule.At}, SessionMode: job.SessionMode, UpdatedAt: job.UpdatedAt.UTC().Format(time.RFC3339), TemplateKey: job.JobKey}
}
