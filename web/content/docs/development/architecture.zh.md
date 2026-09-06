---
title: 架构
---

> 本节面向为 Stella 贡献代码的开发者。

## 系统概述

stella 的结构是一组松耦合的包，在启动时组装在一起。系统支持多用户和多代理，消息路由按消息级别处理。核心流程：

1. **Web UI 或通道**（Telegram、Discord、QQ、Feishu 或微信）接收用户输入。
2. 通道**解析用户**（通过外部 ID + 平台进行 upsert）和**解析代理**（DM 默认、群组绑定或回退）。
3. **ServiceManager** 通过代理 ID 查找该代理的 `agent.Service`。
4. `agent.Service` 通过 `session.Registry` 解析 session intent。
5. `runtime.Runtime` 通过缓存的 **Runner** 执行这一轮。
6. **Runner** 调用 LLM provider，并在循环中执行工具。
7. 响应通过通道流回给用户。

```
Web UI / Channel (Telegram / Discord / QQ / Feishu / WeChat)
    |
    v
Resolve user  -->  Resolve agent
    |
    v
ServiceManager.GetService(agentID)  -->  agent.Service
    |                                      |
    |                                      +--> session.Registry
    |                                      |
    |                                      +--> runtime.Runtime --> Runner
    |                                                             |
    v                                                             v
Channel response stream                                      LLM Provider
```

会话键的作用域为每个代理：`{agentID}:{platform}:{userID}:{context}`，确保同一用户与不同代理对话时拥有独立的对话历史。session/runtime/memory 的设计规则见 [Agent 架构](/docs/development/agent-architecture)。

## 包布局

```
cmd/stellad/             入口点，服务器命令，服务组装
  store/               DBStore：领域包之上的组装层
internal/
  platform/            不知道 agent 存在的基础设施（见下）
    config/            Store 接口、DBStore（PostgreSQL）、Snapshot、类型
    home/              POSIX workspace 物化、所有者验证、删除 fence
    blob/              统一接口下的不透明字节存储（S3 兼容对象存储）
    observability/     进程级 OpenTelemetry tracer 与 logger provider
    cli/               stellad 命令装配：dotenv、日志级别
    diagnostic/        面向运维输出的敏感值脱敏渲染
    version/           构建版本，由 ldflags 注入
    xberg/             Stella 如何调用内置 Xberg CLI
  core/                任何 internal 包都可 import 的叶子内核（见下）
    access/            基于 authz.Authority 的 Agent 访问判定
    agentctx/          Agent/session context key
    agenterr/          共享哨兵错误
    providercred/      按 Agent 解析 provider 凭据
  agent/               Service、ServiceManager、session registry、runtime、runner 工厂
    session/           Session 生命周期、ownership、kind/channel policy
    runtime/           Runner cache、turn 执行、event 持久化
    prompt/            系统提示构建器和模板
    sandbox/           核心沙箱工具（bash、view_image）
    delegate/          内部 managed-session adapter 与 preset
    tracehook/         Agent trace hook：LLM、工具、记忆活动的 slog 与 OTel span
  channel/             Channel 接口、身份解析、斜杠命令、入口租约、通知
  memory/              记忆 provider 注册表 + 实现（lcm、simple）
  server/              HTTP API + 嵌入式 React SPA
  auth/                登录、会话与身份
  authz/               共享授权词汇（Authority、Action）
  controlplane/        控制面域（providers、settings、plugins、channels）
  plugin/              插件机制
    manifest/          manifest 声明的插件、mise runtime、override、reconcile
    host/              按能力限定的插件平台宿主与插件持久状态
  model/               跑哪个模型、花多少钱、怎么做 embedding
    catalog/           models.dev 快照、本地 override,以及 provider override + discovery 的有效模型合并
    usage/             每轮 token 与费用计量
    embedding/         embedding provider、索引、存储
  skill/               托管 Skill 权威、精确 revision、搜索与加载
    access/            谁可以看见或修改一个 skill
    policy/            按 Agent 的托管 skill 启用策略
  library/             文档库：原始存储、派生、检索
    recally/           基于同一套存储的稍后读与订阅后端
  db/                  PostgreSQL（pgx/v5）、goose 迁移、sqlc 查询、内嵌 runtime
  scheduler/           River 持久化调度服务（供 Web UI 和 Agent 原生工具使用）
  tools/               mise 任务调用的代码生成器（toolgen、catalog/二进制同步）；不链接进 stellad
pkg/
  ai/                  Message/Content 类型、Model、Provider 接口、流式事件
  tools/               Tool 接口与注册表
  toolmeta/            公开的生成工具元数据与封闭内建清单
  email/               公开 Email DTO 与 HTTP/plugin 调用契约
plugins/
  channels/            通道插件（telegram、discord、qq、feishu、weixin）
  providers/           类型化 provider definition + LLM 适配器（anthropic、openai、openai-response）
  sandbox/             沙箱后端实现
  email/               用户所有的 Email 实现、配置、传输与工具
```

