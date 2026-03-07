# Deployment

Two deployment methods: **binary** (direct install) and **Docker**.

## Binary

### From Release

Download a pre-built binary from [GitHub Releases](https://github.com/vaayne/anna/releases). Binaries are available for linux, macOS, and Windows on amd64/arm64.

```bash
# Example: Linux amd64
curl -LO https://github.com/vaayne/anna/releases/latest/download/anna_linux_amd64.tar.gz
tar xzf anna_linux_amd64.tar.gz
chmod +x anna
sudo mv anna /usr/local/bin/
```

### From Source

```bash
go install github.com/vaayne/anna@latest
# or
git clone https://github.com/vaayne/anna.git
cd anna && go build -o anna .
```

### Running

Create a config file at `~/.anna/config.yaml` (see [configuration.md](configuration.md) for full reference):

```bash
mkdir -p ~/.anna
cat > ~/.anna/config.yaml <<'EOF'
providers:
  anthropic:
    api_key: "sk-..."

agents:
  provider: anthropic
  model: claude-sonnet-4-6
EOF
```

Start the gateway daemon:

```bash
anna gateway
```

Or use the interactive CLI:

```bash
anna chat
```

### Systemd Service (Linux)

```ini
# /etc/systemd/system/anna.service
[Unit]
Description=anna gateway
After=network.target

[Service]
Type=simple
User=anna
WorkingDirectory=/home/anna
ExecStart=/usr/local/bin/anna gateway
Restart=on-failure
RestartSec=5

# Environment overrides (alternative to config file)
Environment=ANTHROPIC_API_KEY=sk-...
Environment=ANNA_TELEGRAM_TOKEN=123456:ABC...

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now anna
```

## Docker

Images are published to `ghcr.io/vaayne/anna` for `linux/amd64` and `linux/arm64`.

### Tags

| Tag | Description |
|-----|-------------|
| `latest` | Latest stable release |
| `v1.2.3` | Specific version |
| `sha-<commit>` | Specific commit |

### Quick Start

```bash
docker run -d \
  --name anna \
  -v $(pwd)/anna-data:/home/nonroot/.anna \
  -e ANTHROPIC_API_KEY=sk-... \
  -e ANNA_TELEGRAM_TOKEN=123456:ABC... \
  ghcr.io/vaayne/anna:latest
```

The container runs as `nonroot` user. Mount `~/.anna` to persist config, sessions, and cron data. You can set `ANNA_HOME` to change the workspace path inside the container.

### Docker Compose

```yaml
# docker-compose.yml
services:
  anna:
    image: ghcr.io/vaayne/anna:latest
    restart: unless-stopped
    volumes:
      - ./anna-data:/home/nonroot/.anna
    environment:
      - ANTHROPIC_API_KEY=sk-...
      - ANNA_TELEGRAM_TOKEN=123456:ABC...
      # - ANNA_TELEGRAM_NOTIFY_CHAT=123456789
      # - ANNA_TELEGRAM_GROUP_MODE=mention
```

```bash
docker compose up -d
```

### Build Locally

```bash
# Single platform
docker build -t anna .

# Multi-platform
docker buildx build --platform linux/amd64,linux/arm64 -t anna .
```

## Volumes & Data

| Path | Purpose |
|------|---------|
| `~/.anna/config.yaml` | Configuration |
| `~/.anna/sessions/` | Chat session history |
| `~/.anna/cron/` | Cron job persistence |
| `~/.anna/memory/` | Persistent memory (facts + journal) |
| `~/.anna/skills/` | Installed skills |
| `~/.anna/models.json` | Model cache |

All paths are under the workspace root (`~/.anna` by default, configurable via `ANNA_HOME`).

## Environment Variables

All config values can be overridden via environment variables. See [configuration.md](configuration.md#environment-variable-overrides) for the full list.

Key variables for deployment:

| Variable | Required | Description |
|----------|----------|-------------|
| `ANNA_HOME` | No | Workspace root (default `~/.anna`) |
| `ANTHROPIC_API_KEY` | Yes* | Anthropic provider key |
| `OPENAI_API_KEY` | Yes* | OpenAI provider key |
| `ANNA_TELEGRAM_TOKEN` | For Telegram | Bot token from @BotFather |
| `ANNA_TELEGRAM_NOTIFY_CHAT` | No | Chat ID for proactive notifications |

\* At least one provider key is required.

## Health Check

The gateway logs to stdout. Verify it's running:

```bash
# Binary
anna gateway  # Logs appear in terminal

# Docker
docker logs anna
```
