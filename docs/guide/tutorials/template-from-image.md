# Creating Templates from OCI Images

This guide explains how to create, monitor, and delete a template from a standard OCI container image.

Before you begin, consider reading [Templates Overview](../templates.md) to understand the related concepts, including OCI images, template snapshots, ports, probes, and `envd`.

## Prerequisites

- `cubemastercli` installed and able to connect to CubeMaster
- The OCI image must be accessible from the CubeMaster nodes (public registry
  or authenticated private registry)

> Plain HTTP registries (without TLS) must use an `http://` prefix, for example `http://harbor.internal:5000/ns/app:tag`. Without the prefix, CubeMaster uses HTTPS by default, except for localhost and RFC1918 addresses.

## Step 1 — Select an OCI Image

`create-from-image` accepts an OCI image that has already been built and published. This guide uses the CubeSandbox base image, which includes `envd`:

```text
ghcr.io/tencentcloud/cubesandbox-base:latest
```

The image runs `envd` on port `49983` by default, so `GET /health` can be used directly as the template probe.

For custom template images and guidance on integrating other existing images with CubeSandbox, see [Custom Template Images](./bring-your-own-image.md).

## Step 2 — Create the Template from the Image

Use the `tpl create-from-image` sub-command to kick off the build job:

```bash
cubemastercli tpl create-from-image \
  --image     ghcr.io/tencentcloud/cubesandbox-base:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

> Pass `--backend s3` to store the template (and every sandbox / snapshot derived from it) on the cluster-shared S3 CoW backend. That is required for [cross-node Pause / Resume / FromSnap](../cross-node-snapshot.md). Omit the flag to keep the historical `xfs` path.

Template creation can expose multiple ports, use a custom probe path, and pass environment variables:

```bash
cubemastercli tpl create-from-image \
  --image     cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe      49999 \
  --probe-path /health \
  --env        MY_ENV=production
```

## Step 3 — Monitor Progress

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


## Step 4 — Use the Template

Once `template_status: READY`, reference the `template_id` when creating
sandboxes via the E2B SDK:

```bash
export CUBE_TEMPLATE_ID=tpl-748094d2f2374b0a8a37e6ec
python CubeAPI/examples/create.py
```


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
cubemastercli tpl info tpl-748094d2f2374b0a8a37e6ec
```

The template ID can be passed as a positional argument (docker/kubectl style) or with `--template-id`; both forms are equivalent.

Add `--json` for machine-readable output:

```bash
cubemastercli tpl info tpl-748094d2f2374b0a8a37e6ec --json
```

Add `--include-request` when you want to inspect the stored template request body:

```bash
cubemastercli tpl info tpl-748094d2f2374b0a8a37e6ec --json --include-request
```

If you want to preview the effective sandbox payload after template resolution, use:

```bash
cubemastercli tpl render --template-id tpl-748094d2f2374b0a8a37e6ec --json
```

For a user-oriented walkthrough of what each output means and how to preview the effective request, see [Template Inspection and Request Preview](../template-inspection-and-preview.md).


## Deleting a Template

```bash
cubemastercli tpl delete tpl-748094d2f2374b0a8a37e6ec

# Delete multiple templates in one command
cubemastercli tpl delete tpl-first tpl-second tpl-third
```

When multiple template IDs are provided, the CLI attempts every deletion even if one fails, then returns a combined error for the failed templates.

On success:

```
template deleted: tpl-748094d2f2374b0a8a37e6ec
```

> ⚠️ Deletion removes both the template metadata and all node-local artifact
> replicas.  Any sandboxes already running from this template are **not**
> affected, but new sandboxes can no longer be created from it.


## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `phase: PULLING` stuck for a long time | Image pull slow or registry unreachable from cluster nodes | Check network/firewall; for private registries add `--registry-username` / `--registry-password` |
| Plain HTTP registry pull fails (`server gave HTTP response to HTTPS client`) | Image ref is missing the `http://` prefix | Use `http://harbor.internal:5000/ns/app:tag` |
| `status: FAILED` after BUILDING | Build error (disk full, Dockerfile issue, etc.) | Re-run `tpl status --job-id <id> --json` and inspect `last_error` |
| `distribution: 0/N ready` after READY | Artifact distribution still in progress (normal briefly) | Wait and re-run `tpl info`; if stuck check Cubelet logs on target nodes |
| Sandbox readiness probe keeps failing after startup | The service is not listening on the expected port/path, or the HTTP server started before the service was fully ready | Ensure the HTTP server starts only after the application is fully ready, and verify that `--probe-path` is correct |
