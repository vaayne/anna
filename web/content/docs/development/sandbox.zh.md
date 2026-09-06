---
title: 沙箱后端抽象
---

> 本节面向为 Stella 贡献代码的开发者。选择和配置沙箱后端请参阅[沙箱指南](/docs/guides/sandbox)。

## 核心模型

沙箱抽象的目的是使 runner 代码、插件配置和工具执行不依赖于具体的后端类型。执行总是通过 runner 选中的活动后端进行。

- `pkg/sandbox.Policy` — 不可变的、后端无关的执行策略（进程可见的文件系统根目录、工作目录、网络模式、环境变量、超时）
- `pkg/sandbox.Session` — 每次运行的执行边界和生命周期所有者；将生命周期和宿主机访问合并为单一接口
- `pkg/sandbox.FileAccess` — 由 `Session.Files` 返回的中介文件 capability；调用方与命令使用同一套进程可见坐标，且永远不会获得 provider backing path

后端标识保留在 runner 和面向 runner 的 sandbox 包内部。插件包不导入 `internal/agent/sandbox`。

## Session 接口

`pkg/sandbox.Session` 暴露 8 个方法：

| 方法                                                       | 描述                                     |
| ---------------------------------------------------------- | ---------------------------------------- |
| `Policy() Policy`                                          | 返回会话创建时使用的不可变策略           |
| `Exec(ctx, command, ExecOptions) (ExecResult, error)`      | 运行命令并等待结果                       |
| `StartProcess(ctx, ProcessRequest) (ProcessHandle, error)` | 启动带 stdio 句柄的长期运行进程          |
| `Files() FileAccess`                                       | 返回对已授权进程可见数据 root 的中介访问 |
| `WorkingDir() string`                                      | 返回沙箱内的逻辑工作目录                 |
| `Close() error`                                            | 关闭会话并释放资源                       |
| `Alive() bool`                                             | 报告会话是否仍然活跃                     |
| `Done() <-chan struct{}`                                   | 会话终止时关闭的 channel                 |

`FileAccess` 提供 prompt 构建与核心 `view_image` 工具所需的有界操作，以及 managed Skill 在发布时精确、no-replace、disposable 的文件投影。路径相对于 `WorkingDir`，或使用进程视图中的绝对路径。公开的 `Policy`、`Session` 与 `FileAccess` contract 不包含宿主机 mount source、路径 resolver 或路径转换结果。

每个 backend 都在 provider 内部把公开的进程 root 绑定到物理 mount plan。文件操作使用 Session 创建时固定的目录 capability，执行只读 root 约束，并对逃逸或跨 mount symlink fail closed。Provider 的进程准备代码可以读取自己的私有映射，但上层无法先取得物理路径，再用 `os.*` 绕过 capability。

## 本地 workspace 所有权

Phase 1 仅支持一个副本和一个可信 POSIX `STELLA_HOME`。PostgreSQL owner row 是身份和授权 authority；`STELLA_HOME` 下的确定性路径是布局与字节 authority。`internal/platform/home.WorkspaceManager` 是唯一生产物化器：只有确认 user、group 和 Agent owner 存活后才创建缺失目录，并拒绝 symlink、非目录、不安全 ID 和可信根替换。原始 ID 相同的用户和群组使用不同路径。

用户或群组运行使用已授权 `WorkspaceView` 返回的精确 `AgentRoot` 和 `DataRoot`。隔离型 backend 会把这些 root 以读写方式挂载；显式选择的 `none` backend 仍是 trusted-host execution，不提供进程级文件系统隔离。无用户运行保持 disposable scratch 语义，不获得 principal mount。群组 Agent Home 的 Skill materialization 不含 user 或 `user_agent` scope：它不会把群组数据变成某个用户的 `user_agent` Skill。

在 Session 执行之外，`WorkspaceManager.OpenRoot` 生成有 scope 的只读或读写操作 capability。类型化 root component 以 no-follow traversal 物化；操作使用 inode-pinned `os.Root`，因此 root 内的相对 symlink 可用，绝对或逃逸 symlink 会 fail closed。这不是 `Session` filesystem transport：Stella 不提供 `stella-fs` 或 Docker exec filesystem RPC。下游文件 consumer 将由后续变更分别迁移。

显式破坏性删除 user、group 或 Agent 时，会先 fence 本地执行，再删除数据库 owner。文件和 inode 保留，但后续 `WorkspaceView` 因 owner 不存在而失败。`agents/{id}` 的任意文件系统条目都会保留全局 Agent ID。这些保证仅适用于可信宿主和单副本。多副本部署保持相同应用模型，但还需要一个强一致 shared POSIX namespace 与 PostgreSQL generation/lease fencing；S3 不是 live Workspace authority。

