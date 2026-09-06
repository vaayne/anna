---
title: Agent 工具规则
description: 在 Stella 中新增、修改、改名或删除模型可见工具的规范。
---

> 这是给贡献者看的**规则文件**。新增、修改、改名或删除任何模型能调用的工具前，先读这页并遵守它。[`api-design.md`](./api-design) 管 HTTP 契约，[`go-patterns.md`](./go-patterns) 管并发与脱敏，[`doc-style.md`](./doc-style) 管文档，本页管工具面。

工具是 Stella 里唯一能被模型直接调用的东西。它的名字、schema 和描述对模型而言就是公开 API：改一个名字，所有提到它的会话记录、delegate preset 和 `tool_override` 行都会失配。改工具要像改破坏性 HTTP 契约一样谨慎。

每节末尾都有 **验收**：哪个测试或命令会在评审之前拦住违规。

## 1. 先问要不要工具

按顺序往下走，在第一个成立的地方停。

1. **模型用 `bash` 加现成 CLI 就能做到**：写 skill，不要建工具。`xberg` 就是这种：一段说明加一条命令行，没有 Go 代码、没有 schema、不用注册。
2. **需要用户身份、数据库写入、对外副作用，或必须由服务端做、模型不能绕过的校验**：这才是工具。

一个工具只做一件事。**带 `action` 参数、在多个操作之间切换的工具是明令禁止的。** provider 在函数 schema 顶层拒绝 `oneOf`/`const`，所以 union 工具只能把所有 action 的字段平铺成一个对象：provider 无法校验调用，宽松解码器又会把不属于当前 action 的字段静默丢掉。模型于是从一次它根本没发出的调用里，拿到一个看起来成功的结果。

**验收：** 评审。`toolgen` 会拒绝 split 工具上的 `action` 属性（`TestValidateRejectsBadDeclarations`），但"这本该是个 skill"只有人能看出来。

## 2. 工具声明在哪里

