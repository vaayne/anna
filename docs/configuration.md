# Configuration

Config file: `~/.anna/config.yaml`

The workspace root defaults to `~/.anna/workspace` and can be changed by setting the `ANNA_HOME` environment variable. Session, memory, skills, and cron data live under the workspace root. The model cache lives in `~/.anna/cache/`, and runtime state (current provider/model) is stored in `~/.anna/state.yaml` — separate from the static config.

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

# Default LLM provider
provider: anthropic
# Default model ID
model: claude-sonnet-4-6
# Tiered models (optional)
# Fallback chain: fast -> strong -> model
model_strong: claude-opus-4-6
model_fast: claude-haiku-4-5
# Workspace root (default: ANNA_HOME or ~/.anna/workspace)
workspace: "~/.anna/workspace"

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
  data_dir: "~/.anna/workspace/cron"  # Job persistence directory
```

## Directory Layout

| Path | Purpose | Category |
|------|---------|----------|
| `~/.anna/config.yaml` | Static configuration (user-edited) | Config |
| `~/.anna/state.yaml` | Runtime state: current provider/model (program-managed) | State |
| `~/.anna/cache/models.json` | Cached model list (safe to delete) | Cache |
| `~/.anna/workspace/sessions/` | Chat session history | Data |
| `~/.anna/workspace/memory/` | Persistent memory (facts + journal) | Data |
| `~/.anna/workspace/skills/` | Installed skills | Data |
| `~/.anna/workspace/cron/` | Cron job persistence | Data |

- **config.yaml** is static and user-edited — safe to version control.
- **state.yaml** is written by `anna models set` and the `/model` command. It overrides `provider` and `model` from config.yaml.
- **cache/** contains regenerable data. Run `anna models update` to rebuild.
- **workspace/** contains all persistent application data.

## Environment Variable Overrides

All config fields support env var overrides using the `ANNA_` prefix. Nested structs add their own prefix segment (e.g. `runner.type` → `ANNA_RUNNER_TYPE`).

**Priority order** (highest wins): env vars → state.yaml → config.yaml → defaults.

| Variable | Overrides | Notes |
|----------|-----------|-------|
| `ANNA_HOME` | anna home directory | Default `~/.anna` |
| `ANNA_PROVIDER` | `provider` | |
| `ANNA_MODEL` | `model` | |
| `ANNA_MODEL_STRONG` | `model_strong` | |
| `ANNA_MODEL_FAST` | `model_fast` | |
| `ANNA_WORKSPACE` | `workspace` | |
| `ANNA_RUNNER_TYPE` | `runner.type` | |
| `ANNA_RUNNER_IDLE_TIMEOUT` | `runner.idle_timeout` | |
| `ANNA_CRON_ENABLED` | `cron.enabled` | |
| `ANNA_CRON_DATA_DIR` | `cron.data_dir` | |
| `ANNA_TELEGRAM_TOKEN` | `channels.telegram.token` | |
| `ANNA_TELEGRAM_NOTIFY_CHAT` | `channels.telegram.notify_chat` | |
| `ANNA_TELEGRAM_CHANNEL_ID` | `channels.telegram.channel_id` | |
| `ANNA_TELEGRAM_GROUP_MODE` | `channels.telegram.group_mode` | |
| `ANNA_TELEGRAM_ALLOWED_IDS` | `channels.telegram.allowed_ids` | Comma-separated |
| `ANTHROPIC_API_KEY` | `providers.anthropic.api_key` | Standard provider env |
| `ANTHROPIC_BASE_URL` | `providers.anthropic.base_url` | Standard provider env |
| `OPENAI_API_KEY` | `providers.openai.api_key` | Also used by `openai-response` |
| `OPENAI_BASE_URL` | `providers.openai.base_url` | Also used by `openai-response` |

## Defaults

| Field | Default |
|-------|---------|
| `provider` | `anthropic` |
| `model` | `claude-sonnet-4-6` |
| `workspace` | `~/.anna/workspace` |
| `runner.type` | `go` |
| `runner.idle_timeout` | `10` (minutes) |
| `runner.compaction.max_tokens` | `80000` |
| `runner.compaction.keep_tail` | `20` |
| `cron.enabled` | `true` |
| `cron.data_dir` | `~/.anna/workspace/cron` |
| `channels.telegram.group_mode` | `mention` |
