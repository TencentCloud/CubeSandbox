# HTTPS 证书与域名解析

本文说明 Cube Sandbox 中沙箱访问的域名机制，以及如何解决 **HTTPS 证书**和**域名解析**两类常见问题。

> **说明：** TLS 配置仅影响客户端访问 CubeProxy 的方式。`E2B_API_URL` 始终指向 **Cube API Server**（默认端口 `3000`），与 CubeProxy 是独立的组件。

---

## 沙箱域名机制

E2B 客户端访问沙箱时，域名是**动态生成**的，格式如下：

```
<sandbox-service-port>-<sandboxId>.<domain>
```

例如：

```
49983-1aa1fae8fb364edaa8203a7481995b4d.cube.app
```

其中：
- `sandbox-service-port`：沙箱内业务服务监听的端口（如 `49999`）
- `sandboxId`：沙箱唯一 ID
- `domain`：基础域名，默认为 `cube.app`，可通过 `--sandbox-domain` 自定义

由于域名中的 `sandboxId` 部分随每个沙箱动态变化，**E2B 客户端所在的网络环境必须具备泛域名（wildcard）解析能力**，即将 `*.<domain>` 解析到 CubeProxy 所在机器的 IP。

### 一键安装内置 CoreDNS（体验用）

Cube 一键安装脚本会在安装节点上启动一个 **CoreDNS** 服务，自动处理 `*.cube.app` 的泛解析，使你能在该机器上直接体验完整流程，无需额外配置 DNS。

> **注意：** 内置 CoreDNS 仅供本机/快速体验使用，不适合生产环境或多机共享部署。

### 生产环境：自定义域名 + 自有 DNS

生产环境中，建议使用自己的域名并配置泛解析 DNS 记录：

```
*.your.domain.com  →  <CubeProxy 所在节点的 IP>
```

然后在启动 Cube API Server 时通过启动参数或环境变量指定该域名：

```bash
# 启动参数
./cube-api --sandbox-domain your.domain.com

# 或环境变量
export CUBE_API_SANDBOX_DOMAIN=your.domain.com
./cube-api
```

这样 API 响应中的 `domain` 字段会返回 `your.domain.com`，E2B SDK 才能正确构建沙箱访问地址。

---

## HTTPS 证书配置

CubeProxy 开箱提供 **HTTPS（443 端口）和 HTTP（80 端口）** 两种访问方式。E2B SDK 默认使用 HTTPS。Cube 一键安装已预装 `cube.app` 测试证书，可直接体验 HTTPS。

如需替换为自定义域名或生产证书，有以下几种方式：

### 方式 A — mkcert（本地开发快速验证）

`mkcert` 可在几秒内为自定义域名生成本地可信证书：

```bash
mkcert -install
mkcert <your-host-ip-or-domain>
```

设置 `SSL_CERT_FILE`，让 E2B SDK 信任生成的 CA：

```bash
export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

> mkcert 证书只在运行过 `mkcert -install` 的机器上受信任，不适合生产环境或多人共享部署。

### 方式 B — 自有证书 / 域名（生产环境）

修改 CubeProxy 的 `nginx.conf`，使用正式证书和私钥：

```nginx
server {
    listen 443 ssl;
    server_name your.domain.com;

    ssl_certificate     /path/to/your/cert.pem;
    ssl_certificate_key /path/to/your/key.pem;
}
```

同时通过 `--sandbox-domain` 告知 Cube API Server 对外域名（见上文「生产环境：自定义域名」章节）。

### 方式 C — 仅保留 HTTPS（关闭 HTTP）

CubeProxy 默认同时监听 HTTP 和 HTTPS。如需完全关闭 HTTP 端口，删除 `nginx.conf` 中的 HTTP server block，并在 `docker-compose.yaml` 中去掉对应的端口映射即可。

> E2B SDK 仅使用 HTTPS，关闭 HTTP 不影响基于 SDK 的客户端。

---

## 体验方案：e2b-dev-sidecar

如果你只是在本地开发环境中体验 Cube，不想配置泛解析 DNS，也不想处理自签证书信任问题，可以使用 **e2b-dev-sidecar** 方案。

该方案通过在本地启动一个轻量代理，拦截 E2B SDK 的数据面请求，在转发时自动改写 `Host` 头，从而：
- **绕过泛解析 DNS**：本地无需配置 `*.cube.app` 解析
- **绕过 HTTPS 证书**：sidecar 默认不校验服务端证书（`CUBE_REMOTE_PROXY_VERIFY_SSL=false`）

详见：[e2b-dev-sidecar 示例](../../../examples/e2b-dev-sidecar/README_zh.md)

> **注意：** e2b-dev-sidecar 是面向开发体验的最小实现，不适合生产环境。
