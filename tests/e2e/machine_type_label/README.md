# Machine-type node label E2E

Live checks for the node-label feature: CubeOps label management plus the
machine-type contract used by cross-node restore.

## What is covered

`test_label_management.py`

- `GET /api/v1/nodes` exposes `labels`.
- `PATCH` merges; `PUT` replaces user labels; `DELETE` removes one key.
- Delete works for keys containing `/` via `?key=`.
- machine-type is not writable through the generic label API (403 on set/delete/replace payload).
- Membership is the machine type catalog: `POST /api/v1/machine-types`, then
  `PUT /api/v1/nodes/:id/machine-type`.
- `PUT` (replace) preserves machine-type.
- `PUT` with a missing/null `labels` body returns 400 instead of clearing.
- Invalid machine-type names are rejected (shape `^[a-z0-9][a-z0-9-]{0,62}$`).
- Isolation survives a bulk replace.
- A Cubelet re-registration does not overwrite machine-type.

`test_cli.py`

- `cubeopscli node label list/set/replace/delete` against the live cluster.
- `replace` refuses machine-type; `delete` handles a slash key.
- Batch path and `--json` output.

`test_restore_candidates.py`

- Same-node pause/resume still works with a machine-type set.
- CubeMaster observes the label (its metadata sync has a short delay).
- Cross-node resume is attempted only when the cluster can host it: at least two
  healthy nodes with `HostFacts` and the same `cpuid_hash`. Otherwise the test
  **skips with the exact reason** rather than passing vacuously.

`test_node_lifecycle.py`

- A Cubelet restart does not overwrite the operator's machine-type.
- Data-plane labels (`kubernetes.io/*`, `beta.kubernetes.io/*`) come back after
  the restart.
- Marked `node_lifecycle`; skipped unless `--run-destructive` is passed and
  always collected last. Restarting Cubelet makes a single-node cluster
  temporarily unable to create sandboxes until CubeMaster re-learns the local
  template replica, so this cannot share a run with the sandbox tests.

`test_framework.py`

- Hermetic checks that run without `--run-e2e`.

## Label classes

| Class | Example | Admin API |
| --- | --- | --- |
| Operator label | `team=infra`, `zone=gz` | full set/replace/delete |
| Machine type | `cube.cubesandbox.io/machine-type` | catalog bind/unbind only; label API returns 403; `replace` keeps the stored value |
| Data plane | `kubernetes.io/*`, `beta.kubernetes.io/*`, `cube.cloud.tencentcloud.com/*` | read-only (403); kept by `replace` |
| Isolation | `cube.cloud.tencentcloud.com/scheduling-disabled` | only through the isolation endpoint |

Data-plane labels are read-only by design, and that is a behaviour change from
earlier releases: writing them through the admin API used to succeed. A replace
that dropped them left the node looking healthy while the scheduler kept
returning `no more resource`, because `kubernetes.io/instance-type` and friends
are affinity inputs. The isolation key lives in that namespace but is listed
separately because its own endpoint owns it.

## Requirements

- A running cluster with CubeOps (`:3010`) and CubeAPI (`:3000`).
- The default `admin/admin` account, or override `CUBEOPS_USERNAME` /
  `CUBEOPS_PASSWORD`.
- A READY template: `CUBE_TEMPLATE_ID`.
- `cubeopscli` on `PATH` for `test_cli.py`.
- `test_node_lifecycle.py` additionally needs `--run-destructive` and a runner
  where `systemctl restart cube-sandbox-cubelet` works.
- Cross-node resume additionally needs a shared `jfs`/`s3` backend with
  `remote_status=ready`; without it the test skips.

Do not run this suite against a cluster whose labels you care about: tests
mutate labels and restore the previous values afterwards, but an interrupted run
can leave a temporary `e2e-*` key behind.

## Run

```bash
cd tests/e2e/machine_type_label
CUBE_API_URL=http://127.0.0.1:3000 \
CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx \
CUBEOPS_URL=http://127.0.0.1:3010 \
python3 -m pytest --run-e2e
```

Hermetic only:

```bash
python3 -m pytest
```

Include the Cubelet-restart tests (opt-in; they briefly make the node
unschedulable and are collected last):

```bash
python3 -m pytest --run-e2e --run-destructive
```
