# GPU Sandbox Design Proposal (Initial Draft)

This document is an initial design proposal for GPU-enabled sandboxes in Cube Sandbox.

It is a design discussion draft only. It does not represent a delivery commitment, a compatibility guarantee, or an implementation promise for the current release.

## Motivation

Cube Sandbox currently focuses on CPU-oriented sandbox workloads. A growing set of AI and media workloads require direct access to accelerators, especially NVIDIA GPUs, while still keeping the isolation and lifecycle control expected from MicroVM-based sandboxes.

The immediate motivation is to define a system design that can support GPU-backed sandboxes without weakening the existing security and operational model. The design must also avoid painting the project into a corner if future accelerator support expands beyond GPUs to TPU or XPU devices.

## Target Use Cases

- Run a sandbox that needs one or more physical GPUs for inference, model serving, or media processing.
- Schedule GPU workloads across a Cube cluster with explicit device ownership during sandbox lifetime.
- Attach GPUs to a MicroVM through VFIO-based device passthrough.
- Allow sandbox users to bring guest software stacks that depend on NVIDIA kernel modules, user-space driver libraries, and the CUDA toolkit.
- Provide a design foundation that can later be generalized for other PCIe accelerators.

## Non-goals

- This proposal does not implement GPU support.
- This proposal does not define a stable user-facing API yet.
- This proposal does not commit to hot-plug, live migration, or oversubscription.
- This proposal does not include mediated devices, time-slicing, or multi-tenant sharing of a single physical GPU.
- This proposal does not attempt to preserve current snapshot-based lifecycle features for GPU sandboxes.
- This proposal does not optimize for every vendor-specific GPU capability in the first iteration.

## High-level Design

The proposed model is physical accelerator passthrough to a dedicated MicroVM sandbox.

At a high level:

1. CubeMaster discovers GPU-capable nodes and their allocatable accelerator inventory.
2. CubeMaster schedules a GPU sandbox onto a node that can satisfy the accelerator request.
3. Cubelet on the target node reserves the selected device and prepares host-side runtime state.
4. The selected PCI device is bound to `vfio-pci` before sandbox startup.
5. CubeShim launches the sandbox VM with the selected VFIO device attached.
6. The guest image, guest kernel, and `modules.img` must contain the required kernel and user-space support for the target GPU stack.
7. When the sandbox exits, device release happens only after CubeShim exits and all device users are gone.
8. The node restores the device to an allocatable state for future sandboxes.

This model intentionally favors correctness and clear ownership over feature breadth.

## Component Responsibilities

### CubeMaster

- Define accelerator-aware scheduling inputs and internal resource accounting.
- Track node-level GPU inventory and allocation state.
- Reject placements that cannot provide exclusive device ownership.
- Preserve enough metadata for later extension to TPU or XPU devices.

### Cubelet

- Discover local accelerator devices and publish allocatable inventory.
- Reserve and release devices on behalf of sandbox lifecycle operations.
- Coordinate host-side preparation such as VFIO binding and cleanup.
- Prevent double allocation on a single node.

### CubeShim

- Attach VFIO devices to the sandbox VM.
- Keep the device lifecycle tied to the actual VM process lifecycle.
- Ensure teardown ordering is correct so device release happens after shim exit, not before.

### cube-agent

- Load required guest-side kernel modules during boot, such as NVIDIA modules delivered through `modules.img`.
- Configure guest-side NVIDIA runtime settings, such as persistence mode.
- Provide guest-side diagnostics to confirm that the passthrough accelerator is available inside the sandbox.

### Guest Image, Guest Kernel, and Runtime Stack

- Provide a guest environment that can initialize and use the attached accelerator.
- Carry the expected kernel modules, device files, and user-space libraries for the chosen GPU stack.

## GPU Discovery, Scheduling, Allocation, and Release

### Discovery

GPU support starts with accurate device discovery on each node. The node-local control plane needs to identify, at minimum:

- PCI address
- vendor and device identifiers
- device class
- driver binding state
- health and allocatable status
- whether the device is already reserved by another sandbox

The inventory model should describe accelerators generically enough that a future TPU or XPU device can fit the same control-plane shape.

### Scheduling

CubeMaster should treat accelerator requests as hard placement constraints. A sandbox that requests GPUs should only be scheduled to nodes where:

- the requested accelerator type exists
- enough allocatable devices are free
- the node supports the required host runtime stack
- the node is marked capable of running GPU-enabled guests

The first design should assume exclusive assignment of whole devices. That keeps scheduling, accounting, and failure handling simpler than shared-device models.

