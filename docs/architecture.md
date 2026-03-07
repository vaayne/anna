# Architecture

## System Overview

anna is structured as a set of loosely coupled packages wired together in `main.go`. The core flow:

1. A **channel** (CLI or Telegram) receives user input
2. The **Pool** manages sessions and dispatches to a **Runner**
3. The **Go runner** calls LLM providers via `ai/providers/`, executing tools in a loop
4. Responses stream back through the channel to the user

```
Channel (CLI/Telegram)
    |
    v
Pool (sessions + runner lifecycle)
    |
    v
Go Runner (agent loop + tools)
    |
    v
LLM Provider (Anthropic/OpenAI/OpenAI-compatible)
```

## Package Layout

```
main.go                             Entry point, CLI commands, service wiring
config.go                           Config types, YAML loading, env var overrides
models.go                           Model cache, discovery, CLI model commands

ai/
  types/                            Shared types (Model, Message, ToolDefinition, events)
  stream/                           Streaming abstractions
  providers/
    anthropic/                      Anthropic provider (Messages API)
    openai/                         OpenAI provider (Chat Completions API)
    openai-response/                OpenAI-compatible provider (Responses API)
    register_builtins.go            Auto-register all built-in providers
  registry/                         Provider registry
  transform/                        Message format conversions

agent/
  pool.go                           Session pool, runner lifecycle, idle reaping
  session.go                        Per-chat session state and history
  store/                            Session persistence (JSONL file store, index)
  engine/
    engine.go                       Agent loop engine (multi-turn tool execution)
    continue.go                     Resume agent loop from existing history
    types.go                        LoopConfig, ToolSet, ToolFunc
    events.go                       Loop event types (AgentStarted, AssistantDelta, etc.)
    tool_execution.go               Tool call dispatch with callbacks
  runner/
    runner.go                       Runner interface, RPC types, event helpers
    gorunner.go                     GoRunner: native LLM provider calls
    prompt.go                       System prompt builder (memory, tools, context)
    skill.go                        Skill loading from ~/.anna/workspace/skills/
    stream_proxy.go                 Stream proxy utilities
  tool/                             Built-in tools
    tool.go                         Tool interface and registry
    read.go                         Read file contents
    bash.go                         Execute shell commands
    write.go                        Create/overwrite files
    edit.go                         Edit file sections
    truncate.go                     Truncate large outputs to temp files

channel/
  notifier.go                       Notification dispatcher (multi-backend)
  notify_tool.go                    Agent notify tool
  model.go                          Model list/switch function types
  cli/
    cli.go                          Interactive TUI entry points
    chat.go                         Bubble Tea chat model
    command.go                      In-chat slash commands (/compact, /model, etc.)
    model.go                        TUI model switching UI
    style.go                        Terminal styling
  telegram/
    telegram.go                     Bot setup, long polling, notification backend
    handler.go                      Message/callback handlers
    stream.go                       Streaming (draft API + edit fallback)
    render.go                       Markdown rendering, message splitting
    model.go                        Paginated model picker UI

cron/
  cron.go                           Scheduler service (gocron/v2)
  job.go                            Job types, JSON persistence
  tool.go                           Agent cron tool (add/list/remove)

memory/
  memory.go                         Store: Read/Write files, Append/Search journal
  tool.go                           Agent memory tool (update/append/search)

skills/
  tool.go                           Agent skills tool (search/install/list/remove)
  search.go                         Skills ecosystem search via skills.sh API
  install.go                        Git clone + copy install flow (go-git)
  list.go                           List installed skills
  remove.go                         Remove installed skills
```

## Providers

Three LLM providers are supported:

| Provider | API | Use Case |
|----------|-----|----------|
| `anthropic` | Messages API | Claude models |
| `openai` | Chat Completions API | GPT models |
| `openai-response` | Responses API | OpenAI-compatible services (Perplexity, Together.ai, etc.) |

Each provider implements the `stream.Provider` interface for streaming responses and optionally `stream.ModelLister` for model discovery.

## Tools

The Go runner injects tools into LLM calls. Tools follow a common interface:

```go
type Tool interface {
    Definition() ToolDefinition
    Execute(ctx context.Context, args map[string]any) (string, error)
}
```

### Built-in Tools (always available)

| Tool | Description |
|------|-------------|
| `read` | Read file contents with UTF-8 safe truncation |
| `bash` | Execute shell commands |
| `write` | Create/overwrite files atomically |
| `edit` | Edit file sections preserving context |
| `truncate` | Truncate large outputs to temp files |

### Extra Tools (conditionally injected)

| Tool | Condition | Description |
|------|-----------|-------------|
| `memory` | Always | Persistent memory (update facts, append journal, search) |
| `skills` | Always | Skill management (search/install/list/remove from skills.sh) |
| `cron` | `cron.enabled: true` | Schedule tasks (add/list/remove jobs) |
| `notify` | Gateway mode + Telegram configured | Send notifications via dispatcher |

## Session Lifecycle

1. Channel sends message to `Pool.Chat(ctx, sessionID, prompt)`
2. Pool finds or creates a session, loading history from disk if persisted
3. Pool acquires or creates a runner for the session
4. Runner streams events back through a channel
5. On idle timeout, runners are reaped; sessions persist to JSONL

See [session-compaction.md](session-compaction.md) for history management.

## Notification Flow

```
Agent notify tool --> Dispatcher --> Backend (Telegram)
Cron job result   --> Dispatcher --> Backend (Telegram)
```

The dispatcher is created early in setup, but backends are registered later when gateway services start. See [notification-system.md](notification-system.md) for details.
