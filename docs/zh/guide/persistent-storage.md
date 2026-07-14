# 持久化存储（Host Mount）

Cube Sandbox 运行在轻量级 MicroVM 内，默认情况下沙箱内写入的所有数据都是**临时的**，沙箱销毁后即消失。**Host Mount（宿主机挂载）** 是 Cube 特有的扩展能力，它将沙箱宿主机节点上的目录绑定挂载到沙箱内部，无需复制文件即可实现持久化、共享的存储。

## 概念模型

```
沙箱宿主机节点
┌────────────────────────────────────────────────────────────────┐
│  /data/shared/models  ────────► /models   (只读)               │
│  /data/shared/output  ────────► /output   (读写)               │
│                                                                │
│              KVM MicroVM（沙箱）                                │
└────────────────────────────────────────────────────────────────┘
```

Host Mount 将**沙箱宿主机节点上的绝对路径**映射到**沙箱 VM 内的路径**。读写挂载的变更在两侧立即可见——无需同步、无需上传、零延迟。

| 属性 | 说明 |
|------|------|
| **机制** | Linux bind-mount，由 Cubelet 在 VM 启动前执行 |
| **作用域** | 节点本地——`hostPath` 必须存在于运行沙箱的宿主机节点上 |
| **数量** | 一次 `Sandbox.create()` 调用可以指定多个挂载 |
| **访问模式** | 每个挂载可独立设置 `readOnly: true`（只读）或 `readOnly: false`（读写） |
| **路径限制** | `hostPath` 必须位于允许的目录前缀下（默认 `/data/shared/`） |
| **兼容性** | 同时支持 E2B SDK（`e2b_code_interpreter`）和 Cube SDK（`cubesandbox`） |

## 使用场景

- **大型数据集** —— 将多 GB 的数据目录挂载到多个沙箱中，无需逐一复制
- **模型权重** —— 在多个并发推理沙箱间共享只读模型目录
- **输出持久化** —— 将沙箱运行结果写入宿主路径，沙箱销毁后数据仍然保留
- **源码工作区** —— 挂载代码仓库供沙箱按需执行或分析

## 挂载描述符

Host Mount 通过 `Sandbox.create()` 的 `metadata` 字段中的 `host-mount` 键来指定。值为 **JSON 编码的数组**，每个元素是一个挂载描述符：

```json
[
  {
    "hostPath":  "/data/shared/mydir",
    "mountPath": "/mnt/data",
    "readOnly":  false
  }
]
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `hostPath` | string | 是 | **沙箱宿主机节点**上的绝对路径，必须位于允许的前缀下 |
| `mountPath` | string | 是 | 沙箱 VM 内的目标路径 |
| `readOnly` | bool | 是 | `true` = 只读；`false` = 读写 |

::: warning
`hostPath` 指的是**运行沙箱的宿主机节点**的文件系统路径，而不是执行 SDK 脚本的机器。如果你从远程机器调用 API，请确保该路径存在于沙箱宿主机节点上，而不是你的本地电脑上。
:::

## 快速开始

### 前置条件

- 已部署的 Cube Sandbox 集群
- Python 3.8+
- 宿主目录在创建沙箱前必须已存在于沙箱宿主机节点上

在沙箱宿主机节点上准备目录：

```bash
sudo mkdir -p /data/shared/rw /data/shared/ro
echo "hello from host" | sudo tee /data/shared/ro/greeting.txt
sudo chown -R 1000:1000 /data/shared/rw
```

安装 SDK：

```bash
pip install e2b-code-interpreter
# 或者
pip install cubesandbox
```

### 创建带 Host Mount 的沙箱

```python
import json
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    metadata={
        "host-mount": json.dumps([
            {
                "hostPath":  "/data/shared/rw",
                "mountPath": "/mnt/rw",
                "readOnly":  False,
            },
            {
                "hostPath":  "/data/shared/ro",
                "mountPath": "/mnt/ro",
                "readOnly":  True,
            },
        ])
    },
) as sandbox:
    result = sandbox.commands.run("ls /mnt/rw /mnt/ro")
    print("mount contents:", result.stdout.strip())
```

预期输出：

```
mount contents: /mnt/ro:
greeting.txt
/mnt/rw:
```

### 写入沙箱销毁后仍保留的数据

```python
import json
from cubesandbox import Sandbox

mounts = json.dumps([
    {"hostPath": "/data/shared/rw", "mountPath": "/mnt/rw", "readOnly": False},
])

with Sandbox.create(
    template=template_id,
    metadata={"host-mount": mounts},
) as sandbox:
    sandbox.commands.run("echo 'result data' > /mnt/rw/output.txt")

