# 独立捐献域名与受限网关

本文对应 **v0.1.4 起的独立网关**。v0.1.3 将动态捐献操作挂在公开 CPA resource 路由，不符合商店安全要求；升级后这些旧路径全部返回 404，必须迁移。普通插件设置、推理、既有凭据及 CPA 原生调度不依赖网关，捐献功能仍默认关闭。

## 1. 安全边界

```text
捐献者浏览器（无 CPA Key）
  → HTTPS donate.jinzhezeng.group
  → Nginx → 127.0.0.1:8320 独立捐献网关
  → Bearer 管理鉴权 → CPA:8317
  → POST /v0/management/plugins/ustc/donation/{session,start,poll,confirm}
```

网关只提供 `/`、`/donate.js`、`/settings.css` 和四个固定 POST 操作；不是任意管理 API 的反向代理。管理密钥仅由服务器私有环境读取，不发给浏览器，不接受客户端 Authorization，不跟随上游重定向。会话、学校授权、单次确认和凭据写入仍在 CPA 插件内，沿用请求作用域、去重及原生号池。

网关进程持有高权限管理密钥，必须作为可信服务隔离部署；限制该进程及其配置的读取权限，不与不可信程序共享环境。不建议将 CPA 管理端口直接开放到公网；跨机器链路必须 HTTPS，或通过可信隧道访问本机管理端口。仅 localhost 允许明文 HTTP。不要把密钥注入公开 Nginx 路径或上传到静态托管平台。

## 2. 配置与启动

在现有 CPA 插件设置页开放入口，将站点源设为 `https://donate.jinzhezeng.group`，或仅修改现有配置中的两个字段：

```yaml
plugins:
  configs:
    ustc:
      donation-mode: open
      donation-origin: https://donate.jinzhezeng.group
```

从对应版本的 Release 下载独立 `cpa-ustc_<版本>_<系统>_<架构>.zip`，或构建源码的 `dist/cpa-ustc`（Windows 为 `.exe`）。安装相同版本的 CPA 插件动态库。

将现有 CPA 管理密钥通过受保护的服务环境文件 / secret manager 注入 `CPA_MANAGEMENT_KEY`，**不要直接在命令行填写其值**，不要使用推理客户端 Key。启动：

```bash
cpa-ustc serve-donations --listen 127.0.0.1:8320 \
  --cpa-url http://127.0.0.1:8317 \
  --origin https://donate.jinzhezeng.group
```

可用 `--management-key-env <变量名>` 改变环境变量名。默认仅监听 loopback，`--origin` 必须与插件 `donation-origin` 完全一致。请用服务管理器守护进程。若在其他容器 / 主机运行，localhost 指向容器自身；应提供可达的 HTTPS CPA 源，并按 CPA 规则启用远程管理，不能关闭管理鉴权。

## 3. Nginx 示例

假设 Nginx 和网关同机，TLS 证书已由管理员配置：

```nginx
server {
    listen 443 ssl;
    server_name donate.jinzhezeng.group;

    ssl_certificate /etc/letsencrypt/live/donate.jinzhezeng.group/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/donate.jinzhezeng.group/privkey.pem;

    # The gateway enforces its own exact path and method allowlist.
    location / {
        proxy_pass http://127.0.0.1:8320;
        proxy_set_header Host $host;
        proxy_set_header Authorization "";
        proxy_read_timeout 70s;
        client_max_body_size 1k;
        proxy_cache off;
    }
}
```

这里**不转发到 CPA**，不改写路径或 Cookie，不设置任何管理密钥。保留 `Origin`、`Sec-Fetch-*`、`X-USTC-Donation-*` 和 Cookie 的真实值。不要伪造浏览器校验头、放宽 CORS 或把整个 CPA 站点映射到捐献域名。

Cloudflare / CDN 应绕过本站全部响应的缓存，尊重 `Cache-Control: no-store`。建议在反向代理增加全站及 IP 限流，并保留正常轮询频率。域名须由运营者掌控并配置有效 TLS；本项目不自动更改 DNS / 生产配置。

## 4. 上线与迁移检查

1. 升级插件及配套 CLI，移除旧 `/v0/resource/plugins/ustc/donate*` 的 CPA 转发配置，运行新网关并配置上述源地址。
2. 未登录 CPA 的浏览器打开 `https://donate.jinzhezeng.group/`，页面应直接加载并建立会话，不要求 CPA 管理密钥。
3. 检查 Cookie 为捐献域名 host-only、Path `/api/donation/`、Secure / HttpOnly / SameSite=Strict；JS / CSS 可加载，所有响应 no-store。浏览器网络请求不携带 CPA Authorization。
4. 网关域名下 `/v8/management/credentials`、`/v0/management/plugins/ustc/state`、`/management.html`、`/v1/models` 应全部 404；CPA 的四个私有捐献 POST 无管理鉴权必须 401 / 403。CPA 旧公开捐献路径即使开放入口也必须 404。
5. 若会话返回网关 502，由管理员检查 CPA 连通性、管理密钥、插件版本及 donation-mode。不要向捐献者索要管理密钥或关闭 CPA 鉴权。没有浏览器同源校验时的 403 属于正常保护。
6. 自愿进行真实授权验收时，只在学校页面登录自己的账号，回到本站核对身份并再次确认；单独完成学校登录不得写入号池，可重新认证并确认撤回。

切换 origin 或重载插件会清除进行中的会话，但不改变已入池账号。撤回限制及学校侧权限说明见 README；关闭入口不自动撤回已入池授权。测试仅用本地模拟身份，不登录真实账号或消耗学校模型额度。