依赖方向是单向的，由 `internal/boundary_test.go` 从两边守住。`pkg/` 是面向插件的契约层，永远不 import `internal/`。Channel 插件通过 `pkg/channel` 和 `pkg/plugins` 契约作为适配器存在，因此 `plugins/` 下所有生产代码只能 import `pkg/**` 和其他插件实现包。`internal/` 下的生产代码不能 import 具体的非 channel 插件；现有装配路径仍可能依赖 channel 集成。`cmd/stellad` 是同时认识双方的装配根，在这里把实现接到契约上。`internal/platform/**` 是基础设施地基：只能 import 标准库、第三方模块、`pkg/**` 和其他 `internal/platform/**`，因此没有任何 platform 包能反向依赖领域包（`_test.go` 额外允许 `internal/db/dbtest` 这个测试夹具）。`internal/core/**` 是内核：在 platform 白名单之上再加其他 `internal/core/**` 和 `internal/authz`。`internal/db` 刻意不放进 `platform`，它实现了 `internal/auth` 的 store，依赖领域包。哪个包属于哪一层，见 [Go 模式](/docs/development/rules/go-patterns)。

## 配置

配置存储在 PostgreSQL 中，通过 `config.Store` 接口访问。没有 YAML 配置文件；所有设置（提供商、代理、通道、调度器）都通过 admin API 或数据库管理。

- **Store**（`config.Store`）-- 用于读写提供商、代理、通道、用户和聊天-代理绑定的接口。由 `DBStore` 实现。
- **DBStore**（`config.DBStore`）-- 使用 sqlc 生成的查询的 PostgreSQL 支持实现。
- **Snapshot**（`config.Snapshot`）-- 单个代理的只读配置视图。在池创建时从 Store 组装。包含已解析的提供商凭证、模型名称、工作区路径、系统提示和 runner 设置。传递给 runner 工厂和需要每个代理配置的工具。

## Home 持久化与生命周期

`internal/platform/home.WorkspaceManager` 是单个 POSIX `STELLA_HOME` 下唯一的生产物化器。PostgreSQL user、group 与 Agent row 授权确定性本地路径；文件系统拥有布局和字节。owner 存活时会创建缺失 workspace；symlink、非目录、不安全 ID 或可信根被替换时会 fail closed。Phase 1 不存在 PostgreSQL Home catalog。

显式破坏性删除 user、group 或 Agent 时，会先 fence 本地缓存执行，再在既有数据库事务中删除 owner。物理字节和 inode 保留，但 owner 校验阻止后续 workspace 访问。`agents/{id}` 的任意文件系统条目都会保留该全局 Agent ID。移除分配、移除成员、归档 Session 和卸载 Helm 不删除 workspace 字节。这是可信宿主、单副本边界；多副本、Kubernetes 与 S3 authority 需要未来设计。

## 组合与生命周期

