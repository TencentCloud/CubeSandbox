# SDK 与 API 参考指南

> CubeSandbox 的 SDK（Python/Go）用法、与 E2B 的兼容性对照、文件操作、网络设置、元数据管理，以及 E2B 迁移的注意事项。

## Python SDK 快速参考

```bash
pip install cubesandbox
```

### 创建沙箱

```python
from cubesandbox import Sandbox

# 最简创建（使用环境变量 CUBE_TEMPLATE_ID）
sandbox = Sandbox.create(template="<template-id>")

# 完整参数
sandbox = Sandbox.create(
    template="<template-id>",          # 模板 ID
    timeout=300,                        # idle 超时 (秒)
    metadata={"user": "alice"},        # 自定义元数据
    env_vars={"DEBUG": "true"},         # 环境变量
    allow_internet_access=True,        # 是否允许出网
    lifecycle={                         # 生命周期策略
        "on_timeout": "pause",
        "auto_resume": True,
    },
)
```

### 代码执行

```python
# 运行 Python 代码
result = sandbox.run_code("print('hello')")
print(result.stdout)   # "hello\n"
print(result.stderr)   # ""
print(result.exit_code) # 0

# 指定语言和执行超时 (ms)
result = sandbox.run_code(
    "console.log('hello')",
    language="javascript",
    timeout=10000,      # 10 秒超时
)

# 带环境变量
result = sandbox.run_code(
    "import os; print(os.environ['MY_VAR'])",
    env_vars={"MY_VAR": "42"},
)
```

### Shell 命令

```python
# 运行 Shell 命令
result = sandbox.commands.run("ls -la /home")
print(result.stdout)

# 带工作目录
result = sandbox.commands.run(
    "cat config.yaml",
    cwd="/app",
)

# 后台运行
proc = sandbox.commands.run("python server.py", background=True)
print(proc.pid)
# 发送信号
sandbox.commands.kill(proc.pid)
```

### 文件操作

```python
# 写入文件
sandbox.files.write("/app/script.py", "print('hello')")

# 读取文件
content = sandbox.files.read("/app/script.py")
print(content)

# 检查文件是否存在
exists = sandbox.files.exists("/app/script.py")

# 列出目录
entries = sandbox.files.list("/app")
for entry in entries:
    print(entry["name"], entry["type"])  # "script.py", "file"

# 创建目录
sandbox.files.make_dir("/app/data")
```

### 生命周期管理

```python
# 获取沙箱信息
info = sandbox.get_info()
print(info["state"])       # "running"
print(info["sandboxID"])   # "sb-xxxx"
print(info["startedAt"])   # "2026-07-25T10:00:00Z"

# 暂停与恢复
sandbox.pause()            # 手动暂停（释放 CPU/内存）
sandbox.connect()          # 手动恢复并重连

# 列出所有沙箱
for sb in Sandbox.list():
    print(sb["sandboxID"], sb["state"])

# 销毁
sandbox.kill()
```

### 快照与克隆

```python
# 创建快照
snapshot = sandbox.commit("my-checkpoint")
snapshot_id = snapshot.snapshotID

# 回滚到快照
sandbox.rollback(snapshot_id)

# 从快照克隆新沙箱
clone = Sandbox.create(
    template="<template-id>",
    snapshot_id=snapshot_id,
)
```

### 网络

```python
# 获取沙箱的访问 URL
host = sandbox.get_host(8080)  # 暴露端口 8080
# host = "https://8080-sb-xxxx.domain.com"

# HTTP 请求（走 CubeEgress 代理）
import requests
# 沙箱内执行 requests.get("https://api.openai.com")
# → CubeEgress 注入 API Key → 代理转发
```

### 元数据

```python
# 创建时设置元数据
sandbox = Sandbox.create(
    template="<template-id>",
    metadata={
        "user_id": "42",
        "session_id": "abc-123",
        "task": "code_review",
    },
)

# 更新元数据
sandbox.set_metadata({"status": "completed"})
```

## E2B 兼容性对照

CubeSandbox 对 E2B SDK 的 API 做了完全兼容。迁移只需改三个环境变量：

```bash
# E2B 环境变量
E2B_DOMAIN=e2b.dev
E2B_API_KEY=your-e2b-key

# CubeSandbox 环境变量（直接替换）
E2B_DOMAIN=<your-cube-domain>
E2B_API_KEY=<your-cube-api-key>

# 或者用 CubeSandbox 原生的
CUBE_DOMAIN=<your-cube-domain>
CUBE_API_KEY=<your-cube-api-key>
CUBE_TEMPLATE_ID=<your-template-id>
```

### 兼容性对照表

