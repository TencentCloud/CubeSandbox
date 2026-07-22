# 出站 URL 安全层

CubeAPI 在调用外部 URL（如 `auth_callback_url`）之前，会先经过出站 URL 安全层校验，防止 SSRF（Server-Side Request Forgery）攻击和 DNS rebinding 绕过。

## 防护目标

以下保护在默认构建中始终生效：

- 阻止访问本地回环地址：`127.0.0.1`、`::1`、`localhost`
- 阻止访问私网地址：`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`100.64.0.0/10` 等
- 阻止访问链路本地地址：`169.254.0.0/16`（含云厂商 metadata 服务 `169.254.169.254`）
- 阻止访问 IPv6 唯一本地地址、多播地址等
- 通过 DNS pinning 防止“校验时公网 IP、请求时内网 IP”的 DNS rebinding 攻击

额外的 Webhook 专用辅助函数（包括通过 `read_body_with_limit` 限制响应体大小）默认通过 `webhooks` Cargo feature 启用；如需禁用，可使用 `--no-default-features` 构建。

## 配置项

通过环境变量配置，使用 `__` 作为嵌套分隔符：

```bash
# 允许的 URL scheme，默认 https
OUTBOUND_URL_SECURITY__ALLOWED_SCHEMES=https

# 是否允许解析到私网/回环/链路本地地址，默认 false
OUTBOUND_URL_SECURITY__ALLOW_PRIVATE_IPS=false

# DNS 解析超时（毫秒），默认 5000
OUTBOUND_URL_SECURITY__RESOLVE_TIMEOUT_MS=5000
```

`ALLOWED_SCHEMES` 支持多个值，使用逗号分隔：

```bash
OUTBOUND_URL_SECURITY__ALLOWED_SCHEMES=http,https
```

## 默认行为

生产环境默认：

- 只允许 `https`
- 拒绝所有私网/回环/链路本地地址
- DNS 解析超时 5 秒

如果配置的 `auth_callback_url` 不符合上述安全策略，CubeAPI 启动会立即失败并输出明确错误。

## 本地开发

在本地开发或测试环境中，回调地址可能是 `http://127.0.0.1` 或局域网地址。可以通过以下配置放宽限制：

```bash
OUTBOUND_URL_SECURITY__ALLOWED_SCHEMES=http,https
OUTBOUND_URL_SECURITY__ALLOW_PRIVATE_IPS=true
```

::: warning 生产环境不要开启 allow_private_ips
开启 `allow_private_ips=true` 会让 CubeAPI 能够访问内网服务，显著增加 SSRF 风险。请仅在本地开发环境中使用。
:::

## auth_callback_url 的变化

启用 `auth_callback_url` 后：

1. CubeAPI 启动时会校验该 URL 是否符合出站安全策略。
2. 校验通过后，会构建一个专用的加固 HTTP client：
   - 禁用自动 redirect
   - 禁用 HTTP proxy（会忽略 `HTTP_PROXY`/`HTTPS_PROXY`）
   - 设置连接超时和请求超时
   - 固定 DNS 解析结果，防止 DNS rebinding
3. 鉴权中间件使用该专用 client 向回调地址发送 POST 请求。

由于代理被禁用，需要通过正向代理才能访问外网的环境目前无法使用 `auth_callback_url`，除非放宽策略或后续增加支持代理的选项。

如果启动时报错例如 `resolved address 127.0.0.1 is not a public IP`，说明 `auth_callback_url` 指向了被禁止的地址。请改为公网可访问地址，或在本地开发环境中放宽策略。

## 未来 Webhook 复用

该安全层同样适用于未来的 Webhook 实现。Webhook 代码可以：

```rust
use cube_api::security::outbound_url::{OutboundUrlPolicy, build_secure_client};

let policy = OutboundUrlPolicy::webhook_default();
let validated = policy.validate("https://your-webhook.example.com/hook").await?;
let client = build_secure_client(&validated, Duration::from_secs(5), Duration::from_secs(10))?;
```

`webhook_default()` 使用与生产环境一致的严格策略：仅 `https`，拒绝私网地址。

## 验证示例

以下 URL 在默认策略下会被拒绝：

| URL | 拒绝原因 |
|---|---|
| `http://example.com/hook` | scheme 不在白名单 |
| `https://127.0.0.1/hook` | 回环地址 |
| `https://localhost/hook` | 显式禁止的 host |
| `https://10.0.0.1/hook` | 私网地址 |
| `https://192.168.1.1/hook` | 私网地址 |
| `https://169.254.169.254/hook` | 链路本地 / metadata 地址 |
| `https://[::ffff:127.0.0.1]/hook` | IPv4-mapped 回环地址 |
| `https://user:pass@example.com/hook` | URL 嵌入凭证 |

以下 URL 在默认策略下会通过：

| URL | 说明 |
|---|---|
| `https://example.com/hook` | 公网域名，解析到公网 IP |
| `https://1.2.3.4/hook` | 公网 IP 字面量 |
| `https://example.com:8443/hook` | 自定义端口被保留 |
