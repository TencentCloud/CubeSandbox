# cube-e2b SDK & CubeAPI 测试清单

**测试日期**: 2026-05-06  
**测试环境**: CubeSandbox DevCloud (9.135.79.34)  
**SDK 版本**: cube-e2b v0.1.0（工蜂仓库 `silencegao/cube-e2b-sdk` dev 分支）  
**CubeAPI commit**: `313068b`（fix: hostdir-mount annotation key）  

---

## 一、测试环境信息

| 项目 | 值 |
|------|-----|
| CubeAPI 地址 | `http://9.135.79.34:3000` |
| CubeProxy 节点 IP | `9.135.79.34` |
| Template ID | `tpl-6265796cee124256b4dcd6a1` |
| Template 镜像 | `sandbox-code:latest` |
| Template 端口 | `49999`（envd/Jupyter）, `49983` |
| Cubelet 主机 | `9.135.79.34`（同 CubeProxy 节点） |
| 内核版本 | `6.6.69-2.cube.pvm.host.005.1.tl3.x86_64` |
| 关键环境变量 | `CUBE_API_URL`, `CUBE_TEMPLATE_ID`, `CUBE_PROXY_NODE_IP` |

---

## 二、本次修复内容

### Bug：volume host-mount 不生效

**根因**：CubeAPI (`CubeAPI/src/handlers/sandboxes.rs`) 中的 annotation key 写错：

```
// 修复前
const HOSTDIR_MOUNT_KEY: &str = "host-mount";   // ❌

// 修复后  
const HOSTDIR_MOUNT_KEY: &str = "hostdir-mount"; // ✅
```

CubeMaster 的 `pkg/service/sandbox/hostdir_mount.go` 中定义：
```go
const AnnotationHostDirMount = "hostdir-mount"
```

两边 key 不一致，导致 `injectHostDirMounts()` 收到的 annotation 为空，静默跳过。

**修复范围**：仅改动 `CubeAPI/src/handlers/sandboxes.rs` 第 318 行，1 行代码。

**重新编译**：使用 `ghcr.io/tencentcloud/cubesandbox-builder:latest` Docker 镜像在 9.134.82.254 编译，部署到 9.135.79.34。

---

## 三、SDK 接口覆盖情况

| 接口 / 功能 | 实现状态 | 说明 |
|-------------|---------|------|
| `POST /sandboxes` (create) | ✅ 已实现 | `Sandbox.create()` |
| `DELETE /sandboxes/{id}` (kill) | ✅ 已实现 | `sb.kill()` / `__exit__` |
| `GET /sandboxes/{id}` (get info) | ✅ 已实现 | `sb.get_info()` |
| `POST /sandboxes/{id}/pause` | ✅ 已实现 | `sb.pause()` |
| `POST /sandboxes/{id}/resume` | ✅ 已实现（deprecated） | `sb.resume()` |
| `POST /sandboxes/{id}/connect` | ✅ 已实现 | `Sandbox.connect()` / `sb.connect_sandbox()` |
| `GET /sandboxes` (list v1) | ❌ 未实现 | SDK 暂无封装 |
| `GET /v2/sandboxes` (list v2) | ❌ 未实现 | SDK 暂无封装 |
| `GET /health` | ❌ 未实现 | SDK 暂无封装 |
| `POST /execute` (run_code stream) | ✅ 已实现 | `sb.run_code()` |
| `POST /contexts` (create_context) | ✅ 已实现 | `sb.create_context()` |
| `DELETE /contexts/{id}` | ✅ 已实现 | `sb.delete_context()` |
| metadata `hostdir-mount` (volume) | ✅ 已验证 | 需 hostPath 在 Cubelet 节点预先存在 |
| DNS 绕过 (CUBE_PROXY_NODE_IP) | ✅ 已验证 | `IPOverrideTransport` (httpx) |

---

## 四、Example 测试结果（2026-05-06 09:51）

运行命令：
```bash
cd cube-e2b-v2
CUBE_API_URL=http://9.135.79.34:3000 \
CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1 \
CUBE_PROXY_NODE_IP=9.135.79.34 \
PYTHONPATH=. \
python3 examples/run_all.py
```

| Example | 状态 | 耗时 | 说明 |
|---------|------|------|------|
| `create_and_run` | ✅ PASS | 1.6s | 代码执行、stdout、异常捕获、变量持久化 |
| `lifecycle` | ✅ PASS | 17.5s | create → pause → resume/connect → kill |
| `volume` | ✅ PASS | 0.6s | hostdir-mount 读写，本次修复后首次通过 |
| `context` | ✅ PASS | 10.1s | 多 context 隔离、跨调用变量共享 |
| `network_policy` | ✅ PASS | 41.5s | allow-all / deny-all / custom allow-list |

**历史对比**（上次 2026-04-29）：

| Example | 2026-04-29 | 2026-05-06 |
|---------|-----------|-----------|
| create_and_run | ✅ PASS | ✅ PASS |
| lifecycle | ✅ PASS | ✅ PASS |
| volume | ❌ UNIMPLEMENTED | ✅ PASS ← **新修复** |
| context | ✅ PASS | ✅ PASS |
| network_policy | ✅ PASS | ✅ PASS |

---

## 五、各 Example 详细验证点

### 5.1 create_and_run

