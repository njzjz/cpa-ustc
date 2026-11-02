# cpa-ustc：CLIProxyAPI 中科大词元计划插件

通过科大统一身份认证使用词元工坊的学校模型，也可以直接使用大模型公共服务平台 API Key。**这是 CLIProxyAPI 原生 C ABI 动态库插件，不需要另起一个 OpenAI 兼容代理服务，也不用修改 CLIProxyAPI 源码。**

## 功能

- **科大账号登录**：在官方浏览器页面完成密码、通行密钥及多因素认证；自动获取学校下发的模型网关凭据和模型目录，不要求手动申请 API Key。
- **直接 API Key**：支持读取环境变量，或者从标准输入导入，避免把 Key 放进命令行参数、历史记录或普通 YAML。
- **自动 iWAN**：校园网内直连；校外自动启动免 root 的 iWAN HTTP 代理，仅本插件的模型请求经过该代理，不修改系统路由或其他程序的代理设置。
- **自动配置 CPA**：安装动态库、写入凭据、合并现有 YAML；保留其他提供商与插件，修改前备份原配置。
- **原生提供商能力**：账号解析、浏览器登录、登录轮询、刷新令牌、按账号发现模型、非流式及 SSE 流式请求。其他客户端协议的转换交给 CPA。
- **CPA 网页设置**：注册“中科大词元计划”插件菜单，网页编辑网络配置、添加账号 / Key、安装和登录 iWAN；无需另外运行 Web 服务。
- **原生多账号号池**：每个账号都是 CPA 的 `ustc` 提供商凭据；选择、轮询、优先级、禁用和删除沿用 CPA，不另建账号池或调度器。
- **原生错误与冷却**：HTTP 错误及 HTTP 200 JSON / SSE 内的错误通过插件 ABI 回传状态和重试提示，由 CPA 记录错误、决定按模型 / 凭据冷却及切换账号；网页展示 CPA 的账号状态、最近错误和冷却截止时间。

学校账号是否有资格使用服务、可用模型和额度，都以学校返回的结果为准；插件不会绕过权限、额度或 MFA。本项目不是学校官方客户端。

## 快速开始

### 预编译安装包与插件商店

