---
name: scheduler
description: |
  Manage scheduled jobs. Use when the user wants to create, list, inspect, update, pause, resume, or remove recurring or one-time scheduled tasks. Handles cron schedules, interval-based (every), one-time (at) jobs, platform job templates, and scheduled workflow runs.
metadata:
  author: CherryHQ/stella
  version: "1.0"
---

# Scheduler

Use scheduler for time-based triggers. If the user wants to repeat an accepted goal's plan ("save this goal and run it every morning"), save it as a workflow first, then schedule the workflow. If the scheduled work may be long-running, need human review, or need restart resilience, schedule a workflow or a short prompt that creates an async goal instead of doing the whole job inline.

Agents manage schedules with the native `scheduler__job_*` tools. Do not use the Stella CLI from an agent session.

## Actions

- `create`: create a cron, interval, or one-time job. For chat jobs, provide `name`, `message`, exactly one schedule field (`cron`, `every`, or `at`), and optional `session_mode` (`reuse` or `new`).
- `list`: list this agent's scheduled jobs.
- `get`: inspect one job by `id`.
- `update`: change editable fields on a job.
- `pause` / `resume`: disable or re-enable a job without deleting it.
- `delete`: remove a job.

## Schedule types

| Field   | Format                       | Example                        |
| ------- | ---------------------------- | ------------------------------ |
| `cron`  | Standard cron expression     | `"0 9 * * 1-5"` (weekdays 9am) |
| `every` | Go duration                  | `30m`, `2h`, `24h`             |
| `at`    | RFC3339 timestamp (one-time) | `"2024-01-15T14:30:00+08:00"`  |

Session modes:

- `reuse` (default): conversation history is preserved across executions.
- `new`: starts a fresh session on each execution.

## Check before adding

Always run `scheduler__job_list` first to avoid duplicates. If a job with the target name already exists, skip creation and report the existing job to the user.

## Schedule a workflow

For the user-facing loop "save this goal and run it daily":

1. Save the accepted composite goal as a workflow.
2. Read the returned workflow `id`.
3. Create a scheduler workflow job for that workflow id with exactly one schedule field.

Use a workflow schedule when the same accepted plan should replay with only inputs changing. Use a plain chat scheduler job when each run should be planned fresh.

Scheduled workflows instantiate a fresh root goal on each fire. The scheduler skips only when the previous run completed instantiation and its root goal is still active; failed instantiation does not block the next tick, and a stalled instantiation is resumed instead of duplicated. Scheduled workflows are fully frozen by default; if a workflow is partially frozen, only schedule it when the user explicitly wants live replanning and the native tools expose the required opt-in.

If the native scheduler tools do not expose workflow fields, use `workflow_save`, `workflow_list`, `workflow_get` and `workflow_run`; do not rely on stale syntax from memory.

## Patterns

### Scheduled async goal

Create a scheduler job whose `message` asks the agent to create a goal, for example: "Create an async goal to audit the project and request review before making user-visible changes." Use `session_mode: "new"` for independent scheduled work.

### Recurring job

Use `cron` for calendar schedules and `every` for fixed intervals. Keep the scheduled prompt short and specific.

### One-time reminder

Use `at` with an RFC3339 timestamp. Past timestamps are rejected.

## Job template subscriptions

Platform-provided templates are opt-in scheduled jobs with platform-managed prompts. You cannot edit the message of a subscription job; the prompt is resolved from the template registry.

Use `scheduler__job_create` with `template_key` to subscribe, plus optional schedule override fields such as `every`. One subscription per template is allowed. To unsubscribe, delete the subscribed job by id.

Common templates include:

- `recally-rss` — poll RSS feeds.
- `recally-digest` — generate reading digests.

## Limitations

- Scheduler must be enabled on the server.
- One-time jobs (`at`) with a past timestamp are rejected.
- Plugin-owned jobs cannot be modified.
- Subscription job prompts are read-only; message edits are rejected by the server.