# 沙箱已销毁，但文件保留在宿主机上：
# cat /data/shared/rw/output.txt  →  result data
```

## 工作原理

| 步骤 | 发生了什么 |
|------|-----------|
| `Sandbox.create(metadata=...)` | CubeAPI 将 `host-mount` JSON 传递给 CubeMaster |
| CubeMaster 校验路径 | 检查 `hostPath` 是否在允许的前缀下，解析 `..` 防止路径穿越 |
| Cubelet 收到请求 | 解析挂载列表，在 VM 启动前执行 bind-mount |
| VM 启动 | 挂载路径在沙箱内的 `mountPath` 位置可见 |
| 只读挂载 | 内核强制 `MS_RDONLY`，写操作返回 `EROFS` |

## 路径安全限制

出于安全考虑，`hostPath` 被限制在一组**允许的目录前缀**之内。默认情况下，只有 `/data/shared/` 下的路径被允许。尝试挂载该范围之外的路径会在**沙箱创建时被拒绝**。

### 默认行为

开箱即用时，只有如下路径合法：

```python
# ✅ 允许
{"hostPath": "/data/shared/models", "mountPath": "/models", "readOnly": True}
{"hostPath": "/data/shared/team-a/output", "mountPath": "/output", "readOnly": False}

# ❌ 被拒绝
{"hostPath": "/etc/passwd", "mountPath": "/mnt/x", "readOnly": True}
{"hostPath": "/tmp/data", "mountPath": "/mnt/data", "readOnly": False}
{"hostPath": "/data/shared/../etc", "mountPath": "/mnt/x", "readOnly": True}  # 路径穿越被阻止
```

### 错误响应

如果指定了不允许的路径，SDK 会抛出 `ApiError` 异常：

```python
from cubesandbox import ApiError

try:
    sandbox = Sandbox.create(
        template=template_id,
        metadata={"host-mount": json.dumps([
            {"hostPath": "/etc/passwd", "mountPath": "/mnt/x", "readOnly": True}
        ])}
    )
except ApiError as e:
    print(e.status_code)  # 500
    print(str(e))
    # "host-mount" entry[0]: hostPath "/etc/passwd" is not within an allowed mount prefix
```

### 自定义允许的前缀

集群管理员可在 CubeMaster 配置文件中添加额外的允许前缀：

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/data/team-assets/"
    - "/mnt/nfs/datasets/"
```

列表为空或未配置时，默认值为 `["/data/shared/"]`。根路径 `/` 被明确禁止——如果出现在列表中，CubeMaster 将拒绝启动。

### 安全机制

| 机制 | 用途 |
|------|------|
| `filepath.Clean` | 解析 `..` 路径段，防止路径穿越绕过 |
| 前缀末尾 `/` 匹配 | 防止前缀伪造（如 `/data/shared_evil` 不会匹配 `/data/shared/`） |
| 启动时校验 | 拒绝配置中包含 `/`，防止意外暴露整个宿主机 |
| 双重验证 | annotation 路径（CubeAPI → CubeMaster）和直接 volume 路径均受保护 |

## 权限管理

Host Mount 保留宿主目录原始的所有者和权限位。`readOnly` 标志只控制 Cube 是否以只读模式挂载，**不会**覆盖 Linux 文件权限。

如果沙箱用户的 UID 与宿主目录 owner 不匹配，写入会报 `Permission denied`。快速解决：

```bash
# 将目录 owner 对齐为沙箱用户
sudo chown 1000:1000 /data/shared/rw
```

其他方案（只读挂载、POSIX ACL、root 执行等）详见 [Host Mount 权限排障](./troubleshooting/host-mount-permissions.md)。

## 多节点集群：共享存储

Host Mount 是**节点本地的**。在多节点集群中，`hostPath` 必须存在于调度沙箱的那台宿主机节点上。推荐的做法是使用共享存储，将相同的文件系统挂载到所有宿主机节点的统一路径下。

### NFS 示例

在每台沙箱宿主机节点上挂载 NFS：

```bash
# /etc/fstab 添加：
nfs-server:/export/shared  /data/shared  nfs  defaults,hard,intr  0 0
```

```bash
sudo mount -a
ls /data/shared   # 所有节点看到相同内容
```

之后沙箱可直接使用 `/data/shared/` 下的路径，无论调度到哪个节点都能访问到数据。

### 对象存储（S3/COS）示例

使用 FUSE 工具（如 `s3fs`、`cosfs`）将对象存储桶挂载为本地目录：

