# Creating Templates from OCI Images

This guide walks you through how to create, monitor, and delete a Cube-Sandbox
template starting from any standard OCI container image, using the
`cubemastercli` command-line tool.

## Overview

A **template** is a pre-built, immutable rootfs snapshot that the sandbox
runtime uses to cold-boot (or hot-start) a new sandbox instance.  Creating a
template from an OCI image is a three-phase pipeline that runs asynchronously
on the cluster:

```
OCI Image  ──pull──►  ext4 rootfs  ──boot──►  Snapshot  ──register──►  Template READY
```

Once the template reaches `READY` status it can be referenced by its
`template_id` to create sandboxes.

---

## Prerequisites

- `cubemastercli` installed and on `$PATH`
- `CUBEMASTER_ADDR` environment variable set, **or** pass `--server <host>` to
  every command
- The OCI image must be accessible from the CubeMaster nodes (public registry
  or authenticated private registry)

### ⚠️ Your image must expose an HTTP server

During template creation, Cube platform boots the container and **probes it
over HTTP** to determine when it is ready.  This means:

1. Your container image **must** run an HTTP server on a known port.
2. You **must** pass the following flags when creating the template:
   - `--expose-port <port>` — declare the port your HTTP server listens on
   - `--probe <port>` — tell Cube which port to probe
   - `--probe-path <path>` — the HTTP path Cube will `GET` (e.g. `/` or `/health`)
3. Your entrypoint should start the HTTP server **only after** the application
   is fully ready to serve traffic — Cube marks the template ready as soon as
   the probe returns HTTP 2xx, and sandboxes launched from that template will
   immediately receive requests.

Failing to expose an HTTP server or passing wrong probe parameters will cause
the template creation to time out.

---

## Step 1 — Create the Template

Use the `tpl create-from-image` sub-command to kick off the build job:

```bash
cubemastercli tpl create-from-image \
  --image     cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest \
  --writable-layer-size 1G \
  --expose-port 9000 \
  --probe 9000 \
  --probe-path /
```

> **Image registry:** Use `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest` (recommended for international access). If you are in mainland China, use `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest` instead.

On success the CLI immediately prints a `job_id` and a generated
`template_id` and exits — the build continues **asynchronously** on the
cluster.

```
job_id:      0042cd3a-c1d6-45fd-8757-2595ba0027e8
template_id: tpl-4ff5adc5eea44c14b1c8dbb3
attempt_no:  1
artifact_id:
status:      PENDING
phase:       PULLING
progress:    0%
```

#### Example — multiple ports, custom probe path, env var

```bash
cubemastercli tpl create-from-image \
  --image     cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe      49999 \
  --probe-path /healthz \
  --env        MY_ENV=production
```

> **Image registry:** Use `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` (recommended for international access). If you are in mainland China, use `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` instead.

---

## Step 2 — Monitor Progress

There are two ways to follow the build job.

### Watch (blocking, recommended)

`tpl watch` polls the job in a loop and exits only when the job reaches a
terminal state (`READY` or `FAILED`):

```bash
cubemastercli tpl watch --job-id <job_id>
```

Example output when the job completes:

```
job_id:                   2e71b561-153e-4c08-ac37-5270d94f5f15
template_id:              tpl-748094d2f2374b0a8a37e6ec
attempt_no:               1
artifact_id:              rfs-1e8e07c90e9bb8eff94ecde2
status:                   READY
phase:                    READY
progress:                 100%
distribution:             1/1 ready, 0 failed
template_spec_fingerprint: 1e8e07c90e9bb8eff94ecde20396002c411f6b812612a2a05086b85fe245b858
artifact_status:          READY
artifact_sha256:          5d413bc735062d49d36ef9c0e62cd0c3a915853be5ec0c7fba90e13d9fd33f79
template_status:          READY
```

Key output fields:

