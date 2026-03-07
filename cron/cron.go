package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"errors"

	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
)

// errOneTimeJobPast is returned by scheduleJob when a one-time job's timestamp
// has already elapsed. Start suppresses this for persisted jobs; AddJob treats
// it as a hard failure.
var errOneTimeJobPast = errors.New("one-time job timestamp is in the past")

// OnJobFunc is called when a scheduled job fires.
type OnJobFunc func(ctx context.Context, job Job)

// Service manages cron jobs backed by gocron/v2 with JSON persistence.
type Service struct {
	scheduler gocron.Scheduler
	onJob     OnJobFunc
	dataPath  string          // directory containing jobs.json
	ctx       context.Context // lifecycle context from Start
	mu        sync.Mutex
	jobs      map[string]Job
	gids      map[string]uuid.UUID // job ID -> gocron job UUID
	log       *slog.Logger
}

// New creates a cron service. Call Start to load persisted jobs and begin scheduling.
func New(dataPath string) (*Service, error) {
	s, err := gocron.NewScheduler()
	if err != nil {
		return nil, fmt.Errorf("create scheduler: %w", err)
	}
	return &Service{
		scheduler: s,
		dataPath:  dataPath,
		jobs:      make(map[string]Job),
		gids:      make(map[string]uuid.UUID),
		log:       slog.With("component", "cron"),
	}, nil
}

// SetOnJob sets the callback invoked when a job fires.
func (s *Service) SetOnJob(fn OnJobFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onJob = fn
}

// Start loads persisted jobs and starts the scheduler.
func (s *Service) Start(ctx context.Context) error {
	jobs, err := s.loadJobs()
	if err != nil {
		return fmt.Errorf("load jobs: %w", err)
	}

	s.mu.Lock()
	s.ctx = ctx
	for _, j := range jobs {
		s.jobs[j.ID] = j
		if j.Enabled {
			if err := s.scheduleJob(ctx, j); err != nil {
				if errors.Is(err, errOneTimeJobPast) {
					s.log.Info("skipping one-time job with past timestamp", "id", j.ID, "at", j.Schedule.At)
				} else {
					s.log.Warn("failed to schedule persisted job", "id", j.ID, "name", j.Name, "error", err)
				}
			}
		}
	}
	s.mu.Unlock()

	s.scheduler.Start()
	s.log.Info("cron service started", "jobs", len(jobs))
	return nil
}

// Stop shuts down the scheduler.
func (s *Service) Stop() error {
	return s.scheduler.Shutdown()
}

// AddJob creates, persists, and schedules a new job.
// sessionMode controls session reuse: "reuse" (default) or "new".
func (s *Service) AddJob(name, message string, sched Schedule, sessionMode string) (Job, error) {
	if name == "" {
		return Job{}, fmt.Errorf("name is required")
	}
	if message == "" {
		return Job{}, fmt.Errorf("message is required")
	}
	setCount := 0
	if sched.Cron != "" {
		setCount++
	}
	if sched.Every != "" {
		setCount++
	}
	if sched.At != "" {
		setCount++
	}
	if setCount == 0 {
		return Job{}, fmt.Errorf("schedule requires one of cron, every, or at")
	}
	if setCount > 1 {
		return Job{}, fmt.Errorf("schedule must have exactly one of cron, every, or at")
	}

	// Validate schedule before persisting.
	if sched.Every != "" {
		if _, err := time.ParseDuration(sched.Every); err != nil {
			return Job{}, fmt.Errorf("invalid duration %q: %w", sched.Every, err)
		}
	}
	if sched.At != "" {
		t, err := time.Parse(time.RFC3339, sched.At)
		if err != nil {
			return Job{}, fmt.Errorf("invalid at timestamp %q: must be RFC3339 format: %w", sched.At, err)
		}
		if !t.After(time.Now()) {
			return Job{}, fmt.Errorf("at timestamp %q is in the past", sched.At)
		}
	}

	if sessionMode == "" {
		sessionMode = SessionReuse
	}
	if sessionMode != SessionReuse && sessionMode != SessionNew {
		return Job{}, fmt.Errorf("invalid session_mode %q: must be %q or %q", sessionMode, SessionReuse, SessionNew)
	}

	job := Job{
		ID:          uuid.New().String()[:8],
		Name:        name,
		Schedule:    sched,
		Message:     message,
		SessionMode: sessionMode,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.scheduleJob(s.ctx, job); err != nil {
		return Job{}, fmt.Errorf("schedule job: %w", err)
	}

	s.jobs[job.ID] = job
	if err := s.saveJobsLocked(); err != nil {
		// Roll back: remove from memory and unschedule.
		delete(s.jobs, job.ID)
		if gid, ok := s.gids[job.ID]; ok {
			_ = s.scheduler.RemoveJob(gid)
			delete(s.gids, job.ID)
		}
		return Job{}, fmt.Errorf("persist job: %w", err)
	}

	s.log.Info("job added", "id", job.ID, "name", name)
	return job, nil
}

// RemoveJob unschedules and removes a job.
func (s *Service) RemoveJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job %q not found", id)
	}

	// Remove from scheduler first.
	var removedGID uuid.UUID
	var hadGID bool
	if gid, ok := s.gids[id]; ok {
		if err := s.scheduler.RemoveJob(gid); err != nil {
			s.log.Warn("failed to remove gocron job", "id", id, "error", err)
		}
		removedGID = gid
		hadGID = true
		delete(s.gids, id)
	}

	delete(s.jobs, id)
	if err := s.saveJobsLocked(); err != nil {
		// Roll back: restore in-memory state and re-schedule so retry works.
		s.jobs[id] = job
		if hadGID {
			if schedErr := s.scheduleJob(s.ctx, job); schedErr != nil {
				s.log.Warn("failed to re-schedule job during rollback", "id", id, "error", schedErr)
				s.gids[id] = removedGID // keep stale GID as best-effort
			}
		}
		return fmt.Errorf("persist after remove: %w", err)
	}

	s.log.Info("job removed", "id", id)
	return nil
}

