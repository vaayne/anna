---
title: 技能
---

## 什么是技能

技能是可复用的操作手册，教会 Stella 如何执行特定任务。当你让 Stella 做一些事情，比如"创建一个 GitHub release"或"写一篇博客文章"时，她可以加载一个技能，获得该工作流程的分步指令。

技能用纯 Markdown 编写——本质上就是 Stella 阅读并遵循的速查表。你可以从公共注册中心安装技能，也可以自己编写。

## 技能作用域与优先级

每类 Skill 只有一个内容权威。随发行版提供的 builtin 来自不可变、内容寻址的发行 bundle。Project Skill 是持久 Agent/项目工作树中的普通文件。托管的全局、Agent 绑定、用户和用户-Agent Skill 在类型化 Stella Home 根中使用不可变 revision；current selector 决定 Stella 加载哪个精确 revision。

存储的作用域为 `project`、`user_agent`、`user`、`system_agent` 和 `system`。`builtin` 是上下文作用域：发行版 Skill 使用不可变身份 `builtin:<name>`。管理员安装的全局 Skill 是另一个可变身份 `system:<name>`，绑定到 Agent 的管理员 Skill 则是 `system_agent:<name>`。

- **项目技能** — 存放在你的仓库的 `.agents/skills/` 目录下。它们随代码发布，并在当前会话绑定到该项目时可用。
- **用户技能** — 你的个人技能，对你的所有代理可用。
- **用户 · 当前代理** — 你限定于单个代理的个人技能。
- **共享代理技能** — 由管理员管理，对使用该代理的所有人可用。
- **全局技能** — 由管理员管理，处处可用。Stella 随附的技能仍属于安装内容；管理员可以在管理控制台安装、启用、停用和删除受管理的全局技能。

同名时，Stella 按以下顺序选择唯一的胜出项：

```
项目 > 用户 · 当前代理 > 用户 > 共享代理 > 全局 > builtin
```

策略在选择胜出项后才应用。禁用胜出项不会让同名的低优先级 Skill 出现。

## 按 Agent 启用

随插件提供的技能跟随所属插件的权限和启用状态，在插件设置中统一管理，没有第二个技能开关。只包含技能的插件也使用同一套插件设置。项目 `.agents/skills/` 与个人、托管技能保持独立。必需的 Stella 与 Xberg 指导内容随运行环境提供。

Skill 对 Agent 默认启用。管理员或该 Agent 的持久创建者可以在该 Agent 的 **技能** 标签页启用或禁用托管的全局或与该 Agent 匹配的 Agent Skill。这里编辑的是同一份共享设置，最后一次成功提交的更新生效。

启用状态与编辑 Skill 内容的权限、以及 `disable_model_invocation` 相互独立。已被接纳的 turn 保留自己的 Skill 快照；下一次 turn 才会看到已提交的启用状态变更。

指向已不存在 Skill 的禁用引用不影响执行；请在 Web UI 中显式清除。Skill 启用状态是产品偏好设置，而不是文件系统访问控制。

在 **个人设置 → 技能** 管理个人的 `user` 与 `user_agent` 技能。管理员在 **管理控制台 → 部署资源 → 全局技能** 管理部署所有的 `system` 与 `system_agent` 技能。两个页面不会混合不同所有权的作用域。

## 安装技能

### 选择目标位置

每次安装和上传都必须选择目标位置。Stella 不会从对话中推断目标位置，也不会把上一次选择当成后续写入的授权。

- 在 Agent 的 **技能** 标签页中，选择 **仅自己 · 当前 Agent**（`user_agent`）；管理员还可选择 **所有人 · 当前 Agent**（`system_agent`）。
- 在 **个人设置 → 技能** 中，选择个人目标位置（`user` 或 `user_agent`）。
- 在 **管理控制台 → 部署资源 → 全局技能** 中，选择部署所有的目标位置（`system` 或 `system_agent`）。

Web UI 会在执行写入前要求你立即确认目标位置。

### 选择来源

Stella 可以从多个来源安装技能：

- **[clawhub.ai](https://clawhub.ai)** — 在 Web UI 中浏览或搜索市场。
- **GitHub / GitLab** — 在安装表单中输入仓库来源。
- **ZIP 上传** — 上传包含 `SKILL.md` 的 Skill 目录。

如果你在 clawhub.ai 遇到频率限制，可以设置一个免费的 API 令牌：

1. 在 [clawhub.ai](https://clawhub.ai) 注册。
2. 进入 Settings，然后 API Tokens，创建一个令牌。
3. 在聊天中发送：`/config CLAWHUB_TOKEN your-token`

## 管理技能

### 从对话中管理

- **"找一个已安装的技能来部署这个服务。"** — Stella 只搜索当前 Agent 已可见的 Skill。
- **"加载部署技能。"** — Stella 为当前任务加载选中的精确 revision。

对话工具是只读的，不能安装、创建、编辑、升级、弃用或删除 Skill。

### 从 Web UI 管理

在个人设置中浏览、安装和移除你的技能。管理员通过“全局技能”管理部署级与共享代理技能。

## 创建自定义技能

你可以创建自定义技能来教 Stella 你的工作流程。一个技能就是一个包含 `SKILL.md` 文件的目录。

### 技能格式

```markdown
---
name: my-deploy-script
description: Deploy the application to production.
---

# Deploy to Production

Follow these steps to deploy:

1. Run the test suite and confirm all tests pass.
2. Build the production bundle.
3. Push to the production branch.
4. Verify the deployment is healthy.

Always ask the user for confirmation before pushing to production.
```

### Frontmatter 字段

| 字段                       | 必填 | 描述                                 |
| -------------------------- | ---- | ------------------------------------ |
| `name`                     | 是   | 小写加连字符，最长 64 个字符         |
| `description`              | 是   | 一行摘要，显示在搜索结果中           |
| `disable-model-invocation` | 否   | 禁止自动选择，但仍允许显式加载和使用 |

### 保存自定义技能

对于托管 Skill，请在本地创建目录，将其打包为 ZIP，然后在 Web UI 中上传到明确的目标位置。对于 Project Skill，请把目录直接添加到项目仓库的 `.agents/skills/` 下。

## 小贴士

- **先搜索再创建。** 在从头创建技能之前，先在 Web UI 的市场中检查是否已经存在。
- **保持技能专注。** 一个技能对应一个任务。一个"部署"技能和一个"回滚"技能比一个试图同时做两件事的技能要好。
- **团队工作流程使用项目技能。** 把共享技能放在仓库的 `.agents/skills/` 目录中，让团队所有人受益。
- **通过加载来测试技能。** 创建技能后，让 Stella 加载它并尝试工作流程，验证指令是否有效。

## 升级旧的内置技能设置

内置技能统一跟随所属插件的启用状态。旧版本中按 Agent 单独禁用内置技能的设置不再生效，也不会阻止升级。托管技能的启用设置和项目 `.agents/skills/` 的发现行为保持不变。
