# Telegram Bot

anna includes a Telegram bot that runs via long polling -- no webhook or public IP needed.

## Setup

1. Create a bot via [@BotFather](https://t.me/BotFather)
2. Add the token to config:

```yaml
telegram:
  token: "BOT_TOKEN"
```

Or via environment: `ANNA_TELEGRAM_TOKEN=BOT_TOKEN`

3. Start the gateway:

```bash
anna gateway
```

## Streaming Responses

The bot streams LLM responses in real time with two strategies:

### Private Chats: Draft API (Bot API 9.3+)

Uses `sendMessageDraft` for smooth animated streaming without rate-limiting issues. If the API is not available, automatically falls back to edit mode.

### Group Chats: Edit-in-Place

Sends an initial message and edits it periodically (every ~1 second) as tokens arrive. The streaming message is deleted once complete, then the final response is sent.

### Tool Indicators

During tool execution, the stream shows status with emoji indicators:

| Tool | Emoji |
|------|-------|
| `bash` | lightning |
| `read` | book |
| `write` | pencil |
| `edit` | wrench |
| `search` | magnifying glass |

## Group Support

Configure how the bot responds in group chats:

```yaml
telegram:
  group_mode: "mention"   # Only respond when @mentioned (default)
  # group_mode: "always"  # Respond to all messages
  # group_mode: "disabled" # Ignore group messages entirely
```

## Access Control

Restrict which Telegram users can interact with the bot:

```yaml
telegram:
  allowed_ids:
    - 136345060           # Your Telegram user ID
```

Leave empty to allow all users.

## Notifications

The bot doubles as a notification backend. Configure a default chat for proactive messages:

```yaml
telegram:
  notify_chat: "123456789"   # Chat ID for notifications
  channel_id: "@my_channel"  # Optional broadcast channel
```

Used by:
- The `notify` agent tool (in gateway mode)
- Cron job result broadcasting

See [notification-system.md](notification-system.md) for the full notification architecture.

## Model Switching

Users can switch models mid-conversation via an inline keyboard triggered by the `/model` command in Telegram. The model list is paginated with text filtering support.

## Configuration Reference

| Field | Description | Default |
|-------|-------------|---------|
| `token` | Bot API token | (required) |
| `notify_chat` | Chat ID for proactive notifications | |
| `channel_id` | Broadcast channel (@name or numeric ID) | |
| `group_mode` | Group behavior: `mention`, `always`, `disabled` | `mention` |
| `allowed_ids` | User IDs allowed to use bot (empty = all) | `[]` |
