# Cron System

## Status

Implemented — `cron/` package with gocron/v2 scheduler, JSON persistence, and agent tool.

## Overview

Anna supports scheduled task execution so the agent can set reminders, run periodic tasks, and automate recurring work. The cron system delegates all scheduling to [gocron/v2](https://github.com/go-co-op/gocron) and adds persistence and an agent-facing tool on top.

## Architecture

```
Agent (via tool call)
    |
    |  add / list / remove
    v
+----------+       +-------------+
| CronTool | ----> |   Service   |
+----------+       +------+------+
                          |
              +-----------+-----------+
              |                       |
     gocron/v2 Scheduler      jobs.json (disk)
              |
              v
        OnJobFunc callback
              |
              v
      pool.Chat(ctx, "cron:{id}", message)
```

### Package: `cron/`

Top-level package (sibling to `agent/`, `channel/`). Three files:

| File | Purpose |
|------|---------|
| `cron/job.go` | `Job` and `Schedule` types |
| `cron/cron.go` | `Service` — gocron wrapper + JSON persistence |
| `cron/tool.go` | `CronTool` — agent tool implementing `tool.Tool` |

### Key Types

**Schedule** defines when a job runs. Exactly one field must be set:

- `cron` — a cron expression (e.g. `"0 9 * * 1-5"` for weekdays at 9am)
- `every` — a Go duration (e.g. `"30m"`, `"2h"`, `"24h"`)

**Job** is the persisted definition:

```go
type Job struct {
    ID        string    // short UUID
    Name      string    // human-readable name
    Schedule  Schedule  // cron or interval
    Message   string    // prompt sent to agent
    Enabled   bool
    CreatedAt time.Time
}
```

### Service Lifecycle

1. `cron.New(dataPath)` — creates scheduler
2. `service.SetOnJob(fn)` — sets callback (deferred wiring to resolve circular dependency)
3. `service.Start(ctx)` — loads `jobs.json`, registers all jobs with gocron, starts scheduler
4. `service.Stop()` — shuts down scheduler

### Persistence

Jobs are stored as a JSON array in `{dataDir}/jobs.json` (default: `.agents/cron/jobs.json`). Writes are atomic (temp file + rename).

### Session Model

Each cron job gets a dedicated session with ID `cron:{job.ID}`. This means the agent retains conversational memory across scheduled runs of the same job.

## Configuration

Add to `.agents/config.yaml`:

```yaml
cron:
  enabled: true
  data_dir: .agents/cron  # optional, this is the default
```

Cron is only active when:
- `cron.enabled` is `true`
- `runner.type` is `go` (the Pi runner doesn't support custom tools)

## Agent Tool

The `cron` tool is automatically registered with the Go runner when cron is enabled. The agent uses it via tool calls with three actions:

### `add` — Create a job

Parameters:
- `name` (required) — human-readable name
- `message` (required) — the instruction to execute on each run
- `cron` — cron expression (use this OR `every`)
- `every` — Go duration (use this OR `cron`)

Example: _"Set a reminder every 30 minutes to check my email"_ triggers:
```json
{"action": "add", "name": "email check", "message": "Check my email and summarize new messages", "every": "30m"}
```

### `list` — List all jobs

No parameters. Returns all scheduled jobs as JSON.

### `remove` — Delete a job

Parameters:
- `id` (required) — job ID from `add` or `list`

## Wiring

The cron system resolves a circular dependency (service needs pool for the callback, runner needs the tool) via deferred wiring in `main.go`:

1. Create `cron.Service` with no callback
2. Create `cron.NewTool(service)` and pass to runner via `ExtraTools`
3. Create pool with the runner factory
4. Call `service.SetOnJob(...)` with a callback that calls `pool.Chat()`
5. Call `service.Start(ctx)` in command handlers

## Testing

Tests are in `cron/cron_test.go` covering:

- Add, list, remove lifecycle
- Input validation (empty name, missing schedule, invalid duration, etc.)
- Remove non-existent job
- Persistence across service restart
- Callback firing on schedule
- Full tool interface (add/list/remove via `Execute`)
- Error cases (invalid action, missing ID)

Run with:

```bash
go test -race ./cron/
```
