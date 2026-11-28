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

> Pass `--backend s3` to store the template (and every sandbox / snapshot derived from it) on the cluster-shared S3 CoW backend. That is required for [cross-node Pause / Resume / FromSnap](../cross-node-snapshot/requirements.md). Omit the flag to keep the historical `xfs` path.

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

`--user` sets the container user for the template build and for sandboxes created from that template. The value is `uid`, `uid:gid`, `username`, or `username:gid`. The group must be a number. Omit `--user` to keep the default root user.

```bash
cubemastercli tpl create-from-image \
  --image     ghcr.io/tencentcloud/cubesandbox-base:latest \
  --writable-layer-size 1G \
  --user      1000:1000
```

The same field on `POST /templates` is `user`. The template build stays privileged so the guest can set up its devices; only the container process user changes.

### Guest Kernel Command Line Parameters

`create-from-image` can append extra parameters to the guest kernel command line. This is intended for memory-overcommit tuning, where the setup relies on DAMON-based reclaim (`damon_reclaim.*`) and free page reporting (`page_reporting.*`).

Use `--kernel-cmdline-append` to pass parameters directly. The flag is repeatable, and each value is a single `module.param` or `module.param=value` entry:

```bash
cubemastercli tpl create-from-image \
  --image  ghcr.io/tencentcloud/cubesandbox-base:latest \
  --writable-layer-size 1G \
  --memory 4096 \
  --kernel-cmdline-append damon_reclaim.enabled=Y \
  --kernel-cmdline-append damon_reclaim.min_age=1000000 \
  --kernel-cmdline-append page_reporting.page_reporting_order=6
```

Use `--kernel-cmdline-file` to load the same list from a YAML or JSON file. The file must contain a top-level list:

```yaml
- damon_reclaim.enabled=Y
- damon_reclaim.min_age=1000000
- page_reporting.page_reporting_order=6
```

```bash
cubemastercli tpl create-from-image \
  --image  ghcr.io/tencentcloud/cubesandbox-base:latest \
  --writable-layer-size 1G \
  --memory 4096 \
  --kernel-cmdline-file damon.yaml
```

When both are given, the file is loaded first and `--kernel-cmdline-append` wins on duplicate keys (the text before `=`).

Only `page_reporting.*` and `damon_reclaim.*` are accepted. Each entry must match `^(page_reporting|damon_reclaim)\.[A-Za-z0-9_.]+(=[A-Za-z0-9_,.:/-]*)?$`. Whitespace, control characters and shell metacharacters are rejected, so a value cannot smuggle extra tokens into the guest command line. The user-supplied list is capped at 1024 bytes, well below the 2048-byte guest cmdline buffer on aarch64. Invalid parameters fail at `create-from-image` time, before the template build starts.

DAMON needs a physical address range to monitor. The kernel's default picks only a single memory block, which misses RAM above the PCI hole, so Cubelet fills two parameters automatically after placement when at least one `damon_reclaim.*` entry is present:

| Parameter | Value (amd64) |
|-----------|-------|
| `damon_reclaim.monitor_region_start` | `4096` (first page; `0` is treated as unset) |
| `damon_reclaim.monitor_region_end` | The guest RAM size plus the arch-specific PCI hole |

The end value is derived from the guest RAM size of the finalized VM spec, so on amd64 `--memory 4096` (4 GiB) produces `monitor_region_end=5368709120` (5 GiB). On arm64 the guest RAM starts at `1073741824` (1 GiB) and the end is the guest RAM plus 1 GiB, or plus 1.0625 GiB once the guest crosses the VMM's 64 MiB hole at 3.9375 GiB, so the same `--memory 4096` produces `monitor_region_end=5435817984` (5.0625 GiB). Cubelet computes the region after placement, using the worker node's architecture and the finalized VM RAM, so it applies to every instance type. Explicit non-zero `monitor_region_start` / `monitor_region_end` values are preserved and validated (`start < end`). When the region cannot be derived (non-amd64 / non-arm64 host, or zero guest RAM), pass both bounds explicitly; the request is rejected otherwise.

The guest kernel command line is sandbox-wide: every container in the sandbox shares the same VM. When calling the REST API directly, the equivalent annotation is `containers[0].annotations["cube.vm.kernel.cmdline.append"]`, a JSON array of strings. Setting it on any other container, or on the sandbox request itself, is rejected. When creating a sandbox from a template that sets these params, the annotation is inherited only if the request omits `containers[]`. If you supply containers explicitly, the template's container annotations are not copied, so re-send `containers[0].annotations["cube.vm.kernel.cmdline.append"]` in the request.

The parameters take effect only if the guest kernel is built with DAMON support (`CONFIG_DAMON_RECLAIM=y`). `damon_reclaim.enabled` defaults to `false`, so a guest that does not pass `damon_reclaim.enabled=Y` behaves exactly as before. To confirm the parameters reached the guest:

