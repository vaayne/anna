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

	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
)

// OnJobFunc is called when a scheduled job fires.
type OnJobFunc func(ctx context.Context, job Job)

// Service manages cron jobs backed by gocron/v2 with JSON persistence.
type Service struct {
	scheduler gocron.Scheduler
	onJob     OnJobFunc
	dataPath  string // directory containing jobs.json
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
	for _, j := range jobs {
		s.jobs[j.ID] = j
		if j.Enabled {
			if err := s.scheduleJob(ctx, j); err != nil {
				s.log.Warn("failed to schedule persisted job", "id", j.ID, "name", j.Name, "error", err)
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
func (s *Service) AddJob(name, message string, sched Schedule) (Job, error) {
	if name == "" {
		return Job{}, fmt.Errorf("name is required")
	}
	if message == "" {
		return Job{}, fmt.Errorf("message is required")
	}
	if sched.Cron == "" && sched.Every == "" {
		return Job{}, fmt.Errorf("schedule requires either cron or every")
	}
	if sched.Cron != "" && sched.Every != "" {
		return Job{}, fmt.Errorf("schedule must have exactly one of cron or every")
	}

	// Validate schedule before persisting.
	if sched.Every != "" {
		if _, err := time.ParseDuration(sched.Every); err != nil {
			return Job{}, fmt.Errorf("invalid duration %q: %w", sched.Every, err)
		}
	}

	job := Job{
		ID:        uuid.New().String()[:8],
		Name:      name,
		Schedule:  sched,
		Message:   message,
		Enabled:   true,
		CreatedAt: time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.scheduleJob(context.Background(), job); err != nil {
		return Job{}, fmt.Errorf("schedule job: %w", err)
	}

	s.jobs[job.ID] = job
	if err := s.saveJobsLocked(); err != nil {
		return Job{}, fmt.Errorf("persist job: %w", err)
	}

	s.log.Info("job added", "id", job.ID, "name", name)
	return job, nil
}

// RemoveJob unschedules and removes a job.
func (s *Service) RemoveJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("job %q not found", id)
	}

	if gid, ok := s.gids[id]; ok {
		if err := s.scheduler.RemoveJob(gid); err != nil {
			s.log.Warn("failed to remove gocron job", "id", id, "error", err)
		}
		delete(s.gids, id)
	}

	delete(s.jobs, id)
	if err := s.saveJobsLocked(); err != nil {
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
	if job.Schedule.Cron != "" {
		jobDef = gocron.CronJob(job.Schedule.Cron, false)
	} else {
		d, err := time.ParseDuration(job.Schedule.Every)
		if err != nil {
			return fmt.Errorf("parse duration: %w", err)
		}
		jobDef = gocron.DurationJob(d)
	}

	captured := job
	gj, err := s.scheduler.NewJob(jobDef, gocron.NewTask(func() {
		s.mu.Lock()
		fn := s.onJob
		s.mu.Unlock()
		if fn != nil {
			fn(ctx, captured)
		}
	}))
	if err != nil {
		return err
	}

	s.gids[job.ID] = gj.ID()
	return nil
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
