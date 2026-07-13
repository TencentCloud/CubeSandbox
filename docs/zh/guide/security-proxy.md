# 安全代理

Cube Sandbox 在每台宿主机上部署一个透明出网代理 —— **CubeEgress** ——
拦截沙箱发起的所有出方向 HTTP/HTTPS 请求，按运维侧定义的 L7 规则
做匹配，对每个请求选择放行、拒绝、或在转发前重写。数据面位于沙箱
的 TAP 设备和外网之间，沙箱无法绕过它（除非破坏宿主机隔离）。

代理提供三类核心能力，全部由"创建沙箱时携带的同一份规则列表"驱动：

- **域名过滤** —— 按 SNI / Host / 方法 / scheme / 路径放行或拒绝
- **凭证注入** —— 自动追加固定 header（典型场景是
  `Authorization: Bearer …`），密钥不进沙箱
- **访问审计** —— 每一次决策（放行 / 拒绝 / 注入 / TLS 握手结果）
  都落到主机本地的 JSONL 审计日志

## 拦截链路

CubeEgress 是一个 host-network 容器，在面向沙箱的 IP 上 bind 两个
TPROXY listener：

```
sandbox ──→ cube-dev (主机网卡)
              │
              ├─ iptables mangle/PREROUTING -j TPROXY
              │     port 80  → 192.168.0.1:8080  (HTTP)
              │     port 443 → 192.168.0.1:8443  (HTTPS)
              │
              ▼
        CubeEgress (OpenResty + lua)
              │
              ├─ ssl_certificate_by_lua → 按客户端 SNI 现场签发
              │                            一张 leaf 证书,签名链根
              │                            是 CubeEgress 的 root CA
              │                            (在模板构建时已被烘进
              │                            沙箱 rootfs 的系统 CA)
              │
              ├─ access_by_lua → 匹配 L7 规则,放行 / 拒绝 / 注入
              │
              └─ proxy_pass → 原始目的 IP (依赖 IP_TRANSPARENT 保留)
```

由于 leaf 证书的链路能被沙箱系统 CA 信任，工作负载的 TLS 客户端
看不到这次 MITM，代理也就能合法读写请求/响应。

## 域名过滤

每条规则有一个 `match`（描述命中条件）和一个 `action`（描述命中后
做什么）。**先到先得**：第一条命中的规则决定结果；任何规则都不
命中的请求被默认拒绝。

```python
from cubesandbox import Sandbox, Rule, Match, Action

rules = [
    # 用 host 精确匹配阻止 apex
    Rule(
        name="deny_example_apex",
        match=Match(scheme="https", host="example.com"),
        action=Action(allow=False),
    ),
    # 用 *.<domain> 形态的 SNI 通配放行所有子域
    Rule(
        name="allow_example_subdomains",
        match=Match(scheme="https", sni="*.example.com"),
        action=Action(allow=True),
    ),
]

with Sandbox.create(network={"rules": rules}) as sb:
    sb.commands.run("curl -s https://www.example.com")  # → 上游
    sb.commands.run("curl -s https://example.com")      # → CubeEgress 返回 403
```

匹配字段（全部可选，多个字段 AND 关系）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `scheme` | `"http"` / `"https"` | |
| `sni` | string | TLS ClientHello 的 SNI；以 `*.` 开头时表示"任意子域"——`*.example.com` 同时命中 `www.example.com` 和 `foo.bar.example.com`，但**不**命中 apex |
| `host` | string | 匹配 HTTP `Host:` 头（自动去除端口部分）；语义与 `sni` 相同 —— 支持精确匹配，或以 `*.` 开头的子域通配（大小写不敏感）|
| `method` | 方法列表 | 列表内 OR 关系（`["GET", "POST"]`） |
| `path` | string | 匹配 `ngx.var.uri`；默认精确匹配，或以单个 `*` 结尾的前缀匹配（如 `/v1/*` 同时命中 `/v1/chat` 和 `/v1/embeddings`） |

请求要同时满足所有出现的字段；未出现的字段视作通配。

::: tip 单层 vs 多层子域
`*.example.com` 不区分子域层数，**所有**结尾命中的子域都算。
若想只放行单层子域（如 `www`、`api`），需要为不希望放过的嵌套
子域**单独**追加 `host="…"` 的 deny 规则。
:::

