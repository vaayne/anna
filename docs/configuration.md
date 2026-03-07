# Configuration

Config file: `.agents/config.yaml`

## Full Reference

```yaml
provider: anthropic                # Default LLM provider
model: claude-sonnet-4-6           # Default model ID

# Tiered models (optional)
# Fallback chain: fast -> worker -> strong -> model
models:
  strong: claude-sonnet-4-6
  worker: claude-haiku-4-5
  fast: claude-haiku-4-5

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

# Runner settings
runner:
  type: go                         # Runner implementation (only "go" currently)
  system: ""                       # Custom system prompt (bypasses default builder)
  idle_timeout: 10                 # Minutes before reaping idle runners
  compaction:
    max_tokens: 80000              # Auto-compact when history exceeds this
    keep_tail: 20                  # Keep N recent messages after compaction

# Telegram bot
telegram:
  token: "BOT_TOKEN"
  notify_chat: "123456789"         # Chat ID for proactive notifications
  channel_id: "@my_channel"        # Optional broadcast channel
  group_mode: "mention"            # mention | always | disabled
  allowed_ids:                     # Restrict to these user IDs (empty = allow all)
    - 136345060

# Scheduled tasks
cron:
  enabled: true
  data_dir: ".agents/cron"         # Job persistence directory

# Session persistence directory
sessions: ".agents/workspace/sessions"
```

## Environment Variable Overrides

| Variable | Overrides | Notes |
|----------|-----------|-------|
| `ANNA_PROVIDER` | `provider` | |
| `ANNA_MODEL` | `model` | |
| `ANNA_MODEL_STRONG` | `models.strong` | |
| `ANNA_MODEL_WORKER` | `models.worker` | |
| `ANNA_MODEL_FAST` | `models.fast` | |
| `ANNA_RUNNER_TYPE` | `runner.type` | |
| `ANNA_TELEGRAM_TOKEN` | `telegram.token` | |
| `ANNA_TELEGRAM_NOTIFY_CHAT` | `telegram.notify_chat` | |
| `ANNA_TELEGRAM_CHANNEL_ID` | `telegram.channel_id` | |
| `ANNA_TELEGRAM_GROUP_MODE` | `telegram.group_mode` | |
| `ANTHROPIC_API_KEY` | `providers.anthropic.api_key` | |
| `ANTHROPIC_BASE_URL` | `providers.anthropic.base_url` | |
| `OPENAI_API_KEY` | `providers.openai.api_key` | Also used by `openai-response` |
| `OPENAI_BASE_URL` | `providers.openai.base_url` | Also used by `openai-response` |

## Defaults

| Field | Default |
|-------|---------|
| `provider` | `anthropic` |
| `model` | `claude-sonnet-4-6` |
| `runner.type` | `go` |
| `runner.idle_timeout` | `10` (minutes) |
| `runner.compaction.max_tokens` | `80000` |
| `runner.compaction.keep_tail` | `20` |
| `cron.enabled` | `true` |
| `cron.data_dir` | `.agents/cron` |
| `sessions` | `.agents/workspace/sessions` |
| `telegram.group_mode` | `mention` |
