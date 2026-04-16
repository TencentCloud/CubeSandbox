# Templates Overview

A Template is the base image and configuration snapshot used to create Cube-Sandbox instances. This page covers the **concept and lifecycle** of templates.

- To create, monitor, query, or delete templates using the CLI, see [Creating Templates from OCI Images](./tutorials/template-from-image).
- To inspect template build status and preview the effective request, see [Template Inspection and Request Preview](./template-inspection-and-preview).

## Template Lifecycle (Three-Step Process)

1. **Init (Initialization Build)**
   Based on a base image (like Ubuntu) and Dockerfile, use a build engine like Buildkit to package a rootfs filesystem that meets the sandbox runtime requirements.

2. **Boot & Snapshot**
   Cold boot the initialized rootfs inside a MicroVM. Wait for the system and language environment (like Python, Node) to fully load, then take a snapshot of the memory and state at that moment.

3. **Deploy (Registration & Publishing)**
   Register the packaged Rootfs and Snapshot files into the system to become an available Template. Subsequently, this Template can be used to achieve **Hot Start** for sandboxes in the tens-of-milliseconds range.

## Next Steps

- [Creating Templates from OCI Images](./tutorials/template-from-image) — step-by-step CLI guide with probe configuration, progress monitoring, and troubleshooting.
- [Template Inspection and Request Preview](./template-inspection-and-preview) — how to inspect template status and preview the effective request.