deny 命中时 CubeEgress 直接返 HTTP 403，**完全不**接触上游 ——
沙箱马上看到拒绝，没 DNS 泄漏、没 TCP 握手。

## 凭证注入

inject 规则在匹配成功后向出方向请求追加固定 header。最经典的
场景是注入 API token：

```python
from cubesandbox import Sandbox, Rule, Match, Action, Inject

rules = [
    Rule(
        name="deepseek_api",
        match=Match(scheme="https", host="api.deepseek.com",
                    method=["POST"], path="/v1/chat",
                    sni="api.deepseek.com"),
        action=Action(
            allow=True,
            audit="metadata",
            inject=[Inject(
                header="Authorization",
                format="Bearer ${SECRET}",
                secret="sk_xxxxxxxx",  # 运维侧密钥,沙箱永不可见
            )],
        ),
    ),
]
```

行为：

- 工作负载发请求时**不带** `Authorization` header；CubeEgress
  接到请求、走完 match、再按 inject 列表追加
  `Authorization: Bearer sk_xxxxxxxx`，再 forward 给上游。
- `format` 默认 `"${SECRET}"`（即 raw secret 作为 header 整值）；
  非 bearer 方案使用 `"Bearer ${SECRET}"` 或任何含
  `${SECRET}` 占位符的模板。
- inject 仅在 `action.allow=true` 时生效；deny 规则带
  `inject=[…]` 是配置错误，会被忽略。

inject 路径的核心价值在于"密钥留在运维侧"：它存在于规则列表里、
沙箱创建时被推到 CubeEgress、永远不会暴露给沙箱的环境变量、文件
系统或进程空间。

## 访问审计

每个请求按规则上的 `action.audit` 字段在三种审计级别里走一种：

| 级别 | 落盘内容 |
| --- | --- |
| `none` | 不记录 |
| `metadata`（默认） | 时间戳、沙箱 IP、目的 IP/端口、scheme、host、method、path、status、收发字节、延迟、TLS 版本+cipher、上游 addr |
| `full` | 预留 —— 当前等同 `metadata`，未来用于完整请求/响应 body 抓取 |

日志在主机 `/data/log/cube-egress/access.jsonl`，每行一条 JSON：

```json
{
  "ts": "2026-05-29T11:24:01+08:00",
  "sandbox_ip": "192.168.1.154",
  "dst_ip": "104.16.132.229",
  "dst_port": 443,
  "scheme": "https",
  "host": "api.deepseek.com",
  "method": "POST",
  "path": "/v1/chat",
  "status": 200,
  "bytes_in": 412,
  "bytes_out": 1856,
  "latency_ms": 384,
  "tls_version": "TLSv1.3",
  "cipher": "TLS_AES_128_GCM_SHA256",
  "upstream_status": "200",
  "upstream_addr": "104.16.132.229:443"
}
```

同一日志文件还落两类事件，schema 略有不同：

- **`security_event`** —— default-deny 拒绝、规则畸形短路、
  host/SNI 不一致、inject 触发等。会带规则名 + `reason` 字段
  描述哪个守卫触发了。
- **`tls_handshake`** —— TLS 握手本身失败时（leaf 签发失败、
  客户端中途断开等）发出，此时还没有 HTTP 报文。用于把"TLS 没握上"
  和"请求被解密后被拒"区分开。

::: warning 密钥被脱敏
inject 的 `secret` 值在写入任何日志路径前都会被剥除。审计日志
仅记录规则名和 inject 已运行的事实；密钥本身从不离开 lua VM。
:::

## 不经过代理的路径

少数情形不走 CubeEgress，需要清楚边界：

- **`cube-dev` 内部流量** —— 沙箱到沙箱、沙箱到集群内服务（Cube
  API 等）不进 TPROXY 链路，不受规则约束。
- **80/443 之外的 TCP/UDP** —— TPROXY 链路只重定向 80 和 443。
  直连其它端口的 TCP 仍受 CubeNet 数据面的 L3/L4 `allow_out` /
  `deny_out` 策略约束，但 CubeEgress 看不到。
- **没烘 CA 的模板** —— 如果模板用 `--with-cube-ca=false` 创建，
  沙箱里的 TLS 客户端**不**信任 CubeEgress 签的 leaf 证书，
  HTTPS 在规则评估之前就会因 self-signed cert 报错。

