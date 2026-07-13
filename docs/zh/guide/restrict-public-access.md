# 限制公开访问

默认情况下，Cube Sandbox 的[公网 URL](/zh/guide/network-policy) 任何
知道沙箱 ID 的人都能访问 —— URL 不可预测，但这也是工作负载与外网
之间**唯一**的屏障。对敏感场景（长期运行的处理私有数据的 Agent、
对外部网络暴露的看板、不希望被回放的演示）来说，Cube Sandbox 支持
在请求触达沙箱内部服务之前，要求调用方先用每沙箱独立的 token
完成鉴权。

本页是 E2B [Restricting public access][e2b-doc] 在 Cube Sandbox 侧
的对应实现 —— 共享 `network.allowPublicTraffic` 参数与
`e2b-traffic-access-token` header，原有 e2b 代码可零改动迁移过来。

[e2b-doc]: https://e2b.dev/docs/network/restrict-public-access

## 工作原理

使用 `network.allow_public_traffic = false` 创建沙箱时，创建响应里
会带回一个每沙箱独立的 `traffic_access_token`。之后所有访问该沙箱
公网 URL 的入站请求都必须在以下两个等价 header 中任选其一携带
该 token：

- `e2b-traffic-access-token`（与 E2B 完全兼容）
- `cube-traffic-access-token`（CubeSandbox 原生别名）

缺失 header、或携带错误 token 的请求会在触达沙箱之前被 **HTTP 403** 拒绝。

## 快速上手

```python
from cubesandbox import Sandbox
import requests

sandbox = Sandbox.create(
    template=template_id,
    network={"allow_public_traffic": False},
)

print(sandbox.traffic_access_token)
# 例如：4f8a2b1c9d7e3f5a6b0c8d2e4f6a9b1c3d5e7f9a0b2c4d6e8f1a3b5c7d9e0f2a

# 在沙箱内启动一个服务（all-in-one 测试镜像里 nginx 已经监听
# 80 端口，否则用 commands.run 启动自己的进程并 expose 端口）。
url = f"http://{sandbox.get_host(80)}/"

# 不带 token → 403
resp = requests.get(url)
assert resp.status_code == 403

# 带 E2B 兼容 header → 200
resp = requests.get(
    url,
    headers={"e2b-traffic-access-token": sandbox.traffic_access_token},
)
assert resp.status_code == 200

# 带 CubeSandbox 原生别名 header → 也 200
resp = requests.get(
    url,
    headers={"cube-traffic-access-token": sandbox.traffic_access_token},
)
assert resp.status_code == 200
```

带 rich TUI 的完整版示例（含并排 probe 汇总和最终判定面板）位于
[`examples/code-sandbox-quickstart/restrict_public_access.py`][demo]。

[demo]: https://github.com/tencentcloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/restrict_public_access.py

## 默认行为

不传 `allow_public_traffic`（或显式传 `True`）保留历史行为 ——
"任何知道 URL 的人都可访问"。此时不签发 token，
`sandbox.traffic_access_token` 为 `None`。这意味着**升级到带本特性
的版本后，所有存量调用方无需改动即可继续工作**。

| 调用意图 | 怎么传 | `traffic_access_token` | 入站请求行为 |
|---|---|---|---|
| 默认 —— 公网可达 | 不传 `network`，或 `allow_public_traffic=True` | `None` | 接受所有请求 |
| 锁定访问 | `network={"allow_public_traffic": False}` | 不透明 token | 拒绝未携带正确 token 的请求（403） |

## Header 语义

`e2b-traffic-access-token` 与 `cube-traffic-access-token` 接受
相同形式的不透明 token，按 HTTP 规则**不区分大小写**，按顺序取值 ——
两个都存在时以 `e2b-` 为准。可通过任意 HTTP 客户端发送：

```bash
curl -H "e2b-traffic-access-token: $TOKEN" \
     "http://80-$SANDBOX_ID.cube.app/"
```

```javascript
fetch(url, { headers: { "e2b-traffic-access-token": token } })
```

Token 值不会出现在日志中。

## 生命周期与持久化

- **仅一次下发。** Token 只挂在原始创建请求的响应里。如果后续流程
  还要用到，调用方需要在创建那一刻自行持久化。
- **无 rotation API。** 一个沙箱在其整个生命周期内只持有一个
  token。需要轮换请新建沙箱。
- **不影响 `connect()` / `resume()`。** 暂停的沙箱被恢复时不会重发
  token —— 已有调用方继续使用原 token 即可。
- **自动清理。** 销毁沙箱时 token 一并清除。

## 与其他网络策略组合使用

`allow_public_traffic` 控制**入站**：沙箱公网 URL 的访问权限。
与以下能力**正交**，可独立或叠加使用：

- [网络策略](/zh/guide/network-policy) —— 出站 CIDR 白/黑名单
  （`allow_out` / `deny_out`、`allow_internet_access`）。
- [安全代理](/zh/guide/security-proxy) —— 出站 HTTP/HTTPS 七层规则。

典型的"私有 Agent"部署会三者叠加：网络策略限制出站只允许几个
SaaS API、安全代理在出站端注入 API 凭证以避免密钥进入沙箱、
本页 token 机制要求所有入站请求都携带凭证。

## 错误模型

| HTTP 状态 | 触发条件 |
|---|---|
| `200`（或上游返回的状态码） | token 匹配 |
| `403` | 沙箱被标记为 `allow_public_traffic=false`，但请求未携带 `e2b-traffic-access-token` / `cube-traffic-access-token`，或 token 值不匹配 |

所有其他错误路径（沙箱不存在、上游不健康等）与默认公开
访问模式下完全一致，本特性不引入新的失败模式。