[GitHub Releases](https://github.com/njzjz/cpa-ustc/releases) 提供 `ustc_<版本>_linux_amd64.zip`、`ustc_<版本>_linux_arm64.zip` 和 `checksums.txt`。预编译包支持 **Linux x86_64 / ARM64、glibc 2.36+**，可用于 CPA 官方 Debian Bookworm 容器；不适用于 Alpine / musl。两个架构分别在原生运行器上编译，执行竞态检查和真实 CPA 宿主加载测试。macOS、Windows 和其他架构暂需自行构建，尚无对应的商店安装包。

官方商店收录后，可在 CPA 管理中心的“插件商店”搜索 **USTC Token Plan** 或 **中科大词元计划**并安装。收录前可将本项目的单插件索引添加为 CPA 自定义插件源：

```text
https://raw.githubusercontent.com/njzjz/cpa-ustc/master/registry.json
```

也可以下载 zip，核对 `checksums.txt`，将根目录的 `ustc.so` 放入 CPA 插件目录并按 CPA 的插件配置方式启用。不要将 zip 本身当作动态库加载。

安装并启用后，进入“插件 → 中科大词元计划”，按下文“在 CPA 网页中完成设置与登录”操作。**通过商店安装无需命令行辅助程序**；后续命令行步骤仅供从源码构建和手动部署时使用。安装新插件后如未出现菜单，刷新管理页面并检查 CPA 的插件加载状态。

### 1. 构建

需要 Go 1.23+、C 编译器和 `make`；有 `ccache` 时构建自动复用它。

```bash
make build
```

Linux 产物为 `dist/cpa-ustc` 和 `dist/ustc.so`；macOS 动态库为 `ustc.dylib`。动态库通过 **`-buildmode=c-shared`** 构建，不是 Go 的 `-buildmode=plugin`，也没有绑定 CPA 的 Go 模块版本。

构建默认写入本项目的源码仓库标识 `https://github.com/njzjz/cpa-ustc`。从自己的 fork 构建时，可用 `make build REPOSITORY=https://github.com/你的账号/你的仓库` 指定对应的插件源码地址。

**CPA 需要支持原生动态库插件和 Management API / 菜单资源。** 开发时对照的是上游主线提交 `f7331b5197730226829cab32c31fb4ce546ef8fc`，C ABI 1 / RPC schema 6。旧版如果没有 `plugins` 功能，需要先升级。管理中心也需支持插件菜单。已在 Linux 验证构建、动态加载及 Chromium 网页操作；macOS / Windows 的真实登录与网络连接尚未实机验证。

### 2. 推荐：在 CPA 网页中完成设置与登录

已有 CPA 管理中心时，只安装插件，不先在命令行登录：

```bash
./dist/cpa-ustc install --cpa-config /path/to/CLIProxyAPI/config.yaml
```

启动或重启 CPA，刷新管理中心。在插件资源菜单中打开 **“中科大词元计划”**；也可直接访问 CPA 同一站点下的 `/v0/resource/plugins/ustc/settings`。

1. 填写 **CPA 管理密钥**建立连接，不是客户端访问 Key 或科大 API Key。密钥仅在当前页面内存中；断开、刷新或关闭页面后清除。上游管理中心目前用普通 iframe 加载资源，没有提供管理密钥传递桥接；插件不读取它的私有存储。
2. 在“网络与插件设置”里选择 `auto` / `direct` / `iwan`，设置线路、客户端路径或已有代理，并点击“保存设置”。写入的是 **CPA 自己的插件配置**，保留该插件的其他字段，使用 ETag 防止并发覆盖，并由 CPA 热更新。通用插件配置弹窗也可编辑这些字段。
3. 校外需要托管 iWAN 时，可在网页显式安装第三方客户端，保存返回的客户端路径，再点击“登录 / 更新 iWAN”。浏览器完成授权后，将 `com.panabit.mobile://…` 回调粘贴到网页；不用返回终端。
4. 点击“添加科大账号”，打开学校官方认证页面；页面通过 **CPA 原生 OAuth 会话接口**轮询，完成后由 CPA 保存凭据并加入号池。远程 / Docker 部署若无法自动回调，可展开“远程 / Docker 部署”，粘贴浏览器地址栏中的完整本机回调 URL。
5. 或在“添加 API Key”中填写 Key / 环境变量名（二选一）。可以重复添加不同账号；网页列表从 CPA 自身读取，启用、禁用和删除直接调用 CPA 凭据管理接口。

所有模型账号**共用一条 iWAN 通路**，不要求每个模型账号分别登录 iWAN。添加另一个科大账号时，需要在学校认证页面切换浏览器登录身份。号池的轮询策略、优先级等在 CPA 中统一管理；刷新令牌保留 CPA 的 `disabled`、`priority`、`prefix`、`weight` 等凭据字段。

网页操作使用同站点 API。远程访问请使用 HTTPS 或 SSH 隧道，并遵循 CPA 的远程管理权限设置；插件不会放宽 CPA 的管理鉴权。`install` 不会擅自启用远程管理或生成 / 更改已有管理密钥。若新建 CPA 配置，需要先按 CPA 的文档启用管理中心。

### 3. 可选：命令行登录并完成配置

在校园网内，或者已经连接官方 iWAN 客户端：

```bash
./dist/cpa-ustc setup --cpa-config /path/to/CLIProxyAPI/config.yaml
```

程序打开科大官方认证页面；登录完成后，自动取得学校模型配置、安装动态库，并写入 CPA 配置。已有浏览器登录状态或通行密钥可以直接复用，插件不接触科大密码。

校外，希望由插件管理 iWAN：

```bash
./dist/cpa-ustc setup --install-iwan --cpa-config /path/to/CLIProxyAPI/config.yaml
```

`--install-iwan` **显式安装第三方** [ustc-iwan](https://github.com/yyy1mu/ustc-iwan) 的固定版本 `v26.9.1`，校验 GitHub 发布附件的 SHA-256；缺少摘要则拒绝安装。只安装到用户状态目录，不需要 root。已有客户端可用 `--iwan-bin /path/to/iwan-client-oidc` 指定，无需下载。

**首次 iWAN 登录仍需一次人工交互**：上游命令行客户端会输出学校登录链接；浏览器认证后，取消打开 iWAN App 的提示，把页面按钮的 `com.panabit.mobile://...` 回调链接复制回终端。这不是要求手动填写网关配置；后续 CPA 自动复用线路凭据并启动、停止代理。词元工坊账号登录本身使用自动本机回调，不需要复制授权码。

最后启动或重启 CLIProxyAPI。新建的 CPA 配置默认只监听 `127.0.0.1:8317`，并生成随机客户端 `api-keys`，客户端使用这枚 **CPA Key**，不是科大 Key。已有配置的监听地址、端口和客户端 Key 保持不变。

### 4. 命令行直接 API Key

推荐让 CPA 进程读取环境变量：

```bash
export USTC_API_KEY='你的科大 API Key'
./dist/cpa-ustc setup --api-key-env USTC_API_KEY \
  --cpa-config /path/to/CLIProxyAPI/config.yaml
```

环境变量引用写入凭据，**实际 Key 不落盘**。启动 CPA 的进程也必须继承该变量；在 systemd / Docker 中，需要给服务本身设置环境变量。不要只给 setup 子进程临时设置。

或者从标准输入导入，Key 保存在权限为 `0600` 的 CPA 凭据文件：

```bash
printf '%s' "$USTC_API_KEY" | ./dist/cpa-ustc setup --api-key-stdin \
  --cpa-config /path/to/CLIProxyAPI/config.yaml
```

两种方式都支持 iWAN。首次校外使用可加 `--install-iwan`；管道输入且没有交互终端时，应先运行 `iwan-login` 完成 iWAN 登录。校园网或官方客户端已提供通路时无需额外安装。

默认 API 地址为 `https://api.llm.ustc.edu.cn/v1`；可用 `--api-base` 指定平台公布的新 HTTPS 地址。API Key 模式不会调用词元工坊 bootstrap 接口。

## 常用命令

```bash
# 只检查公共网络状态，不发送账号凭据、不调用模型
./dist/cpa-ustc doctor

# 列出账号实际有权调用的模型，而不是硬编码猜测模型名称
./dist/cpa-ustc models --cpa-config /path/to/config.yaml

# 多账号时显式选择凭据
./dist/cpa-ustc models --account ~/.cli-proxy-api/ustc-key-xxxx.json

# 登录另一账号，不重复安装动态库
./dist/cpa-ustc login --cpa-config /path/to/config.yaml

# 只安装插件；所有登录和网络设置留到 CPA 网页完成
./dist/cpa-ustc install --cpa-config /path/to/config.yaml

# iWAN 线路凭据过期时重新认证；不是重新申请 API Key
./dist/cpa-ustc iwan-login

# 指定 iWAN 线路（序号或名称，以该账号线路列表为准）
./dist/cpa-ustc setup --iwan-server 电信 --cpa-config /path/to/config.yaml

# 明确直连，不做校园网检测；适合系统级官方 iWAN 已连接等场景
./dist/cpa-ustc setup --network direct --cpa-config /path/to/config.yaml

# 使用已有本地代理，不启动托管 iWAN 子进程
./dist/cpa-ustc setup --proxy-url http://127.0.0.1:8080

./dist/cpa-ustc setup --help
```

模型调用仍使用标准 CPA 地址，例如 `POST /v1/chat/completions`。`model` 使用 `models` 命令列出的标识。工具调用、图片和推理参数保持为上游的 OpenAI 兼容 JSON，由 CPA 和学校网关决定支持范围。

## 自动写入的配置

无需手工编写，示意如下；路径会转换为当前机器的实际绝对路径：

```yaml
plugins:
  enabled: true
  dir: /absolute/path/to/plugins
  configs:
    ustc:
      enabled: true
      priority: 10
      network: auto
      state-dir: /home/user/.config/cpa-ustc
      iwan-binary: /home/user/.config/cpa-ustc/bin/iwan-client-oidc
      iwan-server: "1"
      proxy-url: ""
      api-base: https://api.llm.ustc.edu.cn/v1
```

- `network: auto`：使用学校 `netcheck` 判断是否在校园网，缓存 30 秒；明确在校外才启动 iWAN。检测失败时报告错误，不将带凭据的请求静默发给其他代理。
- `network: direct`：明确直连。
- `network: iwan`：始终使用托管 iWAN。
- `proxy-url`：显式已有代理，优先级最高；支持 HTTP(S)、SOCKS5 / SOCKS5h。
- 动态库文件名必须为 `ustc.so` / `ustc.dylib` / `ustc.dll`，使 CPA 的插件 ID 与配置键一致。

只覆盖本插件对应的配置字段；其他插件和提供商不删除。原 YAML 的完整字节备份到同目录 `config.yaml.cpa-ustc-<时间>.bak`。格式错误或关键字段类型不正确时停止，不强行重写。

默认公开设置保存到系统用户配置目录下的 `cpa-ustc/config.json`。`--state-dir` 可覆盖状态目录；使用自定义目录后，后续 CLI 命令也应传入同一 `--state-dir`。CPA 通过插件配置中的绝对路径访问对应的 iWAN 凭据。

网页修改以 CPA 的 YAML 为准，不同步覆盖 CLI 的 `config.json`；之后再次运行 CLI `setup` 时，应显式传入需要沿用的设置，避免用旧的 CLI 默认值覆盖网页配置。

### 远程主机 / 无桌面环境

登录回调只绑定本机回环地址，不开放公网。固定端口并做 SSH 转发，例如先在自己的电脑运行：

```bash
ssh -L 18765:127.0.0.1:18765 user@server
```

然后在远程主机执行：

```bash
./dist/cpa-ustc setup --no-browser --callback-port 18765 --cpa-config /path/to/config.yaml
```

在自己的浏览器打开输出链接；回调通过 SSH 返回远程主机。iWAN 首次登录的链接同样在自己浏览器打开，并按提示复制回调到远程终端。

## 安全与边界

- 只在 `id.ustc.edu.cn` 官方认证页面输入密码；不实现账号密码爬虫，不绕过验证码、MFA 或访问权限。
- 登录前验证学校签名公共配置的 Ed25519 签名、包大小和 SHA-256；OIDC 使用 PKCE S256、一次性 state、nonce、RS256/JWKS、issuer/audience、ID Token 时间和 `at_hash` 验证。
- OAuth 身份令牌只发给官方认证 / bootstrap；**不会直接当作模型 API Key**。学校下发的模型网关 Key 仅缓存在进程内，按配置有效期重新拉取。
- OAuth access/refresh token 或手动导入的 API Key 保存于 CPA auth-dir，使用原子写入及 `0600` 权限。它们**不是操作系统密钥环加密存储**，请保护该目录和备份。Windows 还需按本机账户策略设置目录 ACL。
- 不把上游错误响应正文、Cookie、客户端 Authorization 或任意请求头转发到日志 / 学校模型接口。带凭据的请求不跟随重定向。
- JSON / SSE 错误只保留合法 HTTP 状态、已知错误类型和重试时间，显示固定的安全诊断，不展示上游原文；未知错误按 502 上报，不根据任意错误文案猜测限流。SSE 按事件增量转发，单事件上限 8 MiB。已有内容发给客户端后发生限流，仍通过结构化流错误通知 CPA，而不能回改已发送的 HTTP 状态。
- 不修改 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`，也不继承它们进行学校访问；只使用显式配置的代理。
- 插件自身不重试推理 POST，以免重复计费；CPA 宿主的账号轮询 / 重试仍由 CPA 配置控制。iWAN 子进程退出后，在下一次请求时重新启动。单个模型请求最多 10 分钟。
- 学校没有公布精确词元计数接口，插件明确返回 501，不伪造计数；不提供任意 URL 的 executor HTTP 转发。
- 只管理自己启动的 iWAN 子进程；正常卸载 / 关闭时回收它，不停止已运行的官方 VPN。
- 插件菜单 HTML / JS / CSS 是不含凭据的公开静态资源；配置、账号及 iWAN 操作位于 CPA 鉴权保护的管理路由。账号列表只返回必要的展示字段，不返回令牌、Key 或宿主凭据路径。
- iWAN 浏览器流程只提取可信域名的授权 URL，不回传客户端日志；校验一次性回调 state。远程科大回调只接受当前会话的原始回环地址，不能指定任意代理目标。

## 测试

```bash
make test                       # Go 单元测试及并发竞态检查
make check                      # go vet
make build
python3 tests/native_smoke.py    # 真正 dlopen 动态库，模拟 HTTPS 网关及增量 SSE

# 可选：用已构建的真实 CPA 宿主验证注册、模型发现、协议转换及流式调用
CPA_USTC_HOST_BINARY=/path/to/cli-proxy-api python3 tests/native_smoke.py

# 可选：真实 CPA + 无 GPU Chromium，验证网页表单、账号禁用/删除和模拟 iWAN 登录
# Playwright 安装在仓库外；可设置 CPA_USTC_BROWSER 使用已安装的 Chromium
npm install --prefix /tmp/cpa-ustc-browser playwright@1.61.1
/tmp/cpa-ustc-browser/node_modules/.bin/playwright install chromium
CPA_USTC_HOST_BINARY=/path/to/cli-proxy-api \
  CPA_USTC_PLAYWRIGHT_MODULE=/tmp/cpa-ustc-browser/node_modules/playwright \
  python3 tests/native_smoke.py

# 可选：只访问学校公开元数据；不登录、不发推理请求
CPA_USTC_LIVE=1 go test -v ./internal/ustc -run TestPublishedProfileLive
```

离线测试包含签名配置、PKCE / state / issuer、ID Token、防重定向泄密、刷新令牌并发合并、数字时间戳 bootstrap、校园网检测格式、iWAN 子进程重启回收、环境变量 Key 不落盘、配置合并备份和原生 ABI。原生烟测需要 Python 3 与 OpenSSL，用临时测试 CA；不会关闭 TLS 校验。真实宿主集成同样使用本地模拟模型网关；宿主本身可能进行它自己的版本检查。

真实宿主测试还验证插件菜单发现、资源访问、管理鉴权、API Key 从网页管理接口入池、两个凭据由 CPA 原生轮询、原生禁用 / 启用 / 删除、配置保存和热更新。可选浏览器测试验证实际表单点击、账号备注的 XSS 防护、密钥不写浏览器存储，以及由假客户端完成的 iWAN 回调；不启动真实隧道。

尚未用真实科大账号验证 SSO 授权、学校实际推理或校外 iWAN 隧道；这些步骤需要用户授权账号及对应网络环境，模拟测试不能替代实测。

## 资料

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 与其 `examples/plugin` 原生插件示例。
- [科大大模型公共服务平台指南](https://llm.ustc.edu.cn/guide/)。
- [词元工坊](https://id.ustc.edu.cn/tokenworks/) 与官方下载包中的公开认证 / bootstrap 实现。
- [学校 iWAN 使用说明](https://mail.ustc.edu.cn/notice/iwan/)。
- [ustc-iwan 命令行客户端](https://github.com/yyy1mu/ustc-iwan)。
- 实现核对和协议边界见 [docs/protocol.md](docs/protocol.md)。
