<p align="center">
  <strong>cubesandbox</strong> — CubeSandbox Python SDK
</p>

<p align="center">
  <a href="https://github.com/TencentCloud/CubeSandbox"><img src="https://img.shields.io/badge/CubeSandbox-GitHub-blue" alt="CubeSandbox" /></a>
  <a href="../../LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0" /></a>
  <img src="https://img.shields.io/badge/Python-3.9%2B-blue" alt="Python 3.9+" />
  <img src="https://img.shields.io/badge/version-0.7.3-orange" alt="v0.7.3" />
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

### 分叉（Fork）

```python
sb = Sandbox.create()

# 一次服务端调用：后端对 sb 只打一次快照，派生 N 个副本。
forks = sb.fork(count=3)              # list[Sandbox | Exception]，每个 fork 一项
forks = sb.fork(count=3, timeout=60)  # timeout：新副本空闲存活时长，单位秒

for f in forks:
    if isinstance(f, Sandbox):
        f.run_code("print('alive')")  # 运行中的副本
    else:
        print(f)                      # 该 fork 的异常
```

部分成功会保留副本且不抛异常；只有整体失败才抛异常（如源沙箱不存在）。
`count` 为 1..100（默认 1）。与 `clone` 不同，临时快照由服务端管理。

### 指定计算节点

通过计算节点 ID 或 Host IP 限定调度范围。只传一个节点即可将沙箱固定到
该节点；如果目标节点无法运行对应模板，创建会失败。

```python
sb = Sandbox.create(
    distribution_scope=["node-a"],
)
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
from cubesandbox import Sandbox, Volume, VolumeMount

# 创建卷 —— E2B 兼容的 Python 签名要求传入 name。
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
# 包装某个值可将本次沙箱挂载设为只读；原有写法继续保持读写且兼容 e2b。
with Sandbox.create(
    volume_mounts={"/dataset": VolumeMount(vol, read_only=True)},
) as sb:
    print(sb.files.read("/dataset/note.txt"))

# 列出 / 查询信息 / 连接 / 销毁
for v in Volume.list():                 # list[VolumeInfo]，仅有 ID 和 name
    print(v.volume_id, v.name)
Volume.get_info(vol.volume_id)          # -> VolumeAndToken（token 和可选 domain）
vol = Volume.connect(vol.volume_id)     # -> 返回 Volume 实例
Volume.destroy(vol.volume_id)           # -> bool；先杀掉所有挂载它的沙箱（不会自动 detach）
```

访问模式按沙箱挂载设置。同一个 Volume 可以在一个沙箱中读写，在另一个沙箱中只读；原有 e2b 形式无需修改。

卷的 `name` 必须匹配 `^[a-zA-Z0-9_-]+$` 且不超过 128 字符；非法名称
会在任何网络请求之前抛出 `ValueError`。完整 API、参数和错误码请参阅
[`docs/volume.zh.md`](docs/volume.zh.md)。

### 不启动沙箱，直通访问卷内容

同步 `Volume` 内容方法直接访问 E2B 协议的卷内容网关，不创建或连接辅助沙箱，
不需要模板或 `CUBE_PROXY_NODE_IP`。只有现有 `/volumes` 管理接口还不够：需要
已部署的内容网关和真实卷级凭证，默认 S3 插件不签发这种内容凭证。

```python
import os
from contextlib import closing
from cubesandbox import Volume

vol = Volume(os.environ["CUBE_VOLUME_ID"], "dataset", os.environ["CUBE_VOLUME_CONTENT_TOKEN"])
opts = {"api_url": os.environ["CUBE_VOLUME_API_URL"]}
path = os.environ["CUBE_VOLUME_TEST_PATH"]
data = vol.read_file(path, format="bytes", **opts)
info = vol.get_info(path, **opts)
with closing(vol.read_file(path, format="stream", **opts)) as chunks:
    for chunk in chunks:
        pass
```

`vol.list(path)` 是列目录，`Volume.list()` 是管理列表；`vol.get_info(path)` 返回
`VolumeEntryStat`，`Volume.get_info(volume_id)` 返回 `VolumeAndToken`。
支持逐次地址、token、headers 等参数；同步流提前退出时用 `closing` 释放连接。
HTTPX 超时仍是传输阶段/空闲限制，不是严格总传输期限。
详见[固定 E2B 合同](../../docs/e2b-volume-contract.md)和[直通范围](../../docs/volume-direct-access.md)。

### 直通卷 E2E 测试

在 `sdk/python` 安装当前源码后运行：

```sh
python3 -m pip install -e '.[dev]'
python3 -m pytest -q tests/test_volume.py tests/test_volume_content.py
python3 -m pytest -q tests/test_volume_direct_e2e.py --run-e2e
```

