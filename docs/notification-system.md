# Notification System

## Status

Implemented — `channel/notifier.go`, `channel/notify_tool.go`, `channel/telegram/telegram.go`.

## Overview

Anna supports proactive notifications so the agent, cron jobs, and other internal triggers can push messages to users without waiting for a request. The system uses a multi-backend dispatcher that routes notifications to one or more configured backends (Telegram, with Slack/Discord planned).

## Architecture

```
┌─────────────────┐
│  Agent (notify   │──┐
│  tool call)      │  │
└─────────────────┘  │
                      │   Notification{Channel, ChatID, Text, Silent}
┌─────────────────┐  │           │
│  Cron job result │──┼──────────▼──────────────┐
└─────────────────┘  │      Dispatcher          │
                      │  ┌──────────────────┐   │
┌─────────────────┐  │  │ Route by Channel  │   │
│  Future triggers │──┘  │ or broadcast all  │   │
└─────────────────┘     └────────┬──────────┘   │
                                 │              │
                    ┌────────────┼──────────┐   │
                    │            │          │   │
                    ▼            ▼          ▼   │
              ┌──────────┐ ┌────────┐ ┌───────┐│
              │ Telegram │ │ Slack  │ │Discord││
              │ Backend  │ │(future)│ │(future)│
              └──────────┘ └────────┘ └───────┘
```

## Key Types

### `channel.Notification`

```go
type Notification struct {
    Channel string // optional: route to specific backend ("telegram", "slack")
    ChatID  string // target chat/channel within the backend
    Text    string // markdown content
    Silent  bool   // send without notification sound
}
```

- `Channel` empty → broadcast to **all** registered backends
- `Channel` set → route to that specific backend only
- `ChatID` empty → each backend uses its configured default

### `channel.Backend`

Interface that notification backends implement:

```go
type Backend interface {
    Name() string
    Notify(ctx context.Context, n Notification) error
}
```

Currently implemented: `telegram.Bot`.

### `channel.Dispatcher`

Routes notifications to registered backends:

```go
d := channel.NewDispatcher()
d.Register(tgBot, "136345060")   // telegram backend with default chat
d.Register(slackBot, "#alerts")  // future: slack backend

// Broadcast to all backends (each uses its default chat):
d.Notify(ctx, channel.Notification{Text: "hello"})

// Route to specific backend:
d.Notify(ctx, channel.Notification{Channel: "telegram", Text: "hello"})

// Override the default chat:
d.Notify(ctx, channel.Notification{Channel: "telegram", ChatID: "999", Text: "hello"})
```

Partial failures: if one backend fails during broadcast, the others still receive the notification. Errors are joined via `errors.Join`.

### `channel.NotifyTool`

Agent-facing tool that wraps the dispatcher:

```go
tool := channel.NewNotifyTool(dispatcher)
```

The LLM can call it with:

```json
{
  "message": "Build finished, 3 tests failed",
  "channel": "telegram",
  "chat_id": "136345060",
  "silent": false
}
```

- `message` (required) — the notification text
- `channel` (optional) — target a specific backend; omit to broadcast
- `chat_id` (optional) — override the backend's default target
- `silent` (optional) — suppress notification sound

## Wiring

### Startup Flow (`main.go`)

```
setup()
  ├── Create Dispatcher
  ├── Create NotifyTool(dispatcher) → extraTools
  ├── Create runner factory with extraTools
  └── Create Pool

runGateway()
  ├── Create telegram.Bot
  ├── dispatcher.Register(tgBot, notifyChat)  ← backend registered
  ├── wireCronNotifier(cron, pool, dispatcher) ← cron output → dispatcher
  └── tgBot.Start(ctx)                        ← begin polling
```

The dispatcher is created early (in `setup`) so the notify tool can reference it. Backends are registered later (in `runGateway`) when they're created. This avoids circular dependencies.

### Cron → Notification

When a cron job fires:
1. The job runs through `pool.Chat()` to get the agent's response
2. The full response text is collected
3. The text is broadcast via `dispatcher.Notify()` to all backends

### CLI Mode

In CLI mode (`anna chat`), no notification backends are registered, so the `notify` tool is not exposed to the agent. This avoids a broken tool path.

## Configuration

### Telegram

```yaml
channels:
  telegram:
    token: "BOT_TOKEN"
    notify_chat: "123456789"    # default chat for notifications
    channel_id: "@my_channel"   # fallback if notify_chat is empty
    group_mode: "mention"       # mention | always | disabled
    allowed_ids:                # restrict bot to these user IDs
      - 136345060
```

Environment variable overrides:

| Variable | Field |
|----------|-------|
| `ANNA_TELEGRAM_NOTIFY_CHAT` | `channels.telegram.notify_chat` |
| `ANNA_TELEGRAM_CHANNEL_ID` | `channels.telegram.channel_id` |
| `ANNA_TELEGRAM_GROUP_MODE` | `channels.telegram.group_mode` |

### Notify Target Resolution

When `Notify()` is called, the target chat is resolved in this order:

1. `Notification.ChatID` (explicit in the call)
2. Backend's registered default chat (from `dispatcher.Register`)
3. For Telegram: `notify_chat` → `channel_id` → error

## Adding a New Backend

To add Slack, Discord, or any other backend:

1. **Implement `channel.Backend`:**

```go
// channel/slack/slack.go
type Bot struct { ... }

func (b *Bot) Name() string { return "slack" }
func (b *Bot) Notify(ctx context.Context, n channel.Notification) error {
    // Send n.Text to n.ChatID via Slack API
}
```

2. **Register in `runGateway()`:**

```go
if s.cfg.Slack.Token != "" {
    slackBot := slack.New(s.cfg.Slack)
    s.notifier.Register(slackBot, s.cfg.Slack.NotifyChannel)
}
```

3. **Add config fields** to `config.go` and env var overrides.

No changes needed to the dispatcher, notify tool, or cron wiring — they work through the `Backend` interface.

## Telegram-Specific Features

### Group Support

The bot can operate in Telegram groups with configurable behavior:

- `mention` (default) — respond only when @mentioned or replied to
- `always` — respond to every message in the group
- `disabled` — ignore all group messages (including commands)

Session ID for groups = group chat ID (shared context per group).

### Access Control

`allowed_ids` restricts bot interaction to specific Telegram user IDs. When the list is empty, all users are allowed. Unauthorized users are silently ignored — all handlers (commands, callbacks, text) are wrapped in the access check.

### Notification Delivery

`telegram.Bot.Notify()` supports:
- Numeric chat IDs (`"136345060"`)
- Channel usernames (`"@my_channel"`)
- Markdown rendering with MarkdownV2 fallback to plain text
- Message splitting at 4000-char boundaries
- Silent mode (`DisableNotification`)
