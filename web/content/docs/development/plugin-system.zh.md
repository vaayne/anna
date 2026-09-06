---
title: 插件系统
---

插件共享配置和权限规则。CLI、MCP 与 Go 后端保留执行各自能力所需的接口。

## 定义与配置

`PluginDefinition` 确定集成身份、命名空间、后端、发行资源与默认启用状态。
Builtin 定义来自可信的发行声明，数据库中的 builtin 行只是投影。
自定义定义不能选择任意 Go 实现。

`PluginConfig` 保存某个定义在一个范围内的决策：

| 范围         | 适用对象               |
| ------------ | ---------------------- |
| System       | 部署中的所有用户       |
| System agent | 某个 Agent 的所有用户  |
| User         | 某个用户的所有 Agent   |
| User agent   | 某个用户使用某个 Agent |

每个定义在一个范围元组内至多一份配置。选择顺序是 user agent、user、system
agent、system。System 或匹配的 system agent 显式设为 `false`，分别构成独立上限，
更窄范围的 `true` 不能解除其中任何一个限制。`null` 使用所选定义的发行默认值。

这就是完整的配置模型：一份 `PluginDefinition`，加上四种范围元组各自至多一份
`PluginConfig`。`user_id` 和 `agent_id` 由可信 authority 推导，不能接受调用方自填身份。
Definition 拥有稳定的实现和 namespace 身份；所选 Config 拥有该范围的后端 payload 与凭据。

所选范围独立拥有配置，可以覆盖发行定义中的字段，但不同范围之间不合并字段或凭据。
所选配置禁用或不完整时，不回退到更宽范围。Builtin 使用相同规则，管理员可以禁用。

## 一份执行快照

公共服务从可信用户、Agent 或群组身份解析快照。Runner 在创建时捕获一次，工具、
Skills、提示、环境变量和生命周期钩子使用同一代配置。

公开资源遵循命名空间的胜出定义。自定义 MCP 即使占用了 builtin 的命名空间，也不能
取得被遮蔽 builtin 的 Go 钩子或凭据。内部能力检查使用精确 plugin ID，因此命名空间
冲突不能隐藏管理员对渠道或后台任务的限制。模型可见工具名采用
`{namespace}__{local_name}`，授权使用可信 plugin ID 和本地工具身份，不解析展示名称。

配置写入在执行准入屏障内原子提交。空闲 runner 被回收，已经开始的 turn 可以结束后
再回收 runner。凭据读取仍必须匹配捕获的配置版本，旧 runner 不能把新凭据发给旧地址。
插件开关约束 Stella 管理的能力暴露与执行准入；文件系统和网络限制仍由沙箱负责。
禁用插件不会撤销已有 OAuth grant，也不会抹除此前已加载的 Skill。

## CLI 与 Skill 资源

CLI 集成可以包含二进制、Skills、环境声明和提示。CLI 版本与 Skill 来源是独立字段，
更新一个不要求更新另一个。Manifest 只是发行输入的加载器，不再拥有独立权限规则。
CLI 按选中的 snapshot 惰性安装，runner 需要时才物化，没有独立 sync endpoint。

Builtin plugin Skill 的归属来自可信资源路径 `plugins/<kind>/<plugin>/<skill>`。
Core Skill 使用 `core/<skill>`，不是插件贡献。用户 frontmatter 不能把 Core Skill
变成插件 Skill，也不能认领插件 owner。提示列表、搜索与直接加载都在选定资源后检查
同一归属限制。Core Skill 仍可声明 CLI 硬依赖：Web 依赖
Bun，Python Script 依赖 uv。依赖禁用时，同一列表与加载检查会隐藏该 Skill。
Lightpanda 仅影响 Web 中的渲染能力，不影响普通抓取和搜索。

每个 runner 只获得选中的 CLI 文件及入口。可信 system 安装的私有参数不进入 runner
可读的文件系统；Docker 在现有工具缓存内按一个解析后的 image ID 和完整四层范围选择
准备 Linux 文件。selection helper 只提供选中的条目和二进制。Native managed 安装使用
managed tree；User 和 user agent 的安装在各自沙箱目录内执行，并在 PATH 中优先。插件权限控制
Stella 提供的资源；`none` 后端不提供文件系统隔离。

## Channel 与账号

一个 channel 插件代表一个平台。每个账号仍是独立 channel 实例，拥有自己的精确 ID、
凭据、active 状态和持久 Agent 绑定。保存一个账号不能覆盖另一个账号的凭据，也不能
重新启用管理员已禁用的平台。

Listener 检查 system、匹配的 system agent 上限和实例 active 状态。某个用户禁用插件，
不会停止其他用户共享的 listener。事件准入还检查可信执行者的四层范围，以及已有
Agent 访问权限或访客策略。渠道签名、开户和平台身份校验保留在所属 adapter。

现有 `UNIQUE(agent_id, type)` 唯一约束仍规定一个 Agent 每个平台最多绑定一个实例。
每个实例有自己独立的凭据，即使平台相同也不会互相覆盖。多个账号可以绑定不同 Agent。
身份关联与创建 bot 账号是独立操作。

## MCP 凭据与观测

MCP 是插件后端。端点设置和凭据引用属于所选配置，token 保留在 Vault。
Shared 与 per-user 凭据互不回退。

OAuth 客户端注册属于配置所有者。System 和 system agent 配置缺少客户端时，
管理员先通过 OAuth start 初始化，随后用户授权自己的账号。User 和 user agent
配置的所有者可以自行初始化。旧系统级配置若没有 client ID，升级后需要这一次管理员
操作；各用户的 token 仍独立保存。禁用和 reset 保留 grant，删除配置才原子清理其 grant。

远端工具目录和连接状态属于后端观测，以配置及凭据所有者为键，并检查配置版本。
某个用户的工具目录不能成为另一个用户的工具列表。旧 per-user 目录没有可信 owner
来源，迁移后必须冷探测。内部 OAuth bundle 不允许通过公开 Vault 接口访问，也不进入
通用环境变量。

## Core 边界与升级

Provider adapter 与沙箱后端保留显式的编译期 registry。Core 存储、编排和凭据服务
不会因为被插件使用就变成可选能力。例如，禁用公开 Xberg 插件会隐藏其公开资源，
Library 通过显式路径调用的内部解析器依赖仍可用。

`cmd/stellad` 组合各后端和公共 catalog。后端不能另建 scope 或 enabled 解析规则。
Provider 与沙箱 adapter 的生产代码依赖 `pkg/**` 公开契约，不依赖 `internal/**`。

旧数据切换需要维护升级：先停止所有旧写入进程，再启动新运行态。一个事务导入并验证
配置、凭据关联与工具策略，最后记录完成。旧行保留供检查，运行态不同时读取新旧配置。
这次切换不支持新旧进程对同一数据库滚动写入。
