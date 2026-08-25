# Templates Overview

A Template is the base image and configuration snapshot used to create Cube-Sandbox instances. This page covers the **concept and lifecycle** of templates.

- To create, monitor, query, or delete templates using the CLI, see [Creating Templates from OCI Images](./tutorials/template-from-image.md).
- To inspect template build status and preview the effective request, see [Template Inspection and Request Preview](./template-inspection-and-preview.md).

## Template Lifecycle (Three-Step Process)

1. **Init (Initialization Build)**
   Based on a base image (like Ubuntu) and Dockerfile, use a build engine like Buildkit to package a rootfs filesystem that meets the sandbox runtime requirements.

2. **Boot & Snapshot**
   Cold boot the initialized rootfs inside a MicroVM. Wait for the system and language environment (like Python, Node) to fully load, then take a snapshot of the memory and state at that moment.

3. **Deploy (Registration & Publishing)**
   Register the packaged Rootfs and Snapshot files into the system to become an available Template. Subsequently, this Template can be used to achieve **Hot Start** for sandboxes in the tens-of-milliseconds range.

## Committing a Running Sandbox

`tpl commit` creates a new template from the current filesystem and memory state of a running sandbox. `--sandbox-id` identifies the source sandbox and is required.

By default, the CLI sends only the sandbox ID. CubeMaster restores the sandbox's canonical create-time request from its stored sandbox spec. For legacy sandboxes without a stored spec, CubeMaster falls back to the create request of the sandbox's origin template.

The CLI displays build progress and waits until template creation succeeds or fails.

`--file <path>` supplies a complete `CreateCubeSandboxReq` override. The file replaces the automatically resolved request and is not merged with it.

When `--file` is used, the existing network flags can adjust `cube_network_config` in that request:

| Option | Effect |
|--------|--------|
| `--allow-internet-access=false` | Set the request's internet access value explicitly. |
| `--allow-out-cidr <cidr>` | Append an allowed egress CIDR; repeat for multiple CIDRs. |
| `--deny-out-cidr <cidr>` | Append a denied egress CIDR; repeat for multiple CIDRs. |

Network override flags require `--file`.

```bash
# Let CubeMaster restore the sandbox's create-time request.
cubemastercli tpl commit \
  --sandbox-id <sandbox-id>

# Supply a complete request override.
cubemastercli tpl commit \
  --sandbox-id <sandbox-id> \
  --file template-request.json \
  --allow-internet-access=false
```

CubeMaster generates the resulting `tpl-...` ID and creates its initial replica on the source sandbox's node.

## Next Steps

- [Creating Templates from OCI Images](./tutorials/template-from-image.md) — step-by-step CLI guide with probe configuration, progress monitoring, and troubleshooting.
- [Cross-Node Snapshots](./cross-node-snapshot.md) — `--backend s3` and the conditions for Resume / FromSnap on another node.
- [Template Inspection and Request Preview](./template-inspection-and-preview.md) — how to inspect template status and preview the effective request.
