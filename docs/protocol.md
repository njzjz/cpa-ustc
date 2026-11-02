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

插件只声明 OpenAI Chat Completions 输入/输出，ABI 中的规范格式标识是 **`openai`**；`chat-completions` 为宿主接受的别名，调用插件时会被规范化。Anthropic Messages、Responses 等协议由宿主已有 translator 处理；插件不声称学校网关直接支持这些接口。

宿主要求 `GitHubRepository` 非空；默认填写本项目源码仓库 `https://github.com/njzjz-bot/cpa-ustc`，从 fork 构建时可通过 `REPOSITORY` 参数指定对应仓库，不冒充 CPA 或学校官方项目。

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
- 用本地测试 CA 模拟的 HTTPS 模型列表、非流式请求、429 状态及增量 SSE。
- 通过 Python ctypes 真正加载生成的 C ABI 动态库，验证宿主回调、buffer 释放、原生注册和执行。
- 可指定真实 CPA 可执行文件，对本地模拟网关验证插件加载、账号模型发现、Chat Completions / Responses / Anthropic Messages 转换以及 SSE 转发。
- 真实 CPA 管理接口验证插件菜单、静态资源、管理鉴权、Key 导入、原生双账号轮询、禁用 / 启用 / 删除和配置热更新。可选 Chromium 验证网页表单、凭据不落浏览器存储、备注 XSS 防护、模拟 iWAN 客户端的完整回调；不启动真实隧道。
- 可选在线检查只访问学校公开签名配置、OIDC discovery 和 netcheck，不涉及账号、额度或推理。

真实 SSO / 校外隧道仍须有权使用服务的用户完成验收。不得把模拟成功描述成真实账号登录或实际学校模型调用成功。
