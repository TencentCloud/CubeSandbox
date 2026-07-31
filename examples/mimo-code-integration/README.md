# MiMo Code Dual-Fork Rollout Reference Pattern

[中文文档](README_zh.md)

This example combines two independent fork mechanisms:

- MiMo Code forks one parent planning session with `--session ... --fork`.
- CubeSandbox forks one complete MicroVM baseline with snapshot-backed sandboxes.

Each MiMo child implements the same task in an isolated candidate MicroVM. A
deterministic evaluator rejects unsafe or failing patches, selects the smallest
passing change, and promotes only that patch to the source MicroVM. If final
validation fails, CubeSandbox rolls the source back to the baseline snapshot.

This is a speculative coding transaction, not only an "agent runs in a
sandbox" example.

The lifecycle is the reusable reference pattern; the bundled
`fixtures/normalize-slug` project is only a deterministic demonstration task.
Task instructions, test command, editable paths, and candidate strategies live
in `task.json` instead of the orchestration code.

In the issue's use-case taxonomy, this is an enhanced **execute
Agent-generated code in sandboxes and collect the results** flow: candidates
modify task-approved files, the MicroVMs execute fixed acceptance tests, and the host
collects bounded test output and patch metadata before promoting one result.

## Architecture

```text
host driver
  |
  +-- credentialed planner MicroVM
  |     `-- MiMo plan-only turn (parent session)
  |                  |
  |                  `-- copy $MIMOCODE_HOME (not the key)
  |
  +-- source MicroVM
  |     +-- seed fixed acceptance tests
  |     +-- import parent session into a credential-free runtime
  |     +-- create baseline snapshot
  |     `-- apply winner or rollback
  |
  +-- candidate MicroVM A <- baseline snapshot
  |     `-- MiMo child session A <- parent --fork
  |
  `-- candidate MicroVM B <- baseline snapshot
        `-- MiMo child session B <- parent --fork
```

The parent is given a random continuity token that is never written to
`/workspace`. Every child must recall it from conversation state. This proves
that the workflow inherited MiMo context as well as the VM filesystem.

### Relationship to the snapshot examples

The CubeSandbox half follows the lifecycle invariants demonstrated by
[`07_clone_concurrent.py`](../snapshot-rollback-clone/07_clone_concurrent.py)
and
[`08_fork_three_axis.py`](../snapshot-rollback-clone/08_fork_three_axis.py):
create multiple sandboxes from one snapshot, preserve inherited state, isolate
subsequent writes, and keep the source sandbox usable. This example extends
those VM-only primitives by pairing every candidate with a distinct MiMo
`--fork` conversation branch, then adding deterministic selection, patch
promotion, and rollback.

## What the reference pattern proves

1. A plan-only MiMo parent turn leaves its Git workspace unchanged.
2. The driver transfers `$MIMOCODE_HOME` into a credential-free source VM.
3. One full-VM snapshot captures the repository and imported parent session.
4. Multiple candidate MicroVMs start from that identical baseline.
5. `mimo run --session <parent> --fork` creates a unique child session in each
   candidate.
6. Candidate writes remain isolated from siblings and the source.
7. Only text diffs listed in the task's `allowed_paths` can be promoted;
   test changes, other new files, binary/mode diffs, oversized patches, and
   failed tests are rejected. Ignored runtime artifacts never enter the patch.
8. Passing candidates are ranked deterministically by changed-line count and
   candidate name.
9. The selected patch must pass `git apply --check` and the same tests on the
   source.
10. A failed promotion validation triggers `rollback(snapshot_id)`.
11. Planner, candidate, and source sandboxes plus the persistent snapshot are
    cleaned up on success, failure, or interruption.

## Security boundary

The planner and every candidate MicroVM are created with:

- `allow_internet_access=False`;
- one exact allow rule for `api.xiaomimimo.com`;
- CubeEgress injection of the real MiMo Platform `api-key`;
- only `MIMO_API_KEY=cube-egress-managed-placeholder` inside the VM;
- sharing, telemetry, auto-update, model-manifest downloads, LSP downloads,
  and external skills disabled.

Each rollout uses a random CubeEgress rule name, allowing the evidence collector
to select only that run's audit records on a shared host.

The source is created default-deny without a credential rule. Only the
placeholder-bearing MiMo profile is copied into it before snapshotting, so the
real key does not enter the snapshot's VM data or persisted create request.
The real key stays in short-lived host-side CubeEgress rules for the planner
and candidates; it is not stored in a VM environment, MiMo profile, Git
workspace, candidate patch, or snapshot.

The profile handoff accepts at most 16 MiB of Base64 archive data and 64 MiB
after decompression, preventing an unbounded host-side archive expansion.