### Allocation

After placement, the target node reserves the selected devices before VM startup. Reservation must happen before VFIO rebinding to avoid races with concurrent workload admission.

Allocation metadata should at least record:

- sandbox identifier
- node identifier
- selected device addresses
- allocation timestamp
- current lifecycle state

### Release

Device release is not just a control-plane state transition. A device is safe to mark allocatable again only after:

- the guest has stopped using it
- the VM process has exited
- CubeShim has exited and no longer holds the VFIO file descriptors
- the host-side cleanup has restored the expected binding state

The release path must be conservative. A stale allocation is better than reallocating a still-open device to a second sandbox.

## VFIO Binding and the Release-after-Shim-exit Pitfall

The most important operational risk in the first design is device lifecycle ordering.

For passthrough, the device must be rebound from the host driver to `vfio-pci` before VM startup. However, teardown is more subtle. Releasing the device back to the allocator immediately after the guest process exits is unsafe if CubeShim still holds the VFIO device open.

The design requirement is:

- bind to `vfio-pci` before the VM starts
- do not mark the device free when the guest merely begins exiting
- only release the device after CubeShim exits and the host confirms no remaining holder keeps the device busy

If this ordering is violated, the system can produce false-free devices, failed rebinds, or cross-sandbox interference on the next allocation. This risk should be treated as a first-class lifecycle invariant, not an implementation detail.

## Guest Image, Guest Kernel, `modules.img`, and NVIDIA Driver/Toolkit Requirements

GPU sandboxes require a guest software stack that is substantially different from CPU-only sandboxes.

The proposal assumes the guest side may need all of the following, depending on the target GPU stack:

- guest kernel support for PCI device initialization and the required NVIDIA driver modules
- guest kernel modules delivered through the root filesystem or `modules.img`
- NVIDIA kernel driver components inside the guest
- NVIDIA user-space driver libraries inside the guest
- NVIDIA Container Toolkit or equivalent runtime integration if the guest workload expects that model
- version alignment between guest kernel, guest modules, and NVIDIA driver stack

This implies that GPU support is not only a control-plane problem. It also affects how guest images are built, versioned, tested, and distributed. The project likely needs explicit GPU-capable guest image variants rather than assuming the default guest image can serve both CPU-only and GPU-backed sandboxes.

## Unsupported Lifecycle Features in the Initial Design

The initial design should explicitly exclude lifecycle features that conflict with physical accelerator passthrough or would require major additional work.

The following are out of scope for GPU sandboxes:

- no app snapshots
- no 1:N cloning from one source sandbox
- no snapshot rollback
- no general pause/resume

These limitations should be documented as product behavior, not hidden as edge cases. Physical device ownership makes these features much harder than in CPU-only sandboxes.

## Deployment Limitations

GPU sandboxes in the initial design depend on PCI passthrough.

Because of that dependency, PVM-based deployments are out of scope for the initial design.

## Extensibility for TPU and XPU Accelerators

Although the first proposal focuses on NVIDIA GPUs, the resource model should not hardcode GPU-only assumptions into every layer.

The design should leave room for:

- accelerator class as a generic scheduling dimension
- per-device capabilities and runtime prerequisites
- device-specific preparation and release hooks on each node
- different passthrough or attachment models for non-GPU devices

In practice, that means internal resource records, scheduling predicates, and node capability reporting should be accelerator-oriented rather than NVIDIA-only wherever possible.

## Open Questions

- What is the minimal user-facing API for requesting accelerator resources without overcommitting to a long-term schema?
- Should accelerator inventory be reported by Cubelet directly, or by a separate node-local discovery helper?
- What host validation is required before a node can be marked GPU-ready?
- How should health checks distinguish between allocatable, unhealthy, and administratively disabled devices?
- What guest image versioning model should be used for NVIDIA driver and CUDA compatibility?
- Should the project support only full-device passthrough first, or define an extension point for future mediated-device models?
- How should failure recovery behave if VFIO rebinding succeeds but VM startup fails?
- Which accelerator metadata belongs in cluster scheduling state versus node-local runtime state?
- What is the right abstraction boundary to support TPU and XPU devices without overengineering the first GPU iteration?

## Summary

This proposal frames GPU sandboxes as a dedicated passthrough-based design with strict device ownership and conservative lifecycle handling. The main complexity is not only scheduling, but also the end-to-end coordination across CubeMaster, Cubelet, CubeShim, node inventory reporting, guest images, guest kernel modules, and NVIDIA runtime dependencies.

The next step after design review should be to turn the approved parts of this proposal into a phased implementation plan.