| E2B API | CubeSandbox API | 兼容性 |
|---|---|---|
| `Sandbox.create(template, timeout)` | 完全兼容 | ✅ 所有参数一致 |
| `sandbox.run_code(code)` | 完全兼容 | ✅ 返回格式一致 |
| `sandbox.commands.run(cmd)` | 完全兼容 | ✅ |
| `sandbox.files.read/write()` | 完全兼容 | ✅ |
| `Sandbox.list()` | 完全兼容 | ✅ |
| `sandbox.get_info()` | 完全兼容 | ✅ |
| `sandbox.kill()` | 完全兼容 | ✅ |
| `sandbox.pause()` | 完全兼容 | ✅ |
| `sandbox.connect()` | 完全兼容 | ✅ |
| `lifecycle.on_timeout="pause"` | 完全兼容 | ✅ |
| `lifecycle.auto_resume=True` | 完全兼容 | ✅ |
| `snapshot.commit()` | 完全兼容 | ✅ |
| `sandbox.get_host(port)` | 完全兼容 | ✅ |
| `envd API` | Cube agent + vsock | ⚠️ 底层协议不同，SDK 接口兼容 |

### 不兼容的边缘情况

| 情况 | CubeSandbox 行为 | 说明 |
|---|---|---|
| E2B 的 `Sandbox.list()` 跨用户返回 | Cube 返回同一 API Key 下的所有沙箱 | 权限模型不同 |
| E2B 的 24h/1h 硬超时 | Cube 没有硬超时（除非集群配置） | 更灵活 |
| E2B 的 `Sandbox.beta` | 不支持的实验性功能 | |
| E2B 的 `sandbox.envd.debug()` | 不支持 | |

## Go SDK 快速参考

```go
import "github.com/tencentcloud/CubeSandbox/sdk/go/cubesandbox"

// 创建沙箱
sandbox, err := cubesandbox.NewSandboxBuilder().
    WithTemplate("my-template").
    WithTimeout(300).
    WithOnTimeout(cubesandbox.OnTimeoutPause).
    WithAutoResume(true).
    Create()
defer sandbox.Kill()

// 执行代码
result, err := sandbox.RunCode("print('hello')")
fmt.Println(result.Stdout)

// 文件操作
err = sandbox.Files.Write("/app/main.py", []byte("print('ok')"))
data, err := sandbox.Files.Read("/app/main.py")

// 停止并恢复
err = sandbox.Pause()
err = sandbox.Connect()
```

## 错误处理

| HTTP 状态 | 含义 | 处理方式 |
|---|---|---|
| 201 | 沙箱创建成功 | |
| 200 | 操作成功 | |
| 404 | 沙箱不存在 | 可能已被销毁 |
| 409 | 冲突（如 resume 时资源不足） | 等待重试 |
| 410 | 沙箱已销毁 | 停止重试，创建新沙箱 |
| 503 | 服务暂不可用（如 resume 中） | Retry-After 指示等待时间 |

```python
from cubesandbox import Sandbox
from cubesandbox.exceptions import SandboxNotFoundError, TimeoutError

try:
    sandbox = Sandbox.connect("sb-xxx")
    result = sandbox.run_code("...")
except SandboxNotFoundError:
    print("Sandbox was killed or expired")
except TimeoutError:
    print("Code execution timed out")
```

## 使用模式示例

### 无状态代码执行

```python
def execute_code(code: str) -> str:
    sandbox = Sandbox.create(template="python-v1", timeout=30)
    try:
        result = sandbox.run_code(code)
        return result.stdout
    finally:
        sandbox.kill()
```

### 多轮对话 Agent

```python
sandbox = Sandbox.create(
    template="python-v1",
    timeout=300,
    lifecycle={"on_timeout": "pause", "auto_resume": True},
)

def handle_turn(code: str) -> str:
    # Agent 每次对话调用此函数
    # CubeSandbox 自动处理 idle 时的暂停/恢复
    result = sandbox.run_code(code)
    return result.stdout

# 利用空闲 timeout (300s)，多轮对话间隔长时自动暂停
```

### 批量任务

```python
def run_batch(tasks: list[str], concurrency: int = 10):
    from concurrent.futures import ThreadPoolExecutor

    def run_one(code):
        sandbox = Sandbox.create(template="python-v1", timeout=30)
        try:
            return sandbox.run_code(code)
        finally:
            sandbox.kill()

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        results = list(pool.map(run_one, tasks))
    return results
```

## 环境变量参考

| 变量 | 必需 | 默认值 | 说明 |
|---|---|---|---|
| `CUBE_DOMAIN` | 是 | — | CubeAPI 域名或 IP:port |
| `CUBE_API_KEY` | 否 | — | API 鉴权密钥 |
| `CUBE_TEMPLATE_ID` | 否 | — | 默认模板 ID |
| `CUBE_TIMEOUT` | 否 | — | 默认沙箱超时 (秒) |

E2B 兼容变量（也可用）：

| 变量 | 映射 |
|---|---|
| `E2B_DOMAIN` | → `CUBE_DOMAIN` |
| `E2B_API_KEY` | → `CUBE_API_KEY` |

## 总结

CubeSandbox 的 SDK 在 API 层面保持了与 E2B 的完全兼容，99% 的代码只需改环境变量。Python SDK 覆盖了代码执行、Shell 命令、文件操作、生命周期管理、快照/克隆、元数据管理。Go SDK 提供了相同的功能集。错误处理遵循 HTTP 标准语义，auto-pause/resume 策略让 Agent 工作负载在基础设施层实现弹性伸缩。
