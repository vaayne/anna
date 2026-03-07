# Configuration

Config file: `~/.anna/config.yaml`

The workspace root defaults to `~/.anna` and can be changed by setting the `ANNA_HOME` environment variable. All data paths (sessions, memory, skills, models cache, cron) live under this workspace root.

## Full Reference

```yaml
# Provider credentials and metadata
providers:
  anthropic:
    api_key: "sk-..."
    base_url: ""                   # Optional URL override
    models:                        # Optional model metadata
      - id: claude-sonnet-4-6
        name: "Claude Sonnet"
        reasoning: false           # Supports extended thinking
        input: ["text", "image"]   # Input modalities
        context_window: 200000
        max_tokens: 8192
        headers: {}                # Custom HTTP headers
        cost:
          input: 3.0               # Per-million-token pricing
          output: 15.0
          cache_read: 0.3
          cache_write: 3.75
  openai:
    api_key: "sk-..."
    base_url: "https://api.openai.com/v1"
  openai-response:                 # OpenAI-compatible APIs (e.g. Perplexity)
    api_key: "sk-..."
    base_url: "https://api.example.com/v1"

# Channel configuration
channels:
  telegram:
    token: "BOT_TOKEN"
    notify_chat: "123456789"       # Chat ID for proactive notifications
    channel_id: "@my_channel"      # Optional broadcast channel
    group_mode: "mention"          # mention | always | disabled
    allowed_ids:                   # Restrict to these user IDs (empty = allow all)
      - 136345060

# Agent configuration
agents:
  provider: anthropic              # Default LLM provider
  model: claude-sonnet-4-6         # Default model ID
  workspace: "~/.anna"             # Workspace root (default: ANNA_HOME or ~/.anna)

  # Tiered models (optional)
  # Fallback chain: fast -> worker -> strong -> model
  models:
    strong: claude-sonnet-4-6
    worker: claude-haiku-4-5
    fast: claude-haiku-4-5

  # Runner settings
  runner:
    type: go                       # Runner implementation (only "go" currently)
    system: ""                     # Custom system prompt (bypasses default builder)
    idle_timeout: 10               # Minutes before reaping idle runners
    compaction:
      max_tokens: 80000            # Auto-compact when history exceeds this
      keep_tail: 20                # Keep N recent messages after compaction

  # Scheduled tasks
  cron:
    enabled: true
    data_dir: "~/.anna/cron"       # Job persistence directory
```

## Workspace Layout

All data lives under the workspace root (`~/.anna` by default):

| Path | Purpose |
|------|---------|
| `~/.anna/config.yaml` | Configuration file |
| `~/.anna/sessions/` | Chat session history |
| `~/.anna/cron/` | Cron job persistence |
| `~/.anna/memory/` | Persistent memory (facts + journal) |
| `~/.anna/skills/` | Installed skills |
| `~/.anna/models.json` | Model cache |

Sessions are derived from `agents.workspace` as `<workspace>/sessions` and no longer have a separate config key.

## Environment Variable Overrides

| Variable | Overrides | Notes |
|----------|-----------|-------|
| `ANNA_HOME` | workspace root | Default `~/.anna` |
| `ANNA_PROVIDER` | `agents.provider` | |
| `ANNA_MODEL` | `agents.model` | |
| `ANNA_MODEL_STRONG` | `agents.models.strong` | |
| `ANNA_MODEL_WORKER` | `agents.models.worker` | |
| `ANNA_MODEL_FAST` | `agents.models.fast` | |
| `ANNA_RUNNER_TYPE` | `agents.runner.type` | |
| `ANNA_TELEGRAM_TOKEN` | `channels.telegram.token` | |
| `ANNA_TELEGRAM_NOTIFY_CHAT` | `channels.telegram.notify_chat` | |
| `ANNA_TELEGRAM_CHANNEL_ID` | `channels.telegram.channel_id` | |
| `ANNA_TELEGRAM_GROUP_MODE` | `channels.telegram.group_mode` | |
| `ANTHROPIC_API_KEY` | `providers.anthropic.api_key` | |
| `ANTHROPIC_BASE_URL` | `providers.anthropic.base_url` | |
| `OPENAI_API_KEY` | `providers.openai.api_key` | Also used by `openai-response` |
| `OPENAI_BASE_URL` | `providers.openai.base_url` | Also used by `openai-response` |

Note: Environment variable names are unchanged from previous versions.

## Defaults

| Field | Default |
|-------|---------|
| `agents.provider` | `anthropic` |
| `agents.model` | `claude-sonnet-4-6` |
| `agents.workspace` | `~/.anna` |
| `agents.runner.type` | `go` |
| `agents.runner.idle_timeout` | `10` (minutes) |
| `agents.runner.compaction.max_tokens` | `80000` |
| `agents.runner.compaction.keep_tail` | `20` |
| `agents.cron.enabled` | `true` |
| `agents.cron.data_dir` | `~/.anna/cron` |
| `channels.telegram.group_mode` | `mention` |
