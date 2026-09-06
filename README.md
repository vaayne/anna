<p align="center">
  <img src="avatar.png" width="200" alt="stella" />
</p>

<p align="center">
  English | <a href="README.zh.md">中文</a>
</p>

# Stella — Shared AI coworkers for every team

> **⚠️ Under Heavy Development** — Stella is not stable. APIs, config formats, and behavior may change without notice. Not recommended for production use.

Stella turns the expertise your team repeats — finance, HR, engineering, research — into shared AI coworkers. Set an agent up once; everyone else just asks it in the chat tools they already use.

A domain owner gives an agent its instructions, skills, tools, knowledge, and memory rules. After that, nobody has to learn the finance system, the recruiting tool, or the internal toolchain to move work forward — they tell the agent the goal, and it does the work within the boundaries you set. Each person gets their own memory with the agent, so Stella understands different teammates without flattening everyone into one profile.

Under the hood it's a single-tenant, multi-user, multi-agent system: one deployment is one trust boundary, many people can rely on it at once, and each agent has its own role, model, skills, tools, schedules, workspace, and safety boundaries. Deploy it where you want, use your own model API keys, and reach it from Telegram, Discord, QQ, Feishu, DingTalk, WeChat, the Web UI, or the terminal.

Small teams and individual developers can run the same setup — one agent doing the back-office work no one has time for — but Stella is built first for teams who keep paying their experts to answer the same questions.

## Why use Stella

- **Anyone just asks.** A teammate doesn't learn the finance or HR system to get help — they ask the agent in chat, and it does the work.
- **One expert, shared by everyone.** A domain owner builds an agent once; the whole team reuses it instead of interrupting the specialist.
- **Remembers each teammate.** Memory is scoped per user per agent, so nobody re-explains their context.
- **Acts within boundaries you set.** Agents work in dedicated workspaces with sandbox policies and controlled tool access, and stop for human review where you require it.
- **Lives in the chat you already use.** Telegram, Discord, QQ, Feishu, DingTalk, WeChat, the Web UI, and the terminal are all front doors to the same agents.
- **Keeps routines moving.** Schedule reminders, recurring jobs, reading digests, and background tasks that persist across restarts and notify the right people.

## Quick start

```bash
# 1. Install
brew install CherryHQ/tap/stella

# 2. Start the server
stellad server

# 3. Open the Web UI at http://localhost:25678
#    Add your provider and API key under Providers

# 4. Open Chat and start talking
```

You can also download binaries from [Releases](https://github.com/CherryHQ/stella/releases), or build from source with `git clone` and `mise run build`. `go install` is not supported: the binary embeds generated code, the Web UI, and the bundled runtimes, none of which are in version control.

See the [full quickstart guide](web/content/docs/getting-started/quickstart.md) for detailed steps. To run Stella on Kubernetes, use the production [Helm chart](web/content/docs/admin/kubernetes.md).

## Connect your channels

All channels share the same memory. Chat from one, switch to another, and Stella picks up where you left off.

| Channel  | How to connect             | Streaming support |
| -------- | -------------------------- | ----------------- |
| Terminal | Built-in TUI               | Token-by-token    |
| Telegram | Long polling, no public IP | Yes               |
| Discord  | Gateway WebSocket          | Final response    |
| QQ       | WebSocket                  | Yes               |
| Feishu   | WebSocket, no public IP    | Edit-in-place     |
| DingTalk | Stream mode, no public IP  | Final response    |
| WeChat   | Long polling (iLink Bot)   | No                |

You can bind each channel to a specific agent in the Web UI.

## MCP Tools

Stella connects agents to remote MCP (Model Context Protocol) servers over streamable HTTP — with OAuth 2.1, bearer, or no auth — and installs new servers from the official MCP Registry marketplace in the Web UI. Every tool a server exposes is per-agent and per-user switchable, with the same four-scope permissions as everything else.

MCP registrations keep their UUID when they become plugin configurations. Shared
and per-user OAuth observations stay isolated; older per-user catalogs are cold
probed after migration. OAuth client initialization for system and system-agent
configurations requires an administrator.

## Skills

Skills are reusable playbooks that teach Stella how to perform specific tasks. In conversation, Stella can search the Skills already available to the active Agent and load an exact revision. Install, upload, edit, and remove Skills from the Web UI, where every write has an explicit ownership scope.

Release-provided core Skills are read-only resources. Plugin Skills inherit the
plugin's four-scope decision, while core Skills keep their explicit core
dependencies. Administrators can disable any builtin plugin; a disabled winning
scope does not fall back to a broader configuration. See the [Skills guide](web/content/docs/guides/skills.md) for scopes, per-Agent activation, and precedence.

## Documentation

| Section         | What's inside                                   | Link                                            |
| --------------- | ----------------------------------------------- | ----------------------------------------------- |
| Getting Started | Install, deploy, configure                      | [Quick Start](/docs/getting-started/quickstart) |
| Guides          | Memory, scheduling, skills, notifications       | [Guides](/docs/guides/memory)                   |
| Channels        | Telegram, Discord, QQ, Feishu, DingTalk, WeChat | [Channels](/docs/channels/telegram)             |
| Webhooks        | Personal HTTP invocation capabilities           | [Webhooks](/docs/webhooks/webhook)              |
| Admin           | Kubernetes / Helm deployment                    | [Kubernetes](/docs/admin/kubernetes)            |
| Development     | Architecture, plugins, contributing             | [Development](/docs/development/architecture)   |

## CLI reference

```bash
stellad server                          # Start server; Web UI at http://localhost:25678
stellad server --port 8080              # Custom port
stellad upgrade                         # Self-update to latest release
stellad upgrade 0.50.0                   # Self-update to a specific release
stellad version                         # Print version
stellad vault keygen                    # Generate a vault bootstrap key
stellad system-bundle revision          # Print the builtin Skill bundle revision
stellad system-bundle install            # Install the verified builtin Skill bundle
stellad system-bundle verify             # Verify the builtin Skill bundle
```

## Development

Development requires [mise](https://mise.jdx.dev/). On a fresh clone:

```bash
mise run setup    # Set up dev environment and pre-commit hooks
mise run build    # Build binary
mise run test     # Run tests
mise run format   # Lint and format
```

## License

GNU Affero General Public License v3.0 or later. See [LICENSE](LICENSE).