**有 HTTP 操作的工具，声明在那个操作上。** 在 `api/spec/domain/<domain>/paths.yaml` 加 `x-agent-tool` 注解。还没有端点就先按 [`api/CLAUDE.md`](https://github.com/CherryHQ/stella/blob/main/api/CLAUDE.md) 和 [`api-design.md`](./api-design) 加一个。一份契约、一份 schema、一个改动点。

**确实没有 HTTP 操作的工具，声明在 `api/spec/agent-tools/<domain>.yaml`。** 往另一个 session 发消息、把 skill 投影进沙箱，这些都不是 REST 资源；为了挂工具而编一个端点，是对契约撒谎。这类工具自带 schema，但仍然走 `toolgen`，因此享有同样的命名检查、同样的封闭 schema 和同样的生成输入类型：

```yaml
family: session
package: agent/session/access # internal/ 下的输出目录
tools:
  - action: list
    description: List this agent's recent sessions for the current user.
    input:
      type: object
      properties:
        include_archived: { type: boolean, default: false, description: Include archived sessions. }
  - action: send
    description: Continue one of this agent's sessions and wait for its reply.
    input:
      type: object
      properties:
        session_id: { type: string, description: The session to continue. }
        message: { type: string, description: The session's next request. }
    required: [message, session_id]
```

这份声明生成 `session_list` 和 `session_send`。给第二个工具加上 `resource: message`，名字就变成 `session_message_send`：resource 是名字里真实的一段，不是注释。

`input` 可以是内联 schema，也可以是指向已装配 OpenAPI components 的 `$ref`；这里只接受 `#/components/schemas/...`，嵌套层级同样如此。`$ref` 引入的是整份 schema，因此 `required` 只能写这份 schema 真有的属性——要求一个输入里不存在的字段，等于造出一份没有任何入参能满足的 schema，`validate` 会拒绝。`package` 是接收 `tool_<family>_gen.go` 的 `internal/` 子目录，最后一段就是 Go 包名。同一 family 的实现分布在不同包时，单个 tool 可以覆盖文件级 `package`。`batch: <field>` 把输入包成数组属性，与注解修饰符行为一致。

声明式工具生成的类型是 `<Family><Action>Input`（`SessionSendInput`），不是 `<Action>Input`：它们落在已有手写代码的包里，`internal/agent/session/access` 自己就有一个 `SendInput`，裸名字根本编译不过。

**手写工具是一份封闭清单**：`bash`、`view_image`（核心沙箱）、`notify`（渠道分发）、`goal_control`（attempt 协议）、`code`（元工具）。动态发现的 MCP 工具携带可信插件元数据，工具名前缀不授予手写工具资格。往固定清单里加一项，等于宣称这个工具既没有 HTTP 操作、也没有能被声明的 schema。改 `pkg/toolmeta` 里的清单，并在 PR 里说明理由。

上面那份清单现在就是全部。`memory` 是最后一个待拆的 union，装着它的 `pendingSplit` 已随拆分一起删除，而不是留成空表——空着的第二套机制只会招来第三条例外。没有 HTTP 操作的工具应该进 `api/spec/agent-tools/`，而不是进第二份例外清单。

**验收：** `TestGeneratedFixtureIsCurrent` 与 `TestValidateRejectsUnsatisfiableRequired`（`internal/tools/toolgen`）——前者把 `test/toolgenfixture/agent-tools/session.yaml` 走真实流水线渲染成 Go，并让 `go build ./...` 在一个存在同名手写 `SendInput` 的包里编译它；`TestEveryBuiltinIsGeneratedOrAnAcceptedException` 与 `TestExceptionListsAreExactlyWhatTheRuleDocuments`（`pkg/toolmeta`）——把每个固定 builtin 对着上面两份清单核一遍；`mise run generate:api:check`。

## 3. `x-agent-tool` 参考

一条注解，或者当一个操作支撑多个 action 时写成列表：

```yaml
x-agent-tool:
  - { tool: "scheduler", resource: "job", action: "update" }
  - { tool: "scheduler", resource: "job", action: "pause", fixed: { enabled: false }, body: false }
```

| 字段            | 作用                                                                              |
| --------------- | --------------------------------------------------------------------------------- |
| `tool`          | 家族，必填。必须在 `domainPackages`（`internal/tools/toolgen/main.go`）里有映射。 |
| `resource`      | 这个 action 作用的子资源。决定工具名与 `id` 参数（§4）。                          |
| `action`        | 分发键，必填。生成 `Handler` 方法名与 `Dispatch` 分支。                           |
| `name_override` | 显式工具名，只用于 §4 那一个语法例外，其余情况不要用。                            |
| `description`   | 覆盖 operation summary 作为声明描述。                                             |
| `fixed`         | 服务端常量，属性从模型输入里删除。                                                |
| `restrict`      | 收窄属性 enum 而不动 HTTP API，首值成为 default。                                 |
| `require`       | 仅在工具 schema 里把可选字段标为必填。                                            |
| `optional`      | 仅在工具 schema 里把必填字段标为可选。                                            |
| `add`           | 只属于工具的属性。batch 时加到每个 item 上。                                      |
| `omit`          | 从工具输入里删属性，不动 HTTP 契约。                                              |
| `rename`        | 重命名工具输入里的属性（连同 `required` 项）。                                    |
| `body`          | `false` 表示只取 path/query 参数，忽略 request body。                             |
| `batch`         | 把 request body 包成该名字的数组属性，`minItems 1`、`maxItems 20`。               |

修饰符按固定顺序生效：构建输入（params，除非 `body: false` 否则并入 body）→ `add` → `fixed` → `omit` → `rename` → `restrict` → `require`/`optional`。所以 `restrict` 和 `require` 用的是模型看到的属性名，即重命名之后的名字。

值得记住的陷阱：

- **身份字段永远被剥离。** `agent_id`、`user_id` 及其驼峰写法不会到达模型，身份来自请求上下文。
- **`fixed` 在 `restrict` 之前执行。** 对一个已经 fixed 的属性再 restrict 是空操作，因为属性已经没了。
- **`restrict`/`require`/`optional` 作用于工具输入，不作用于 batch item。** batch 时工具输入是外层包装；item 级属性用 `add`。
- **未知或拼错的修饰符会让构建失败**，不会被忽略。
- **不要在属性描述里解释别的 action。** "只有 entry_update 用这个字段"是 union 时代的文案，split 工具会继承它，而它自己的精确 schema 早就回答了这个问题。

**验收：** `internal/tools/toolgen/main_test.go`（每个修饰符一条测试）；`TestParseActionSpecsRejectsUnknownModifier`。

## 4. 命名

**插件工具名使用 `{namespace}__{local_name}`**，例如 `recally__feed_add`、`scheduler__job_pause`。Go 与 MCP 使用相同规则。Namespace 不允许包含 `__`，完整名称最多 64 字符。授权使用可信元数据中的 plugin ID 与 local name，不能解析导出名称的前缀来授予权限。

**Core 工具保留 `<domain>_<resource>_<action>`**，resource 与 domain 同名时省略，例如 `goal_create`。动词用小而无聊的那一组：`create`、`get`、`list`、`update`、`delete`、`search`、`add`、`remove`、`save`、`send`。资源用单数，并与 HTTP 资源名对齐。禁止裸名词、复数和动宾倒置。

唯一的例外：当创建的是本 domain 自己的资源、而 object 只是它的来源或载荷时，`<domain>_<action>_<object>` 读起来更准确——`share_create_artifact` 创建的是 **share**，不是 artifact。这种情况用 `name_override`，也只有这种情况。

**参数命名八条：**

1. 一律 `snake_case`。指向本工具资源的 path 参数（job 工具上的 `{jobId}`）映射为 `id`，其他 `{xId}` 映射为 `x_id`。这是 `toolFieldName` 里的规则而不是逐名映射表，新 domain 不需要加条目。
2. 本资源主键叫 `id`，外键叫 `<resource>_id`（`goal_id`、`article_id`、`feed_id`、`session_id`）。一个 schema 里不得出现两个语义不同的标识字段却只有一个叫 `id`。
3. 分页统一 `page_size` + `page_token`，返回 `next_page_token`。`limit` 只用于无分页的"最多返回 N 条"，且 schema `maximum` 必须等于 handler 的上限。
4. 全文检索统一 `q`；时间用 RFC3339 字符串，参数名 `since`/`before`/`at`；布尔用肯定式（`enabled`、`unread`），不用 `disabled`/`no_*`。
5. 幂等键统一 `idempotency_key`，凡有对外副作用的工具都应要求它。
6. 批量统一 `<resource>s: [...]`，`minItems 1`、`maxItems 20`。
7. 身份字段（`agent_id`、`user_id`）永远不出现在工具参数里。
8. 描述遵守 §6。

**验收：** `TestToolFieldNameMapsIdentifiersByResource`；`TestValidateRejectsBadDeclarations`（provider 合法名、snake_case 属性名）。

## 5. schema 规则

split 工具的 schema 是契约不是提示：provider 在调用前按它校验，`DecodeInputStrict` 拒绝任何它没声明的字段。

- **精确且封闭。** 只含自己的属性和自己的 `required`；没有 `action` 属性，动作由名字承载。`toolgen` 给工具输入和 batch item 都加 `additionalProperties: false`；真正是自由 map 的属性自带 `additionalProperties`，原样保留。
- **顶层保持朴素 object。** 顶层不得出现 `oneOf`/`anyOf`/`allOf`/`enum`/`const`/`not`，OpenAI 兼容 provider 会拒绝。
- **每个属性有一句描述。** 取值受限用 `restrict`，不要写在描述里。
- **数值上限等于 handler 上限。** schema 写 500、handler 截到 100，是在教模型一件假事。
- **大正文走沙箱路径，不走内联字符串。** 照 `recally__article_save` 的 `content_path` 先例：单文件 1 MB、单次调用 4 MB，并且工具要报告它实际存下了什么。
- **输出有明确上限**，触顶时在结果里说明（`truncated`、`note`）。时间用 RFC3339。永远不返回密钥值，vault 工具只回元信息。

**验收：** `TestBatchAnnotationWrapsRequestBody`、`TestActionSchemaKeepsDeclaredAdditionalProperties`、`TestToolSchemaIsPlainObjectWithActionEnum`；`TestValidateRejectsBadDeclarations`。

## 6. 描述规则

- **不超过 60 词。**
- **第一句说做什么**，第二句说副作用或前置条件——"fetches a new URL server-side when no body is given"、"sends mail; requires `idempotency_key`"。
- **需要跨工具引用时用真实工具名**（"then call `oauth_flow_status`"）。
- **不复述 schema。** 字段级说明写在字段上。
- **不要与兄弟 action 消歧。** 精确 schema 已经做完了。

operation 背书的工具把模型可见文案放在 handler 旁边的手写适配层（`internal/library/recally/tool.go` 的 `actionDescriptions`），因为 endpoint summary 是写给 API 读者的。声明式工具把描述写在声明文件里。

**验收：** `TestValidateRejectsBadDeclarations`（没有描述的工具会让构建失败）；词数靠评审。

## 7. Go 实现形态

在 `internal/tools/toolgen/main.go` 的 `domainPackages` 加映射，跑 `mise run generate:api`，然后写 `tool.go` 适配层：

`domainPackage.Root` 选择生成目录。现有 family 默认使用 `internal`，Email 明确映射到 `plugins/email`。生成器只清理受支持的 `internal` 与 `plugins` 根目录，因此删除某个根下最后一个 family 时也会清理陈旧生成文件，同时保留手写文件。

- `Tool{spec, svc}`，由 `NewTool` 构建；需要沙箱会话时用 `NewRuntimeTool`。
- `Definition()` 返回 `spec.Definition(description)`。
- `Execute` 五步：nil-service 守卫 → `authz.ToolIdentity(ctx, name)` → `ToAuthority()` → `Dispatch(ctx, handler, spec.Action, args)` → `authz.MapToolError(tool, discover, err)` + 序列化。`discover` 是列出「本 agent 能访问什么」的同族工具，让恢复建议指向真实存在的工具；该族没有 list action 时传 `""`。
- Handler 方法保持薄。**身份永远来自 ctx，不来自参数。** per-action 授权在 `Access` 层，不在 handler。
- Plugin 工具通过注入的系统 adapter 完成 `ToolIdentity` 与 `MapToolError`；plugin 不 import `internal/authz`，也不铸造 Authority。
- 所有校验先于任何写入。错误文案可操作、指向真实工具名。"没找到"在 list 类返回空列表，在 get 类返回 not-found。
- 有对外副作用的工具必须幂等：按 `idempotency_key` 去重，并报告重复而不是发两次。

**验收：** `go build ./...`；该 domain 的 handler 测试；`internal/authz/tool_authz_test.go`。

## 8. 注册与可见性

工具在 `cmd/stellad/commands.go` 注册。split 家族按生成的 `ActionTools()` 逐条注册 `agent.BuiltinTool`，所以新增一个 action 不需要改注册代码。

- **`Available` 决定可见性，它出错是致命的，不是静默的。** 基线是 `agent.BuiltinToolAvailable`（有 user 且有 agent）。检查本身出错时错误必须向上传播：registry 与 runner 构建中止，`GET /api/agents/{id}/tools` 返回 5xx，并且不缓存任何残缺子集。悄悄少了一个工具的工具集比一次失败的请求更糟——模型会把这个缺口当成事实来推理。
- **核心名字是保留字。** builtin 和插件不得占用核心工具名。插件命名空间由公共快照选定，没有独立 MCP 前缀，也不能根据名称隐式授予能力。
- **Code Mode hot 集刻意保持小。** `pkg/agent/code_strategy.go` 的 `HotToolNames` 列出值得每轮直接摆在模型面前、而不是藏在 `tools.search` 后面的工具。它被导出，是为了让 prose guard 拿它和引用这个集合的四份文档逐一对齐；加一个名字意味着同时改这份列表、system prompt 和这四份文档的中英两版。不一致时 `TestHotSetProseMatchesTheDeclaredHotTools`（`cmd/stellad`）会失败。

**验收：** PR-1（[#1175](https://github.com/CherryHQ/stella/pull/1175)）新增的 runtime registry 与 catalog availability 测试；`cmd/stellad` 与 `internal/agent` 的注册测试；`pkg/tools` registry 测试（重名）。

## 9. 必须同步的消费面

工具名是编译器管不到的字符串。新增、改名或删除时，逐条走一遍：

- `plugins/<category>/<plugin>/skills/<skill>/SKILL.md`——示例必须用真实名字和真实字段。
- `plugins/core/skills/stella/SKILL.md`——工具清单。
- `internal/agent/prompt/template/system_prompt.tmpl`。
- scheduler 内置任务模板，它们的 prompt 里写了工具名。
- `web/content/docs/development/architecture.md`（EN + ZH）的工具表。
- `resources/builtin_manifest_gen.go`——重新生成。
- Web UI 的工具元数据，名字在那里决定图标或标签。
- release note——只要名字变了或消失了就必须写。

**验收：** `resources/recally__skill_test.go` 与 `internal/scheduler/builtin_schema_test.go`（skill 与模板示例对照实际 schema 检查）；`mise run generate:check`。

## 10. 改名、拆分、删除

同一能力改名时，必须保留已有权限决策。插件 override 使用 plugin ID、local tool name
和 scope owner；Core override 使用 core tool name。运行时配置 UUID 不作为策略身份。

1. 改名必须有明确且已验证的身份映射，保留每条 enabled 与 scope owner。旧名称歧义或
   owner 冲突时中止迁移，不能通过恢复默认可见性解决。
2. 仅当能力确实删除时移除其策略。Down 迁移不能猜测已删除的权限或凭据。
3. 统一插件切换不增加运行时别名或旧数据查询回退。自定义 Skill 与 preset 中的旧名称
   需要显式修改。
4. 列出完整旧名与新名，验证策略保留、冲突和回滚。Family selector 使用可信 registry
   元数据，不猜测名称前缀。

本次切换采用停止旧写入进程的维护升级。旧来源行保留供检查，新运行态不读取它们。

## 11. 测试要求

每个工具至少要有：

- **一条授权用例**，在 `internal/authz/tool_authz_test.go`：一次必须被拒的调用，并证明拒绝不泄漏"存在与否"。
- **一条 handler 测试**，覆盖 dispatch 之外的逻辑——互斥字段、路径展开、上限、投影。
- **schema 与 skill 守卫。** `internal/scheduler/builtin_schema_test.go` 和 `resources/recally__skill_test.go` 会拿文档示例对照实际 schema；扩展它们，不要另起一套。
- **`mise run generate:api:check` 干净。** 它经 Redocly bundler（`vp dlx`）重新生成，因此需要 node 工具链；它同时检查 untracked 文件——新 family 的第一个 `tool_gen.go` 是新增而不是修改。
- **catalog 断言**：工具出现在 `GET /api/agents/{id}/tools`，schema 精确且没有 `action` 属性。
- **一条 smoke 用例**，在 `TestToolSmoke`（`cmd/stellad/tool_smoke_test.go`）：它对着真实数据库与生产 registry，经 Code Mode 把每个面向模型的工具真实调用一遍。覆盖集合以严格集合等式闭合，所以新工具在补上用例之前会让构建失败；没有 pending 清单，也没有 skip。
  - **有副作用的工具由姐妹读工具判定，不由它自己的返回值判定。** 用 `confirm` 让第二次调用在自己的 turn 里把副作用读回来：删掉的对象确实不在了，暂停的任务读回来是 disabled，撤销的分享不会连累它的兄弟。只报成功正是坏掉的写入也能做对的那一件事。
  - **成功路径依赖外部依赖、无法在此产生的工具**可以改为断言它的规范错误（`assertsErrorShapeOnly`）。用例仍须真正走到工具自身的逻辑（停在 schema 拒绝上就算失败），并且注释必须指名覆盖成功路径的**具体测试函数名**，或者明写"没有任何测试覆盖"。"由 X 包覆盖"不算，要写出测试名。
  - **真正无法从聊天会话调用的工具**写进 `protocolExceptions`，并写明替代它的具体测试。

  测试会逐项打印当前协议例外与 error-shape-only 工具。动态 MCP 覆盖依据可信插件身份，不依据导出名前缀；以这份报告核对当前覆盖清单。

只有跨进程的接缝才需要 system test，`goal_control` 的 attempt 协议是那个例子。见 [`testing.md`](./testing)。

**Harbor 不覆盖 builtin 工具。** 任何 PR 都不得引用 Harbor 分数作为工具改动的证据；要明确写出这一点，而不是让分数暗示覆盖。

**验收：** `mise run test`；`mise run generate:api:check`。

## 12. Code Mode 注意事项

- **工具名原样成为 JavaScript 标识符**，在生成的目录里长什么样，在这里就长什么样。
- **`tools.search` 按家族前缀匹配**，所以一致的 `<domain>_<resource>_<action>` 语法才能让一个家族在一次搜索里被找全；冷工具只能通过这种方式被发现。
- **大结果不要经 `code` 往返。** 用 `content_path` 模式，让载荷根本不进模型上下文。
- **hot/cold 是可见性，不是授权。** 从 `code` 里调用的工具，身份与授权和直接调用完全一致。

**验收：** `pkg/agent` 的 code strategy 测试。

## 13. PR checklist

把这段贴进 PR 的 Test 段，逐条回答。

- [ ] 这个能力需要的是工具而不是 skill（§1）。
- [ ] 声明在 HTTP 操作上，或声明在 `api/spec/agent-tools/` 并写明为什么没有端点（§2）。
- [ ] 修饰符按文档使用，不依赖 `fixed` 的副作用（§3）。
- [ ] 名字符合 `<domain>_<resource>_<action>`，参数符合八条（§4）。
- [ ] schema 精确、封闭、没有 `action` 属性，上限与 handler 一致（§5）。
- [ ] 描述不超过 60 词并写明副作用（§6）。
- [ ] Handler 从 ctx 取身份，校验先于写入，对外副作用幂等（§7）。
- [ ] `Available` fail-closed（§8）。
- [ ] §9 的消费面全部更新，包括中英两个版本的文档。
- [ ] 改名带上删除退休行的 override 迁移和 release note 对照表（§10）。
- [ ] 已补 `TestToolSmoke` 的 smoke 用例（有副作用的工具带 `confirm` 读回），或写了带替代覆盖来源的 `protocolExceptions` 条目（§11）。
- [ ] 每条 `assertsErrorShapeOnly` 用例都指名了覆盖其成功路径的具体测试，或明写没有（§11）。
- [ ] 授权用例、handler 测试与守卫已补，`generate:api:check` 干净（§11）。
- [ ] 没有引用 Harbor 分数作为本次工具改动的证据（§11）。
