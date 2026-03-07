# Session Compaction

## Status

Implemented — `agent/pool.go` (orchestration), `agent/store/store.go` (file rewriting), channels expose `/compact`.

## Problem

LLM runners have finite context windows. As a session accumulates messages, the
JSONL history grows unbounded. Eventually the runner's context fills up, leading
to degraded responses or hard failures. Long-lived sessions (Telegram chats,
multi-day coding sessions) hit this wall quickly.

We need a way to compress old history without losing critical context, and
without requiring the user to manually start a new session.

## Design: Handoff-Style Compaction

The core idea is borrowed from the "handoff" pattern used by coding agents: when
context gets too large, ask the LLM itself to produce a self-contained summary,
then replace the old history with that summary plus a small tail of recent
messages.

```
Before compaction:
┌──────────────────────────────────┐
│ session header                   │
│ message 1                        │
│ message 2                        │
│ ...                              │
│ message N-20                     │
│ message N-19                     │  ← kept verbatim
│ ...                              │
│ message N                        │
└──────────────────────────────────┘

After compaction:
┌──────────────────────────────────┐
│ session header                   │
│ compaction { summary }           │  ← LLM-generated summary
│ message N-19                     │  ← last 20 messages kept
│ ...                              │
│ message N                        │
└──────────────────────────────────┘
```

The summary is structured so the runner can continue the conversation without
the original messages — it contains the goal, progress, decisions, files
changed, current state, blockers, and next steps.

## Architecture

```
Channel (/compact or auto)
    |
    v
Pool.CompactSession(ctx, sessionID)
    |
    ├─ getOrCreateRunner()       load session from disk if needed, ensure runner
    │
    ├─ collectFullResponse()     send compaction prompt to runner, collect summary
    │
    ├─ store.Compact()           rewrite JSONL: header + compaction entry + tail
    │
    ├─ replace in-memory events  swap sess.Events with compacted version
    │
    └─ kill runner               next Chat() starts fresh with clean context
```

### Token Estimation

`store.EstimateTokens()` scans the JSONL file, sums bytes of `message` and
`compaction` lines, and divides by 4 (rough heuristic: ~4 bytes per token for
English text with JSON overhead). This avoids loading entire sessions into
memory just to check size.

### Compaction Prompt

The prompt asks the runner to produce a structured summary:

- **Goal** — original session objective
- **Progress** — what was completed or partially done
- **Key Decisions** — decisions and rationale
- **Files Changed** — paths with context
- **Current State** — what works, what doesn't
- **Blockers / Gotchas** — issues or edge cases
- **Next Steps** — concrete, actionable tasks

Guidelines enforce self-containment: the summary must make sense to a reader
with zero access to the prior conversation.

### JSONL File Format

The compaction entry is a single JSON line:

```json
{"type":"compaction","id":"abc123","summary":"## Goal\n..."}
```

On `Load()`, the store converts this into a pair of `RPCEvent`s — a user
message containing the summary and an assistant acknowledgment — so the runner
sees it as normal conversation history.

Message `parentId` fields are re-chained so the compaction entry becomes the
parent of the first kept message, preserving the linked-list structure.

## Triggers

### Manual — `/compact` command

Available in both CLI and Telegram:

```
/compact
```

Calls `Pool.CompactSession()` directly. Returns the summary text to the user.

### Automatic — token threshold

`Pool.Chat()` checks `Pool.NeedsCompaction()` before each message. If the
estimated token count exceeds the threshold, compaction runs automatically
before the user's message is sent.

If auto-compaction fails, the system logs a warning and continues with the full
history — it never blocks the user's message.

## Configuration

In `.agents/config.yaml` under `runner`:

```yaml
runner:
  compaction:
    max_tokens: 80000   # auto-compact threshold (0 = default 80k, -1 = disabled)
    keep_tail: 20       # recent messages to preserve verbatim
```

Both fields have defaults applied via `CompactionConfig.WithDefaults()`:
- `max_tokens`: 80,000 (when 0 or omitted; set to `-1` to disable)
- `keep_tail`: 20

Setting `max_tokens: -1` in config disables automatic compaction; `/compact`
still works manually.

## Stateful Runners

Some runners (like the Pi subprocess) maintain their own context in-process and
ignore the `history` parameter passed to `Chat()`. For these runners, killing
the process after compaction would destroy live context for no benefit — the new
process can't replay the compacted history anyway.

Runners signal this by implementing the optional `runner.Stateful` interface:

```go
type Stateful interface {
    Stateful() bool
}
```

When a runner is stateful, `CompactSession()` skips the runner kill. The
compacted history is still written to disk (for crash recovery and session
restore), but the live runner keeps its in-process context intact.

For stateless runners that rebuild context from history, the runner is killed as
before so the next `Chat()` call starts fresh with the compacted history.

## Session Loading

`CompactSession()` uses `getOrCreateRunner()` — the same path as `Chat()` — so
it works even when the session exists only on disk (e.g., after process restart
or for Telegram sessions that haven't been accessed in this process lifetime).
This ensures `/compact` is always available for any persisted session.

## Failure Modes

| Scenario | Behavior |
|---|---|
| No store configured | Returns error: "compaction requires a persistent store" |
| Runner fails to summarize | Returns error, session untouched |
| Store rewrite fails | Returns error, original file preserved |
| Auto-compaction fails | Logs warning, continues with full history |
| Empty summary from runner | Returns error: "empty summary response" |

The original JSONL file is only replaced after the compaction entry and tail
messages are successfully built. The write uses `os.Rename` for atomicity where
the OS supports it.