`cmd/stellad` 是唯一的手动组合根。没有 DI 框架，也没有通用 `Lifecycle` 接口——各子系统在同一处显式构造和装配，使布线可审计。启动按严格阶段进行，每一阶段必须先于下一阶段完成：

1. **启动配置** — `serverAction` 在启动边界一次性解析 `config.LoadServerConfig(os.LookupEnv)` 与 `oidc.LoadLoginConfig(os.LookupEnv, baseURL)`。其它包一律不读环境变量（由测试三线闸强制，仅对 `STELLA_HOME`/OTel/运行时透传保留小白名单）。最终 base URL 在此解析并向下传递，共享服务直接用它构造——绝不用 `localhost` 占位符再事后改写。
2. **Build（构建）** — `setup()` 一次性构造每个子系统。共享的 credentials/email/share/recally/MCP 服务只建一次（每个域通过 `*ForPool` 构造子自持查询集），因此同一实例同时支撑 agent 工具与 HTTP 端点。
3. **Bind（绑定）** — 真正的反向边用一次性的预启动绑定闭合，拒绝 nil/重复/迟到绑定：PoolManager 的 `BindVaultEnvLoader`/`BindMCPToolProvider`/`BindOAuthRegistry`（在 `StartAll` 之前）、scheduler/goal/embedding/session-media 服务上共享 River 客户端的 `BindRiverClient`，以及 `AddBuiltinTool`（去重，由 `StartAll` 密封）。普通依赖走构造注入，不走绑定。
4. **Validate / Seal（校验/密封）** — `pluginhost.Seal()` 校验全部静态注册与能力绑定后拒绝进一步静态注册；动态期望态接口（`ApplyChannel`/`RegisterManifestPlugins`）保持开放。admin 服务由不可变、已校验的 `server.Deps` 经 `server.New(ctx, deps)` 构建，缺任一必需依赖即快速失败。`server.New` 不读环境、不构造服务、无 setter。
5. **可观测性** — 全局 OTel 追踪在服务阶段之前初始化，因此任何产生 span 的组件（经 HTTP/通道入口的 agent 运行）都不会在 exporter 装好之前启动。
6. **Run（运行）** — 至此组合根才启动入口，且必须在其依赖的所有后端就绪之后。先接好静态回调（`notifier.SetAuthService`、scheduler 的 `OnJob` 处理器——均为互斥保护的一次性写入），并启动包含 scheduler、goal、embedding worker 的唯一共享 River client，再启动 scheduler、goal 调度 tick、embedding backfill；scheduler 处理器在 River 启动**之前**接好，因为 River 一启动就可能处理已持久化的作业。之后入口才上线——group 调度循环、受管通道运行时，最后是 `httpSrv.Serve`（监听器提前绑定但不 serve）。组合根持有单个 `errgroup`：`httpSrv.Serve` 与 `groupDispatcher.Run(ingressCtx)` 在其下运行。预期的关停错误归一为 `nil`（`http.ErrServerClosed`、`context.Canceled`）；任何其它组件错误取消同伴并成为根错误。组件构造子不启动 goroutine——后台循环由显式阻塞式 `Run(ctx)` 或组合根拥有的 `Start` 进入（例如 trace hook 的空闲会话回收器）。

**不可变 Server Deps。** `server.Deps` 是由应用服务组成的值结构体：Account、Profile、Project、Inbox、Agent/Session/Skill access、Group、控制面和共享能力服务。`internal/server` 不持有持久化 store、查询句柄或连接池；唯一带数据库形状的依赖是仅用于存活探针的 `DBPinger`。终态 AST 守卫拒绝宽泛 `Deps` 字段、Server 持久化选择器以及 `sqlc`/`pgxpool` 导入；其反例覆盖嵌套字段、别名、无导入的 handler 查询使用、仅 DTO 导入和 dot import。可选能力容忍 nil，并通过单一集中的 503 映射退化。

