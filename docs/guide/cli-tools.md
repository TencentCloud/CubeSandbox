# CLI Tools

CubeSandbox ships several host-side command-line tools for operations and debugging. The one-click package installs them on hosts. In Kubernetes deployments, node-local tools such as `cubecli` and `cube-runtime` are staged into component images or the host toolbox.

Use these tools from trusted operator machines only. They bypass the public CubeAPI user experience and can inspect or mutate cluster/runtime state directly.

## Tool Summary

| Tool | Run From | Talks To | Main Use |
|------|----------|----------|----------|
| `cubemastercli` | Control node, jumpserver, or any host that can reach CubeMaster | CubeMaster HTTP API, default port `8089` | Cluster-wide sandbox, template, snapshot, and volume operations |
| `cubeopscli` | Control node, jumpserver, or any host that can reach CubeOps | CubeOps HTTP API, default port `3010` | Node list, isolate/unisolate, delete node |
| `cubecli` | The compute node that runs Cubelet and containerd | Local Cubelet/containerd state | Per-node sandbox/container inspection, container shell, logs, storage cleanup, local runtime debugging |
| `cube-runtime` | The compute node that hosts the sandbox MVM | Local CubeShim hybrid-vsock/debug console | Enter the guest MVM or run low-level VM snapshot helpers |

The one-click installer creates `/usr/local/bin` symlinks for `cube-runtime`, `containerd-shim-cube-rs`, `cubecli`, `cubemastercli`, and `cubeopscli`. `cubemastercli` and `cubeopscli` are included in the release package and installed on the Terraform jumpserver.

## `cubemastercli`

`cubemastercli` is the cluster-level management CLI. It targets CubeMaster, so commands usually need `--address` and `--port` unless they run on a host where the defaults are correct.

```bash
cubemastercli --address <cubemaster-host> --port 8089 --help
cubemastercli --address <cubemaster-host> --port 8089 version
```

Common cluster checks:

```bash
# List all sandboxes known to CubeMaster.
cubemastercli --address <cubemaster-host> --port 8089 list --all

# Inspect one sandbox.
cubemastercli --address <cubemaster-host> --port 8089 info --sandboxid <sandbox-id>
```

Template operations:

```bash
# List templates.
cubemastercli --address <cubemaster-host> --port 8089 tpl ls

# Show template metadata and node replicas.
cubemastercli --address <cubemaster-host> --port 8089 tpl info <template-id>

# Rebuild or redistribute a template on one node after adding compute capacity.
cubemastercli --address <cubemaster-host> --port 8089 tpl redo \
  --template-id <template-id> \
  --node <node-id-or-host>

# Create a template from an OCI image.
cubemastercli --address <cubemaster-host> --port 8089 tpl create-from-image \
  --image <registry>/<repo>:<tag> \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983
```

Destructive operations should be used carefully:

```bash
# Destroy one sandbox through CubeMaster.
cubemastercli --address <cubemaster-host> --port 8089 cubebox destroy <sandbox-id>
```

For multi-node deployment and template distribution context, see [Multi-Node Cluster](./multi-node-deploy.md).

## `cubeopscli`

`cubeopscli` is the node management CLI. It targets CubeOps, so commands need `--address` and `--port` unless running on a host where the defaults are correct.

```bash
cubeopscli --address <cubeops-host> --port 3010 node list
cubeopscli --address <cubeops-host> --port 3010 node isolate <node-id>
cubeopscli --address <cubeops-host> --port 3010 node unisolate <node-id>
cubeopscli --address <cubeops-host> --port 3010 node delete <node-id>
cubeopscli --version
cubeopscli version
cubeopscli version --versiononly
```

Deleting a node requires it to be **isolated and free of sandboxes**; in a batch, a failure on one node does not stop the rest, and the command exits non-zero if any deletion fails. `delete` is aliased as `rm`; use `--force` to delete when the sandbox inventory cannot be verified (isolation is still required).