| Field | Description |
|-------|-------------|
| `status` / `template_status` | Overall job and template state. `READY` means the template is usable. |
| `phase` | Current pipeline phase: `PULLING` → `BUILDING` → `DISTRIBUTING` → `READY`. |
| `progress` | Percentage completion of the current phase. |
| `distribution` | `N/M ready` — how many cluster nodes have received the artifact. |
| `artifact_id` | Stable ID of the built rootfs artifact. |
| `artifact_sha256` | SHA-256 digest of the rootfs artifact for integrity verification. |
| `template_spec_fingerprint` | Deterministic fingerprint of the full template spec (image + flags). Same input always produces the same fingerprint. |

### Status (single snapshot)

If you only want a one-shot status check without blocking:

```bash
cubemastercli tpl status --job-id <job_id>
```

---

## Step 3 — Use the Template

Once `template_status: READY`, reference the `template_id` when creating
sandboxes via the E2B SDK:

```bash
export CUBE_TEMPLATE_ID=tpl-748094d2f2374b0a8a37e6ec
python CubeAPI/examples/create.py
```

---

## Querying Templates

### List all templates

```bash
cubemastercli tpl list
```

Output:

```
TEMPLATE_ID                  INSTANCE_TYPE   STATUS   CREATED_AT             IMAGE_INFO
tpl-748094d2f2374b0a8a37e6ec cubebox         READY    2026-04-02T08:10:30Z   docker.io/library/nginx:latest@sha256:abcd...
tpl-4ff5adc5eea44c14b1c8dbb3 cubebox         READY    2026-04-01T17:42:11Z   docker.io/library/python:3.11
```

`CREATED_AT` is returned in UTC RFC3339 format. `IMAGE_INFO` shows image reference
and digest when available (`image@sha256:...`), and falls back to the image
reference when digest is unavailable.

Use wide output when you need `VERSION` and `LAST_ERROR`:

```bash
cubemastercli tpl list -o wide
```

Add `--json` to get the full JSON payload for scripting:

```bash
cubemastercli tpl list --json | jq '.data[].template_id'
```

### Inspect a single template

```bash
cubemastercli tpl info --template-id tpl-748094d2f2374b0a8a37e6ec
```

Add `--json` for machine-readable output:

```bash
cubemastercli tpl info --template-id tpl-748094d2f2374b0a8a37e6ec --json
```

Add `--include-request` when you want to inspect the stored template request body:

```bash
cubemastercli tpl info --template-id tpl-748094d2f2374b0a8a37e6ec --json --include-request
```

If you want to preview the effective sandbox payload after template resolution, use:

```bash
cubemastercli tpl render --template-id tpl-748094d2f2374b0a8a37e6ec --json
```

For a user-oriented walkthrough of what each output means and how to preview the effective request, see [Template Inspection and Request Preview](../template-inspection-and-preview.md).

---

## Deleting a Template

```bash
cubemastercli tpl delete --template-id tpl-748094d2f2374b0a8a37e6ec
```

On success:

```
template deleted: tpl-748094d2f2374b0a8a37e6ec
```

> ⚠️ Deletion removes both the template metadata and all node-local artifact
> replicas.  Any sandboxes already running from this template are **not**
> affected, but new sandboxes can no longer be created from it.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `phase: PULLING` stuck for a long time | Image pull slow or registry unreachable from cluster nodes | Check network/firewall; for private registries add `--registry-username` / `--registry-password` |
| `status: FAILED` after BUILDING | Build error (disk full, Dockerfile issue, etc.) | Re-run `tpl status --job-id <id> --json` and inspect `last_error` |
| `distribution: 0/N ready` after READY | Artifact distribution still in progress (normal briefly) | Wait and re-run `tpl info`; if stuck check Cubelet logs on target nodes |
| Sandbox fails readiness probe | Service not listening on the expected port/path at startup | Verify your container starts the HTTP server before signalling ready; adjust `--probe-path` if needed |