**授权。** Agent 的 HTTP、webhook 和 channel 入口统一使用权威的 `internal/core/access` 域服务。Session 与 Workspace 用例使用 `internal/agent/session/access`：它先加载持久化的 owner、agent、kind 和生命周期事实，再创建带作用域的 registry 访问，并基于不可变的 `authz.Authority` 按其自身的静态规则决定 Agent、Session 和 Workspace；原先的 RBAC/ABAC 策略引擎与临时的通用策略引擎均已移除，不再有独立的中心化决策路径。Authority 只能由可信身份适配器（`internal/auth`、`internal/credential`、`internal/authz`）以及 `internal/core/access` 中的 durable worker/group 适配器铸造；请求 body/path 字段永远不能铸造或覆写 actor。

执行域采用相同形态：Account、Profile、Project、Inbox、Group、Workflow、Scheduler、Goal 和 Skills 均暴露自持用例的应用服务，并向传输层返回领域值，绝不返回生成的 API 类型。每个 HTTP、channel、tool 和 worker 用例都先通过该服务绑定一个不可变 Authority，再加载或变更受保护资源；传输层不会为了可选认证预加载资源。跨资源的 agent 门禁通过用同一个 Authority 直接调用 `agentaccess` 折叠进来。持久化 worker 从持久可信状态重建 owner/executor Authority，并在每次动作时重新决策。`admin` 是每个域通过 `Authority.IsAdmin()` 认可的超级用户，而非散落的 `role == admin` 检查。

用户能力域均为用户所有——被委派的 agent 回合以其用户的访问权限行事，而非受 executor 限制（agent 共享用户的密钥、邮件、连接与阅读库）——但按形态分为两类。**Vault 自持 scope 规则**：`vault.Service` 每个用例绑定 Authority 并据其自身静态规则决定，因为 vault 条目具有真实的 `user`/`user_agent`/`system`/`system_agent` scope 区分（`user`/`user_agent` 归用户所有并折叠 agent-read 门禁；管理员管理的 `system`/`system_agent` 仅管理员可达）。它保留静态加密、不回读密文、保留名保护与 runner 失效。**Connections、Email、Share、Recally 是粗粒度能力**：它们各是没有 scope 或管理员区分的按用户能力，因此 `connections.Service`、`share.Service` 与 `recally.Service` 绑定一个可信的 `authz.Authority` 并通过按用户限定的持久查询强制归属。`plugins/email` 负责实现，并在 `internal/plugin/host` 的可信 context adapter 中解析用户；`pkg/email` 只承载共享 DTO 与 `Service.Access(context.Context)`，HTTP 安装已验证的 Authority，工具保留 `ToolIdentity`/`ToAuthority` 及现有错误映射。Email 只接收组合根提供的该用户 `EMAIL_CONFIG` 读取器，Vault 仍是存储和宿主侧授权边界。OAuth bundle 与 flow 以用户为键，share 以 `WHERE user_id = ?` 删除，recally 行按 uid 限定故外来行直接不存在。仅以父 id 为键的操作（recally 文章正文、feed entry）先做一次 uid 限定的父加载以证明父归属，Share 工件对 agent 作用域的行为方保留 os.Root 工作区限制。有若干面刻意保持可信或公开：vault 的宿主侧调用方（MCP、OAuth、邮箱配置、频道配置、沙箱环境、密钥发放）使用原始服务方法；OAuth 回调与令牌刷新路径以 flow/user 为键，而非实时请求；公开分享视图是一个不可猜测的能力 URL，仅凭 token 哈希与过期时间授权，无需会话。参见[授权指南](/docs/development/authorization)了解资源矩阵与配方。