```bash
# 安装 cosfs（腾讯云 COS）
sudo apt-get install cosfs

# 配置凭证
echo "my-bucket:AKIDxxxx:xxxxxx" > /etc/passwd-cosfs
chmod 600 /etc/passwd-cosfs

# 挂载到 /data/shared
cosfs my-bucket /data/shared -ourl=https://cos.ap-guangzhou.myqcloud.com \
  -oallow_other -ouid=1000 -ogid=1000
```

```bash
# AWS S3 使用 s3fs
s3fs my-bucket /data/shared \
  -o iam_role=auto \
  -o url=https://s3.amazonaws.com \
  -o allow_other -o uid=1000 -o gid=1000
```

::: tip
对象存储挂载适合只读场景（模型权重、数据集）。写入性能和 POSIX 兼容性不如 NFS，频繁小文件写入建议使用 NFS 或块存储。
:::

## 多租户隔离

在多租户场景下，每个租户应只能访问自己的数据目录，不能看到或操作其他租户的文件。推荐的做法是利用目录结构 + 应用层控制实现租户间隔离。

### 目录规划

按租户 ID 划分子目录：

```
/data/shared/
├── tenant-a/
│   ├── datasets/
│   └── output/
├── tenant-b/
│   ├── datasets/
│   └── output/
└── tenant-c/
    └── ...
```

每个租户的沙箱只挂载属于自己的子目录：

```python
import json
from cubesandbox import Sandbox

tenant_id = "tenant-a"

mounts = json.dumps([
    {
        "hostPath": f"/data/shared/{tenant_id}/datasets",
        "mountPath": "/datasets",
        "readOnly": True,
    },
    {
        "hostPath": f"/data/shared/{tenant_id}/output",
        "mountPath": "/output",
        "readOnly": False,
    },
])

with Sandbox.create(
    template=template_id,
    metadata={"host-mount": mounts},
) as sandbox:
    sandbox.commands.run("ls /datasets /output")
```

租户 A 的沙箱只能看到 `/data/shared/tenant-a/` 下的内容，无法访问 `tenant-b` 或 `tenant-c` 的目录。

### 应用层强制隔离

在调用 `Sandbox.create()` 的业务代码中，根据当前认证用户的租户信息拼接 `hostPath`，**禁止用户自行传入任意路径**：

```python
def create_tenant_sandbox(tenant_id: str, template_id: str):
    """由平台侧控制挂载路径，租户无法指定任意 hostPath。"""
    base = f"/data/shared/{tenant_id}"
    mounts = json.dumps([
        {"hostPath": f"{base}/input",  "mountPath": "/input",  "readOnly": True},
        {"hostPath": f"{base}/output", "mountPath": "/output", "readOnly": False},
    ])
    return Sandbox.create(
        template=template_id,
        metadata={"host-mount": mounts},
    )
```

### 文件系统权限加固（可选）

结合 Linux 权限进一步确保隔离性——即使路径被绕过，操作系统层面也拒绝越权访问：

```bash
# 为每个租户创建独立目录，使用不同的 UID 或 GID
sudo mkdir -p /data/shared/tenant-a /data/shared/tenant-b
sudo chown 1001:1001 /data/shared/tenant-a
sudo chown 1002:1002 /data/shared/tenant-b
sudo chmod 0700 /data/shared/tenant-a
sudo chmod 0700 /data/shared/tenant-b
```

这样即使某个沙箱尝试越权访问（通过路径穿越等方式），也会被 CubeMaster 的路径校验和操作系统权限双重拦截。

### 隔离层次总结

| 层次 | 机制 | 作用 |
|------|------|------|
| 目录规划 | 按租户 ID 划分独立子目录，每个沙箱只挂载本租户路径 | 结构上隔离数据，租户间互不可见 |
| 应用层 | 平台代码拼接路径，不信任用户输入 | 正常场景下的租户隔离 |
| 操作系统 | 目录 owner/mode 权限 | 兜底防护，防止任何绕过 |

## 嵌套挂载：共享可读、写入范围独立

当一个挂载的 `mountPath` 位于另一个挂载的子目录下时，这两个挂载构成嵌套挂载。它适用于一组沙箱共享同一个工作区、但每个沙箱只能写入自己子目录的场景。这里的“组”可以是 Agent Team、项目、部门或租户；Cube Sandbox 本身不负责管理这些成员关系。

例如，一个 Agent Team 可以使用下面的宿主机目录结构：

```text
/data/shared/tenant-a/team-blue/
├── shared-input.txt
└── members/
    ├── agent-a/
    └── agent-b/
```

Agent A 以只读方式挂载共享工作区，再把自己的目录嵌套挂载为读写：

```python
import json

mounts = json.dumps([
    {
        "hostPath": "/data/shared/tenant-a/team-blue",
        "mountPath": "/workspace",
        "readOnly": True,
    },
    {
        "hostPath": "/data/shared/tenant-a/team-blue/members/agent-a",
        "mountPath": "/workspace/members/agent-a",
        "readOnly": False,
    },
])
```