代理本身的部署（systemd 单元、CA 引导、iptables oneshot 服务）见
[`deploy/one-click`](https://github.com/tencentcloud/CubeSandbox/tree/master/deploy/one-click)
源码 —— 那一套会自动把 CubeEgress 接到 control 和 compute target
里。

## 扩展代理

上面说的 match / inject 语法**故意**写得很窄 —— 它覆盖 80% 的常见
场景（放行某个 host、拒绝另一个、附加 token），仅靠运维侧配置就够。
当你需要规则表达不出的行为 —— 内容检查、跨请求状态、调外部分类
服务 —— CubeEgress 本质就是一个 OpenResty 服务，**你可以自行修改
或新增 lua 脚本**来扩展它。

### lua 文件布局

数据面按 nginx phase 拆成 `CubeEgress/lua/` 下的多个文件：

| 文件 | Phase | 作用 |
| --- | --- | --- |
| `cert_signer.lua` | `ssl_certificate_by_lua` | 按 SNI 现场签发 leaf 证书 |
| `bootstrap.lua` | `init_worker_by_lua`（仅 worker 0）| 启动时从 network-agent 拉初始策略 |
| `access_phase.lua` | `access_by_lua` | 每个请求做 match → action → inject 决策 |
| `policy.lua` | （模块）| 内存中的策略表，由 `admin.lua` 和 `bootstrap.lua` 写入 |
| `admin.lua` | `:9090` 上的 `content_by_lua` | 策略 CRUD admin API |
| `audit.lua` | `log_by_lua` | 落 JSONL 审计日志 |
| `redactor.lua` | （helper）| 把敏感数据从用户可见路径里抹掉 |

`nginx.conf` 在每个 phase block 里通过 `require("…")` 调它们；
比如 HTTP 和 HTTPS server 块的 `access_by_lua_block` 都调
`require("access_phase").decide()`。

`lua_package_path` 配置为
`/usr/local/openresty/nginx/lua/?.lua`，所以放进这个目录的任何
新文件，nginx reload 之后就能 `require("name")` 用。

### 添加一个新的 phase 钩子

最简单的扩展形态是 —— 写一个新模块，在 `nginx.conf` 的某个 phase
block 里 `require` 它。下面的例子：一个 prompt 内容过滤器，拦截
发往 LLM 端点的请求体，扫 prompt 文本，命中黑名单就拒。

**1) 在 `CubeEgress/lua/` 下加一个新模块：**

```lua
-- lua/prompt_filter.lua
-- 第二阶段(在 access_by_lua 决定 allow 之后):检查发往 LLM 上游
-- 的 POST 请求体,命中禁用模式则拒,可选地重写有风险的内容。

local cjson = require("cjson.safe")
local _M = {}

-- 简陋启发式;真实部署应换成你安全团队给的方案。
-- 生产里通常用 lua-resty-http(openresty-tproxy 基础镜像已带)
-- 调外部分类服务,或从 policy 加载一个正则集合。
local FORBIDDEN_PATTERNS = {
    "ignore previous instructions",
    "system prompt",
    "DAN mode",
}

local function is_target_endpoint(host, path)
    -- 只对已知 LLM chat 端点做过滤。可以加更多 host,或从一个
    -- lua_shared_dict 读这份列表,让 admin API 可运行时改。
    return host == "api.deepseek.com" and path == "/v1/chat/completions"
end

function _M.inspect()
    local host = ngx.var.cube_audit_host or ngx.var.http_host
    local path = ngx.var.uri
    if not is_target_endpoint(host, path) then
        return
    end

    -- 必须显式 read_body —— nginx 默认不把 POST body 给 lua。
    -- nginx.conf 已经开了 proxy_request_buffering=on,所以这里
    -- 读完之后,缓存的副本仍会发给上游。
    ngx.req.read_body()
    local body = ngx.req.get_body_data() or ""

    local payload, err = cjson.decode(body)
    if not payload or err then
        return  -- 非 JSON,放过
    end

    -- 走 OpenAI 风格的 messages 数组
    for _, msg in ipairs(payload.messages or {}) do
        local content = (msg or {}).content or ""
        for _, pat in ipairs(FORBIDDEN_PATTERNS) do
            if string.find(string.lower(content), pat, 1, true) then
                ngx.log(ngx.WARN, "prompt filter rejected: pattern=", pat,
                                  " sandbox=", ngx.var.remote_addr)
                -- 复用 audit 模块,让这次拒绝跟普通 deny 一样落到
                -- access.jsonl
                local audit = require("audit")
                pcall(audit.write_security_event, "prompt_filter:" .. pat,
                      ngx.ctx.cube_decision)
                return ngx.exit(ngx.HTTP_FORBIDDEN)
            end
        end
    end
end

return _M
```