控制面各域收尾闭环。Provider、Settings、Plugin 与 Channel 管理经由 `internal/controlplane`——一个 `Begin(authority) → Access` 的域，取代旧的 `requireAdmin` 门禁加直接访问 `config.Store` 的做法。它们仅限管理员：`Begin` 校验 Authority 并要求一次 `IsAdmin()`，因此 Access 只为管理员而存在。Channel 管理是一个能力：一次决策同时覆盖通道持久化及其 channel plugin 的启用和应用，而不是再做一次 Plugin 决策。插件宿主也按能力限定：`pluginhost` 的 `Platform` 不再是环境式（ambient）；只有静态 Go `PluginInfo.RequiredCapabilities` 声明可访问宿主端口，并且启动托管运行时前会校验其注入支撑。manifest 插件可以描述插件特征，但不能请求宿主端口。至此，旧的 `auth.PolicyEngine` 与临时的通用策略引擎均已移除：授权由 `internal/authz`（共享的 `Authority`/`Action` 词汇）加各域自持的静态规则承担。

**静态 vs 动态。** 启动静态能力在启动前绑定一次并随后密封。热重配（插件工具/钩子/提供商重载、agent 同步、runner 失效）是独立接口，启动后仍可用并原子应用——绝不重跑一次性绑定。

**关停顺序。** 首个 `SIGINT`/`SIGTERM` 启动优雅排空（第二个坍缩为硬停）。`drainSequence` 依次：标记 `/readyz` 不就绪并通知 SSE 流 → **停止每一个非 HTTP 入口源**（group 调度受理、通道 bot 轮询、以及 scheduler/goal/embedding/session-media 的 River 周期任务与一次性派发），各由幂等的 stop-once 闭包完成，故排空开始后不再有新工作或周期触发 → 在 `STELLA_HTTP_SHUTDOWN_TIMEOUT` 内排空在途 HTTP（超时强制关闭）→ 在同一预算内等待不持有 HTTP 连接的已接受 agent 轮次（通道消息、webhook 运行、scheduler 立即运行）完成 → 取消工作上下文，随后 River 在软停预算内排空其在途作业，LIFO defer 链逆序 Close 各子系统。主 goroutine 在其拆除 defer 运行前会 join 排空监督者，因此进程退出绝不与排空竞态；第二个信号坍缩共享预算并硬停。group 调度循环跑在独立的 `ingressCtx`（errgroup 上下文的子上下文）上，故可在不取消工作上下文的情况下被叫停；出站依赖（池、notifier）在最终取消前保持存活，故排空前已接受的工作仍能完成并投递。同一批 stop-once 闭包同时支撑 `stopIngress` 与逆序 defer 清理，故崩溃/启动错误路径也能安全拆除、不重复停止。子系统崩溃取消 errgroup 并在无就绪排空的情况下拆除。

## 多用户多代理路由

每条传入消息在到达代理循环之前都要经过两步解析：

1. **用户解析**（`channel.ResolveUser`）-- 通过外部平台 ID 对发送者进行 upsert，返回带有稳定内部用户 ID 的 `config.User` 记录。
2. **代理解析**（`channel.ResolveAgent`）-- 确定哪个代理处理此消息：
   - 在 DM 中，使用用户的 `default_agent_id`。
   - 在群聊中，`chat_agents` 绑定将 `(platform, chat_id)` 映射到代理。
   - 如果两者都未设置，则使用第一个启用的代理作为回退。

已解析的用户和代理被打包到 `ResolvedChat` 结构中，该结构贯穿所有处理器和命令路径。此结构包含目标 `Service`、`User`、`AgentID` 和 `SessionKey`。

`ServiceManager`（由 `PoolManager` 实现）维护 `map[agentID]*Service` 并在首次访问时延迟创建。每个 Service 通过 runner 工厂使用其代理的 `Snapshot`（模型、凭证、工作区、系统提示）进行配置。

### Agent 路由

渠道绑定 agent 时使用该专用 agent。否则，私聊使用用户的默认 agent，并在未配置时回退到第一个启用的 agent。每条群消息会唤醒所有合格成员 agent，再由每个成员的本地确定性 triage 决定是否发言。

## 提供商

LLM provider 使用类型化的编译内置 registry。Stella 内置三种 provider：