## Directory layout

```text
mimo-code-integration/
├── Dockerfile
├── build-template.sh
├── speculative_mimo_code.py  # reusable fork/test/promote/rollback lifecycle
├── rollout_task.py            # bounded task.json and fixture loader
├── fixtures/
│   └── normalize-slug/        # demonstration task.json + project/
├── run_mimo_code.py          # minimal template and NDJSON smoke test
├── network_policy.py         # focused CubeEgress preflight
├── env_utils.py
├── _mimo_common.py
├── collect_e2e_evidence.sh
├── requirements.txt
├── tests/
├── README.md
└── README_zh.md
```

## Prerequisites

- A running CubeSandbox deployment with CubeAPI reachable at
  `http://<cube-host>:3000`.
- CubeSandbox snapshot/rollback support and CubeEgress credential injection.
- `cubemastercli`, Docker, and a registry reachable by Cube nodes.
- Python 3.10+ and `cubesandbox>=0.6.0` on the host.
- A MiMo Platform API key from <https://platform.xiaomimimo.com>.

## 1. Build and register the template

```bash
export MIMO_IMAGE="<your-registry>/mimo-code-cube:0.1.7"
./examples/mimo-code-integration/build-template.sh
cubemastercli tpl watch --job-id <job_id>
```

The image pins `@mimo-ai/cli@0.1.7`, verifies both `mimo --version` and the
presence of the `mimo run --fork` option, and inherits CubeSandbox's `envd`
entrypoint.

## 2. Configure the host

```bash
cd examples/mimo-code-integration
install -m 600 .env.example .env
# Set E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID, and MIMO_API_KEY.
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

Important settings:

| Variable | Purpose |
| --- | --- |
| `E2B_API_URL` / `E2B_API_KEY` | CubeAPI connection |
| `CUBE_TEMPLATE_ID` | READY MiMo template ID |
| `MIMO_API_KEY` | Host-side secret used by CubeEgress |
| `MIMO_MODEL` | Defaults to `mimo/mimo-v2.5-pro` |
| `MIMOCODE_HOME` | MiMo profile root; defaults to `/root/.mimocode` |
| `MIMO_WORKSPACE` | Candidate Git workspace; defaults to `/workspace` |
| `MIMO_SANDBOX_TIMEOUT` | Sandbox timeout; defaults to 1800 seconds |
| `MIMO_AGENT_EXEC_TIMEOUT` | MiMo turn timeout; defaults to 900 seconds |
| `MIMO_EGRESS_AUDIT_PATH` | Optional host audit JSONL path for evidence collection |

Use HTTPS for a remote authentication-enabled CubeAPI. Plain HTTP is only
appropriate on a trusted local network.

## 3. Run the reference pattern

```bash
python speculative_mimo_code.py \
  --task fixtures/normalize-slug/task.json \
  --candidates 2 \
  --concurrency 2 \
  --evidence-file output/speculative-success.json
```

The bundled demonstration fixture contains an unimplemented `normalize_slug`
function and fixed acceptance tests. Its `task.json` declares the planning
and implementation instructions, test command, editable `app.py` path, and
candidate strategies. A short-lived planner runs a MiMo turn with file edits
and permission auto-approval disabled, then copies only the MiMo profile into
the credential-free source. Candidate MicroVMs fork that imported session.
`--concurrency` must be at least `--candidates`, ensuring every created
candidate is actively evaluated.

A successful run prints:

```text
CUBE_MIMO_PROMOTION_OK
```

The evidence JSON contains bounded execution metadata: sandbox and snapshot
IDs, parent/child MiMo session IDs, candidate test output, changed paths and
line counts, errors, winner, and final outcome. The schema deliberately omits
the patch body, but bounded test output is untrusted and may echo source text,
so review evidence before sharing it. The collector separately verifies that
the real MiMo key is absent.

### Exercise the rollback path

```bash
python speculative_mimo_code.py \
  --force-promotion-failure \
  --evidence-file output/speculative-rollback.json
```

This intentionally fails only the final source validation after a valid winner
has been applied. The driver must restore the clean source snapshot and print:

```text
CUBE_MIMO_ROLLBACK_OK
```

## Reuse the pattern with another task

Copy `fixtures/normalize-slug/` and change only its task profile and project:

```text
my-task/
├── task.json
└── project/
    ├── source files
    └── fixed acceptance tests