必填环境变量：`CUBE_VOLUME_API_URL`（或 `E2B_VOLUME_API_URL`）、`CUBE_VOLUME_ID`、
`CUBE_VOLUME_CONTENT_TOKEN`、`CUBE_VOLUME_TEST_PATH`，最后一项指向稳定的已有文件。
凭证从环境注入，不写入文件或命令行；非本机网关须用 HTTPS。

| 可选环境变量 | 用途 |
|---|---|
| `CUBE_VOLUME_TEST_SHA256` | 已有文件的 64 位十六进制 SHA-256，校验真实预期内容 |
| `CUBE_VOLUME_TEXT_PATH` | 额外的已有 UTF-8 文件，验证文本格式 |
| `CUBE_VOLUME_TEST_DIR` | 列目录路径，默认测试文件的父目录 |
| `CUBE_VOLUME_TEST_MANAGEMENT=1` | 启用已有管理列表/详情/connect 验证；需显式 `CUBE_API_URL`/`E2B_API_URL` 和服务要求的管理凭证 |
| `CUBE_VOLUME_E2E_WRITE=1` | 单独启用写文件、建目录、修改元数据、删除路径；需读写 token |
| `CUBE_VOLUME_WRITE_ROOT` | 唯一 `sdk-e2e-*` 测试目录的已有可写父目录，默认 `/` |

默认只读，不创建/删除卷，不启动沙箱。写入测试只清理自己新建的唯一目录与文件，
不会删除传入的卷；清理失败会报告待手动删除的路径，进程崩溃可能留下测试数据。
测试拦截 SDK 沙箱创建/连接及非卷 HTTP 路由；服务端是否偷偷启动 VM、租约是否正确
仍需服务端观测验证。未加 `--run-e2e` 时全部跳过；开启后缺必填配置会失败。
这不是完整 E2B 服务差分或租户/符号链接隔离认证。流测试会缓冲一次基准文件，
请选择测试机器内存可容纳的文件。

### 列表与健康检查

```python
from cubesandbox import Sandbox

print(Sandbox.health())     # {"status": "ok"}
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
| `Sandbox.create(template, *, timeout, env_vars, metadata, distribution_scope, volume_mounts, config)` | `POST /sandboxes` — 创建新沙箱（可限定计算节点或挂载卷） |
| `Sandbox.connect(sandbox_id, timeout=None, *, config)` | `POST /sandboxes/:id/connect` — 连接（暂停状态下自动恢复），并可选地重置空闲超时 |
| `Sandbox.list(config)` | `GET /sandboxes` — 列出运行中沙箱（v1） |
| `Sandbox.list_v2(config)` | `GET /v2/sandboxes` — 列出运行中与已暂停的沙箱（v2 E2B 视图） |
| `Sandbox.health(config)` | `GET /health` — 服务健康检查 |

### `Sandbox` — 实例方法

| 方法 | 说明 |
|---|---|
| `sb.run_code(code, *, on_stdout, on_stderr, on_result, on_error, envs, timeout)` | `POST /execute` — 执行代码，返回 `Execution` |
| `sb.get_info()` | `GET /sandboxes/:id` — 获取沙箱状态和元数据 |
| `sb.is_running(request_timeout=None)` | 判断沙箱是否运行中 — 先查控制面状态，仅 running/unknown 时才探测 envd `/health`，返回 `bool`（不可达/传输错误返回 `False`） |
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
| `Volume.create(name, **opts)` | `POST /volumes` → `Volume`；可选 Cube `driver` 扩展 |
| `Volume.connect(volume_id, **opts)` | `GET /volumes/:id` → `Volume`，不创建沙箱 |
| `Volume.list(**opts)` | `GET /volumes` → `list[VolumeInfo]`，仅 ID/name |
| `Volume.get_info(volume_id, **opts)` | `GET /volumes/:id` → `VolumeAndToken`，包含 token/可选 domain |
| `Volume.destroy(volume_id, **opts)` | `DELETE /volumes/:id` → `bool` |

沙箱挂载与直通访问是两条独立链路；`VolumeMount(vol, read_only=True)` 仅控制一次沙箱挂载。`create` / `connect` 返回 `Volume`，类级 `list` 返回 `VolumeInfo` 列表，类级 `get_info` 返回 `VolumeAndToken`；列表条目没有 token 字段。同步实例内容方法包括 `read_file`、`write_file`、`list`、`make_dir`、`get_info`、`update_metadata`、`exists`、`remove`。完整参考：[`docs/volume.zh.md`](docs/volume.zh.md)。

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
