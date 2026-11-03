# USTC Token Plan v0.1.4

CLIProxyAPI 原生中科大词元计划提供商插件（`ustc`）。

## 商店审查修复

- 补充 Darwin amd64 / arm64、Windows amd64 原生构建与实际 ABI 加载测试，保留 Linux amd64 / arm64 Bookworm 构建；发布前强制校验五个平台、动态库架构、ZIP 根目录及 SHA-256，任一缺失则不发布。
- **安全迁移**：移除所有 `/v0/resource/plugins/ustc/donate*` 路径，公开 CPA resource 仅提供静态设置资源；捐献状态机及号池回调只在已鉴权的管理 POST 路由执行。
- 新增独立 `cpa-ustc serve-donations` 受限网关，服务器环境变量私有持有 CPA 管理密钥；捐献者仍只需学校 OAuth，不能访问通用管理 API。新捐献网址为网关根路径。旧 Nginx 转发必须迁移，详见 `docs/donation-domain.md`。
- 独立 companion CLI ZIP 与插件库 ZIP 分开发布；Windows 私有凭据使用受保护的用户专属 DACL，补充对应测试。
- 五个平台的测试、实际 ABI 加载及产物完整性是发布门禁；Linux 额外验证真实 CPA 宿主，CI 使用 Chromium 验证管理设置和独立网关。测试使用本地模拟身份与网关，不登录真实学校账号或消耗学校模型额度。发布不表示已被官方商店收录。

## v0.1.3 历史记录（部署方式已被上述修复取代）

- 新增可选的公开捐献网页：捐献者通过学校官方 OAuth 设备授权、核对身份并确认后加入 CPA 原生号池，不需要 CPA 管理密钥、客户端 Key 或手动申请学校 API Key。
- 捐献入口默认关闭。v0.1.3 原公开资源路由已移除，升级必须按上述方式迁移到独立网关。
- 支持重新通过学校认证撤回自己的捐献：停用相应凭据并清除令牌及缓存，保留无令牌的停用记录。不会覆盖管理员添加的普通账号，重复捐献也不会覆盖原有禁用状态或优先级。
- 修复插件设置页保存时的 HTTP 412 / `precondition_failed`：兼容代理对 CPA 配置 ETag 添加 `W/` 前缀；并发修改时重新读取、合并并最多尝试 3 次。缺失或未知 ETag 时拒绝无条件覆盖，失败保留表单内容。此修复不修改 CPA 管理中心自己的通用配置弹窗。
- 新增设备授权、捐献生命周期及配置版本冲突的回归测试，包含真实 CPA 与浏览器验收；测试不使用真实学校身份或消耗模型额度。

## 支持范围与已知限制

- 继续支持科大统一身份认证、直接 API Key、自动 iWAN、网页设置，以及 CPA 原生多账号号池。无需向插件提交科大密码。
- **捐献是模型调用授权，不是额度转账。** CPA 运营者保存登录及自动刷新令牌，可消耗学校授权范围内的额度；没有独立的捐献消费上限。请确认学校允许共享使用，并仅向可信运营者授权。
- 撤回受宿主 ABI 限制保留停用记录，重新捐献须由管理员先删除该记录；不会撤销学校侧 OAuth grant、清理历史备份或追回已发出的请求。真人学校 OAuth 登录尚未完成端到端验证。
- 公开入口当前限制为最多 64 个会话、全站每分钟 6 次设备授权初始化、最多 256 个捐献记录（含已撤回记录）。公网部署仍应增加反向代理防滥用措施。
- **额度查询目前仅支持统一认证账号。** 直接导入的 API Key / 环境变量 Key 没有接入仅凭 Key 查询余额的接口；CPA 可能把“不支持查询”的错误显示为 502，这不代表该 Key 没有额度。
- 统一认证账号的额度仅对应学校接口返回的关联 Key / 账户，不会自动查询其他独立导入的 API Key。
- 预算保护不会重置学校额度；401 鉴权失败也不等同于额度耗尽。若账号加载时查询模型列表就被学校拒绝，CPA 可能没有可供该账号注册的模型，需要恢复权限 / 额度后重新加载。

## 安装

- 发布平台：**Darwin amd64 / arm64、Linux amd64 / arm64、Windows amd64**。Linux 要求 glibc 2.36+，兼容 CPA 官方 Debian Bookworm 容器，不支持 Alpine / musl。
- 插件安装包为 `ustc_0.1.4_<goos>_<goarch>.zip`，根目录只有对应的 `ustc.dylib` / `ustc.so` / `ustc.dll`；独立 CLI 另见 `cpa-ustc_0.1.4_<goos>_<goarch>.zip`。
- 每个平台均在原生运行器执行 Go 竞态检查、vet、构建和实际 ABI 加载。Linux 执行真实 CPA 宿主集成测试；macOS / Windows 的真人学校登录和 iWAN 连接仍未实机验证。
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