```

`task.json` defines:

- `name` and a short `summary`;
- `planning_instructions` and `implementation_instructions`;
- the fixed `test_command` and its `test_timeout_seconds`;
- existing files in `allowed_paths`;
- named candidate `strategies`;
- `expect_baseline_failure` for the fixture's initial test result.

The loader rejects absolute/traversal paths, duplicate paths or strategies,
symlinks, oversized fixtures, and editable files absent from the baseline.
The rollout lifecycle, credential boundary, snapshot handling, selection,
promotion, rollback, cleanup, and evidence format remain unchanged.

This is the task extension seam for later applications. The current evaluator
is intentionally binary—fixed tests pass or fail, then passing patches are
ranked by changed lines. A later MiMo Code `research-experiment` integration can
add a metric-aware evaluator while reusing the dual-fork transaction,
credential, rollback, cleanup, and evidence infrastructure.

## Supporting preflights

Run the minimal template and MiMo NDJSON smoke test (also uses CubeEgress
placeholder credentials, not a raw VM API key):

```bash
python run_mimo_code.py
```

Run the focused default-deny and credential-boundary preflight:

```bash
python network_policy.py
```

These are supporting checks. The speculative workflow is the integration's
main use case.

## Verification

Offline checks require no model key or CubeSandbox cluster:

```bash
python -m unittest discover -s tests -v
python -m py_compile *.py tests/*.py \
  fixtures/normalize-slug/project/*.py \
  fixtures/normalize-slug/project/tests/*.py
bash -n build-template.sh collect_e2e_evidence.sh
```

For a live cluster, the evidence collector runs both promotion and rollback
scenarios and checks that none of the run-owned sandbox or snapshot IDs remain:

```bash
./collect_e2e_evidence.sh
```

Generated evidence is written below `output/`, which is ignored by Git. Review
all evidence before sharing it.

## Deterministic selection

Candidate selection never asks another model to choose a winner. A candidate is
eligible only if:

- its forked MiMo session is distinct from the parent;
- it recalls the continuity token;
- the fixed acceptance tests pass and are rerun unchanged on the source;
- every changed path is declared by the task's `allowed_paths`;
- the patch is non-empty, textual, and within the configured size limit.

Eligible candidates are ordered by `(changed_lines, candidate_name)`. This
makes the same candidate set produce the same winner and keeps the security
decision outside model output.

## Failure and cleanup semantics

- Partial candidate creation is all-or-nothing; successful siblings are killed
  if any create call fails.
- One failed candidate does not discard other valid candidates.
- No eligible candidate aborts promotion and leaves the source unchanged.
- Failed final validation rolls the source back in place.
- The persistent baseline snapshot is explicitly deleted; killing its source
  sandbox does not delete it automatically.
- Cleanup failures are fatal unless another primary error is already being
  reported, in which case they are printed as warnings with resource IDs.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `mimo run` has no `--fork` | Stale template/CLI | Rebuild the pinned template |
| `401` / `403` from MiMo Platform | Missing, expired, or incorrectly injected API key | Check `MIMO_API_KEY`, the `api-key` injection rule, and the redacted CubeEgress audit |
| Template import cannot pull the image | Image was not pushed, registry credentials are unavailable to Cube nodes, or the architecture is wrong | Push an `linux/amd64` image to a registry every Cube node can pull from and configure registry credentials |
| Sandbox or MiMo command times out | Cluster capacity is exhausted or the task exceeds its limit | Reduce candidates, then raise `MIMO_SANDBOX_TIMEOUT` or `MIMO_AGENT_EXEC_TIMEOUT` as appropriate |
| No child session ID | MiMo CLI event contract changed | Keep `--format json` and inspect raw events |
| Continuity report rejected | Child did not inherit the parent context | Verify snapshot timing and `--session ... --fork` |
| Candidate changed a disallowed path | Agent edited tests or files outside the task policy | Tighten the prompt or update `allowed_paths` only when that file is intentionally editable |
| No eligible candidate | All tests or patch checks failed | Inspect per-candidate evidence |
| Promotion failed | Patch drift or source test failure | The driver rolls back automatically |
| TLS error | MiMo runtime does not trust CubeEgress CA | Set `MIMO_NODE_EXTRA_CA_CERTS` correctly |
| `403` or curl status `000` | Host does not match the exact allow rule | Use `api.xiaomimimo.com` and inspect audit logs |
| Snapshot remains after exit | Cleanup request failed | Delete the recorded snapshot/template ID manually |

## References

- [MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code)
- [MiMo Code sessions](https://mimo.xiaomi.com/mimocode/sessions)
- [CubeSandbox snapshot, rollback, and clone](../../docs/guide/snapshot-rollback-clone.md)
- [CubeEgress security proxy](../../docs/guide/security-proxy.md)
- [Documentation integration guide](../../docs/guide/integrations/mimo-code.md)