Agent B 使用相同的只读父挂载，只把 `members/agent-b` 挂载为读写。各 Agent 可以使用不同的沙箱模板，存储目录和访问模式不需要因此改变。

| Agent A 可见的路径 | 可读 | 可写 | 原因 |
|--------------------|------|------|------|
| `/workspace/shared-input.txt` | 是 | 否 | 来自共享的只读父挂载 |
| `/workspace/members/agent-a/` | 是 | 是 | 该位置被 Agent A 的读写子挂载替换 |
| `/workspace/members/agent-b/` | 是 | 否 | 仍然来自共享的只读父挂载 |
| 其他租户的工作区 | 否 | 否 | 对应宿主机路径没有挂载到当前沙箱 |

这是一种**共享可读、写入范围独立**的模型。“写入范围独立”表示只有指定沙箱会获得该目录的可写挂载；如果只读父挂载暴露了这个目录，并不代表其他成员无法读取它。

### 挂载语义和约束

- AppSnapshot 恢复时，无论描述符按什么顺序传入，Cubelet 都会先应用父目标路径，再应用嵌套的子目标路径，避免后挂载的父目录遮住已经挂载的子目录。无关路径也会使用确定的顺序，但这个顺序不表示它们存在父子关系。
- 子挂载会在对应子树位置遮住父挂载；两个目录视图不会合并。子挂载生效期间，父挂载在该位置原有的文件会被隐藏。
- `readOnly` 对每个挂载独立生效，但仍需满足 Linux 的 UID、GID、mode 和 ACL 权限。即使子挂载是读写模式，如果宿主机权限不允许沙箱用户写入，仍会返回 `Permission denied`。
- 推荐使用一个只读父挂载和明确的读写子挂载。避免重复目标路径或相互重叠的读写挂载，否则目录归属和最终可见结果会变得难以判断。
- `mountPath` 应使用不含 `.` 或 `..` 路径段的规范绝对路径。不要依赖输入顺序处理相互重叠的目标路径。
- Host Mount 是节点本地的，并且数据位于沙箱快照之外。恢复时，每个 `hostPath` 都必须在调度节点上可用；如需多节点运行，应在所有节点的同一路径挂载共享文件系统。

::: warning 鉴权边界
调用 `Sandbox.create()` 的平台必须根据已经认证的沙箱归属主体和适用的组边界（如租户、团队或项目）生成 `hostPath`。不要接受用户任意传入的宿主机路径，也不要把 `/data/shared/tenants/` 之类的全局根目录挂载到某个租户的沙箱中：只读挂载只能阻止写入，不能阻止该沙箱读取其他租户的数据。
:::

## 最佳实践

- **输入数据优先使用只读挂载**（数据集、模型、配置文件等）。这可以防止沙箱意外写入，是最安全的默认选项。
- **为读写挂载使用专用目录**。避免挂载 `/` 或 `/home` 等范围过大的路径，创建专用目录（如 `/data/shared/output`）并只挂载它。
- **尽早检查权限**。在沙箱内运行 `id` 和 `stat` 确认有效 UID 与宿主目录所有者一致，再依赖写入操作。
- **定期清理输出目录**。宿主机上的输出目录会跨沙箱会话累积文件——与沙箱不同，它们不会在沙箱销毁时自动清理。
- **保持允许前缀尽量窄**。只向 `allowed_host_mount_prefixes` 添加具体的受信路径。不要添加 `/data/` 这样的顶层路径，优先使用更深的路径如 `/data/shared/`。

## 故障排查

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| `hostPath "..." is not within an allowed mount prefix` | 路径不在允许的前缀范围内 | 将数据放到 `/data/shared/` 下，或在 CubeMaster 配置中更新 `allowed_host_mount_prefixes` |
| 沙箱内 `No such file or directory` | 沙箱宿主机节点上 `hostPath` 不存在 | 在运行前在节点上创建目录 |
| 写入时 `Read-only file system` | 使用了 `readOnly: true` 挂载 | 改为 `readOnly: false` |
| 写入时 `Permission denied` | 宿主目录所有者与沙箱用户不匹配 | 参见上方[权限管理](#权限管理) |
| `Template not found` | 模板 ID 错误 | 运行 `cubemastercli tpl list` 确认 |
| `Connection refused` | CubeAPI 不可达 | 检查 `E2B_API_URL` 及端口 3000 是否开放 |

## 参考

- 示例代码：[`examples/host-mount/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount)
- 相关 issue：[#239](https://github.com/TencentCloud/CubeSandbox/issues/239)
