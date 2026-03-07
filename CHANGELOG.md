# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### ✨ Features

- **CI/CD**: Add CI/CD workflows, Docker, and release infrastructure (#25)
- **Skills**: Native skill management tool (#23)
- **Cron**: Add one-time scheduled jobs (#20)
- **Telegram**: Add streaming draft support (#18)
- **Telegram**: Paginated model list with text filter
- **Telegram**: Notification channel, group support & model switch fix (#13)
- **Telegram**: Enhance bot UX (#12)
- **Telegram**: Add allowed_ids access control
- **Tools**: Truncate large tool outputs to temp file (#9)
- **Models**: Tiered model config (strong/worker/fast) (#10)
- **Session**: Session compaction with LLM-generated handoff summaries (#4)
- **Memory**: Persistent memory system with consolidated file layout (#3)
- **Context**: Support AGENTS.md project context files in system prompt
- **Cron**: Cron scheduling system (#2)

### 🐛 Bug Fixes

- **Docker**: Support multi-platform builds with TARGETARCH/TARGETOS
- **CI**: Resolve 50 errcheck lint issues and add coverage reporting (#26)
- **Telegram**: Notification SendOptions bug and missing callback updates
- **Telegram**: Callback guard in groups and cron startup race
- **Core**: Nil sender panic and notify tool in CLI mode

### ♻️ Refactoring

- Fix remaining lint issues (gocritic, gofmt, staticcheck, ineffassign)
- Multi-backend notification dispatcher
- Remove Pi runner (agent/runner/pi) (#8)
- Move restart-gateway.sh to mise file-based task

### 📝 Documentation

- Restructure README and docs for maintainability
- Add notification system design doc
- Clarify exclusive scope for memory files to prevent duplication (#21)

### 📦 Dependencies

- Bump github.com/cloudflare/circl from 1.6.1 to 1.6.3 (#24)
