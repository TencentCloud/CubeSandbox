# Free Page Reporting E2E

This node-local test validates that a sandbox created through CubeAPI returns Guest-free pages to the host while it remains running. It repeats the same high/low memory cycle after Cube pause/resume, which exercises snapshot restore.

The runner must execute on the selected Cubelet node because it reads the VMM process RSS from `/proc/<pid>/status`. It resolves the process from `vmm.pid` when that file is available, verifies that the PID belongs to the requested sandbox, then falls back to the CubeShim command line for runtimes that remove the containerd task-state directory after startup. Pinning the sandbox with `--node` prevents a host RSS sample from being associated with a sandbox running elsewhere.

```bash
CUBE_API_URL=http://127.0.0.1:3000 \
CUBE_E2E_IMAGE=registry.example.com/sandbox-code:tag \
CUBE_E2E_NODE=node-name \
tests/e2e/memory_reclaim/free_page_reporting.sh
```

Using `--image` builds a temporary 2 GiB template with the currently deployed components, then deletes it after the test. The runner tracks the template as soon as the create response returns, including build failure and timeout paths. This is the preferred default-wiring check: a legacy template keeps the device topology saved by its older snapshot and therefore does not gain a balloon when restored.

Free-page reporting is disabled by default on aarch64, so this reclamation test requires `CUBE_BALLOON_FREE_PAGE_REPORTING=on` in the Cubelet environment on that architecture. Set the variable, restart Cubelet so its embedded containerd inherits it, and only then create the temporary template or redo an existing one. On Helm deployments, add the variable to `cubeNode.env`; on one-click deployments, add it to the bundle `.env` and rerun the installer. Existing running shims and snapshots retain their previous device topology.

To validate the upgrade path for an old template, redo that template with the upgraded components before running this test, then pass its ID with `--template`. Redo is an explicit test prerequisite and is intentionally not performed by the E2E runner.

The default workload allocates and touches 1,536 MiB. The test waits for the Guest workload's ready marker before sampling the peak, so the complete mapping has been touched before release. It checks for at least a 1,024 MiB rise, then waits for released RSS to return within 384 MiB of the pre-workload baseline without enforcing a fixed reclamation latency. The result records the actual reclaimed amount for both cold boot and pause/resume.

Use `--runtime-state-root` when the Cubelet containerd task-state directory differs from the standard `/data/cubelet/state` or `/data/cubelet/root` layouts. The script destroys its sandbox by default and waits for the API resource, shim process, and task-state directory to disappear. Cleanup failures make the E2E fail; `--keep-resources` is only intended for debugging.