// ListJobs returns all jobs.
func (s *Service) ListJobs() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		result = append(result, j)
	}
	return result
}

// scheduleJob registers a job with gocron. Caller must hold s.mu.
func (s *Service) scheduleJob(ctx context.Context, job Job) error {
	var jobDef gocron.JobDefinition
	switch {
	case job.Schedule.Cron != "":
		jobDef = gocron.CronJob(job.Schedule.Cron, false)
	case job.Schedule.Every != "":
		d, err := time.ParseDuration(job.Schedule.Every)
		if err != nil {
			return fmt.Errorf("parse duration: %w", err)
		}
		jobDef = gocron.DurationJob(d)
	case job.Schedule.At != "":
		t, err := time.Parse(time.RFC3339, job.Schedule.At)
		if err != nil {
			return fmt.Errorf("parse at timestamp: %w", err)
		}
		if !t.After(time.Now()) {
			return errOneTimeJobPast
		}
		jobDef = gocron.OneTimeJob(gocron.OneTimeJobStartDateTime(t))
	}

	captured := job
	isOneTime := job.Schedule.At != ""
	gj, err := s.scheduler.NewJob(jobDef, gocron.NewTask(func() {
		s.mu.Lock()
		fn := s.onJob
		s.mu.Unlock()
		if fn != nil {
			fn(ctx, captured)
		}
		if isOneTime {
			go s.removeOneTimeJob(captured.ID)
		}
	}))
	if err != nil {
		return err
	}

	s.gids[job.ID] = gj.ID()
	return nil
}

// removeOneTimeJob cleans up a one-time job after it fires.
func (s *Service) removeOneTimeJob(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if gid, ok := s.gids[id]; ok {
		_ = s.scheduler.RemoveJob(gid)
		delete(s.gids, id)
	}
	delete(s.jobs, id)
	if err := s.saveJobsLocked(); err != nil {
		s.log.Warn("failed to remove one-time job after execution", "id", id, "error", err)
	} else {
		s.log.Info("one-time job auto-removed after execution", "id", id)
	}
}

// jobsFile returns the path to the jobs JSON file.
func (s *Service) jobsFile() string {
	return filepath.Join(s.dataPath, "jobs.json")
}

// loadJobs reads the persisted jobs from disk.
func (s *Service) loadJobs() ([]Job, error) {
	data, err := os.ReadFile(s.jobsFile())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var jobs []Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("parse jobs.json: %w", err)
	}
	return jobs, nil
}

// saveJobsLocked writes all jobs to disk atomically. Caller must hold s.mu.
func (s *Service) saveJobsLocked() error {
	if err := os.MkdirAll(s.dataPath, 0o755); err != nil {
		return err
	}

	jobs := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}

	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.jobsFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.jobsFile())
}
