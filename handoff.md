# Handoff

## Goal

Restructure anna's config: flatten `agents` to top level, simplify model tiers, move default workspace to `~/.anna/workspace`.

## Progress

- Config struct flattened: `Provider`, `Model`, `ModelStrong`, `ModelFast`, `Workspace`, `Runner`, `Cron` are top-level fields on `Config`
- Removed `AgentsConfig` and `ModelsConfig` structs
- Removed `worker` model tier — only `strong` and `fast` remain, both fall back to `model`
- Default workspace changed from `~/.anna` to `~/.anna/workspace`
- `~/.anna` remains the anna home (config.yaml lives here)
- All code, tests, and docs updated
- All tests pass with `-race`, `go vet` clean
- Two commits on branch `refactor/config-workspace`

## Key Decisions

- **Flat top-level config**: `provider`, `model`, `model_strong`, `model_fast`, `workspace`, `runner`, `cron` are top-level YAML keys. Only `providers` and `channels` remain nested (they're maps/groups).
- **Two model tiers**: `model_strong` and `model_fast`, both fall back to `model`. No more `worker` tier or nested `models:` section.
- **`~/.anna/workspace` as default workspace**: Sessions, memory, skills, cron, models.json, logs live under `~/.anna/workspace/`. Config stays at `~/.anna/config.yaml`.
- **`~/.anna` as anna home**: `ANNA_HOME` env var overrides this. Config is at `$ANNA_HOME/config.yaml`, workspace defaults to `$ANNA_HOME/workspace`.
- **Env vars unchanged**: `ANNA_PROVIDER`, `ANNA_MODEL`, `ANNA_MODEL_STRONG`, `ANNA_MODEL_FAST`, `ANNA_RUNNER_TYPE` still work. `ANNA_MODEL_WORKER` removed.

## New Config YAML Format

```yaml
provider: anthropic
model: claude-sonnet-4-6
model_strong: claude-opus-4-6
model_fast: claude-haiku-4-5
workspace: "~/.anna/workspace"

runner:
  type: go
  idle_timeout: 10

cron:
  enabled: true
  data_dir: "~/.anna/workspace/cron"

providers:
  anthropic:
    api_key: "sk-..."
    base_url: "https://..."

channels:
  telegram:
    token: "..."
    notify_chat: "..."
```

## Files Changed

- `config.go` — Flattened Config struct, removed AgentsConfig/ModelsConfig, simplified resolveModelID
- `config_test.go` — All tests updated for flat field access
- `main.go` — All `cfg.Agents.X` → `cfg.X`
- `models.go` — All `cfg.Agents.X` → `cfg.X`
- `.agents/config.yaml` — Updated to flat format
- `README.md` + 6 docs files — Updated config examples and references

## Next Steps

1. Review the diff and merge `refactor/config-workspace` into `main` when ready
2. Consider migrating existing `~/.anna/` data to `~/.anna/workspace/` (migration script or first-run logic)
3. Update CHANGELOG for the breaking config change
