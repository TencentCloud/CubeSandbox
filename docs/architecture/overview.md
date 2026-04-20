# Architecture Overview

Cube Sandbox follows a clear layered architecture from top to bottom.

## Layered Architecture

![Cube Sandbox Architecture](../assets/cube-sandbox-arch.png)

## Key Components

1. **CubeAPI**: E2B-compatible REST API gateway. Switch from E2B Cloud to Cube Sandbox seamlessly by simply replacing environment variables such as the URL.
2. **CubeMaster**: Orchestration scheduler that receives E2B API requests and dispatches them to the corresponding Cubelet, handling resource scheduling and cluster state management.
3. **CubeProxy**: Reverse proxy and request routing component that parses the `<port>-<sandbox_id>.<domain>` format in the Host header to forward SDK client requests to the target sandbox instance.
4. **Cubelet**: Node-local scheduling component that manages the full lifecycle of all sandbox instances on a single node.
5. **CubeVS**: eBPF-based kernel-level packet forwarding, providing comprehensive network isolation and security policy enforcement.
6. **CubeHypervisor & CubeShim**: The virtualization layer of Cube Sandbox. CubeHypervisor manages KVM MicroVMs, CubeShim implements the containerd Shim v2 API to integrate sandboxes into the container runtime.