## 当前架构

### 会话所有权

runner 为每次运行创建一个 `sandbox.Session` 并持有其生命周期所有权。当没有可用的活动沙箱会话时，runner 构建失败。

### 后端解析

runner 会从 `STELLA_SANDBOX_BACKEND` 解析部署时后端，并通过注入的已编译后端 registry 分派。生产部署使用 `docker`、`local` 或 `none`；Harbor 评测 harness 还会接入仅供评测使用的 `bridge` 后端。

### 执行时中介

所有必须遵守沙箱策略的本地执行路径都通过活动 runner 会话进行中介：

- 核心 `bash` 工具通过 runner 拥有的会话使用 `Session.Exec`
- 核心 `view_image` 工具与活动 prompt context 读取使用 `Session.Files`
- managed Skill revision 通过 `FileAccess.ProjectFiles` 复制到精确、no-replace 的 Session 投影；已存在但内容冲突的 tree 会 fail closed
- 插件工具接收 `ToolContext.Runtime`，这是活动会话上的 `pkg/plugins.ToolRuntime` 适配器
- 技能和代理预设加载在代理会话内运行时使用 `ToolRuntime`

读取文件的核心工具每次调用只选择一个 `FileView`。其中的策略环境、工作目录与 `FileAccess` 来自同一个 resilient generation，因此路径展开不会在中途静默切换 backing tree。跨越该边界的 provider 错误只标识逻辑进程 mount，不暴露物理 source path。

managed Skill 投影会原子发布，并在每次 load 时校验，但它不是针对同一用户身份运行命令的独立隔离边界。此类命令可能与校验并发，或在校验后修改 disposable tree。只要 load 观察到不一致，就会 fail closed，而不会替换该路径。Session 关闭时会删除其临时 backing；Docker 启动清理还会移除被中断 Session 遗留的临时目录。

### 长期运行进程

`Session.StartProcess` 可供后端拥有的长期运行进程使用。MCP 插件连接仍是远程 HTTP
传输；这个接口不会增加另一条本地 MCP 执行路径。

### 非 runner 文件系统访问

某些代码路径需要在没有已注入运行时的情况下访问本地文件系统，例如活动代理运行之外的提示渲染或元数据发现。

runner 外的项目提示上下文与项目级 Skill 读取会解析精确的用户、Agent 与项目，打开只读 Agent Home capability，复制有界逻辑内容，并在提示或 Skill 处理前关闭 capability。逻辑项目 `base_dir` 不会被当作进程工作目录。其他可信的非项目元数据发现仍可使用 local runtime。这些是有意为之的非 runner 路径，而不是沙箱化工具执行的回退。

### 显式例外边界

远程 MCP HTTP/SSE/StreamableHTTP 传输目前被视为独立的信任边界。

- 远程传输拨号目前**不**由 `ToolRuntime` 中介
- 此例外被显式跟踪为 `EX-009`，并记录为 `runtime.exception_path`

## 拒绝失败行为

Stella 优先选择显式拒绝而非静默降级：

- 会话创建时 Docker 不可用 → runner 启动失败
- 不支持的策略 → `PolicyCompatibilityError`，runner 启动失败
- 直接非中介的插件 exec → 拒绝失败
- 远程 MCP HTTP/SSE/StreamableHTTP → 显式例外，而非隐式沙箱绕过

## 验证

该抽象由以下测试覆盖：

- 会话/宿主机契约测试
- 策略兼容性测试
- 核心工具一致性测试
- Docker 后端集成测试
- 已迁移运行时路径的静态绕过回归保护

## 本地运行 Docker 后端

`mise run dev:docker` 一条命令拉起整套栈，对齐生产的 `docker-compose.yml`：`stellad` 跑在**容器内**，docker 沙箱后端走 **volume 模式**（`STELLA_SANDBOX_BACKEND=docker`、`STELLA_DOCKER_SANDBOX_MODE=volume`、`STELLA_HOME_VOLUME=stella-data`），外加一个 `otel-lgtm` 边车。它会构建本地镜像（`docker:build` → `stella:latest`、`sandbox:docker:build` → `stella-sandbox:dev`）、按需新建命名卷，并确保 `~/.stella-dev/.env` 里有 dev vault key。它跑的是和 prod 同一份 `docker-compose.yml`，只是导出 `STELLA_IMAGE=stella:latest`，从而用本地构建而非发布镜像。

容器内 Go 服务器在 `localhost:25688` 提供其烤进镜像的内嵌 SPA（见 `web/embed.go`），Grafana 在 `localhost:13413`。

用 `docker compose down` 停掉整套栈。

