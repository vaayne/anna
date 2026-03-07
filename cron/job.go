package cron

import "time"

// Schedule defines when a job runs. Exactly one field must be set.
type Schedule struct {
	Cron  string `json:"cron,omitempty"`  // "0 9 * * 1-5"
	Every string `json:"every,omitempty"` // "30m", "2h"
	At    string `json:"at,omitempty"`    // RFC3339: "2024-01-15T14:30:00+08:00"
}

// Job is the persisted job definition.
type Job struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Schedule  Schedule `json:"schedule"`
	Message   string   `json:"message"`
	Enabled   bool     `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}
