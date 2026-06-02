# 快照 · 回滚 · 克隆

[English](README.md)

`cubesandbox` Python SDK 中 snapshot / rollback / clone 三组接口的端到端示例。
每个脚本都是独立、可直接运行的。

## 示例清单

| # | 脚本 | 主题 |
|---|------|------|
| 01 | `01_create_snapshot.py` | `sb.create_snapshot()` 基础用法 |
| 02 | `02_list_snapshots.py` | `Sandbox.list_snapshots()`：全量 / 按 sandbox_id 过滤 / 分页 |
| 03 | `03_clone_from_snapshot.py` | 用 `template=` 参数从快照启动新沙箱 |
| 04 | `04_state_preserved.py` | 文件系统与内存状态在 snapshot + clone 后均得以保留 |
| 05 | `05_snapshot_outlives_sandbox.py` | 快照生命周期独立于源沙箱 |
| 06 | `06_clone_n.py` | 一行 `sb.clone(n=N)` 派生 N 个沙箱 |
| 07 | `07_clone_concurrent.py` | `sb.clone(n=N, concurrency=C)` 并发派生 |
| 08 | `08_fork_three_axis.py` | 连续性 / 继承性 / 隔离性验证 |
| 09 | `09_rollback.py` | `sb.rollback(snapshot_id)` 原地回滚 |
| 10 | `10_rollback_then_continue.py` | 回滚后继续执行 + 在新分支上再次快照 |
| 11 | `11_delete_snapshot.py` | `Sandbox.delete_snapshot()` 基础用法 |

## 环境准备

> 本目录示例依赖 [`cubesandbox`](https://pypi.org/project/cubesandbox/) **>= 0.2.0**。

```bash
pip install "cubesandbox>=0.2.0"
# 或：
pip install -r requirements.txt

export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx
```

## 运行

每个 demo 都是独立脚本。在本目录下：

```bash
python 01_create_snapshot.py
python 04_state_preserved.py
python 09_rollback.py
# ...
```

## API 速查

```python
from cubesandbox import Sandbox

sb = Sandbox.create(template=TEMPLATE_ID)

# 快照
snap = sb.create_snapshot()              # → SnapshotInfo(snapshot_id=...)
items, token = Sandbox.list_snapshots()  # 分页；token 为 None 表示结束
Sandbox.delete_snapshot(snap.snapshot_id)

# 克隆（从运行中的沙箱一对多派生）
clones = sb.clone(n=5)                   # 串行
clones = sb.clone(n=10, concurrency=4)   # 通过线程池并发

# 回滚（原地，sandbox_id 保持不变）
sb.rollback(snap.snapshot_id)

# 把快照当作模板启动新沙箱
fresh = Sandbox.create(template=snap.snapshot_id)
```

## 相关文档

- [教程：快照、回滚与克隆](../../docs/zh/guide/snapshot-rollback-clone.md)
- SDK 源码：[`sdk/python/cubesandbox`](../../sdk/python/cubesandbox)
- 其他示例：[`examples/code-sandbox-quickstart`](../code-sandbox-quickstart) 演示 E2B 兼容的基础用法