sandbox 镜像包含发行版自带的 mise 工具链和 builtin CLI 文件。运行时从
`system`、`system_agent`、`user`、`user_agent` 四层解析一份插件 snapshot，再由
selection helper 只物化选中的条目。Docker preparation 按一个解析后的 image ID 和完整
选择身份做缓存键。Native managed 安装使用 managed tree；user 和 user-agent 安装留在
自己的沙箱目录，并在 `PATH` 中优先。不会使用宿主机 `_builtin.toml`、manifest 权限面，
也不会把宿主机平台的安装作为 Docker 回退。

## builtin Skill bundle 与投影

`resources.Registry` 是发行版自带 core Skill 的唯一权威。它产出不可变、内容寻址的
bundle，供原生 `local` 和 `none` 执行安装到 `$STELLA_HOME/bundles/<revision>`。隔离执行
将这一精确 bundle 以只读方式投影到 `/opt/stella/skills/builtin`；`/opt` 是执行坐标而非
另一份权威，bundle 中辅助可执行文件的模式必须在投影中保留。Plugin Skill 由
PluginDefinition 与选中的 PluginConfig 拥有，同一份四层范围决策控制其暴露。任何 builtin
插件都可以通过配置禁用。

Project Skill 仍是持久 Agent/项目工作树中的普通文件；在活动执行之外通过有界、只读的 Home snapshot 读取。可变 `system`、`system_agent`、`user` 和 `user_agent` identity 仍登记在 PostgreSQL 中，其当前选中 revision 的 manifest 与 bytes 则以持久 Home storage 为权威。活动 Session 只获得 disposable、digest-pinned 的精确投影；revision history 不会进入 Agent workspace 的搜索树。

Docker 沙箱镜像会烤入并标记精确 core Skill revision，不会回退到宿主机 builtin。Docker
provider preflight 拒绝二进制与镜像 revision 不匹配的组合，从而阻止 runner session 启动。
操作员命令语法使用 `stellad system-bundle --help` 查询。开发镜像用
`mise run sandbox:docker:build` 重建；每个自定义沙箱镜像都必须从匹配的 Stella revision 重建。

这次切换属于维护升级。启动新运行态前必须停止所有旧写入进程，并在一个事务中完成导入
与校验。不支持新旧写入进程对同一数据库滚动运行。

## Agent Skill 策略

独立 Skill 仍使用 `system`、`system_agent`、`user`、`user_agent`、`project` 以及上下文
`builtin` 身份。Plugin Skill 使用 PluginConfig 的四层范围模型，不另建全局 builtin 或
manifest 权限面。发行版的 `builtin:<name>` core 资源不可变；管理员安装的 `system:<name>`
与绑定 Agent 的 `system_agent:<name>` 独立可变。

解析会先选择唯一的胜出项，再应用策略：`project > user_agent > user > system_agent > system > builtin`。禁用该胜出项不会暴露同名的低优先级 Skill。托管的 `system:*` 与 `system_agent:*` 策略默认启用、按 Agent 共享，且与编辑内容的授权、`disable_model_invocation` 彼此独立。随插件发布的资源只通过所属插件启停，不再使用 `builtin:*` 策略；`.agents` 项目技能保持独立。已接纳的 turn 保留其快照，下一次 turn 才会看到成功提交。悬空的禁用引用不影响执行，需显式清理。

## 添加新后端

每个新沙箱后端需要在以下所有位置进行修改——遗漏任何一处都会导致运行时错误：

| 步骤 | 文件                                      | 操作                                                                                             |
| ---- | ----------------------------------------- | ------------------------------------------------------------------------------------------------ |
| 1    | `internal/platform/config/sandbox.go`     | 添加 `SandboxBackend<Name> = "<name>"` 常量                                                      |
| 2    | `internal/platform/config/sandbox_env.go` | 在 `ActiveSandboxBackend` 的 `STELLA_SANDBOX_BACKEND` switch 中接受该名称                        |
| 3    | `plugins/sandbox/<name>/session.go`       | 实现 `sandbox.Factory` 和 `sandbox.Session`                                                      |
| 4    | `cmd/stellad/setup_sandboxes.go`          | 注册 `sandbox.BackendDefinition` adapter，并提供所有由进程持有的依赖                             |
| 5    | 测试                                      | 覆盖后端实现和 composition-root adapter，并确保 `internal/boundary_test.go` 中的依赖守卫保持通过 |
| 6    | 文档                                      | 更新[沙箱指南](/docs/guides/sandbox)和本文件                                                     |

## 相关文档

- [沙箱指南](/docs/guides/sandbox) — 选择和配置后端
- [架构](/docs/development/architecture)
