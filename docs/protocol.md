# 协议依据与维护说明

核对日期：**2026-11-02（Asia/Shanghai）**。本项目独立实现客户端协议，不复制或保存用户的学校密码。

## 1. CLIProxyAPI

对照 [CLIProxyAPI 提交 f7331b5197730226829cab32c31fb4ce546ef8fc](https://github.com/router-for-me/CLIProxyAPI/tree/f7331b5197730226829cab32c31fb4ce546ef8fc)：

- `examples/plugin/auth/go/main.go`：动态库入口 `cliproxy_plugin_init`、宿主/插件 buffer 所有权、函数指针表。
- `sdk/pluginabi/types.go`：C ABI 1、JSON envelope、错误 HTTP 状态。
- `sdk/pluginapi/types.go` 与 `internal/pluginhost/rpc_schema.go`：JSON 字段大小写、`[]byte` 的 base64 编码、`config_yaml`。
- `internal/pluginhost/rpc_client_stream.go` 与 `stream_bridge.go`：宿主传入 `stream_id`，插件异步调用 `host.stream.emit` / `host.stream.close`。
- `internal/pluginhost/adapters_executors_deframe.go`：宿主重建 SSE 事件边界；插件保持原始响应字节，不自行改写工具调用片段。

凭据型模型使用 `executor_model_scope: oauth`，其中 `oauth` 是 CPA 的“按 auth 绑定模型”分类；本插件的手动 API Key 也作为单独的 provider auth 文件接入。静态目录为空，真实目录来自该账号。

### 错误与冷却回传

执行失败通过 ABI envelope 的 `error.http_status`、`error.retry_after_seconds` 返回；流式开始后通过 `host.stream.close` 的 `error_detail` 返回相同结构，并保留兼容旧宿主的安全 `error` 文案。由 CPA executor / auth manager 记录结果、错误状态、冷却及故障切换，插件不自行修改 CPA 的调度状态。宿主提供的 `host.routing.reset_cooldown` 用于重置，而不是执行失败时手动设置冷却。

实测模型网关对预算耗尽返回 HTTP 422，封包为 `error.code: "422"`、`error.type: "budget_exceeded"`，且可能无任何重试提示。只对这种明确的预算标识将 422 规范化为 429，保留实际校验错误的 422。HTTP 200 JSON / SSE 中同样识别。优先上游重试提示，否则使用公开配置 `budget-cooldown`（默认 `30m`，允许 `1s`–`168h`），不臆测学校窗口重置时刻。

该版宿主的 `rpcError` 仅保留状态和重试提示，没有实现 auth manager 识别凭据级错误所需的 `IsCredentialScoped()`；无提示时宿主的 quota backoff 从 1 秒指数增长、最高 30 分钟，选择阶段还有 10 秒下限，不能当成学校预算重置周期。插件因此在现有账号缓存中保留预算截止时间，冷却时阻止该 CPA 凭据跨模型再次请求上游，并回传向上取整的剩余秒数，让 CPA 继续负责选取其他凭据。并发失败复用首个仍有效的窗口，不滑动延长截止时间；普通 429、校验错误和认证错误不触发此全凭据保护。此保护仅存在当前插件实例，重启或配置变更会清除；宿主“重置冷却”只重置宿主调度，不清除此保护，更不重置学校预算。多个凭据即使实际共享学校账户预算，也不会在无法验证关联时被自动合并。

不能仅根据 HTTP 200 判断成功。普通 JSON 中的 `error`、SSE `data` 中的错误封包或 `event: error` 都必须转成结构化失败；SSE 按完整事件识别错误，其余事件保留原字节增量转发，单事件最多 8 MiB。只提取合法状态、已知错误码与重试提示，不返回上游错误文案。`Retry-After` 同时接受秒数和 HTTP 日期，日期转换向上取整。未知错误按 502 回传，不根据任意文案臆造 429。

插件网页的账号列表保留 CPA 原生的 `status_message`、`unavailable`、`next_retry_after`，而不是只显示 `status`。这些是宿主账号汇总；多个模型的可用性与冷却仍由 CPA 自己按模型维护。

插件只声明 OpenAI Chat Completions 输入/输出，ABI 中的规范格式标识是 **`openai`**；`chat-completions` 为宿主接受的别名，调用插件时会被规范化。Anthropic Messages、Responses 等协议由宿主已有 translator 处理；插件不声称学校网关直接支持这些接口。

宿主要求 `GitHubRepository` 非空；默认填写本项目源码仓库 `https://github.com/njzjz/cpa-ustc`，从 fork 构建时可通过 `REPOSITORY` 参数指定对应仓库，不冒充 CPA 或学校官方项目。

### 网页与号池接入

声明 `management_api`，通过 `management.register` 的 `resources` 注册 `/settings` 菜单及 JS / CSS。CPA 将菜单暴露在 `/v0/resource/plugins/ustc/settings`，无需修改宿主源码或额外 Web 服务。采用 **RPC schema 6**：此前版本会对管理响应 JSON 的字符串进行 HTML 实体转义，会破坏 OAuth URL 中的 `&` 和账号备注，因此不能沿用 schema 1。C ABI 仍为 1。

对照上游 `sdk/pluginapi/types.go`、`internal/pluginhost/management.go`、`examples/plugin/host-callback-auth-files/go/main.go`，资源路由**不经过管理鉴权**。因此只在资源路由提供静态页面，所有配置读取、Key 导入、账号列表及 iWAN 操作都放在 `/v0/management/plugins/ustc/…`，由宿主管理中间件鉴权。请求正文限 64 KiB，修改操作只接受 JSON；响应禁止缓存，页面 CSP 不允许内联脚本或任意外部资源。

管理中心前端对照 `router-for-me/Cli-Proxy-API-Management-Center` 提交 `f154fa9380825c4fe94bc577a9abaa7e505f8875` 的 `src/features/plugins/PluginResourcePage.tsx`、`src/services/api/plugins.ts`：菜单使用普通 iframe，不传递管理密钥。页面要求管理员重新输入一次管理密钥，仅存于闭包内存，不读取或写入 localStorage / sessionStorage。

- 配置：先本地校验，再读取 `/v8/management/config/plugins/configs/ustc`，合并公共字段后 PUT 同一个插件对象，携带 ETag / If-Match；不覆盖其他提供商或插件。
- 科大账号：调用 CPA `/v8/management/oauth/auth-url?provider=ustc` 与 `/oauth/status`，由宿主调用本插件的登录能力并保存 auth。
- API Key：`host.auth.save` 保存为 `type: ustc` 的原生 auth 文件，宿主即时解析、注册凭据和模型。账号列表只使用 `host.auth.list` 的白名单展示字段。
- 账号禁用 / 删除：直接使用 CPA `/v8/management/credentials/status` 和 `/credentials`，不维护第二份账号状态，也不实现自定义 scheduler。
- 保存和刷新：provider auth JSON 保留 CPA-owned 路由字段；`auth.parse` 同步返回禁用、前缀及路由元数据。个人模型账号独立缓存和刷新，网络层 / iWAN 身份仍共享。

远程登录时，页面可以提交学校跳回浏览器本机的回调 URL。仅允许精确匹配该会话保存的 scheme / host / port / path 和一次性 state，转交插件原有回环回调，继续执行 PKCE、issuer、nonce 等验证；不根据用户提供的主机发起任意请求。

## 2. 科大 API Key 模式

官方依据：[公共服务平台 API 指南](https://llm.ustc.edu.cn/guide/api-usage/)。

```text
Base URL: https://api.llm.ustc.edu.cn/v1
GET /models
POST /chat/completions
Authorization: Bearer <API Key>
```

不硬编码模型名；不能根据旧版宣传页推断当前账号有权限使用哪个模型。

## 3. 词元工坊账号模式

公开分发信息来自 [词元工坊官网](https://id.ustc.edu.cn/tokenworks/)、官网更新清单及官方 `TokenWorks-0.2.106-windows-x64-electron.zip` 下载包。仅用 HTTP Range 读取 ZIP 目录和相关公开配置/源码，无需下载或执行整份桌面程序。相关包为 `@ustc/tokenworks-bootstrap`，其发行元数据标注 MIT。

### 信任锚

官方下载包 `resources/product/resources/desktop/publisher-bootstrap.json` 给出：

```text
publisher: ustc-tokenworks
content base: https://id.ustc.edu.cn/doc/claude/tokenworks-content/v2
manifest: <base>/stable/latest.json
Ed25519 SPKI: MCowBQYDK2VwAyEAQaBXGreNtCH3e1iRrPECbtZofjjB/OJefrBKgEpQ5lI=
```

清单以 base64url `payload` 和 Ed25519 `signature` 包装。先验签，再检查 publisher/channel、包 URL 路径、字节数、SHA-256；签名配置中 `ustc` 条目目前为：

```text
issuer: https://id.ustc.edu.cn/oidc
client_id: tokenworks
scopes: openid profile email offline_access
```

这些是公开桌面客户端参数，不是 Client Secret。本插件不伪装成官方产品发送 User-Agent；使用 `cpa-ustc/<version>`。学校可以调整客户端准入规则；授权拒绝时应联系学校或使用正式申请的 API Key，不能绕过服务器策略。

本版本对客户端标识采取 fail-closed 策略：签名配置变更了 issuer/client ID 时要求更新插件，不猜测新的认证参数。本地测试固定一份真实签名公共配置，因此也能离线验证信任锚。该配置不含用户凭据。

### 登录与模型凭据

1. 从 `<issuer>/.well-known/openid-configuration` 读取 OIDC 元数据，要求 issuer 完全一致，端点在固定官方 issuer 路径下。
2. 使用 Authorization Code + PKCE S256；随机本地回调 `http://127.0.0.1:<port>/oauth/callback` 与桌面实现一致。
3. 验证 callback state、授权响应 issuer、ID Token 签名/声明，以及 UserInfo 的 subject 一致性；refresh token 在账号级互斥锁下轮换。
4. 使用 access token 请求 **`https://id.ustc.edu.cn/ai/api/v1/bootstrap`**，不是拿 access token 直接请求模型。
5. 读取 `inferenceProvider=gateway`、`inferenceCredentialKind=static`、`inferenceGatewayBaseUrl`、`inferenceGatewayApiKey`、`inferenceGatewayAuthScheme=bearer` 和 `inferenceModels`。
6. `expiresAt` 是**数字时间戳，允许秒或毫秒**；与 `configRecheckIntervalMinutes` 取较早的刷新时刻。SSO 网关仅接受 HTTPS 的 `ustc.edu.cn` 域名，Key 只存在内存。
7. 模型请求走 `<gateway base>/v1/chat/completions`；base 已包含 `/v1` 时不重复拼接。

目前拒绝自定义 bootstrap HTTP 头、非 static 凭据种类和非 bearer 鉴权，防止对尚未实现的协议静默降级。配置过期、权限拒绝或 bootstrap 拉取失败时不会继续使用失效缓存。

通用 OIDC 实现参考上游 [EduWork 提交 8d05453f5095ee979666f8a7f5c910892f2fbbd5](https://github.com/ECNU/EduWork/tree/8d05453f5095ee979666f8a7f5c910892f2fbbd5) 的 `packages/dsh-oidc`；学校特有 bootstrap 依据为学校自己的下载包，不将通用 OIDC 登录误当作模型授权。

### 原生额度提供商

注册 `quota_provider`，实现 `quota.describe` / `quota.fetch` / `quota.reset`；只读查询声明 `supports_reset=false`。CPA 根据账号的 `ustc` 提供商标记原生 `supports_quota`，网页额度功能通过 `POST /v8/management/plugins/ustc/quota` 和 `auth_index` 查询，无需改 CPA 源码。额度 RPC 的 `storage_json` 使用 snake_case，不能沿用执行器的 `StorageJSON` 字段名。

依据同一份官方 `TokenWorks-0.2.106-windows-x64-electron.zip` 中 `@ustc/tokenworks-bootstrap/lib/index.js` 的 `usageUrl` / `fetchUsage()` 和 `@ustc/tokenworks-usage/lib/upstream.js`、`test/fixtures.mjs`：

- `GET https://id.ustc.edu.cn/ai/api/v1/usage` 使用统一认证的 Bearer access token，由学校代查平台额度快照，不传模型网关 Key，也不调用推理或猜测其他计费接口。
- `key.budget_windows[]` 给出 `budget_duration` / `max_budget` / `current_spend` / `reset_at`；`account` 给出 `scope` / `shared` / `budget_duration` / `max_budget` / `spend` / `reset_at`。数值允许 Decimal 的字符串形式，重置时间允许带时区 ISO 或 epoch 秒 / 毫秒。
- `status` / `usage_status` / `error_code` / `freshness` 决定快照是否可用；未知、缺失、非有限数值及不同步的金额不能解释成零。明确 `unlimited` 的窗口不伪造百分比，没有可显示有限窗口时提示到学校平台确认。
- 学校可能返回新鲜的 `status=partial`、`key.usage_status=partial`、`key.error_code=snapshot_incomplete`：例如 3h / 12h Key 窗口为 `ok`，但 24h account 的 `spend=null`、`status=unavailable`。这是聚合不完整，不是所有窗口过期；逐窗验证后保留有效窗口和摘要，省略未知窗口，并在所有已展示窗口标注部分数据未知。整体过旧、Key 同步错误或所有窗口无效仍报错，不用 0 / 100% 代替缺失值。
- Key 与共享账户分组展示，不合计预算。CPA 的 `remainingFraction` 限制在 `[0,1]`，超支的负余额仍在摘要和说明中保留。计费单位未作货币假设，不能换算成 token 数。
- 统一认证刷新与推理共用互斥状态，即使后续用量请求失败也将已轮换令牌保存给 CPA。请求最长 15 秒，正文最多 256 KiB，拒绝带凭据重定向；只输出白名单数值、窗口、重置时间与固定说明，不输出原始上游 JSON / Key 名称。

API Key 模式没有已确认的独立额度查询接口，返回明确的不可用提示，不尝试将 API Key 当身份令牌使用。官方包另有带副作用的 `quota-reset` 协议；本插件不实现，不把 CPA 调度冷却重置与学校预算重置混为一谈。

排查 502 时不能只看 Cloudflare 的错误 JSON：实测 CPA 自己返回的额度查询 502 也可能被 Cloudflare 替换正文，不等于 CPA 崩溃。`POST /v0/management/quota/fetch`（`auth_index`、`provider: ustc`、`plugin_id: ustc`）会在 CPA 日志记录底层错误；结合只读管理日志诊断，不通过重置预算或反复推理探测。

### 3.1 公开捐献与设备授权

开发版新增默认关闭的 `donation-mode: open` 和 `donation-origin`。使用已公布的 `tokenworks` public client，通过 OIDC discovery 的可信 `device_authorization_endpoint` 初始化 RFC 8628 授权；不猜测私有客户端、不需要 client secret 或自定义回调 URI。设备私密码仅留在服务器内存，浏览器只获取用户验证码和 `id.ustc.edu.cn` 上的授权链接。

令牌轮询在服务器强制执行 interval、`authorization_pending`、`slow_down`、网络 / 5xx 退避和失效截止时间。成功后验证 RS256/JWKS、issuer、audience / azp、有效期、签发时间、可选 `at_hash`、userinfo subject，并要求可自动刷新的令牌。授权成功仅进入待确认状态，不能自动写入号池。

公开页面及接口位于 `/v0/resource/plugins/ustc/donate` 前缀。当前 CPA 只提供 GET resource handlers，因此这些非缓存接口使用浏览器控制的同源 Fetch Metadata、与配置匹配的 origin、自定义请求头、host-only HttpOnly / SameSite=Strict Cookie、内存 CSRF 和每次授权独立的一次性确认码；不把任何会话凭据放在 URL，不开放管理接口。CSP 禁止第三方资源和 iframe 嵌入。公开资源回调在请求作用域内使用 `host.auth.list/get/save`，浏览器不能指定账号、文件名或令牌。

新记录只使用经学校验证身份派生的 `ustc-donation-…` ID，且不覆盖普通 SSO 记录或已有捐献设置。自助撤回要求新学校认证及确认，只保存本身份的 `donated` / `donation_revoked` / `disabled` 无令牌记录，并丢弃进程内对应缓存；已发出的请求不可追回。宿主没有 auth delete callback，因此不会伪称已从学校侧撤销 OAuth grant，也不从公开入口调用管理删除 API。

## 4. iWAN

[官方说明](https://mail.ustc.edu.cn/notice/iwan/)指出，客户域为 `iwan.ustc`，使用科大统一身份认证；隧道使用 UDP，常见端口为 6001/6002。官方客户端正常提供通路时无需重复安装第三方客户端。

本插件不重新实现 iWAN UDP 数据面，显式复用第三方 [ustc-iwan](https://github.com/yyy1mu/ustc-iwan)；对照提交 `d62318c2161d3299ae6f23e7f40899260f10e79a`，安装固定 release `v26.9.1`。调用只通过参数数组，不经过 shell：

```text
iwan-client-oidc --fetch --config-dir <state>/iwan
iwan-client-oidc --connect --server <line> --http \
  --http-listen 127.0.0.1:<random-port> --config-dir <state>/iwan
```

`servers.json` 的格式和线路凭据加密由上游维护；本插件不复制其密码解密逻辑。首次登录遵循上游的 mobile-scheme 回调复制流程，不能宣称完全无交互。网页在后台运行同一个 `--fetch` 客户端，只提取官方 iWAN 授权 URL，保留 helper 的 PKCE；额外校验回调 scheme、host、单次 state 和 code，并通过 stdin 传回，不将授权码放入进程参数或日志。后续后台代理进程不继承终端输入、不输出潜在凭据日志，退出后下一次请求重启；过期时用网页“登录 / 更新 iWAN”或 `iwan-login` 重新取线路凭据。

校园网检测端点为 **`https://id.ustc.edu.cn/doc/claude/netcheck`**，当前响应是 `{"campus":1}` / `{"campus":0}`，不是 JSON boolean。未知网络状态应报告错误，不能猜测自己在校内。

## 5. 实测范围

- Go 竞态测试与 `go vet`。
- 用本地测试 CA 模拟的 HTTPS 模型列表、非流式请求、429 状态、HTTP 200 错误封包及增量 SSE；检查 HTTP 日期重试提示、分片 / 未终止错误事件、大小限制和错误文案脱敏。
- 通过 Python ctypes 真正加载生成的 C ABI 动态库，验证宿主回调、buffer 释放、原生注册和执行。
- 可指定真实 CPA 可执行文件，对本地模拟网关验证插件加载、账号模型发现、Chat Completions / Responses / Anthropic Messages 转换以及 SSE 转发。
- 真实 CPA 管理接口验证插件菜单、静态资源、管理鉴权、Key 导入、原生双账号轮询、禁用 / 启用 / 删除和配置热更新；HTTP 200 SSE 内的 429 触发 CPA 原生冷却，凭据列表记录错误及截止时间，冷却中请求不再访问上游。可选 Chromium 验证网页表单、错误及冷却显示、凭据不落浏览器存储、备注与错误消息的 XSS 防护、模拟 iWAN 客户端的完整回调；不启动真实隧道。
- 可选在线检查只访问学校公开签名配置、OIDC discovery 和 netcheck，不涉及账号、额度或推理。

真实 SSO / 校外隧道仍须有权使用服务的用户完成验收。不得把模拟成功描述成真实账号登录或实际学校模型调用成功。