| 提供商            | API                  | 使用场景                                      |
| ----------------- | -------------------- | --------------------------------------------- |
| `anthropic`       | Messages API         | Claude 模型                                   |
| `openai`          | Chat Completions API | GPT 模型                                      |
| `openai-response` | Responses API        | OpenAI 兼容服务（Perplexity、Together.ai 等） |

每个提供商都实现 `ai.ProviderAdapter` 接口以进行流式响应，并可选实现 `ai.ModelLister` 以进行模型发现。提供商适配器可以把 `ImageContent` 编码成各自的原生图像格式（Anthropic 的 base64 块、OpenAI 的 data URI `image_url`），但 agent 边界只会在模型声明支持图片输入且图片处于引入它的当前回合时创建它。历史图片以文字基线的形式到达适配器。

Provider 位于 `plugins/providers/`，并导出 `providers.Definition`。`cmd/stellad` 中的组合根显式列出这些 definitions、校验 registry，再把它注入 runner 与控制面。因此新增 provider 既要创建对应包，也要在组合根有意接线。Provider 包不得 import `internal/**`。详见[插件系统](/docs/development/plugin-system)。

提供商管理（与设置、插件、通道一样）是一项控制面操作，直接经由 `internal/controlplane` 授权，而非裸角色检查。它仅限管理员：`Begin` 在铸造 Access 前要求 `IsAdmin()`。

### Agent 提供商凭证覆盖

提供商元数据与默认密钥仍是全局控制面状态。独立的 `agent_provider_credential` 关系为 `(agent_id, canonical provider_id)` 存储加密覆盖密钥。`providercred.Service` 是唯一接触明文的加密边界，并使用 Vault system cipher；`config.Agent` 与普通 Agent 投影都不包含凭证字段。

凭证感知的 Snapshot loader 在 Agent 边界装饰全局 Snapshot。它只替换目标提供商全部 canonical 与 type-alias 条目的 API 密钥，并同步旧版默认凭证字段。提供商类型、base URL、模型目录和启用状态绝不进入 Agent scope。缺少或删除覆盖行时使用全局密钥；已引用但无法解密的行会 fail closed，不会静默回退。所有宿主侧 Agent 消费者共用这一 loader，包括 Runner、memory summarization、intent 与 semantic routing、Reflect 和 Vision。

安全元数据遵循 Agent Read 权限。变更要求 Agent Manage，因此管理员与持久化的 Agent 创建者可以操作，被分配但不是创建者的用户不能操作。变更先提交，再定向调用 `SyncAgent`；它不会重载全局提供商。只写 HTTP 子资源提供分页 List、Get、PATCH 轮换和幂等 DELETE 回退。

## 工具

Runner 将工具注入 LLM 调用。工具遵循定义在 `pkg/tools/` 中的通用接口。`tools.Definition` 类型是 `ai.ToolDefinition` 的类型别名，使领域包保持解耦：

```go
type Tool interface {
    Definition() tools.Definition
    Execute(ctx context.Context, args map[string]any) (string, error)
}
```

### 核心沙箱工具

| 工具         | 可用条件 | 描述                                             |
| ------------ | -------- | ------------------------------------------------ |
| `bash`       | 始终     | 执行 shell 命令，包括文本文件读取与编辑          |
| `view_image` | 始终     | 根据父模型路由到图片像素、不可信文字或可行动错误 |

公开网页不是工具：`web` skill 的脚本（搜索、抓取、site scripts）通过 `bash` 在沙箱内运行，见[网页研究](/docs/guides/web-research)。

核心本地工作区工具通过 Docker 沙箱后端运行。`bash` 通过 `Session.Exec` 执行，是通用文件操作工具；其描述承载了原先由 read/write/edit schema 编码的契约。`view_image` 使用进程可见路径与中介的 `Session.Files` capability，并根据当前父模型路由：支持图片的父模型收到经过校验的像素，其他父模型收到视觉服务或通用基线生成的不可信文字，或可行动错误。Provider backing path 不会进入工具层。Runner 启动时如果 Docker 不可用则失败关闭。