```bash
cat /proc/cmdline
cat /sys/module/damon_reclaim/parameters/enabled   # expects Y
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
| `template_spec_fingerprint` | Deterministic key for artifact reuse. Excludes runtime-only overrides (`--cpu`/`--memory`, `--cmd`/`--arg`, `--env`, `--probe`, working dir, volume mounts, rlimit); templates differing only in these fields share one artifact (`artifact_id`). `--expose-port`, `--network-type`, `--dns`, `--writable-layer-size`, `--instance-type` and `--user` still change the key. |

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

## Step 5 — (Optional) Migrate Legacy Local Artifacts

Most newly created `from-image` templates already build through the current TC data plane, so you usually do **not** need to run `tpl merge` for them. The typical migration case is older templates whose rootfs artifacts still live on CubeMaster local disk, and the cluster later enables `s3Backed=true` so those historical artifacts need to be moved into S3-backed storage.

Keep the wording consistent in operations docs: **`tpl merge` solves historical artifact storage convergence, while `tpl redo` solves node-side redistribution / rebuild when needed.** If the same maintenance window needs both storage migration and node repopulation, run them in this order:

```bash
cubemastercli tpl merge tpl-748094d2f2374b0a8a37e6ec
cubemastercli tpl redo --template-id tpl-748094d2f2374b0a8a37e6ec
```

> ⚠️ In default co-located / shared-PVC deployments, skipping `tpl merge` does not usually break downloads for existing `READY` templates immediately. The real issue is that those historical artifacts have not yet converged from local disk into S3-backed storage. If the local ext4 is already gone, rerunning `tpl merge` cannot recover it; for rebuildable `from-image` templates, `tpl redo` must fall back to rebuild.

## Step 6 — Deleting a Template

```bash
cubemastercli tpl delete tpl-748094d2f2374b0a8a37e6ec

# Delete multiple templates in one command
cubemastercli tpl delete tpl-first tpl-second tpl-third

# Clear stuck build work before deleting
cubemastercli tpl delete --force tpl-748094d2f2374b0a8a37e6ec
```

When multiple template IDs are provided, the CLI attempts every deletion even if one fails, then returns a combined error for the failed templates.

`--force` clears a stuck build job or definition build so deletion can proceed. It does **not** override the in-use check: the backend rejects deletion, even with `--force`, while a live sandbox still depends on the template.

On success:

```
template deleted: tpl-748094d2f2374b0a8a37e6ec
```

> ⚠️ A successful delete removes the template metadata and the replicas on every
> reachable node. New sandboxes can no longer be created from the template.

If a template's rootfs artifact is shared with other templates, deleting this
template keeps the artifact until the last template referencing it is deleted.

**Unreachable nodes.** A node CubeMaster has not heard from for more than 15
minutes does not block the delete: it is recorded for later cleanup and the
delete still succeeds, so that node may keep the template's local data for a
while. Once the node returns, CubeMaster finishes the cleanup in the background,
usually within 10 minutes. A node that never returns keeps its pending record.
A node unreachable for less than 15 minutes is still contacted; if that fails,
the delete returns an error and can be retried.

Do not reuse a deleted template ID until its pending cleanup has finished.

To follow pending cleanup, check the CubeMaster logs for these messages:

| Message | Meaning |
|---------|---------|
| `unreachable past grace` | A node was skipped and recorded for later cleanup |
| `orphan replica cleanup still failing` | The node is back but cleanup is still failing |
| `orphan replica cleanup converged` | The template's local data on that node is removed |
| `deferred placement cleanup converged` | The image file on that node is removed |

While `CUBE_MASTER_IMAGE_JOB_RECONCILE_DISABLED` is set on CubeMaster, the
template's local data on returning nodes is not cleaned up; image files still
are.


## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `phase: PULLING` stuck for a long time | Image pull slow or registry unreachable from cluster nodes | Check network/firewall; for private registries add `--registry-username` / `--registry-password` |
| Plain HTTP registry pull fails (`server gave HTTP response to HTTPS client`) | Image ref is missing the `http://` prefix | Use `http://harbor.internal:5000/ns/app:tag` |
| `status: FAILED` after BUILDING | Build error (disk full, Dockerfile issue, etc.) | Re-run `tpl status --job-id <id> --json` and inspect `last_error` |
| `distribution: 0/N ready` after READY | Artifact distribution still in progress (normal briefly) | Wait and re-run `tpl info`; if stuck check Cubelet logs on target nodes |
| Sandbox readiness probe keeps failing after startup | The service is not listening on the expected port/path, or the HTTP server started before the service was fully ready | Ensure the HTTP server starts only after the application is fully ready, and verify that `--probe-path` is correct |