**2) 接进 `nginx.conf`：**

HTTPS server 块已经有
`access_by_lua_block { require("access_phase").decide() }`。把新
模块挂在同一个 block 里，让它**在规则放行之后**才跑：

```nginx
server {
    listen 192.168.0.1:8443 ssl transparent reuseport;
    # ...

    location / {
        access_by_lua_block {
            require("access_phase").decide()
            require("prompt_filter").inspect()   -- ← 新加这行
            require("debug_dump").dump_request("https")
        }
        # ...
    }
}
```

**3) 重新构建并重启：**

lua 文件是烘进 cube-egress 容器镜像的：

```bash
cd CubeEgress && make build        # 重新构建镜像
sudo systemctl restart cube-sandbox-cube-egress
```

如果想**不重新打镜像**地快速迭代，把本地 `lua/` 目录 bind-mount
进运行中的容器，然后 reload：

```bash
sudo docker run -d --name cube-egress \
  --network=host \
  -v $PWD/CubeEgress/lua:/usr/local/openresty/nginx/lua:ro \
  -v $PWD/CubeEgress/nginx.conf:/usr/local/openresty/nginx/conf/nginx.conf:ro \
  ...其它启动参数...
sudo docker exec cube-egress nginx -s reload
```

### 自定义 phase 里能拿到什么

你的模块跑在 `access_by_lua` 或更靠后的 phase 时，下列东西已经
就绪：

| 来源 | 取法 | 用途 |
| --- | --- | --- |
| `ngx.ctx.cube_decision` | access_phase 留下的决策结构：`rule_name`、`allow`、`audit_level`、`inject_count` | 按命中规则分支 |
| `ngx.var.ssl_server_name` | 握手时记下的原始 SNI | 身份锁定路由 |
| `ngx.var.cube_audit_host` | 匹配 host（SNI > Host header > dst IP）| 端点查找 |
| `ngx.var.remote_addr` | 沙箱 IP —— policy_store 的主键 | 按沙箱维度做状态 |
| `lua_shared_dict policy_store` | 当前生效的 policy 表 | 运行时读规则，例如把扩展字段塞进 rule body 里读出来 |
| `lua_shared_dict cert_cache` | leaf 证书缓存 | TLS 元信息 |
| `audit.write_security_event(reason, decision)` | 写一行 `security_event` JSONL | 让你的拒绝出现在审计里 |
| `redactor.scrub(s)` | 把字符串里的敏感数据抹掉 | 日志任何用户可见的字段 |

### 实现建议

- **挂在 `access_phase` 之后，不要在前。** 这样 deny 规则先短路，
  你的 hook 不会为已经会被拒的请求做昂贵计算。
- **走 audit 模块、不要光 `ngx.log`。** 手写日志不会进
  `access.jsonl`，下游工具（SIEM ingest、SOC 大盘）按 JSONL
  schema 解析。
- **控制延迟。** 调外部分类服务时，`lua-resty-http` 的超时要
  明显小于上游的 `proxy_connect_timeout`。沙箱在等你。
- **失败要 fail-safe。** 分类服务挂时，提前决定走"拒绝（安全
  优先）"还是"放行（可用性优先）"，但一定**不要**让 worker 崩。
  外部调用包 `pcall`，无论结果如何写一条 `security_event`。
- **别把密钥塞进 `ngx.shared.*`。** 那些 dict 对所有 worker 可见；
  敏感材料应该走 inject 路径（它只写出方向）或者
  `init_by_lua` 的 per-worker 全局变量。

更复杂的场景 —— 中间件链、内容改写、实时分类 —— 用同一套机制即可；
OpenResty 的完整 lua API 都可用，含 `lua-resty-http`（出方向调用）
和 `cjson`（body 处理）。
