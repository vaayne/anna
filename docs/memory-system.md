# Memory System

## Status

Implemented — `memory/` package with two-tier storage, agent tool, and system prompt integration.

## Overview

Anna has persistent memory across sessions so the agent can recall facts, log events, and search past interactions. The design prioritizes simplicity: two flat files, no database, no background consolidation, no new dependencies.

## Architecture

```
Agent (via tool call)
    |
    |  update / append / search
    v
+------------+       +----------+
| MemoryTool | ----> |  Store   |
+------------+       +----+-----+
                          |
              +-----------+-----------+
              |                       |
     .agents/memory.md       .agents/journal.jsonl
     (facts — in prompt)     (events — searchable)
```

## Two-Tier Storage

### Tier 1: Facts (`memory.md`)

- Always loaded into the system prompt under `<memory>` tags
- Contains durable knowledge: user preferences, project context, key decisions
- Written as a whole (full replacement via atomic write)
- Target size: ≤4KB — only what matters
- Location: `.agents/memory.md` (unchanged from prior design)

### Tier 2: Journal (`journal.jsonl`)

- Append-only JSONL file, one entry per line
- NOT loaded into system prompt (too large over time)
- Searchable via the `memory` tool's `search` action
- Each entry has timestamp, tags, and text:

```json
{"ts":"2026-03-06T10:30:00Z","tags":["deploy","staging"],"text":"Deployed v2.1 to staging. User confirmed it works."}
```

- Location: `.agents/journal.jsonl`

## Tool Interface

A single `memory` tool with three actions, following the same pattern as `cron`:

| Action | Input | Effect |
|--------|-------|--------|
| `update` | `content` (string) | Atomically overwrite `memory.md` |
| `append` | `text` (string), `tags` ([]string, optional) | Append timestamped entry to `journal.jsonl` |
| `search` | `query` (string), `tag` (string), `limit` (int) | Search journal by substring + tag filter |

### Search Behavior

- Case-insensitive substring match on entry text
- Optional tag filter (also case-insensitive)
- Returns up to `limit` results (default 20) in reverse chronological order
- No indexing — linear scan over JSONL is fast enough for years of personal assistant use

## System Prompt Integration

The system prompt instructs the agent:

1. Facts in `memory.md` are always visible under `<memory>` tags
2. Use `memory` tool action `update` to modify facts (not edit/write)
3. Use `append` to log events worth remembering
4. Use `search` to recall past events by keyword or tag

The agent decides what to save — no automatic consolidation.

## Data Safety

- **Atomic writes** for `memory.md`: write to `.tmp` file, then `os.Rename`. No partial writes.
- **Append-only** for `journal.jsonl`: open with `O_APPEND|O_CREATE`, write one line, close. Crash-safe for single lines within filesystem block size.
- **No background goroutines**: all writes happen synchronously in tool execution.
- **Malformed lines skipped**: corrupted JSONL entries are skipped during search (logged via scanner error only).

## Wiring

```go
// In main.go setup():
if cfg.Runner.Type == "go" {
    memStore := memory.NewStore(configDir())
    extraTools = append(extraTools, memory.NewTool(memStore))
}
```

The memory tool is always available for the Go runner. No config flag needed — memory is a core capability.

## File Layout

```
memory/
├── memory.go       # Store: ReadFacts, WriteFacts, Append, Search
├── memory_test.go  # 6 tests covering all operations
└── tool.go         # MemoryTool: Definition, Execute (update/append/search)
```

## Design Decisions

### Why no automatic consolidation?

Nanobot's approach (LLM-driven consolidation every N messages) adds complexity: async tasks, locks, partial failure handling, `last_consolidated` bookkeeping. Agent-initiated writes via the tool are simpler and produce higher-quality memory — the agent has full context to judge what matters.

### Why no fuzzy search?

Bub uses `rapidfuzz` for fuzzy matching. For a personal assistant's journal, substring match is sufficient and requires zero dependencies. If recall becomes a problem, fuzzy search can be added later without changing the storage format.

### Why not a database?

JSONL is human-readable, trivially backed up, and works with standard Unix tools (`grep`, `wc`, `jq`). A single user's journal will stay small enough that linear scan is fast for years.

### Why a single tool with actions (not three tools)?

Follows the existing `cron` tool pattern. One tool with an `action` enum keeps the tool list short and groups related operations.
