# 使用独立捐献域名

v0.1.3 起，捐献页可以通过独立 HTTPS 域名提供，例如 `donate.jinzhezeng.group`。不需要第二个 CPA 实例或额外的 OAuth client；捐献者无需 CPA 管理密钥，管理员仍在原管理界面配置，也不改变 CPA 原生号池。

推荐使用反向代理：浏览器始终访问捐献域名下的页面和公开接口，由代理转发到现有 CPA。不能仅上传静态 HTML 后跨域请求 CPA；页面使用同源 fetch、host-only Cookie、Fetch Metadata 和 CSRF 校验，不应通过放宽 CORS 或伪造请求头来绕过。

## 1. 配置插件

在原 CPA 管理域名的“中科大词元计划”插件设置页中，开放捐献入口，并将站点源设为 `https://donate.jinzhezeng.group`。对应 CPA YAML 为：

```yaml
plugins:
  configs:
    ustc:
      donation-mode: open
      donation-origin: https://donate.jinzhezeng.group
```

只修改现有插件配置中的这两个字段，不要替换整个 CPA 配置文件。保存并等待热更新后，重新读取设置，公开链接就会指向新域名。

当前只允许一个捐献站点源；配置新域名后，旧 CPA 域名上的捐献接口会拒绝页面请求，捐献者应统一访问新域名。原 CPA 管理界面、管理密钥和已入池账号不受影响。该域名必须属于你，并已配置 DNS 和有效 TLS 证书；本文不自动创建 DNS 或更改线上部署。

## 2. Nginx 反向代理示例

以下示例假定 Nginx 与 CPA 在同一台机器，CPA 监听 `127.0.0.1:8317`，证书由现有证书工具管理。请按实际部署替换上游地址与证书路径；如果 Nginx 在独立容器中，`127.0.0.1` 是容器自身，应改为可达的 CPA 内网地址。

```nginx
server {
    listen 443 ssl;
    server_name donate.jinzhezeng.group;

    ssl_certificate /etc/letsencrypt/live/donate.jinzhezeng.group/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/donate.jinzhezeng.group/privkey.pem;

    location = / {
        return 302 /v0/resource/plugins/ustc/donate;
    }

    location ~ ^/v0/resource/plugins/ustc/(donate(\.js|/(session|start|poll|confirm))?|settings\.css)$ {
        limit_except GET { deny all; }

        proxy_pass http://127.0.0.1:8317;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Authorization "";
        proxy_cache off;
        proxy_hide_header Cache-Control;
        add_header Cache-Control "no-store" always;
    }

    location / {
        return 404;
    }
}
```

根路径跳转到现有资源路径，因此页面内的 JS / CSS 相对地址和 Cookie Path 无需改写。`proxy_pass` 不附带 URI，保留原始资源路径。

此域名只公开以下路径：

- `/v0/resource/plugins/ustc/donate`
- `/v0/resource/plugins/ustc/donate.js`
- `/v0/resource/plugins/ustc/settings.css`
- `/v0/resource/plugins/ustc/donate/session`
- `/v0/resource/plugins/ustc/donate/start`
- `/v0/resource/plugins/ustc/donate/poll`
- `/v0/resource/plugins/ustc/donate/confirm`

不要转发 `/v0/management`、`/v8/management`、模型调用路径或整个 CPA 站点；捐献者不需要这些权限。保持 `Origin`、`Sec-Fetch-*`、`X-USTC-Donation-*` 和 Cookie 的原始值，不在代理中伪造校验请求头、注入管理密钥或改写 Cookie 域名。

若前方还有 Cloudflare / CDN，需要为以上全部资源和接口配置缓存绕过，不要使用“缓存所有内容”覆盖 `Cache-Control: no-store`。公网部署建议在代理层增加适当限流，保留学校授权轮询所需的正常频率。

## 3. 上线检查

1. 在服务器检查反向代理配置并按既有运维流程重新加载，例如 `nginx -t` 后再 reload；确认 TLS 证书与域名匹配。
2. 用未登录 CPA 的浏览器打开 `https://donate.jinzhezeng.group/`，应跳转到捐献页并成功建立会话，不要求 CPA 管理密钥。
3. 检查 JS / CSS 能加载，公开响应不缓存，Cookie 带有 Secure、HttpOnly、SameSite=Strict 且属于捐献域名。会话接口没有有效浏览器校验时返回 403 属于预期行为。
4. 确认捐献域名下的 `/v8/management/credentials`、`/v0/management/plugins/ustc/state`、`/management.html` 和 `/v1/models` 均返回 404。
5. 若自愿进行真实授权验收，先阅读授权范围，在学校官方页面登录自己的账号，返回后核对身份，再确认捐献。仅完成学校登录不会立即写入号池；可重新认证并确认撤回。

捐献撤回、记录上限、令牌存储和学校侧权限限制与主 README 中的说明相同。关闭入口不会自动撤回已经加入 CPA 的账号。
