---
name: lark-shared
version: 3.0.0
description: "飞书/Lark CLI 共享基础（Stella 适配版）：说明 managed OAuth、身份选择和按需增量 scope 授权。"
---

# lark-cli 共享规则

本技能指导你在 Stella 中通过 lark-cli 操作飞书/Lark 资源。

## Stella 与 lark-cli 的边界

- Stella 管理 OAuth 应用凭据、用户 token、刷新和沙盒环境变量注入。
- lark-cli 只负责调用 API；不要在沙盒中运行 `config init`、`config bind`、`auth login` 或 `auth logout`。
- Stella 为当前用户注入 `LARKSUITE_CLI_USER_ACCESS_TOKEN`、`LARKSUITE_CLI_APP_ID` 和 `LARKSUITE_CLI_BRAND`。
- 同一用户的私聊 Agent 共享 Stella 管理的 OAuth connection；群聊和无用户会话不加载个人 token。
- 飞书/Lark Channel 凭据与 Stella 登录 OAuth 不授权 lark-cli，它们是独立边界。

## 首次连接

当 lark-cli 提示未连接或缺少 user access token：

1. 使用 `oauth_list` 查看 provider 状态。
2. 使用 `oauth_list`，选择 `required_by` 包含 Lark CLI 的 provider，再对它调用 `oauth_connect`。默认部署是 `feishu`，Lark 国际版部署可以由管理员绑定为 `lark`；不要硬编码猜测。
3. 把工具返回的验证链接和代码原样发给用户，并结束当前回复，不阻塞等待。
4. 用户确认后，用 `oauth_flow_status` 检查 flow；授权完成后在下一轮重试原命令。

不要让用户运行 CLI 命令，也不要要求用户在聊天中发送 App Secret。

## 按需增量授权

当 API 返回用户 token 缺少 scope：

1. 保留并检查错误中的接口、错误码、`message`、`permission_violations`、`console_url` 和 `hint`。
2. 只提取当前操作实际缺少的 user scopes。
3. 使用 `oauth_list` 找到 `required_by` 包含 Lark CLI 的实际 provider，再调用 `oauth_connect`，传 `provider=<实际 provider>`、`scopes=[...]`。Stella 会把新 scopes 与该用户已有 desired scopes 取并集；能否授予由飞书授权页面决定。
4. 把验证链接发给用户；用户完成授权后检查 flow，再重试原操作。

禁止为了省事请求全部权限。一次业务操作缺少多个 scope 时可以合并成一次增量授权。

### 两类权限结果

- **用户 token 缺少 scope**：使用上面的增量 OAuth connection 流程。
- **应用 scope 未开通**：授权页面不会出现该权限，重新授权后依旧缺少。停止调用，把 provider 返回的 `console_url` 和缺少的 scope 交给管理员；通用 OAuth 层无法替管理员在开发者后台开权限。

## 身份选择

| 身份          | 使用方式    | 适用场景                                               |
| ------------- | ----------- | ------------------------------------------------------ |
| user 用户身份 | `--as user` | 员工个人资源、需要员工本人作为发起人或操作者的业务流程 |
| bot 应用身份  | `--as bot`  | 管理员另行配置了 bot 身份，且操作不依赖个人私有资源    |

- 默认使用 `--as user`。
- 审批创建、个人云盘、邮箱、个人日历和“以我名义”操作必须使用 user。
- 不得因 user 未登录、token 过期或 scope 不足而切换 bot。
- Bot 不能代表员工成为审批发起人，也通常看不到员工私有资源。

## Token 过期

Stella 在创建或刷新沙盒会话时主动刷新即将过期的 token。若 token 在当前会话中途过期：

1. 结束当前操作，不用新幂等键重试写请求。
2. 让用户开启下一轮对话，使 runner 重新加载刷新后的环境。
3. 若仍失败，使用 `oauth_flow_status`；refresh token 失效时重新连接 provider。

## 安全规则

- 禁止输出 App Secret、access token、refresh token 等密钥。
- 写入或删除操作前确认用户意图。
- 用 `--dry-run` 预览危险请求。
- 同一业务写操作使用稳定的幂等键；失败后不得换键重试。
- 不得用 curl、Python 或直接 OpenAPI 调用绕过 OAuth scope policy。

## 更新检查

lark-cli 输出 `_notice.update` 时，完成当前请求后告知用户当前版本和最新版本。Stella 固定并分发二进制与内置 skills；不要在沙盒里自行更新。