### 沙箱

沙箱系统为 agent 工具执行提供进程、文件系统和网络隔离。所有核心工具在每个 runner 中共享同一个 `sandbox.Session`：`bash` 使用 `Session.Exec`；`view_image` 使用 `Session.Files`。公开 policy 只包含进程可见 root；物理 mount 映射和 rooted file capability 由各 backend 持有。具体后端位于 `plugins/sandbox/`，实现公开 sandbox 接口，并由 `cmd/stellad` 适配成经过校验的 registry；`internal/agent/sandbox` 只从注入的 registry 中选择。所选后端不可用时 runner 启动失败关闭。详见[沙箱后端抽象](/docs/development/sandbox)了解完整的 Session 接口、执行中介、拒绝失败行为和例外边界。

沙箱工具（`bash`、`view_image`）位于 `internal/agent/sandbox/`；公开网页研究是一个 skill 而非工具包：`plugins/tools/bun/skills/web/` 提供 `web` skill（`web.ts` 的 search/fetch 与 site scripts），`cmd/stellad` 将内置工具注册到目录。声明式 CLI 集成使用内建 manifest。扩展边界详见[插件系统](/docs/development/plugin-system)。

### Session 工具

面向模型的 `session_*` 工具统一负责 Session 管理、有界检查、创建和同步通信。内容回忆归 `memory` 工具负责：

- `session_list` 列出最近、活跃或已归档的 Session 卡片，不做语义搜索。
- `session_get` 返回 metadata 与 context stats，并按完整逻辑回合分页。
- `session_create` 打开持久的聚焦 Session，并可应用内部 preset。
- `session_send` 在当前 Agent 拥有且可发送的 Session 上运行一个回合，包括旧 delegate Session。

runtime 保留 `DelegateTool` 和 delegate Session kind，作为 preset 和已有 ID 的内部兼容机制。模型工具注册表不再注册 `delegate`。

Agent 发送会先持久化一行输入，再进入进程内按 Session 划分的 FIFO，最后经过标准 runtime admission guard。队列限制等待深度和 admission 等待时间，传播来源 context，但不取代 runtime 正确性 guard。LCM 会在追加 transcript 消息的同一事务中认领该行。启动恢复会重新鉴权 pending 行并只追加消息；它绝不会启动模型或工具 turn。嵌套调用在 context 中携带深度和祖先链，拒绝循环，继承根 deadline，并在同一根回合的同级与嵌套调用间共享 16 次调用预算。Agent 输入会持久化 actor 和来源 Session；prompt 渲染把它标记为信息，而不是用户权威。同步 Session 回合不会隐式发布到外部渠道，inbox 持久化也不会让回复或执行本身变得持久。

### 内置共享工具

| 工具               | 条件                  | 描述                                                                            |
| ------------------ | --------------------- | ------------------------------------------------------------------------------- |
| `memory`           | 始终                  | 跨对话与持久记忆的统一搜索和读取                                                |
| `session_*`        | 一对一 Agent 会话     | Session 列表、有界检查、创建和同步发送                                          |
| `skill_*`          | 始终                  | 搜索已安装 Skill，并加载选中的精确 revision                                     |
| `scheduler__job_*` | Scheduler 插件启用    | 安排任务：每个 action 一个工具（`scheduler__job_create`、`_list`、`_pause` 等） |
| `notify`           | 网关模式 + 通道已配置 | 通过分发器发送通知                                                              |

记忆是两个工具共享一个 `memory.Recall`：`memory_search` 联合检索当前快照可见的 LCM 消息/摘要和持久 facts、profile、soul、constraints；`memory_read` 解析 opaque result ref 或 well-known 的身份、约束、历史 ref。Dynamic read 会重新经过 Session access 授权；summary read 则通过有界 child ref 保留 LCM describe/expand 能力。对话统计、整条消息读取，以及持久 profile、soul、constraint 管理都不再是工具——它们归负责各自授权的 internal、Reflect 或 manual surface。

