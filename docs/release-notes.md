# USTC Token Plan v0.1.0

CLIProxyAPI 原生中科大词元计划提供商插件（`ustc`）。

- 科大统一身份认证或直接 API Key；无需把科大密码交给插件。
- 校园网直连，校外可托管 iWAN；网页支持安装客户端和完成首次登录。
- 内置网页设置、账号添加及管理；多个账号复用 CPA 原生号池。
- 支持模型发现、非流式及 SSE 流式推理。

## 安装

- 预编译平台：**Linux x86_64 / ARM64、glibc 2.36+**，兼容 CPA 官方 Debian Bookworm 容器；不支持 Alpine / musl。
- 安装包：`ustc_0.1.0_linux_amd64.zip` 和 `ustc_0.1.0_linux_arm64.zip`，根目录均包含对应架构的 `ustc.so`。
- 两种架构均在原生运行器上构建，执行 Go 竞态检查、原生 ABI 及真实 CPA 宿主集成测试。
- `checksums.txt` 包含 zip 的 SHA-256；CPA 插件商店会校验摘要。
- 商店收录前，可添加自定义插件源 `https://raw.githubusercontent.com/njzjz/cpa-ustc/master/registry.json`；收录后直接在官方商店搜索 USTC Token Plan。
- 启用插件后，在“插件 → 中科大词元计划”中设置网络并添加账号；不需要额外运行代理或 Web 服务。

需要支持 C ABI 1 / RPC schema 6、Management API 和资源菜单的 CPA，推荐使用当前 v8 系列；开发验收基准为上游提交 `f7331b5197730226829cab32c31fb4ce546ef8fc`。首次进入插件网页，需要手动输入 CPA 管理密钥；首次 iWAN 登录需要将浏览器回调粘贴回网页。

此为社区插件，并非学校官方客户端。账号资格、模型权限与额度由学校决定；本地测试不消耗真实账号额度。认证令牌与手动导入的 Key 由 CPA auth-dir 保存，请保护该目录及其备份。第三方 iWAN 客户端只在用户明确操作后安装。
