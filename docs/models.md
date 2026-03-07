# Model Management

## Tiered Models

anna supports two model tiers for different workloads:

| Tier | Use Case |
|------|----------|
| `strong` | Heavy reasoning, complex tasks |
| `fast` | Quick responses, simple queries |

Each tier falls back independently to `model` (top-level default) when not set.

```yaml
model: claude-sonnet-4-6
model_strong: claude-opus-4-6
model_fast: claude-haiku-4-5
```

## CLI Commands

```bash
anna models             # List available models (alias for list)
anna models list        # List all models grouped by provider
anna models update      # Fetch models from provider APIs and update cache
anna models current     # Show active provider/model
anna models set <p/m>   # Switch model (e.g. anna models set openai/gpt-4o)
anna models search <q>  # Search models by name
```

### Model Cache

`anna models update` queries all configured provider APIs and saves results to `~/.anna/cache/models.json`. The cache is used by `list`, `search`, and the Telegram model picker.

If no cache exists, only models explicitly listed in the config are shown.

## Provider Setup

### Anthropic

```yaml
providers:
  anthropic:
    api_key: "sk-..."
```

Or: `export ANTHROPIC_API_KEY="sk-..."`

### OpenAI

```yaml
providers:
  openai:
    api_key: "sk-..."
    base_url: "https://api.openai.com/v1"  # optional
```

Or: `export OPENAI_API_KEY="sk-..."` and optionally `export OPENAI_BASE_URL="..."`

### OpenAI-Compatible (Responses API)

For services like Perplexity, Together.ai, or any OpenAI-compatible API:

```yaml
providers:
  openai-response:
    api_key: "sk-..."
    base_url: "https://api.perplexity.ai"
```

Uses the same `OPENAI_API_KEY` and `OPENAI_BASE_URL` env vars as the `openai` provider.

## Runtime Switching

Models can be switched at runtime:
- **CLI**: Via in-chat `/model` command
- **Telegram**: Via inline keyboard model picker
- **Config**: `anna models set provider/model` persists the selection to config

## Model Metadata

Providers can include detailed model metadata in config:

```yaml
providers:
  anthropic:
    api_key: "sk-..."
    models:
      - id: claude-sonnet-4-6
        reasoning: false
        input: ["text", "image"]
        context_window: 200000
        max_tokens: 8192
        headers: {}
        cost:
          input: 3.0
          output: 15.0
          cache_read: 0.3
          cache_write: 3.75
```

This metadata is used for model resolution and display. When not provided, models are constructed with minimal defaults.