## 会话生命周期

1. 通道解析用户和代理，产生 `ResolvedChat`
2. 调用 `ResolvedChat.Chat(ctx, message)` -- message 是 `string`（文本）或 `[]ContentBlock`（多模态）
3. `Service.Chat` 通过 `session.Registry` 使用作用域键解析或创建会话
4. `runtime.Runtime` 获取或为会话创建 runner，使用代理的 Snapshot 配置
5. Runner 通过通道流回事件
6. 空闲超时后，runner 被回收；会话通过 `memory.Provider` 持久化到 PostgreSQL

有关历史管理，请参阅[会话压缩](/docs/development/session-compaction)。

## 通道接口

所有消息平台都实现 `channel.Channel` 接口：

```go
type Channel interface {
    Name() string
    Start(ctx context.Context) error
    Stop()
    Notify(ctx context.Context, n Notification) error
}
```

共享命令逻辑（`/new`、`/compact`、`/abort`）位于通道协调层，每个通道委托给它以处理核心逻辑；适配器可以提供平台专用的帮助和身份命令。`/new` 将聊天轮换到一个新会话——此前的会话被归档而非删除——并作为控制操作进入与聊天轮次相同的按会话队列，因此不会与进行中的轮次竞争。`/new` 仅适用于私聊：群聊上下文是共享的，因此群里的 `/new` 会在写入共享事件日志之前被拒绝，这样被拒绝的命令也不会进入任何 agent 的上下文。聊天轮次按解析的 Stella 会话进行序列化，因此重叠的通道消息不会竞争相同的会话历史；`/abort` 取消该会话当前正在运行的轮次。

### 通道入口归属

Stella 只支持一个服务副本（[#637](https://github.com/CherryHQ/stella/issues/637)）。Helm chart 强制 `replicaCount: 1` 和 `Recreate` 发布策略，因此托管通道机器人会在依赖完成装配后无条件启动。针对同一份通道配置运行两个 `stellad` 进程不受支持：Telegram 可能返回 409，Discord、QQ、Feishu 和微信可能重复投递。多副本通道入口需要完整的 offset 与 fencing 设计；单独的数据库租约并不能解决它。

优雅排空时，`pluginHost.Quiesce` 停止新的通道轮询，同时保留已接受的工作和通知发送器。最终的 `pluginHost.Stop` 只在 River 排空后执行，确保已接受工作仍可向外投递。

投递保证如实陈述：

- **群消息至少一次。** 入口由 event-log 去重并持久化，派发使用 CAS 租约认领的 durable outbox；分发器丢失后另一个可接管。租约过期且部分发送后重投时，平台侧可能收到重复消息。
- **内联 DM 回合至多一次。** 没有持久队列；进程崩溃中断的回合会丢失而不是重试。
- **通道入口不宣称也未实现 exactly-once。**

## Admin API

`internal/server/` 包提供用于管理系统的 HTTP API 和嵌入式 SPA。它将 HTTP 转为应用服务调用和 API DTO；持久化与查询句柄不进入该包。控制面管理——LLM 提供商、部署设置、插件与通道——直接经由 `internal/controlplane` 授权（仅限管理员：`Begin` 要求 `IsAdmin()`），而非裸角色检查。

## 通知流程

```
Agent notify tool      --> Dispatcher --> Channel (Telegram/Discord/QQ/Feishu/WeChat)
Scheduler job result   --> Dispatcher --> Channel (Telegram/Discord/QQ/Feishu/WeChat)
```

分发器在设置早期创建，但后端在网关服务启动时稍后注册。ServiceManager 通过 `BuiltinToolsFactory` 按代理注入通知工具，把通知保留在始终启用的内建工具集合中，而外部工具继续由插件管理。