`cubeopscli --version` and `cubeopscli version` print the release version (e.g. `cubeopscli v0.7.0 (<commit>) built at <timestamp>`); `cubeopscli version --versiononly` prints just the semantic version.

For adding, isolating, and deleting nodes, see [Node Operations](./node-operations.md).

## `cubecli`

`cubecli` is a compute-node tool. Run it on the node that hosts the target sandbox unless the command explicitly targets Cubelet over a configured address.

```bash
cubecli --help
cubecli version
```

Common node-local checks:

```bash
# List sandboxes on the local Cubelet.
cubecli cubebox ls

# Filter the local Cubelet sandbox list by sandbox ID.
cubecli cubebox ls --sandbox <sandbox-id>

# Inspect containerd metadata for a container ID.
cubecli container info <container-id>

# Read stdout/stderr logs for a sandbox or template.
cubecli logs <sandbox-id>
cubecli logs --stderr <sandbox-id>

# List local storage volumes known by Cubelet.
cubecli storage ls

# Dry-run local orphan storage cleanup before deleting anything.
cubecli storage cleanup --dry-run

# Show Cubelet network runtime tap state. Requires the Cubelet toolbox config and prompts for confirmation.
cubecli network ls
```

To enter the sandbox container/rootfs view:

```bash
cubecli exec -it <sandbox-id> bash
```

This creates an exec process through the local container runtime. It is useful for checking user processes, files, environment variables, command behavior, and container-level logs. It is not the same as logging into the guest MVM.

Use unsafe commands only when you understand the local-node blast radius:

```bash
# Example: remove all local sandboxes from this node only.
cubecli unsafe rm --all
```

In multi-node clusters, local `cubecli` operations only cover the node where the command runs. Prefer `cubemastercli` for cluster-wide operations.

## `cube-runtime`

`cube-runtime` is a lower-level runtime helper from the CubeShim workspace. Operators mostly use it to enter a sandbox MVM through the debug console.

```bash
cube-runtime --help
cube-runtime login --help
```

To log in to the sandbox MVM:

```bash
cube-runtime login <sandbox-id>
```

`login` connects to the sandbox's local hybrid-vsock path and then to the debug console port. The default debug console port is `1026`; the default connection timeout is `10` seconds.

```bash
cube-runtime login <sandbox-id> --port 1026 --timeout 10
```

Use `cube-runtime login` when you need the guest VM view, for example to inspect guest kernel state, guest network interfaces, agent state, mounts, or MVM-level pause/resume behavior.

`cube-runtime snapshot` exists for low-level snapshot workflows and is normally driven by higher-level Cubelet/CubeMaster paths. Prefer documented template and snapshot operations through `cubemastercli` unless you are debugging runtime internals.

## Choosing the Right Tool

Use `cubemastercli` when the question is cluster-level:

- Which nodes are healthy?
- Which template replicas are ready?
- Which node hosts a sandbox?
- How do I redo a template after adding a node?
- How do I destroy a sandbox through the control plane?

Use `cubecli` when the question is node-local:

- Is this sandbox present on this Cubelet?
- Can I enter the sandbox container?
- What does the local container log say?
- Are there orphan local storage volumes?
- What local tap/network state does Cubelet see?

Use `cube-runtime` when the question is inside the MVM:

- Can I enter the guest debug console?
- What does the guest kernel or VM-level network see?
- Is the problem below the container/rootfs layer?

## Safety Notes

- Avoid pasting secrets, API keys, registry credentials, or private endpoint values into shell history.
- Prefer `--json` where available when collecting evidence for issues; redact sensitive values before sharing logs.
- In multi-node clusters, first identify the sandbox's host node with `cubemastercli info --sandboxid <sandbox-id>`, then run `cubecli` or `cube-runtime` on that compute node.
- Treat `cubecli unsafe ...` and destructive `cubemastercli` commands as operational changes, not read-only diagnostics.
