# Snapshot · Rollback · Clone

[中文文档](README_zh.md)

End-to-end examples for the `cubesandbox` Python SDK's snapshot, rollback,
and clone APIs. Each script is standalone and runnable.

## What you get

| # | Demo | Topic |
|---|------|-------|
| 01 | `01_create_snapshot.py` | `sb.create_snapshot()` basics |
| 02 | `02_list_snapshots.py` | `Sandbox.list_snapshots()` — global / per-sandbox / paginated |
| 03 | `03_clone_from_snapshot.py` | Spawn a sandbox from a snapshot via `template=` |
| 04 | `04_state_preserved.py` | Filesystem and memory state survive snapshot + clone |
| 05 | `05_snapshot_outlives_sandbox.py` | Snapshot lifecycle is independent of the sandbox |
| 06 | `06_clone_n.py` | One-line `sb.clone(n=N)` to fan out N sandboxes |
| 07 | `07_clone_concurrent.py` | Parallel clone via `sb.clone(n=N, concurrency=C)` |
| 08 | `08_fork_three_axis.py` | Continuity / inheritance / isolation |
| 09 | `09_rollback.py` | `sb.rollback(snapshot_id)` to revert in place |
| 10 | `10_rollback_then_continue.py` | Rollback, keep running, re-snapshot the new branch |
| 11 | `11_delete_snapshot.py` | `Sandbox.delete_snapshot()` basics |

## Setup

> These examples require [`cubesandbox`](https://pypi.org/project/cubesandbox/) **>= 0.2.0**.

```bash
pip install "cubesandbox>=0.2.0"
# or:
pip install -r requirements.txt

export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx
```

## Run

Each demo is a self-contained script. From inside this directory:

```bash
python 01_create_snapshot.py
python 04_state_preserved.py
python 09_rollback.py
# ...
```

## API cheat sheet

```python
from cubesandbox import Sandbox

sb = Sandbox.create(template=TEMPLATE_ID)

# Snapshot
snap = sb.create_snapshot()              # → SnapshotInfo(snapshot_id=...)
items, token = Sandbox.list_snapshots()  # paginated; token=None when done
Sandbox.delete_snapshot(snap.snapshot_id)

# Clone (fan-out from a running sandbox)
clones = sb.clone(n=5)                   # serial
clones = sb.clone(n=10, concurrency=4)   # parallel via thread pool

# Rollback (in place — same sandbox_id afterwards)
sb.rollback(snap.snapshot_id)

# Spawn from a snapshot as a normal template
fresh = Sandbox.create(template=snap.snapshot_id)
```

## See also

- [Guide: Snapshot, Rollback & Clone](../../docs/guide/snapshot-rollback-clone.md)
- SDK source: [`sdk/python/cubesandbox`](../../sdk/python/cubesandbox)
- Other examples: [`examples/code-sandbox-quickstart`](../code-sandbox-quickstart) for the basic E2B-compatible flow