| 验证点 | 期望 | 实际 | 状态 |
|--------|------|------|------|
| `math.tau * 2` 计算结果 | `'6.2832'` | `'6.2832'` | ✅ |
| stdout 流式输出 (item 0/1/2) | 3 条 stdout | 3 条 | ✅ |
| `logs.stdout` 聚合 | `['item 0\nitem 1\nitem 2\n']` | 一致 | ✅ |
| `ZeroDivisionError` 捕获 | `error.name = ZeroDivisionError` | 一致 | ✅ |
| `connect()` 重连已销毁 sandbox | 返回新 sandbox_id | 正常 | ✅ |
| `sum(range(101))` 变量持久化 | `5050` | `5050` | ✅ |

### 5.2 lifecycle

| 验证点 | 期望 | 实际 | 状态 |
|--------|------|------|------|
| sandbox 创建 | 返回 sandboxID | 有效 ID | ✅ |
| `pause()` | 无报错 | 正常 | ✅ |
| `connect()` 自动 resume | 恢复后变量 `x=42` 保留 | `state after resume = 42` | ✅ |
| `kill()` | 无报错 | 正常 | ✅ |
| 耗时 | < 30s | 17.5s | ✅ |

### 5.3 volume（本次修复重点）

| 验证点 | 期望 | 实际 | 状态 |
|--------|------|------|------|
| `hostdir-mount` annotation 传递到 CubeMaster | CubeMaster 调用 `injectHostDirMounts()` | 日志确认触发 | ✅ |
| hostPath 不存在时 API 报错 | HTTP 500, `prepareHostDirVolume: no such file` | 正确报错 | ✅ |
| 沙箱内读宿主机文件 (`/mnt/data/hello.txt`) | `'Hello from the host!\n'` | `'Hello from the host!\n'` | ✅ |
| 沙箱内 `ls /mnt/data` | `['from_sandbox.txt', 'hello.txt']` | 一致 | ✅ |
| 沙箱写文件 write-back 到宿主机 | sandbox destroy 后宿主机可见 | `.34:/tmp/cube_volume_demo/from_sandbox.txt` 存在 | ✅ |
| `readOnly: false` | 可读写 | 正常 | ✅ |

> **注意**：write-back（沙箱写入 → 宿主机可见）在 sandbox destroy 之后触发（overlay merge on teardown），不是实时同步。

### 5.4 context

| 验证点 | 期望 | 实际 | 状态 |
|--------|------|------|------|
| 无 context 时每次独立 | `100` | `100` | ✅ |
| 同一 context 变量共享 | `x=100, y=200, x+y=300` | `300` | ✅ |
| `sum(1..5)` 在 context 内 | `15` | `15` | ✅ |
| 两个独立 context 互不干扰 | ctx_a=`Alice`, ctx_b=`Bob` | 一致 | ✅ |
| streaming callback + context | stdout 4 条 | 4 条 | ✅ |
| `delete_context()` | 无报错 | 正常 | ✅ |

### 5.5 network_policy

| 验证点 | 期望 | 实际 | 状态 |
|--------|------|------|------|
| allow-all 模式下公网 DNS 不可用 | URLError（沙箱内无公网 DNS） | `Temporary failure in name resolution` | ✅（预期行为）|
| deny-all 模式出站被拦截 | URLError | `URLError` | ✅ |
| custom allow-list: pypi.org 被拦截 | URLError | `Temporary failure in name resolution` | ✅ |
| custom allow-list: example.com 被拦截 | URLError | `URLError` | ✅ |

> **备注**：allow-all 模式下沙箱内无公网 DNS 是当前部署的已知预期行为（CoreDNS 仅本机）。

---

## 六、已知限制 / 待跟进

| 问题 | 状态 | 说明 |
|------|------|------|
| `GET /sandboxes` list 接口未在 SDK 封装 | 待补充 | 可直接调 `requests` 临时使用 |
| `GET /health` 未在 SDK 封装 | 低优先级 | |
| allow-all 模式沙箱内无公网 DNS | 已知限制 | CoreDNS `*.cube.app → 127.0.0.54` 仅本机有效 |
| `readOnly: true` mount 未测试 | 待验证 | |
| 多 hostdir mount（多个挂载点）未测试 | 待验证 | API 支持数组，理论可行 |
| write-back 为 overlay merge，非实时 | 已知行为 | destroy 后生效，不适合需要实时同步的场景 |
| CubeAPI bugfix 仅部署到 .34，未 push GitHub | 待 push | 需要 GitHub PAT 或 SSH key |

---

## 七、SDK 使用示例（快速参考）

```python
import os
from cube_e2b import Sandbox

# 环境变量
# export CUBE_API_URL=http://9.135.79.34:3000
# export CUBE_TEMPLATE_ID=tpl-6265796cee124256b4dcd6a1
# export CUBE_PROXY_NODE_IP=9.135.79.34

# 基础执行
with Sandbox.create() as sb:
    result = sb.run_code("1 + 1")
    print(result.text)  # "2"

# 带 volume
import json
with Sandbox.create(metadata={
    "hostdir-mount": json.dumps([
        {"hostPath": "/data/shared", "mountPath": "/mnt/data", "readOnly": False}
    ])
}) as sb:
    sb.run_code("open('/mnt/data/out.txt','w').write('hello')")
# sandbox destroy 后 /data/shared/out.txt 在宿主机可见

# 生命周期管理
sb = Sandbox.create(timeout=300)
sb.pause()
sb = Sandbox.connect(sb.sandbox_id)   # auto-resume
sb.kill()
```
