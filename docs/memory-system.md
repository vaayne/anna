# Memory System

## Status

Implemented -- `memory/` package with multi-file storage, agent tool, and system prompt integration.

## Overview

Anna has persistent memory across sessions so the agent can recall facts, log events, and search past interactions. The design prioritizes simplicity: flat files, no database, no background consolidation, no new dependencies.

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
     ~/.anna/memory/            ~/.anna/memory/
     SOUL.md                    JOURNAL.jsonl
     USER.md                    (events -- searchable)
     FACT.md
     (in system prompt)
```

## Storage

### Markdown Files

Three persistent markdown files, always loaded into the system prompt under `<memory>` tags:

| File | Purpose |
|------|---------|
| `SOUL.md` | Agent identity and personality |
| `USER.md` | User preferences and context |
| `FACT.md` | Durable knowledge, key decisions, project context |

- Written as a whole (full replacement via atomic write)
- Case-insensitive file lookup (resolves existing files regardless of case)
- Location: `~/.anna/memory/`

### Journal (`JOURNAL.jsonl`)

- Append-only JSONL file, one entry per line
- NOT loaded into system prompt (too large over time)
- Searchable via the `memory` tool's `search` action
- Each entry has timestamp, tags, and text:

```json
{"ts":"2026-03-06T10:30:00Z","tags":["deploy","staging"],"text":"Deployed v2.1 to staging. User confirmed it works."}
```

- Location: `~/.anna/memory/JOURNAL.jsonl` (or `$ANNA_HOME/memory/JOURNAL.jsonl`)

## Tool Interface

A single `memory` tool with three actions:

| Action | Input | Effect |
|--------|-------|--------|
| `update` | `content` (string) | Atomically overwrite `FACT.md` |
| `append` | `text` (string), `tags` ([]string, optional) | Append timestamped entry to `JOURNAL.jsonl` |
| `search` | `query` (string), `tag` (string), `limit` (int) | Search journal by substring + tag filter |

### Search Behavior

- Case-insensitive substring match on entry text
- Optional tag filter (also case-insensitive)
- Returns up to `limit` results (default 20) in reverse chronological order
- No indexing -- linear scan over JSONL is fast enough for years of personal assistant use

## System Prompt Integration

The system prompt instructs the agent:

1. Facts in `FACT.md` (plus `SOUL.md` and `USER.md`) are always visible under `<memory>` tags
2. Use `memory` tool action `update` to modify facts (not edit/write)
3. Use `append` to log events worth remembering
4. Use `search` to recall past events by keyword or tag

The agent decides what to save -- no automatic consolidation.

## Data Safety

- **Atomic writes** for markdown files: write to `.tmp` file, then `os.Rename`. No partial writes.
- **Append-only** for `JOURNAL.jsonl`: open with `O_APPEND|O_CREATE`, write one line, close. Crash-safe for single lines within filesystem block size.
- **No background goroutines**: all writes happen synchronously in tool execution.
- **Malformed lines skipped**: corrupted JSONL entries are skipped during search.

## Wiring

```go
// In main.go setup():
memStore := memory.NewStore(filepath.Join(configDir(), "memory"))
extraTools = append(extraTools, memory.NewTool(memStore))
```

The memory tool is always available. No config flag needed -- memory is a core capability.

## File Layout

```
memory/
  memory.go       # Store: Read, Write, Append, Search
  memory_test.go  # Tests covering all operations
  tool.go         # MemoryTool: Definition, Execute (update/append/search)
```
