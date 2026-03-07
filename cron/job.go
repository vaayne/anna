package cron

import (
	"fmt"
	"time"
)

const (
	// SessionReuse reuses the same session across job executions (default).
	SessionReuse = "reuse"
	// SessionNew creates a fresh session for each execution.
	SessionNew = "new"
)

// Schedule defines when a job runs. Exactly one field must be set.
type Schedule struct {
	Cron  string `json:"cron,omitempty"`  // "0 9 * * 1-5"
	Every string `json:"every,omitempty"` // "30m", "2h"
	At    string `json:"at,omitempty"`    // RFC3339: "2024-01-15T14:30:00+08:00"
}

// Job is the persisted job definition.
type Job struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Schedule    Schedule  `json:"schedule"`
	Message     string    `json:"message"`
	SessionMode string    `json:"session_mode"` // "reuse" (default) or "new"
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// SessionID returns the session identifier for this job execution.
// In "reuse" mode, the ID is stable across executions. In "new" mode,
// a timestamp suffix ensures each execution gets a fresh session.
func (j Job) SessionID() string {
	base := "cron:" + j.ID
	if j.SessionMode == SessionNew {
		return fmt.Sprintf("%s:%d", base, time.Now().UnixNano())
	}
	return base
}
