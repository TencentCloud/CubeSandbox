<p align="center">
  <strong>cubesandbox</strong> — CubeSandbox Python SDK
</p>

<p align="center">
  <a href="https://github.com/TencentCloud/CubeSandbox"><img src="https://img.shields.io/badge/CubeSandbox-GitHub-blue" alt="CubeSandbox" /></a>
  <a href="../../LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0" /></a>
  <img src="https://img.shields.io/badge/Python-3.9%2B-blue" alt="Python 3.9+" />
  <img src="https://img.shields.io/badge/version-0.6.0-orange" alt="v0.6.0" />
</p>

---

`cubesandbox` 是 [CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
官方 Python SDK。提供简洁、Pythonic 的接口来创建沙箱、执行代码，并控制
完整的沙箱生命周期——包括带内存快照的暂停/恢复。

## 安装

```bash
pip install cubesandbox
```

## 快速开始

设置必需的环境变量：

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 远程访问时需要（绕过 DNS 解析 *.cube.app）
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
```

运行第一个沙箱：

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    result = sb.run_code("1 + 1")
    print(result.text)   # "2"
```

## 功能

### 执行代码

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    # 简单表达式
    result = sb.run_code("x = 42\nx * 2")
    print(result.text)          # "84"

    # 捕获 stdout
    result = sb.run_code('print("hello")')
    print(result.logs.stdout)   # ["hello\n"]

    # 实时流式输出
    sb.run_code(
        'for i in range(3): print(i)',
        on_stdout=lambda msg: print("out:", msg.text),
    )
```

### 运行 shell 命令

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    result = sb.commands.run("echo hello cube")
    print(result.stdout)  # "hello cube\n"
```

当省略 `user` 参数时，SDK 默认以 `root` 身份发送请求，以兼容拒绝无显式用户
的进程/文件请求的 envd 版本。

### 沙箱内变量持久化

一次 `run_code` 调用中赋值的变量在沙箱生命周期内持续有效——无需单独
的上下文对象：

```python
with Sandbox.create() as sb:
    sb.run_code("x = 100")
    result = sb.run_code("x + 1")
    print(result.text)   # "101"
```

### 暂停与恢复

```python
sb = Sandbox.create()

# 暂停 —— 保留内存快照，轮询直到状态为 paused
sb.pause()                         # wait=True, timeout=30s 默认
sb.pause(wait=False)               # 即发即忘
sb.pause(timeout=60, interval=0.5) # 自定义轮询参数

# 通过连接恢复 —— 自动恢复已暂停的沙箱
sb2 = Sandbox.connect(sb.sandbox_id)
```

### 网络策略

`network=` 内部可以组合两个层次：

- **L3/L4** — `allow_out` / `deny_out`，CIDR 或主机名列表。
- **L7** — `rules`，主机/路径/SNI 匹配、审计和凭证注入。使用类型化
  `Rule` / `Match` / `Action` / `Inject` 数据类。

```python
from cubesandbox import Sandbox, Rule, Match, Action, Inject

rules = [
    Rule(
        name="deepseek_api",
        match=Match(
            scheme="https",
            host="api.deepseek.com",
            method=["POST"],
            path="/v1/chat",
            sni="api.deepseek.com",
        ),
        action=Action(
            allow=True,
            audit="metadata",
            inject=[Inject(
                header="Authorization",
                format="Bearer ${SECRET}",
                secret="sk_xxxxxxxx",
            )],
        ),
    ),
]

with Sandbox.create(
    network={"allow_out": ["172.67.0.0/16"], "rules": rules},
) as sb:
    sb.run_code("import requests; requests.post('https://api.deepseek.com/v1/chat')")
```

规则按列表顺序**先匹配先生效**。凭证注入仅在 SNI 和 Host 均匹配的 HTTPS
请求上执行（服务端强制）。

#### E2B 每主机请求转换（兼容格式）

为与 E2B 的
[per-host request transforms](https://e2b.dev/docs/network/internet-access#per-host-request-transforms)
无缝兼容，`network["rules"]` 也接受以主机名为键的映射。每个
`transform.headers` 条目会被转换为 CubeEgress L7 规则，其 `action.inject`
会在发往该主机的出站 HTTPS 请求中注入相同的请求头：

```python
from cubesandbox import Sandbox

with Sandbox.create(
    network={
        # 主机仍须通过 allow_out 引用 —— 仅注册规则不会授予出站权限。
        "allow_out": ["api.example.com"],
        "deny_out": ["0.0.0.0/0"],
        "rules": {
            "api.example.com": [
                {"transform": {"headers": {"X-Header": "Content"}}},
            ],
        },
    },
) as sb:
    sb.run_code("import requests; requests.get('https://api.example.com/')")
```

兼容格式与类型化 Rule 格式可互换：选择适合代码库的即可。不支持在单次
`Sandbox.create` 调用中混用两种格式——要么传 `Rule` 列表（类型化），
**要么**传主机名键值字典（E2B 格式）。

遗留的 `metadata={"network-policy": ...}` 接口仍然支持纯 IP 的
deny-all / 自定义 allow-list 场景。

### 文件系统

```python
from cubesandbox import Sandbox

with Sandbox.create() as sb:
    # 读写
    sb.files.write("/tmp/hello.txt", "Hello, world!")
    print(sb.files.read("/tmp/hello.txt"))  # "Hello, world!"

    # 批量写入
    sb.files.write_files([
        ("/tmp/a.txt", "aaa"),
        ("/tmp/b.txt", b"bbb"),  # 也接受 bytes
    ])

    # 目录操作
    sb.files.make_dir("/tmp/mydir")
    entries = sb.files.list("/tmp")          # list[dict]
    info = sb.files.stat("/tmp/hello.txt")   # dict，含 name、type、size...
    print(sb.files.exists("/tmp/hello.txt")) # True
    sb.files.rename("/tmp/hello.txt", "/tmp/renamed.txt")
    sb.files.remove("/tmp/renamed.txt")

    # 监听变更
    with sb.files.watch_dir("/tmp") as watcher:
        for event in watcher:
            print(event.name, event.type)  # 如 "a.txt" "EVENT_TYPE_CREATE"
```

### 宿主机目录挂载

```python
import json
from cubesandbox import Sandbox

mounts = json.dumps([{"hostPath": "/data/shared", "mountPath": "/mnt/data"}])
with Sandbox.create(metadata={"host-mount": mounts}) as sb:
    result = sb.run_code("open('/mnt/data/hello.txt').read()")
    print(result.text)
```

### 持久化卷（Persistent Volumes）

持久化卷是与 e2b 兼容的存储，底层由卷插件（COS、NFS…）支撑。通过
`Volume` 工具类管理卷的生命周期，再通过
`Sandbox.create(volume_mounts={...})`（e2b mapping）挂载进沙箱。数据可跨沙箱重启保留，
也可在多个沙箱之间共享。

```python
from cubesandbox import Sandbox, Volume

# 创建卷 —— name 可选（省略时服务端生成 UUID）。
# 省略 driver 即 e2b 兼容：不发送 driver，后端使用第一个已配置的插件。
# 传入非空 driver 可绑定指定插件（如 cos、cfs）。
vol = Volume.create("my-data")                   # 默认插件
# vol = Volume.create("my-data", driver="cos")   # 绑定插件
print(vol.volume_id, vol.token)

# 将卷挂载进沙箱
with Sandbox.create(
    volume_mounts={"/workspace": vol},
) as sb:
    sb.files.write("/workspace/note.txt", "已持久化！")
    print(sb.files.read("/workspace/note.txt"))

# 值可以是 Volume、VolumeInfo 或 volume_id 字符串。

# 列出 / 查询信息 / 连接 / 销毁
for v in Volume.list():                 # list[VolumeInfo]（token 恒为 ""）
    print(v.volume_id, v.name)
Volume.get_info(vol.volume_id)          # -> VolumeInfo（含 token）
vol = Volume.connect(vol.volume_id)     # -> 返回 Volume 实例
Volume.destroy(vol.volume_id)           # -> bool；先杀掉所有挂载它的沙箱（不会自动 detach）
```

卷的 `name` 必须匹配 `^[a-zA-Z0-9_-]+$` 且不超过 128 字符；非法名称
会在任何网络请求之前抛出 `ValueError`。完整 API、参数和错误码请参阅
[`docs/volume.zh.md`](docs/volume.zh.md)。

### 列表与健康检查

```python
from cubesandbox import Sandbox

print(Sandbox.health())     # {"status": "ok", "sandboxes": 4}
print(Sandbox.list())       # 运行中沙箱列表
print(Sandbox.list_v2())    # v2 API（支持过滤）
```

## 配置

| 环境变量 | 是否必填 | 默认值 | 说明 |
|---|:---:|---|---|
| `CUBE_API_URL` | ✅ | `http://127.0.0.1:3000` | CubeAPI 管控面地址 |
| `CUBE_TEMPLATE_ID` | ✅ | — | 沙箱创建所用模板 ID |
| `CUBE_PROXY_NODE_IP` | 远程时必填 | — | CubeProxy 节点 IP，绕过 DNS 解析 `*.cube.app` |
| `CUBE_PROXY_PORT_HTTP` | | `80` | CubeProxy HTTP 端口 |
| `CUBE_SANDBOX_DOMAIN` | | `cube.app` | 沙箱域名后缀 |

也可以通过 `Config` 对象显式传入：

```python
from cubesandbox import Config, Sandbox

cfg = Config(
    api_url="http://10.0.0.1:3000",
    template_id="tpl-xxxxxxxxxxxxxxxxxxxxxxxx",
    proxy_node_ip="10.0.0.1",
)
with Sandbox.create(config=cfg) as sb:
    print(sb.run_code("2 ** 10").text)   # "1024"
```

## API 参考

### `Sandbox` — 类方法

| 方法 | 说明 |
|---|---|
| `Sandbox.create(template, *, timeout, env_vars, metadata, volume_mounts, config)` | `POST /sandboxes` — 创建新沙箱（可选挂载卷） |
| `Sandbox.connect(sandbox_id, *, config)` | `POST /sandboxes/:id/connect` — 连接（暂停状态下自动恢复） |
| `Sandbox.list(config)` | `GET /sandboxes` — 列出运行中沙箱（v1） |
| `Sandbox.list_v2(config)` | `GET /v2/sandboxes` — 列出沙箱（v2） |
| `Sandbox.health(config)` | `GET /health` — 服务健康检查 |

### `Sandbox` — 实例方法

| 方法 | 说明 |
|---|---|
| `sb.run_code(code, *, on_stdout, on_stderr, on_result, on_error, envs, timeout)` | `POST /execute` — 执行代码，返回 `Execution` |
| `sb.get_info()` | `GET /sandboxes/:id` — 获取沙箱状态和元数据 |
| `sb.pause(*, wait, timeout, interval)` | `POST /sandboxes/:id/pause` — 暂停沙箱 |
| `sb.resume(timeout)` | `POST /sandboxes/:id/resume` — 恢复（已弃用，请用 `connect`） |
| `sb.kill()` | `DELETE /sandboxes/:id` — 销毁沙箱 |
| `sb.set_timeout(timeout)` | `POST /sandboxes/:id/timeout` — 设置沙箱空闲超时 |
| `sb.get_host(port)` | 返回虚拟主机名 `{port}-{id}.{domain}` |

### `sb.files` — 文件系统

| 方法 | 说明 |
|---|---|
| `sb.files.read(path)` | 通过 `GET /files` 下载文件内容 |
| `sb.files.write(path, data)` | 通过 `POST /files` 上传（octet-stream，multipart 回退） |
| `sb.files.write_files(files)` | 批量写入 `[(path, data), ...]`，遇错即停 |
| `sb.files.list(path)` | 列出目录条目 → `list[dict]` |
| `sb.files.stat(path)` | 文件/目录元数据 → `dict` |
| `sb.files.exists(path)` | 路径存在则返回 `True`（stat + 404 检测） |
| `sb.files.make_dir(path)` | 创建目录 → `dict` |
| `sb.files.rename(old, new)` | 移动/重命名 → `dict` |
| `sb.files.remove(path)` | 删除文件或目录 |
| `sb.files.watch_dir(path)` | 流式监听文件系统事件 → `Watcher`（上下文管理器 + 迭代器） |

### `Volume` — 持久化卷（类方法）

| 方法 | 说明 |
|---|---|
| `Volume.create(name=None, *, driver=None, config=None)` | `POST /volumes` — 创建卷；省略 `driver` 即 e2b 兼容（后端第一个插件），或传非空 `driver`（如 `"cos"`）绑定插件 → `Volume` |
| `Volume.connect(volume_id, *, config)` | `GET /volumes/:id` — 连接已存在的卷 → `Volume` |
| `Volume.list(config)` | `GET /volumes` — 列出卷 → `list[VolumeInfo]`（不含 token） |
| `Volume.get_info(volume_id, *, config)` | `GET /volumes/:id` — 查询单个卷信息（含 token）→ `VolumeInfo` |
| `Volume.destroy(volume_id, *, config)` | `DELETE /volumes/:id` — 删除卷 → `bool` |

通过 `Sandbox.create(volume_mounts={path: vol})` 将卷挂载进沙箱。
`Volume.create` / `connect` 返回 `Volume` 实例，`list` / `get_info` 返回
`VolumeInfo`；二者都暴露 `.volume_id`、`.name`、`.token`。完整参考：
[`docs/volume.zh.md`](docs/volume.zh.md)。

### `Execution` 对象

| 属性 | 类型 | 说明 |
|---|---|---|
| `.text` | `str \| None` | 最终表达式值（主结果） |
| `.logs.stdout` | `list[str]` | 全部 stdout 行 |
| `.logs.stderr` | `list[str]` | 全部 stderr 行 |
| `.error` | `ExecutionError \| None` | 执行失败的异常信息 |
| `.results` | `list[Result]` | 全部结果事件 |

## 示例

| 脚本 | 说明 |
|---|---|
| `examples/create_and_run.py` | 创建沙箱并执行代码 |
| `examples/context.py` | 内核上下文（服务端暂未实现） |
| `examples/lifecycle.py` | 暂停 / 连接 / 销毁 |
| `examples/list_and_health.py` | 列出沙箱与健康检查 |
| `examples/network_policy.py` | 网络策略（deny-all / 自定义） |
| `examples/volume.py` | 宿主机目录挂载 |
| `examples/run_all.py` | 运行所有示例 |

## DNS 绕过（远程访问）

在 CubeSandbox 节点之外运行时，操作系统 DNS 无法解析 `*.cube.app`。
设置 `CUBE_PROXY_NODE_IP` 以启用 `IPOverrideTransport`：所有数据面
连接直接路由到该 IP，并保留虚拟 `Host` 请求头供 CubeProxy 路由。

```
不使用 CUBE_PROXY_NODE_IP：
  SDK → OS DNS (*.cube.app) → CubeProxy

使用 CUBE_PROXY_NODE_IP：
  SDK → TCP 直连 CUBE_PROXY_NODE_IP:80
        Host: 49999-{sandboxID}.cube.app（保留用于路由）
```

## 许可证

Apache-2.0 © 2026 Tencent Inc.
