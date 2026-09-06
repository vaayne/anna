---
title: CLI 设计规则
description: Stella 的 stellad 运维命令约定。
---

> 这是给贡献者看的**规则文件**。新增或修改 `stellad` 命令前，先读这页并遵守它。Stella 遵循 [Command Line Interface Guidelines](https://github.com/cli-guidelines/cli-guidelines) 的精神：命令要可预测、可脚本化、可组合，并且善待凌晨两点还在终端里的用户。

Stella 现在只有一个二进制：`stellad`。它负责启动服务器、后台服务管理、升级、引导工具和数据库维护。它不是人类聊天客户端，也不是代理集成面。代理能力通过 x-agent-tool/toolgen 以原生工具交付，并使用服务端身份；不要新增面向 sandbox 的 CLI 命令、scoped bearer token 捷径，或第二个客户端二进制。

当前 `stellad` 命令面包括：

- `stellad server` / `stellad serve` — 启动服务器和 Web UI。
- `stellad service ...` — 安装、启动、停止、查看和卸载后台服务。
- `stellad upgrade` — 升级已安装的守护进程二进制。
- `stellad version` — 打印版本。
- `stellad postgres ...` — 管理内嵌 PostgreSQL runtime。
- `stellad vault keygen` — 生成引导用的 `STELLA_VAULT_KEY`。
- `stellad system-bundle ...` — 查看、安装和验证 builtin Skill bundle。

为服务端功能设计命令行为前，先读[CLI 与原生代理工具](../cli-as-client)和 [API 设计规则](./api-design)。

## 核心原则

1. **只做运维入口。** 只有引导、维护、迁移、服务管理和诊断才应该加入 `stellad`。产品功能属于 Web UI、HTTP API 和原生代理工具。
2. **默认给人读，需要时给机器读。** 默认输出应适合终端阅读；结构化输出放在 `--json` 后面。
3. **无聊胜过聪明。** 命令应该如其名。避免隐藏行为、隐式网络写入和惊喜默认值。
4. **可组合很重要。** 命令应能在脚本中使用：稳定退出码、诊断写 stderr、数据写 stdout，非交互场景不要突然提示输入。
5. **保护用户数据。** 破坏性命令必须表达明确意图并给出清晰错误；绝不静默删除、覆盖或吊销。

## 命令形状

使用项目模式：

```text
stellad <domain> <verb> [args] [flags]
```

示例：

```text
stellad vault keygen
stellad system-bundle verify
stellad service install --system
```

### 命名

- 顶层命令是守护进程领域或生命周期动词：`server`、`service`、`upgrade`、`version`、`postgres`、`vault`、`system-bundle`。
- 子命令使用动词，或在需要更深层级时使用资源名：`keygen`、`revision`、`install`、`verify`、`status`。
- 多词命令和 flag 使用小写短横线。
- 常见动作保持一致：

| 动作           | 使用                                   | 避免                               |
| -------------- | -------------------------------------- | ---------------------------------- |
| 显示多个资源   | `list`                                 | `ls`、`all`、`show-all`            |
| 显示单个资源   | `get` 或 `read`                        | `show`、`info`，除非已有约定       |
| 创建资源       | `create` 或领域内常用的 `add` / `save` | 随机混用 `new` 和 `create`         |
| 修改资源       | `update`                               | `modify`、`edit`，除非是交互式编辑 |
| 删除资源       | `delete` 或领域内常用的 `remove`       | `destroy`、`rm`                    |
| 停止运行中工作 | `cancel`                               | `kill`、`abort`                    |

保持现有命令名稳定。如果值得重命名，旧名称至少保留一个 release 的 alias，除非旧行为本身不安全。

### 参数与 flags

主要操作对象用位置参数：

```text
stellad upgrade 0.50.0
stellad postgres logs <instance-id>
```

修饰项、可选上下文、过滤条件和输出控制用 flags：

```text
stellad service install --system
stellad postgres status --json
```

规则：

- 必填位置参数必须出现在 `ArgsUsage`。
- 只有没有自然的位置参数形式时，才使用必填 flag。
- 布尔 flag 使用正向表达（`--follow`、`--json`、`--force`）。除非功能就是关闭默认行为，否则避免 `--no-cache` 这类否定 flag。
- 复用常见 flag 名：`--json`、`--force`、`--install-dir`、`--system`。
- 不要设计面向 sandbox 的 CLI 命令。代理能力必须以带服务端身份的原生工具交付，而不是 CLI flag 或 scoped bearer token。

## Help 文本

用户应能通过 `stellad help <command>` 或 `stellad <command> --help` 理解每个命令。

在 `urfave/cli` 里：

- `Name`：短小的小写命令 token。
- `Usage`：一句话，祈使句或名词短语，不加句号。
- `Description`：只在命令需要背景、示例或警告时使用。
- `ArgsUsage`：包含每个位置参数，例如 `<version>`。
- `Category`：顶层命令在有助于主帮助页时设置为 `System`、`Admin` 等。

好：

```go
Usage:    "Copy a legacy SQLite database into PostgreSQL",
ArgsUsage: "",
```

坏：

```go
Usage: "Does stuff with the thing",
```

Help 文本就是用户文档。命令名、usage 或重要 flag 改了，就更新用户文档。代理提示词和技能应指向原生工具，而不是 CLI 命令语法。

## 输出

### stdout 放数据

用户可能 pipe 给其他程序的内容写 stdout：

```bash
stellad version
```

人类可读表格和成功摘要也可以写 stdout。它们应足够稳定，别让轻量脚本用户吃苦，但不要把表格格式承诺成 API。

### stderr 放诊断

进度、警告、提示和错误写 stderr。这样 stdout 才适合 pipe 和命令替换。

```text
Downloading release archive...       # stderr
0.50.0                               # stdout
```

### JSON 输出

预期会被脚本使用的命令应支持 `--json`，除非它唯一输出本来就是原始标量或文件内容。

规则：

- stdout 只输出合法 JSON，不混入其他内容。
- 映射 API 数据时使用与 API 相同的 `snake_case` 字段名。
- 优先复用 API 响应形状，不要发明第二套 CLI schema。
- 为人类阅读 pretty-print 可以，但不要改变结构。
- 错误仍写 stderr 并返回非零退出码；失败时不要打印“成功 JSON envelope”。

### 表格

人类列表输出使用对齐列和简短表头。避免把大段文本塞进表格；长内容放到 `read`、`get` 或 `--json` 输出。

尽量把稳定标识放在第一列：

```text
ID        STATUS    TITLE
abc123    open      Fix scheduler retry
```

## 错误与退出码

错误应简短、具体、可行动：

```text
connect to Stella server: connection refused (start it with `stellad server`)
```

坏错误会泄漏实现细节或让用户猜：

```text
sql: no rows in result set
invalid input
```

退出码规则：

| 代码 | 含义                                         |
| ---- | -------------------------------------------- |
| 0    | 成功                                         |
| 1    | 预期失败：校验、未找到、服务器错误、认证失败 |
| 2    | CLI 框架能区分出的命令行用法错误             |

不要新增复杂退出码体系，除非有真实自动化场景需要。大多数时候一位失败信号就够了。

Go 中包装错误时，只加一次操作上下文：

```go
return fmt.Errorf("system-bundle install: %w", err)
```

不要每层都重复同一个名词，把消息包成闹鬼堆栈。

## 交互

只有当 stdin 是终端、且命令明显面向用户操作时，才允许交互行为。

规则：

- 提示输入前先检测非交互场景。
- 为脚本场景提供 flags，不要强制提示。
- 破坏性确认提示要展示将被改变的对象。
- `--force` 可以跳过确认，但不能扩大操作范围。
- 如果值可从标准环境变量读取，或命令就是生成引导输出，不要提示用户输入 secret。

## 破坏性命令

会删除、吊销、覆盖、取消、归档或发送外部可见内容的命令都算破坏性命令。

要求：

1. 命令名必须让动作明显：`delete`、`remove`、`cancel`、`revoke`、`send`。
2. 目标选择必须明确。避免对隐式“当前”资源做破坏性操作。
3. 批量破坏操作需要窄过滤条件加确认，或显式 `--force`。
4. 宽范围操作优先支持 dry-run。

不要让 `stellad <thing> sync` 删除远端数据，除非 help 文本和确认提示把这件事说清楚。惊喜删除是工具被卸载的捷径。

## 配置与环境变量

配置优先级保持一致：

```text
flag > environment variable > persisted config > default
```

环境变量影响行为时，在 help 文本中说明。常见变量包括：

| 变量                  | 用途                              |
| --------------------- | --------------------------------- |
| `STELLA_HOME`         | Stella 主目录                     |
| `STELLA_DATABASE_URL` | 外部 PostgreSQL 连接 URL          |
| `STELLA_VAULT_KEY`    | 守护进程保险库加密用的 age 私钥   |
| `LOG_LEVEL`           | CLI 日志详细程度（默认 `INFO`）   |
| `LOG_LEVEL_RIVER`     | River 任务队列日志（默认 `WARN`） |

除了 `stellad vault keygen` 这类明确生成 secret、且 secret 就是请求输出的命令，不要打印 secret。

## 服务访问

需要服务端状态的 `stellad` 命令应优先调用 `stellad server` 使用的同一内部服务层；如果命令刻意操作一个正在运行的服务器，再使用生成的 API client。不要在第二条命令路径里复制业务逻辑。

模式：

1. 从 args 和 flags 构建类型化请求。
2. 调用服务层或生成的 client。
3. 渲染响应。
4. 返回带命令上下文的错误。

如果命令需要服务器但服务器不可用，说明怎么修：

```text
connect to Stella server: connection refused (start it with `stellad server`)
```

## 日志与详细程度

- 正常成功命令应保持安静。
- 只有操作明显耗时时，进度才写 stderr。
- Debug 日志由 `LOG_LEVEL` 控制，且不能污染 stdout。
- 不要记录包含 secret、token、邮件内容或用户提示词的请求体，除非已明确脱敏。

## 兼容性

CLI 用户什么都会脚本化。命令名、flag 名、JSON 字段和退出行为都是兼容性表面。

- 新增 flag，而不是改变已有 flag 含义。
- 重命名命令时保留旧 alias。
- 如果用户可能 pipe 默认输出，就避免改变默认顺序。
- JSON 字段优先增量添加，不改变字段类型。
- 移除行为前检查文档、测试和已知调用方。

**发布前例外。** Stella 还没有稳定发布，所以没有外部脚本需要保护。在第一个 release 前，优先完全符合新设计，而不是保留兼容垫片：把命令改成正确形状，并直接删除 legacy alias。发布后再完整执行上面的兼容性规则。

## 实现检查清单

新增或修改 `stellad` 命令时：

1. 这真的是运维、引导或维护命令，而不是应该放进 Web UI、API 或原生代理工具的产品功能吗？
2. 命令是否遵循 `stellad <domain> <verb>` 和现有领域命名？
3. 主要目标是否是位置参数，修饰项是否是 flags？
4. `Usage`、`Description`、`ArgsUsage` 在 `stellad help` 中是否清楚？
5. stdout 是否只包含命令数据，诊断是否写 stderr？
6. 适合脚本的输出是否支持 `--json`？
7. 错误是否可行动，并带有有用的命令上下文？
8. 破坏性动作是否明确，并防止意外扩大范围？
9. 配置优先级是否遵循 `flag > env > config > default`？
10. 命令用法变化时，是否更新了文档和 `internal/agent/prompt/template/system_prompt.tmpl`？
11. 是否更新了 args、flags、输出和错误行为测试？
