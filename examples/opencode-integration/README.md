# OpenCode + CubeSandbox Example

[中文文档](README_zh.md)

Run the stable [OpenCode](https://opencode.ai/) terminal coding agent inside a
CubeSandbox MicroVM. The example uses Tencent TokenHub Hy3 as a concrete
OpenAI-compatible model, while keeping image analysis, test execution, and file
changes inside the isolated VM.

## What is included

```text
opencode-integration/
├── Dockerfile               # pinned Cube base + OpenCode binary
├── build-template.sh        # reproducible amd64 build/push helper
├── opencode.json            # Hy3 provider and bounded permissions
├── .env.example             # host-only configuration template
├── env_utils.py             # URL/key validation and command builder
├── _opencode_common.py      # E2B command and JSONL helpers
├── run_opencode.py          # failing test -> Hy3 repair -> verification
├── resume_opencode.py       # OpenCode session + VM pause/resume
├── network_policy.py        # default-deny + CubeEgress key injection
├── tests/                   # offline unit tests
├── README.md
└── README_zh.md
```

## Compatibility

| Component | Version |
|---|---|
| OpenCode | `1.18.9` (stable V1 config) |
| Cube base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| CubeSandbox | `>= 0.3.0` for pause/resume; `>= 0.4.0` for CubeEgress vault |
| Host Python | 3.10+ |
| Model API | Tencent TokenHub Hy3, OpenAI-compatible Chat Completions |

OpenCode 2 is currently beta and uses a different `providers` schema. This
example deliberately pins stable OpenCode 1 and its singular `provider` config.

## Prerequisites

- A running CubeSandbox deployment and CubeAPI URL.
- `cubemastercli` connected to the cluster.
- Docker and a registry reachable from Cube nodes.
- A TokenHub API key with access to model `hy3`.
- Python 3.10+ on the host.

## 1. Build and register the template

```bash
IMAGE=<your-registry>/opencode-cube:1.18.9 PUSH=1 \
  ./examples/opencode-integration/build-template.sh
```

The Dockerfile pins both the OpenCode release and its SHA256 digest. It also
disables updates, sharing, external plugins, LSP downloads, and models.dev
fetches so the runtime needs only the configured LLM host.

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:1.18.9 \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Record the `template_id` after the job reaches `READY`.

## 2. Configure the host

```bash
cd examples/opencode-integration
cp .env.example .env
# Fill E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID, and HY3_API_KEY.
python -m venv .venv
.venv/bin/pip install -r requirements.txt
```

The API key remains in the ignored host `.env`. The image contains only
`{env:HY3_API_KEY}`.

## 3. Run the repair workflow

```bash
.venv/bin/python run_opencode.py
```

The driver creates this deterministic workflow:

```text
seed one failing unittest
-> start a MicroVM
-> inject the Hy3 key into one exec process
-> OpenCode asks Hy3 to inspect/run/edit
-> host verifies the test file is unchanged
-> host reruns tests and checks the diff
-> destroy the MicroVM
```

The bug uses floor division in a mean function. Acceptance is based on the
post-run test and Git diff, not on the model's prose.

> The direct flavor leaves internet egress open. A compromised agent could
> exfiltrate its process-level key. Use the vault flavor below on shared
> clusters.

## 4. Pause and resume

```bash
.venv/bin/python resume_opencode.py
```

Turn 1 creates `plan.md`; the driver captures OpenCode's actual `sessionID`,
pauses the MicroVM, reconnects, verifies both `/workspace` and
`/root/.local/share/opencode`, then passes the same session ID into turn 2.
The key is injected again per process and is not written into the image.

## 5. Default-deny egress and credential vault

```bash
.venv/bin/python network_policy.py
```

This recommended flavor:

- sets `allow_internet_access=False`;
- allows only the host parsed from `HY3_BASE_URL`;
- injects `Authorization: Bearer <secret>` inside CubeEgress;
- gives the VM only a harmless placeholder key;
- audits the allowed TokenHub request metadata;
- demonstrates that an unrelated host is blocked.

The script sets both `SSL_CERT_FILE` and `NODE_EXTRA_CA_CERTS` to the system
bundle so OpenCode trusts the CubeEgress interception CA. Override
`OPENCODE_CA_BUNDLE` if the image uses another path.

## Verify without a cluster

```bash
python -m unittest discover -s tests -v
python -m compileall -q .
docker build --check .
```

The OpenCode config was also checked with the pinned `1.18.9` binary and a live
Hy3 request on 2026-07-29, including one native `read` tool call. Full
pause/resume and CubeEgress validation still requires a running CubeSandbox
cluster.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `opencode: command not found` | Old template | Rebuild and register this image |
| `Model not found: tokenhub/hy3` | V1/V2 config mixed | Keep OpenCode `1.18.9` and singular `provider` |
| `401` | Key missing in direct mode or inject rule absent | Check `.env`; for vault check the Authorization inject |
| `404` | Base URL path is wrong | Keep exactly one trailing `/v1` |
| `403 Forbidden - CubeEgress` | Host does not match the rule | Derive it from the actual `HY3_BASE_URL` |
| TLS error in vault mode | Runtime does not trust CubeEgress CA | Set `OPENCODE_CA_BUNDLE` to the installed CA bundle |
| OpenCode tries another host | Updates/models/plugins not disabled | Keep the supplied image env flags and `--pure` |
| `sessionID` missing | Non-JSON output or interrupted turn | Keep `--format json`; finish turn 1 before pause |
| Template remains `PULLING` | Nodes cannot access registry | Push to an accessible registry and configure auth |

## References

- [Integration guide](../../docs/guide/integrations/opencode.md)
- [Snapshot, clone, and rollback](../../docs/guide/snapshot-rollback-clone.md)
- [Security proxy](../../docs/guide/security-proxy.md)
- [OpenCode providers](https://opencode.ai/docs/providers/)
