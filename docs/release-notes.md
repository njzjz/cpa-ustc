# USTC Token Plan v0.1.2

CLIProxyAPI 原生中科大词元计划提供商插件（`ustc`）。

## 本次更新

- 新增统一认证账号的原生额度查询，在 CPA 额度页和插件账号列表展示学校返回的预算窗口、余额及重置时间；区分 Key 限额与账户共享额度。
- 修复学校返回新鲜但不完整的额度快照时查询失败：保留有效窗口并提示部分数据未知，不把缺失或过期的余额伪装成零或满额。
- 将学校返回的 HTTP 422 `budget_exceeded` 归一化为 429；继续识别 HTTP 200 JSON / SSE 中的结构化错误，让 CPA 记录失败、冷却并切换账号。
- 新增 `budget-cooldown` 网页 / YAML / CLI 配置：优先采用上游重试提示，无提示时默认保护 30 分钟；同一账号跨模型共享预算保护，避免切换模型后继续请求已耗尽的账号。号池调度仍由 CPA 负责。
- 保留 `Retry-After` 秒数与 HTTP 日期、最近错误和冷却截止时间；额度查询中的令牌轮换即使遇到后续错误也会保存。

## 支持范围与已知限制

- 继续支持科大统一身份认证、直接 API Key、自动 iWAN、网页设置，以及 CPA 原生多账号号池。无需向插件提交科大密码。
- **额度查询目前仅支持统一认证账号。** 直接导入的 API Key / 环境变量 Key 没有接入仅凭 Key 查询余额的接口；CPA 可能把“不支持查询”的错误显示为 502，这不代表该 Key 没有额度，也不是本次快照修复的适用场景。
- 统一认证账号的额度仅对应学校接口返回的关联 Key / 账户，不会自动查询其他独立导入的 API Key。
- 预算保护不会重置学校额度；401 鉴权失败也不等同于额度耗尽。若账号加载时查询模型列表就被学校拒绝，CPA 可能没有可供该账号注册的模型，需要恢复权限 / 额度后重新加载。

## 安装

- 预编译平台：**Linux x86_64 / ARM64、glibc 2.36+**，兼容 CPA 官方 Debian Bookworm 容器；不支持 Alpine / musl。
- 安装包：`ustc_0.1.2_linux_amd64.zip` 和 `ustc_0.1.2_linux_arm64.zip`，根目录均包含对应架构的 `ustc.so`。
- 两种架构均在原生运行器上构建，执行 Go 竞态检查、原生 ABI 及真实 CPA 宿主集成测试。
- `checksums.txt` 包含 zip 的 SHA-256；CPA 插件商店会校验摘要。
- 商店收录前，可添加自定义插件源 `https://raw.githubusercontent.com/njzjz/cpa-ustc/master/registry.json`；收录后直接在官方商店搜索 USTC Token Plan。
- 启用插件后，在“插件 → 中科大词元计划”中设置网络并添加账号；不需要额外运行代理或 Web 服务。
- 从旧版本升级后，在 CPA 中重新加载插件；如果宿主无法热重载动态库，重启 CPA。现有账号和配置可以继续使用。

需要支持 C ABI 1 / RPC schema 6、Management API 和资源菜单的 CPA，推荐使用当前 v8 系列；开发验收基准为上游提交 `f7331b5197730226829cab32c31fb4ce546ef8fc`。首次进入插件网页，需要手动输入 CPA 管理密钥；首次 iWAN 登录需要将浏览器回调粘贴回网页。

此为社区插件，并非学校官方客户端。账号资格、模型权限与额度由学校决定；本地测试不消耗真实账号额度。认证令牌与手动导入的 Key 由 CPA auth-dir 保存，请保护该目录及其备份。第三方 iWAN 客户端只在用户明确操作后安装。

## AI Attribution

```text
Coding agent: Codex
Codex version: codex-cli 0.166.0
Model: gpt-6.1-sol
Reasoning effort: xhigh
```
