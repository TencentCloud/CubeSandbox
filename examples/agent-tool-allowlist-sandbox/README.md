# Agent Tool Allowlist Sandbox

[中文文档](README_zh.md)

Demonstrates argv allowlisting for agent tools **before** `Sandbox.create`:
allowlisted binaries run in a Cube Sandbox MicroVM; others fail on the host
(no sandbox on the deny path).

The gate is host-side (`assert_allowlisted`). It is not egress CIDR policy or
guest kernel enforcement.

**Use when:** the host may only forward a fixed tool set (`echo` / `ls` /
`cat`, …) and must reject shells / network tools early. Interpreters are an
explicit capability (`enable_code_execution=True`), not part of the default set.

**Not for:** full agent frameworks, or replacing
[`network-policy`](../network-policy).

## 1. Prerequisites

- A running Cube Sandbox deployment ([dev environment](../../docs/guide/dev-environment.md))
- Python 3.8+
- Docker only if you use Path B

```bash
pip install -r requirements.txt
```

## 2. Quick Start

### Step 1 — Create a template (Recommended)

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Note the printed `template_id`. Outside CN, use
`cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`.

| Item | Suggestion |
|------|------------|
| Writable layer | `1G` |
| Ports | `49999`, `49983`; `--probe 49999` |

### Step 2 — Configure environment

```bash
cp .env.example .env
# set E2B_API_URL (e.g. http://127.0.0.1:13000) and CUBE_TEMPLATE_ID
```

### Step 3 — Run

```bash
python verify_local.py          # unit + deny; no sandbox
python run_allowlisted.py       # allow (needs *.cube.app DNS or run in dev VM)
python run_denied.py            # deny on host; no sandbox
```

Allow (host gate passes, then create with `allow_internet_access=False` —
[`network-policy`](../network-policy) Mode 1 airgap; argv allow ≠ network):

```text
egress: allow_internet_access=False (airgap; argv gate != network)
agent-tool-allowlist-ok
artifact: artifact-ok
```

Deny:

```text
denied_as_expected: command not on tool allowlist: 'bash' ...
```

Evidence that deny never calls `Sandbox.create`: see
`test_assert_allowlisted_raises_before_create` and
`test_run_denied_script_has_no_sandbox_create` in `test_allowlist.py`
(also covered by `verify_local.py`).

On a host without `*.cube.app` DNS, use `python run_allowlisted_sidecar.py`
instead of `run_allowlisted.py` (set proxy vars in `.env.example`; see
[`e2b-dev-sidecar`](../e2b-dev-sidecar)). Optional `verify_local` knobs:
`ALLOWLIST_IMAGE_TAG`, `ALLOWLIST_USE_SIDECAR` (see `.env.example`).

## 3. Path B (optional) — build this example image

```bash
docker build -t agent-tool-allowlist-sandbox:latest .
# optional: --build-arg SANDBOX_CODE_IMAGE=.../sandbox-code:latest

cubemastercli tpl create-from-image \
  --image agent-tool-allowlist-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

If CubeMaster cannot see a local tag, retag/push to a reachable registry first.

## 4. Default allowlist

`allowlist.py` (`DEFAULT_ALLOWED_BINARIES`): `echo`, `uname`, `pwd`, `ls`,
`cat`, `head`, `wc`, `sha256sum`. Path-style binaries and shells such as
`bash` / `curl` are rejected. `python3` lives in `CODE_EXECUTION_BINARIES` and
is only added when `enable_code_execution=True`.

## 5. Limitations

- This demo is **capability-style tool gating** on the first argv token — not
  full parameter policy. Combining small tools / redirects is out of scope.
- Default policy **denies interpreter execution**. Enabling `python3` grants
  arbitrary code execution inside the guest, not "one more binary".
  Passing a custom `allowed_binaries` that includes `python3` also grants that
  capability without `enable_code_execution=True`.
- Host-side gate only — callers that skip `assert_allowlisted` can still send
  any command to the API. MicroVM isolation ≠ guest capability control.
- Mode 1 airgap blocks egress; it does **not** stop local misuse in the guest.
  Network tools stay off the default argv list; use [`network-policy`](../network-policy)
  for CIDR policy.
- In-image `/etc/cube-sandbox/tool-allowlist.txt` (Path B) is informational;
  the host gate is authoritative for this demo. Production should set
  capabilities explicitly (do not casually `| CODE_EXECUTION_BINARIES`).

## 6. Directory

```text
agent-tool-allowlist-sandbox/
├── README.md
├── README_zh.md
├── Dockerfile
├── allowlist.py
├── allowlist_sync.py
├── verify_local.py
├── run_allowlisted.py
├── run_allowlisted_sidecar.py
├── run_denied.py
├── test_allowlist.py
├── env_utils.py
├── requirements.txt
└── .env.example
```
