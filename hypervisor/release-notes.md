- [v28.0](#v280)
    - [Community Engagement (Reminder)](#community-engagement-reminder)
    - [Long Term Support (LTS) Release](#long-term-support-lts-release)
    - [Virtualised TPM Support](#virtualised-tpm-support)
    - [Transparent Huge Page Support](#transparent-huge-page-support)
    - [README Quick Start Improved](#readme-quick-start-improved)
    - [Notable Bug Fixes](#notable-bug-fixes)
    - [Removals](#removals)
    - [Contributors](#contributors)
- [v27.0](#v270)
    - [Community Engagement](#community-engagement)
    - [Prebuilt Packages](#prebuilt-packages)
    - [Network Device MTU Exposed to Guest](#network-device-mtu-exposed-to-guest)
    - [Boot Tracing](#boot-tracing)
    - [Simplified Build Feature Flags](#simplified-build-feature-flags)
    - [Asynchronous Kernel Loading](#asynchronous-kernel-loading)
    - [GDB Support for AArch64](#gdb-support-for-aarch64)
    - [Notable Bug Fixes](#notable-bug-fixes-1)
    - [Deprecations](#deprecations)
    - [Contributors](#contributors-1)
- [v26.0](#v260)
    - [SMBIOS Improvements via `--platform`](#smbios-improvements-via---platform)
    - [Unified Binary MSHV and KVM Support](#unified-binary-mshv-and-kvm-support)
    - [Notable Bug Fixes](#notable-bug-fixes-2)
    - [Deprecations](#deprecations-1)
    - [Removals](#removals-1)
    - [Contributors](#contributors-2)
- [v25.0](#v250)
    - [`ch-remote` Improvements](#ch-remote-improvements)
    - [VM "Coredump" Support](#vm-coredump-support)
    - [Notable Bug Fixes](#notable-bug-fixes-3)
    - [Removals](#removals-2)
    - [Contributors](#contributors-3)
- [v24.0](#v240)
    - [Bypass Mode for `virtio-iommu`](#bypass-mode-for-virtio-iommu)
    - [Ensure Identifiers Uniqueness](#ensure-identifiers-uniqueness)
    - [Sparse Mmap support](#sparse-mmap-support)
    - [Expose Platform Serial Number](#expose-platform-serial-number)
    - [Notable Bug Fixes](#notable-bug-fixes-4)
    - [Notable Improvements](#notable-improvements)
    - [Deprecations](#deprecations-2)
    - [New on the Website](#new-on-the-website)
    - [Contributors](#contributors-4)
- [v23.1](#v231)
- [v23.0](#v230)
    - [vDPA Support](#vdpa-support)
    - [Updated OS Support list](#updated-os-support-list)
    - [`AArch64` Memory Map Improvements](#aarch64-memory-map-improvements)
    - [`AMX` Support](#amx-support)
    - [Notable Bug Fixes](#notable-bug-fixes-5)
    - [Deprecations](#deprecations-3)
    - [Contributors](#contributors-5)
- [v22.1](#v221)
- [v22.0](#v220)
    - [GDB Debug Stub Support](#gdb-debug-stub-support)
    - [`virtio-iommu` Backed Segments](#virtio-iommu-backed-segments)
    - [Before Boot Configuration Changes](#before-boot-configuration-changes)
    - [`virtio-balloon` Free Page Reporting](#virtio-balloon-free-page-reporting)
    - [Support for Direct Kernel Booting with TDX](#support-for-direct-kernel-booting-with-tdx)
    - [PMU Support for AArch64](#pmu-support-for-aarch64)
    - [Documentation Under CC-BY-4.0 License](#documentation-under-cc-by-40-license)
    - [Deprecation of "Classic" `virtiofsd`](#deprecation-of-classic-virtiofsd)
    - [Notable Bug Fixes](#notable-bug-fixes-6)
    - [Contributors](#contributors-6)
- [v21.0](#v210)
    - [Efficient Local Live Migration (for Live Upgrade)](#efficient-local-live-migration-for-live-upgrade)
    - [Recommended Kernel is Now 5.15](#recommended-kernel-is-now-515)
    - [Notable Bug fixes](#notable-bug-fixes-7)
    - [Contributors](#contributors-7)
- [v20.2](#v202)
- [v20.1](#v201)
- [v20.0](#v200)
    - [Multiple PCI segments support](#multiple-pci-segments-support)
    - [CPU pinning](#cpu-pinning)
    - [Improved VFIO support](#improved-vfio-support)
    - [Safer code](#safer-code)
    - [Extended documentation](#extended-documentation)
    - [Notable bug fixes](#notable-bug-fixes-8)
    - [Contributors](#contributors-8)
- [v19.0](#v190)
    - [Improved PTY handling for serial and `virtio-console`](#improved-pty-handling-for-serial-and-virtio-console)
    - [PCI boot time optimisations](#pci-boot-time-optimisations)
    - [Improved TDX support](#improved-tdx-support)
    - [Live migration enhancements](#live-migration-enhancements)
    - [`virtio-mem` support with `vfio-user`](#virtio-mem-support-with-vfio-user)
    - [AArch64 for `virtio-iommu`](#aarch64-for-virtio-iommu)
    - [Notable bug fixes](#notable-bug-fixes-9)
    - [Contributors](#contributors-9)
- [v18.0](#v180)
    - [Experimental User Device (`vfio-user`) support](#experimental-user-device-vfio-user-support)
    - [Migration support for `vhost-user` devices](#migration-support-for-vhost-user-devices)
    - [VHDX disk image support](#vhdx-disk-image-support)
    - [Device pass through on MSHV hypervisor](#device-pass-through-on-mshv-hypervisor)
    - [AArch64 for support `virtio-mem`](#aarch64-for-support-virtio-mem)
    - [Live migration on MSHV hypervisor](#live-migration-on-mshv-hypervisor)
    - [AArch64 CPU topology support](#aarch64-cpu-topology-support)
    - [Power button support on AArch64](#power-button-support-on-aarch64)
    - [Notable bug fixes](#notable-bug-fixes-10)
    - [Contributors](#contributors-10)
- [v17.0](#v170)
    - [ARM64 NUMA support using ACPI](#arm64-numa-support-using-acpi)
    - [`Seccomp` support for MSHV backend](#seccomp-support-for-mshv-backend)
    - [Hotplug of `macvtap` devices](#hotplug-of-macvtap-devices)
    - [Improved SGX support](#improved-sgx-support)
    - [Inflight tracking for `vhost-user` devices](#inflight-tracking-for-vhost-user-devices)
    - [Notable bug fixes](#notable-bug-fixes-11)
    - [Contributors](#contributors-11)
- [v16.0](#v160)
    - [Improved live migration support](#improved-live-migration-support)
    - [Improved `vhost-user` support](#improved-vhost-user-support)
    - [ARM64 ACPI and UEFI support](#arm64-acpi-and-uefi-support)
    - [Notable bug fixes](#notable-bug-fixes-12)
    - [Removed functionality](#removed-functionality)
    - [Contributors](#contributors-12)
- [v15.0](#v150)
    - [Version numbering and stability guarantees](#version-numbering-and-stability-guarantees)
    - [Network device rate limiting](#network-device-rate-limiting)
    - [Support for runtime control of `virtio-net` guest offload](#support-for-runtime-control-of-virtio-net-guest-offload)
    - [`--api-socket` supports file descriptor parameter](#--api-socket-supports-file-descriptor-parameter)
    - [Bug fixes](#bug-fixes)
    - [Deprecations](#deprecations-4)
    - [Contributors](#contributors-13)
- [v0.14.1](#v0141)
- [v0.14.0](#v0140)
    - [Structured event monitoring](#structured-event-monitoring)
    - [MSHV improvements](#mshv-improvements)
    - [Improved aarch64 platform](#improved-aarch64-platform)
    - [Updated hotplug documentation](#updated-hotplug-documentation)
    - [PTY control for serial and `virtio-console`](#pty-control-for-serial-and-virtio-console)
    - [Block device rate limiting](#block-device-rate-limiting)
    - [Deprecations](#deprecations-5)
    - [Contributors](#contributors-14)
- [v0.13.0](#v0130)
    - [Wider VFIO device support](#wider-vfio-device-support)
    - [Improved huge page support](#improved-huge-page-support)
    - [MACvTAP support](#macvtap-support)
    - [VHD disk image support](#vhd-disk-image-support)
    - [Improved Virtio device threading](#improved-virtio-device-threading)
    - [Clean shutdown support via synthetic power button](#clean-shutdown-support-via-synthetic-power-button)
    - [Contributors](#contributors-15)
- [v0.12.0](#v0120)
    - [ARM64 enhancements](#arm64-enhancements)
    - [Removal of `vhost-user-net` and `vhost-user-block` self spawning](#removal-of-vhost-user-net-and-vhost-user-block-self-spawning)
    - [Migration of `vhost-user-fs` backend](#migration-of-vhost-user-fs-backend)
    - [Enhanced "info" API](#enhanced-info-api)
    - [Contributors](#contributors-16)
- [v0.11.0](#v0110)
    - [`io_uring` support by default for `virtio-block`](#io_uring-support-by-default-for-virtio-block)
    - [Windows Guest Support](#windows-guest-support)
    - [`vhost-user` "Self Spawning" Deprecation](#vhost-user-self-spawning-deprecation)
    - [`virtio-mmio` Removal](#virtio-mmio-removal)
    - [Snapshot/Restore support for ARM64](#snapshotrestore-support-for-arm64)
    - [Improved Linux Boot Time](#improved-linux-boot-time)
    - [`SIGTERM/SIGINT` Interrupt Signal Handling](#sigtermsigint-interrupt-signal-handling)
    - [Default Log Level Changed](#default-log-level-changed)
    - [New `--balloon` Parameter Added](#new---balloon-parameter-added)
    - [Experimental `virtio-watchdog` Support](#experimental-virtio-watchdog-support)
    - [Notable Bug Fixes](#notable-bug-fixes-13)
    - [Contributors](#contributors-17)
- [v0.10.0](#v0100)
    - [`virtio-block` Support for Multiple Descriptors](#virtio-block-support-for-multiple-descriptors)
    - [Memory Zones](#memory-zones)
    - [`Seccomp` Sandbox Improvements](#seccomp-sandbox-improvements)
    - [Preliminary KVM HyperV Emulation Control](#preliminary-kvm-hyperv-emulation-control)
    - [Notable Bug Fixes](#notable-bug-fixes-14)
    - [Contributors](#contributors-18)
- [v0.9.0](#v090)
    - [`io_uring` Based Block Device Support](#io_uring-based-block-device-support)
    - [Block and Network Device Statistics](#block-and-network-device-statistics)
    - [HTTP API Responses](#http-api-responses)
    - [CPU Topology](#cpu-topology)
    - [Release Build Optimization](#release-build-optimization)
    - [Hypervisor Abstraction](#hypervisor-abstraction)
    - [Snapshot/Restore Improvements](#snapshotrestore-improvements)
    - [Virtio Memory Ballooning Support](#virtio-memory-ballooning-support)
    - [Enhancements to ARM64 Support](#enhancements-to-arm64-support)
    - [Intel SGX Support](#intel-sgx-support)
    - [`Seccomp` Sandbox Improvements](#seccomp-sandbox-improvements-1)
    - [Notable Bug Fixes](#notable-bug-fixes-15)
    - [Contributors](#contributors-19)
- [v0.8.0](#v080)
    - [Experimental Snapshot and Restore Support](#experimental-snapshot-and-restore-support)
    - [Experimental ARM64 Support](#experimental-arm64-support)
    - [Support for Using 5-level Paging in Guests](#support-for-using-5-level-paging-in-guests)
    - [Virtio Device Interrupt Suppression for Network Devices](#virtio-device-interrupt-suppression-for-network-devices)
    - [`vhost_user_fs` Improvements](#vhost_user_fs-improvements)
    - [Notable Bug Fixes](#notable-bug-fixes-16)
    - [Command Line and API Changes](#command-line-and-api-changes)
    - [Contributors](#contributors-20)
- [v0.7.0](#v070)
    - [Block, Network, Persistent Memory (PMEM), VirtioFS and Vsock hotplug](#block-network-persistent-memory-pmem-virtiofs-and-vsock-hotplug)
    - [Alternative `libc` Support](#alternative-libc-support)
    - [Multithreaded Multi Queued `vhost-user` Backends](#multithreaded-multi-queued-vhost-user-backends)
    - [Initial RamFS Support](#initial-ramfs-support)
    - [Alternative Memory Hotplug: `virtio-mem`](#alternative-memory-hotplug-virtio-mem)
    - [`Seccomp` Sandboxing](#seccomp-sandboxing)
    - [Updated Distribution Support](#updated-distribution-support)
    - [Command Line and API Changes](#command-line-and-api-changes-1)
    - [Contributors](#contributors-21)
- [v0.6.0](#v060)
    - [Directly Assigned Devices Hotplug](#directly-assigned-devices-hotplug)
    - [Shared Filesystem Improvements](#shared-filesystem-improvements)
    - [Block and Networking IO Self Offloading](#block-and-networking-io-self-offloading)
    - [Command Line Interface](#command-line-interface)
    - [PVH Boot](#pvh-boot)
    - [Contributors](#contributors-22)
- [v0.5.1](#v051)
- [v0.5.0](#v050)
    - [Virtual Machine Dynamic Resizing](#virtual-machine-dynamic-resizing)
    - [Multi-Queue, Multi-Threaded Paravirtualization](#multi-queue-multi-threaded-paravirtualization)
    - [New Interrupt Management Framework](#new-interrupt-management-framework)
    - [Development Tools](#development-tools)
    - [Kata Containers Integration](#kata-containers-integration)
    - [Contributors](#contributors-23)
- [v0.4.0](#v040)
    - [Dynamic virtual CPUs addition](#dynamic-virtual-cpus-addition)
    - [Programmatic firmware tables generation](#programmatic-firmware-tables-generation)
    - [Filesystem and block devices vhost-user backends](#filesystem-and-block-devices-vhost-user-backends)
    - [Guest pause and resume](#guest-pause-and-resume)
    - [Userspace IOAPIC by default](#userspace-ioapic-by-default)
    - [PCI BAR reprogramming](#pci-bar-reprogramming)
    - [New `cloud-hypervisor` organization](#new-cloud-hypervisor-organization)
    - [Contributors](#contributors-24)
- [v0.3.0](#v030)
    - [Block device offloading](#block-device-offloading)
    - [Network device backend](#network-device-backend)
    - [Virtual sockets](#virtual-sockets)
    - [HTTP based API](#http-based-api)
    - [Memory mapped virtio transport](#memory-mapped-virtio-transport)
    - [Paravirtualized IOMMU](#paravirtualized-iommu)
    - [Ubuntu 19.10](#ubuntu-1910)
    - [Large memory guests](#large-memory-guests)
- [v0.2.0](#v020)
    - [Network device offloading](#network-device-offloading)
    - [Minimal hardware-reduced ACPI](#minimal-hardware-reduced-acpi)
    - [Debug I/O port](#debug-io-port)
    - [Improved direct device assignment](#improved-direct-device-assignment)
    - [Improved shared filesystem](#improved-shared-filesystem)
    - [Ubuntu bionic based CI](#ubuntu-bionic-based-ci)
- [v0.1.0](#v010)
    - [Shared filesystem](#shared-filesystem)
    - [Initial direct device assignment support](#initial-direct-device-assignment-support)
    - [Userspace IOAPIC](#userspace-ioapic)
    - [Virtual persistent memory](#virtual-persistent-memory)
    - [Linux kernel bzImage](#linux-kernel-bzimage)
    - [Console over virtio](#console-over-virtio)
    - [Unit testing](#unit-testing)
    - [Integration tests parallelization](#integration-tests-parallelization)

# v28.0

This release has been tracked in our new [roadmap
project](https://github.com/orgs/cloud-hypervisor/projects/6) as iteration
v28.0.

### Community Engagement (Reminder)

Just a reminder that we have a new mailing list to support broader community
discussions.  Please consider
[subscribing](https://lists.cloudhypervisor.org/g/dev/). We plan to use
this to announce a regular meeting for those interested in talking about Cloud
Hypervisor development.

### Long Term Support (LTS) Release

This is the first version of Cloud Hypervisor to be released under the LTS
release process. Point releases for bug fixes will be made for the next 18
months; live migration and live upgrade will be supported between the point
releases of the LTS.

### Virtualised TPM Support

Support for adding an emulated CRB TPM has been added. This has it's own [TPM
documentation](docs/tpm.md).

### Transparent Huge Page Support

By default, but controllable through `--memory thp=off` if it possible to back
the guest memory with Transparent Huge Pages (no file backing/`shared=off`)
then this will be used resulting in improved boot performance.

### README Quick Start Improved

The README has been refreshed with the quick start guide updated to reflect the
different firmware options and to recommend the use of pre-built binaries.

### Notable Bug Fixes

* Inappropriate Copy-on-Write of pinned pages (e.g. VFIO) leading to higher
  memory consumption (#4835)
* Multiple `virtio` device bug fixes found through fuzzing (#4859, #4799)
* Large QCOW files (> 4TiB) are now supported (#4767)
* Support for > 31 vCPUS on aarch64 (#4863)
* Multiple fixes to OpenAPI specification file (#4720, #4811)
* Programming of the MSI-X table has been optimised leading to faster boot on
  newer Linux kernels (#4744)
* Error on reboot from race to close TAP devices (#4871)
* Non-spec compliant virtio-block read-only support (#4888)
  
### Removals

The following functionality has been removed:

* Support for non-PVH firmware booting has been removed (#4511)
* I/O ports used for older versions of firmware have been removed (#3926)
* Deprecated API options for kernel/cmdline/initramfs have been removed (#4737)
 
### Contributors

Many thanks to everyone who has contributed to our release:

* Anatol Belski <anbelski@linux.microsoft.com>
* Bo Chen <chen.bo@intel.com>
* Fabiano Fidêncio <fabiano.fidencio@intel.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Jinank Jain <jinankjain@microsoft.com>
* Jinrong Liang <cloudliang@tencent.com>
* lv.mengzhao <lv.mengzhao@zte.com.cn>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Praveen K Paladugu <prapal@linux.microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>

# v27.0

This release has been tracked in our new [roadmap
project](https://github.com/orgs/cloud-hypervisor/projects/6) as iteration
v27.0.

### Community Engagement

A new mailing list has been created to support broader community discussions.
Please consider [subscribing](https://lists.cloudhypervisor.org/g/dev/); an
announcement of a regular meeting will be announced via this list shortly.

### Prebuilt Packages

Prebuilt packages are now available. Please see [this
document](https://github.com/cloud-hypervisor/obs-packaging/blob/main/README.md)
on how to install. These packages also include packages for the different
firmware options available.

### Network Device MTU Exposed to Guest

The MTU for the TAP device associated with a `virtio-net` device is now exposed
to the guest. If the user provides a MTU with `--net mtu=..` then that MTU is
applied to created TAP interfaces. This functionality is also exposed for
`vhost-user-net` devices including those created with the reference backend
(#4658, #4676.)

### Boot Tracing

Support for generating a trace report for the boot time has been added
including a script for generating an SVG from that trace (#4659.)

### Simplified Build Feature Flags

The set of feature flags, for e.g. experimental features, have been simplified:

* `msvh` and `kvm` features provide support for those specific hypervisors
  (with `kvm` enabled by default),
* `tdx` provides support for Intel TDX; and although there is no MSHV support
  now it is now possible to compile with the `mshv` feature (#4696,)
* `tracing` adds support for boot tracing,
* `guest_debug` now covers both support for gdbing a guest (formerly `gdb`
  feature) and dumping guest memory.

The following feature flags were removed as the functionality was enabled by
default: `amx`, `fwdebug`, `cmos` and `common` (#4679, #4632.)

### Asynchronous Kernel Loading

AArch64 has gained support for loading the guest kernel asynchronously like
x86-64. (#4538)

### GDB Support for AArch64

GDB stub support (accessed through `--gdb` under `guest_debug` feature) is now
available on AArch64 as well as as x86-64.

### Notable Bug Fixes

* This version incorporates a version of `virtio-queue` that addresses an issue
  where a rogue guest can potentially DoS the VMM (rust-vmm/vm-virtio#196.)
* Improvements around PTY handling for `virtio-console` and serial devices
  (#4520, #4533, #4535.)
* Improved error handling in virtio devices (#4626, #4605, #4509, #4631, #4697)

### Deprecations

Deprecated features will be removed in a subsequent release and users should
plan to use alternatives.

* Booting legacy firmware (compiled without a PVH header) has been deprecated.
  All the firmware options (Cloud Hypervisor OVMF and Rust Hypervisor Firmware)
  support booting with PVH so support for loading firmware in a legacy mode is no
  longer needed. This functionality will be removed in the next release.

### Contributors

Many thanks to everyone who has contributed to our release:

* Anatol Belski <anbelski@linux.microsoft.com>
* Bo Chen <chen.bo@intel.com>
* James O. D. Hunt <james.o.hunt@intel.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Markus Napierkowski <markus.napierkowski@cyberus-technology.de>
* Michael Zhao <michael.zhao@arm.com>
* Nuno Das Neves <nudasnev@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Smit Gardhariya <sgardhariya@microsoft.com>
* Wei Liu <liuwe@microsoft.com>

# v26.0

This release has been tracked through the [v26.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/30).

### SMBIOS Improvements via `--platform`

`--platform` and the appropriate API structure has gained support for supplying
OEM strings (primarily used to communicate metadata to systemd in the guest)
(#4319, #4446) and support for specifying the UUID (#4389.)

### Unified Binary MSHV and KVM Support

Support for both the MSHV and KVM hypervisors can be compiled into the same
binary with the detection of the hypervisor to use made at runtime.

### Notable Bug Fixes

* The prefetchable flag is preserved on BARs for VFIO devices (#4353, #4454)
* PCI Express capabilities for functionality we do not support are now filtered
  out (#4456)
* GDB breakpoint support is more reliable (#4354, #4363)
* `SIGINT` and `SIGTERM` signals are now handled before the VM has booted
  (#4269, #4293)
* Multiple API event loop handling bug fixes (#4309, #4362)
* Incorrect assumptions in virtio queue numbering were addressed, allowing
  the`virtio-fs` driver in OVMF to be used (#4341, #4314)
* VHDX file format header fix (#4291)
* The same VFIO device cannot be added twice (#4453, #4463)
* SMBIOS tables were being incorrectly generated (#4442)

### Deprecations

Deprecated features will be removed in a subsequent release and users should
plan to use alternatives.

* The top-level `kernel` and `initramfs` members on the `VmConfig` have been
  moved inside a `PayloadConfig` as the `payload` member. The OpenAPI document
  has been updated to reflect the change and the old API members continue to
  function and are mapped to the new version. The expectation is that these old
  versions will be removed in the v28.0 release.

### Removals

The following functionality has been removed:

* The unused `poll_queue` parameter has been removed from `--disk` and
  equivalent. This was residual from the the removal of the `vhost-user-block`
  spawning feature (#4402.)

### Contributors

Many thanks to everyone who has contributed to our release:

* Alyssa Ross <hi@alyssa.is>
* Anatol Belski <ab@php.net>
* Archana Shinde <archana.m.shinde@intel.com>
* Bo Chen <chen.bo@intel.com>
* lizhaoxin1 <Lxiaoyouling@163.com>
* Maximilian Nitsch <maximilian.nitsch@d3tn.com>
* Michael Zhao <michael.zhao@arm.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Steven Dake <sdake@lambdal.com>
* Wei Liu <liuwe@microsoft.com>

# v25.0

This release has been tracked through the [v25.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/29).

### `ch-remote` Improvements

The `ch-remote` command has gained support for creating the VM from a JSON
config and support for booting and deleting the VM from the VMM.

### VM "Coredump" Support

Under the `guest_debug` feature flag it is now possible to extract the memory
of the guest for use in debugging with e.g. the `crash` utility. (#4012)

### Notable Bug Fixes

* Always restore console mode on exit (#4249, #4248)
* Restore vCPUs in numerical order which fixes aarch64 snapshot/restore (#4244)
* Don't try and configure `IFF_RUNNING` on TAP devices (#4279)
* Propagate configured queue size through to vhost-user backend (#4286)
* Always Program vCPU CPUID before running the vCPU to fix running on Linux
  5.16 (#4156)
* Enable ACPI MADT "Online Capable" flag for hotpluggable vCPUs to fix newer
  Linux guest

### Removals

The following functionality has been removed:

* The `mergeable` option from the `virtio-pmem` support has been removed
  (#3968)
* The `dax` option from the `virtio-fs` support has been removed (#3889)

### Contributors

Many thanks to everyone who has contributed to our release:

* Dylan Bargatze <dbargatz@users.noreply.github.com>
* Jinank Jain <jinankjain@microsoft.com>
* Michael Zhao <michael.zhao@arm.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* Yi Wang <wang.yi59@zte.com.cn>

# v24.0

This release has been tracked through the [v24.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/28).

### Bypass Mode for `virtio-iommu`

`virtio-iommu` specification describes how a device can be attached by default
to a bypass domain. This feature is particularly helpful for booting a VM with
guest software which doesn't support `virtio-iommu` but still need to access
the device. Now that Cloud Hypervisor supports this feature, it can boot a VM
with Rust Hypervisor Firmware or OVMF even if the `virtio-block` device exposing
the disk image is placed behind a virtual IOMMU.

### Ensure Identifiers Uniqueness

Multiple checks have been added to the code to prevent devices with identical
identifiers from being created, and therefore avoid unexpected behaviors at boot
or whenever a device was hot plugged into the VM.

### Sparse Mmap support

Sparse mmap support has been added to both VFIO and vfio-user devices. This
allows the device regions that are not fully mappable to be partially mapped.
And the more a device region can be mapped into the guest address space, the
fewer VM exits will be generated when this device is accessed. This directly
impacts the performance related to this device.

### Expose Platform Serial Number

A new `serial_number` option has been added to `--platform`, allowing a user to
set a specific serial number for the platform. This number is exposed to the
guest through the SMBIOS.

### Notable Bug Fixes

* Fix loading RAW firmware (#4072)
* Reject compressed QCOW images (#4055)
* Reject virtio-mem resize if device is not activated (#4003)
* Fix potential mmap leaks from VFIO/vfio-user MMIO regions (#4069)
* Fix algorithm finding HOB memory resources (#3983)

### Notable Improvements

* Refactor interrupt handling (#4083)
* Load kernel asynchronously (#4022)
* Only create ACPI memory manager DSDT when resizable (#4013)

### Deprecations

Deprecated features will be removed in a subsequent release and users should
plan to use alternatives

* The `mergeable` option from the `virtio-pmem` support has been deprecated
  (#3968)
* The `dax` option from the `virtio-fs` support has been deprecated (#3889)

### New on the Website

A new blog post [Achieving Bare Metal Performance Within a Virtual
Machine](https://www.cloudhypervisor.org/blog/achieving-bare-metal-performance-within-a-virtual-machine)
has been added to the Cloud Hypervisor website.

### Contributors

Many thanks to everyone who has contributed to our release:

* Anatol Belski <anbelski@linux.microsoft.com>
* Bo Chen <chen.bo@intel.com>
* Fabiano Fidêncio <fabiano.fidencio@intel.com>
* LiHui <andrewli@kubesphere.io>
* Maksym Pavlenko <pavlenko.maksym@gmail.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Steven Dake <steven.dake@gmail.com>
* Vincent Batts <vbatts@hashbangbash.com>
* Wei Liu <liuwe@microsoft.com>

# v23.1

This is a bug fix release. The following issues have been addressed:

* Add some missing seccomp rules
* Remove `virtio-fs` filesystem entries from config on removal
* Do not delete API socket on API server start (#4026)
* Reject `virtio-mem` resize if the guest doesn't activate the device
* Fix OpenAPI naming of I/O throttling knobs

# v23.0

This release has been tracked through the [v23.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/27).

### vDPA Support

A vDPA device has a datapath that complies with the virtio specification but
with a vendor specific control path. The addition of `--vdpa` and the REST API
equivalent allows the use of these devices with Cloud Hypervisor.

### Updated OS Support list

The list of officially supported and tested OS versions has been updated to
include Ubuntu "jammy" 22.04 and EOLed versions removed.

### `AArch64` Memory Map Improvements

The memory map when running on `AArch64` has been improved for the handling of
the UEFI region which means that the booted guest OS now has full access to its
allocated RAM. (#3938)

### `AMX` Support

Under a compile time gate of `amx` it is possible compile in support for the
`AMX` instruction set extension for guest use. This also requires runtime
enabling with `--cpu features=amx`.

### Notable Bug Fixes

* Generate error when incorrect HTTP method used for some API endpoints (#3887)
* CMOS based reset is now available to support rebooting on "jammy" (#3914)
* ACPI tables are not produced for memory hotplug when running with
  `virtio-mem` (#3883)
* `virtio-iommu` backed PCI segments are now comprehensively placed behind the
  vIOMMU (#3870)
* Seccomp rules have been extended for `virtio-fs` to support direct access
  (#3848)

### Deprecations

Deprecated features will be removed in a subsequent release and users should
plan to use alternatives

* The `mergeable` option from the `virtio-pmem` support has been deprecated
  (#3968)
* The `dax` option from the `virtio-fs` support has been deprecated (#3889)

### Contributors

Many thanks to everyone who has contributed to our release:

* Bo Chen <chen.bo@intel.com>
* Fabiano Fidêncio <fabiano.fidencio@intel.com>
* Henry Wang <Henry.Wang@arm.com>
* Jianyong Wu <jianyong.wu@arm.com>
* LiHui <andrewli@kubesphere.io>
* Michael Zhao <michael.zhao@arm.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* William Douglas <william.douglas@intel.com>
* Yi Wang <wang.yi59@zte.com.cn>

# v22.1

This is a bug fix release. The following issues have been addressed:

* VFIO ioctl reordering to fix MSI on AMD platforms (#3827)
* Fix `virtio-net` control queue (#3829)

# v22.0

This release has been tracked through the [v22.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/25).

### GDB Debug Stub Support

Cloud Hypervisor can now be used as debug target with GDB. This is controlled
by the `gdb` compile time feature and details of how to use it can be found in
the [gdb
documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/gdb.md).

### `virtio-iommu` Backed Segments

In order to facilitate hotplug devices that require being behind an IOMMU (e.g.
QAT) there is a new option `--platform iommu_segments=<list_of_segments>` that
will place all the specified segments behind the IOMMU.

### Before Boot Configuration Changes

It is now possible to change the VM configuration (e.g. add or remove devices,
resize) before the VM is booted.

### `virtio-balloon` Free Page Reporting

If `--balloon free_page_reporting=on` is used then the guest can report pages
that is it not using to the VMM. The VMM will then notify the host OS that
those pages are no longer in use and can be freed. This can result in improved
memory density.

### Support for Direct Kernel Booting with TDX

Through the use of `TD-Shim` lightweight firmware it is now possible to
directly boot into the kernel with TDX. The [TDX
documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/intel_tdx.md#tdshim)
has been updated for this usage.

### PMU Support for AArch64

A PMU is now available on AArch64 for guest performance profiling. This will be
exposed automatically if available from the host.

### Documentation Under CC-BY-4.0 License

The documentation is now licensed under the "Creative Commons Attribution 4.0
International" license which is aligned with the project charter under the
Linux Foundation.

### Deprecation of "Classic" `virtiofsd`

The use of the Rust based [virtiofsd](https://gitlab.com/virtio-fs/virtiofsd)
is now recommended and we are no longer testing against the C based "classic"
version.

### Notable Bug Fixes

* Can now be used on kernels without `AF_INET` support (#3785)
* `virtio-balloon` size is now validated against guest RAM size (#3689)
* Ensure that I/O related KVM VM Exits are correctly handled (#3677)
* Multiple TAP file descriptors can be used for `virtio-net` device hotplug (#3607)
* Minor API improvements and fixes (#3756, #3766, #3647, #3578)
* Fix sporadic seccomp violation from glibc memory freeing (#3610, #3609)
* Fix Windows 11 on AArch64 due to wider MSI-X register accesses (#3714, #3720)
* Ensure `vhost-user` features are correct across migration (#3737)
* Improved vCPU topology on AArch64 (#3735, #3733)

### Contributors

Many thanks to everyone who has contributed to our release:

* Akira Moroo <retrage01@gmail.com>
* Barret Rhoden <brho@google.com>
* Bo Chen <chen.bo@intel.com>
* Fabiano Fidêncio <fabiano.fidencio@intel.com>
* Feng Ye <yefeng@smartx.com>
* Henry Wang <Henry.Wang@arm.com>
* Jianyong Wu <jianyong.wu@arm.com>
* lizhaoxin1 <Lxiaoyouling@163.com>
* Michael Zhao <michael.zhao@arm.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>

# v21.0

This release has been tracked through the [v21.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/24).

### Efficient Local Live Migration (for Live Upgrade)

In order to support fast live upgrade of the VMM an optimised path has been
added in which the memory for the VM is not compared from source to
destination. This is activated by passing `--local` to the `ch-remote
send-migration` command. This means that the live upgrade can complete in the
order of 50ms vs 3s. (#3566)

### Recommended Kernel is Now 5.15

Due to an issue in the `virtio-net` code in 5.14 the recommended Linux kernel
is now 5.15. (#3530)

### Notable Bug fixes

* Multiple fixes were made to the OpenAPI YAML file to match the implementation (#3555,#3562)
* Avoid live migration deadlock when triggered during the kernel boot (#3585)
* Support live migration within firmware (#3586)
* Validate the `virtio-net` descriptor chain (#3548)
* `direct=on` (`O_DIRECT`) can now be used with a guest that makes unaligned accesses (e.g. firmware) (#3587)

### Contributors

Many thanks to everyone who has contributed to our release:

* Anatol Belski <anbelski@linux.microsoft.com>
* Barret Rhoden <brho@google.com>
* Bo Chen <chen.bo@intel.com>
* Fabiano Fidêncio <fabiano.fidencio@intel.com>
* Henry Wang <Henry.Wang@arm.com>
* Liang Zhou <zhoul110@chinatelecom.cn>
* Michael Zhao <michael.zhao@arm.com>
* Muhammad Falak R Wani <falakreyaz@gmail.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* Ziye Yang <ziye.yang@intel.com>
 
# v20.2

This is a bug fix release. The following issues have been addressed:

* Don't error out when setting up the SIGWINCH handler (for console resize)
  when this fails due to older kernel (#3456)
* Seccomp rules were refined to remove syscalls that are now unused
* Fix reboot on older host kernels when SIGWINCH handler was not initialised
  (#3496)
* Fix virtio-vsock blocking issue (#3497)

# v20.1

This is a bug fix release. The following issues have been addressed:

* Networking performance regression with `virtio-net` (#3450)
* Limit file descriptors sent in `vfio-user` support (#3401)
* Fully advertise PCI MMIO config regions in ACPI tables (#3432)
* Set the TSS and KVM identity maps so they don't overlap with firmware RAM
* Correctly update the `DeviceTree` on restore

# v20.0

This release has been tracked through the [v20.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/23).

### Multiple PCI segments support

Cloud Hypervisor is no longer limited to 31 PCI devices. For both `x86_64` and
`aarch64` architectures, it is now possible to create up to 16 PCI segments,
increasing the total amount of supported PCI devices to 496.

### CPU pinning

For each vCPU, the user can define a limited set of host CPUs on which it is
allowed to run. This can be useful when assigning a 1:1 mapping between host and
guest resources, or when running a VM on a specific NUMA node.

### Improved VFIO support

Based on VFIO region capabilities, all regions can be memory mapped, limiting
the amount of triggered VM exits, and therefore increasing the performance of
the passthrough device.

### Safer code

Several sections containing unsafe Rust code have been replaced with safe
alternatives, and multiple comments have been added to clarify why the remaining
unsafe sections are safe to use.

### Extended documentation

The documentation related to VFIO has been updated while some new documents have
been introduced to cover the usage of `--cpus` parameter as well as how to run
Cloud Hypervisor on Intel TDX.

### Notable bug fixes

* Naturally align PCI BARs on relocation (#3244)
* Fix panic in SIGWINCH listener thread when no seccomp filter set (#3338)
* Use the tty raw mode implementation from libc (#3344)
* Fix the emulation of register D for CMOS/RTC device (#3393)

### Contributors

Many thanks to everyone who has contributed to our release:

* Alyssa Ross <hi@alyssa.is>
* Bo Chen <chen.bo@intel.com>
* Fabiano Fidêncio <fabiano.fidencio@intel.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* Willen Yang <willenyang@gmail.com>
* William Douglas <william.douglas@intel.com>
* Ziye Yang <ziye.yang@intel.com>

# v19.0

This release has been tracked through the [v19.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/22).

### Improved PTY handling for serial and `virtio-console`

The PTY support for serial has been enhanced with improved buffering when the
the PTY is not yet connected to. Using `virtio-console` with PTY now results in
the console being resized if the PTY window is also resized.

### PCI boot time optimisations

Multiple optimisations have been made to the PCI handling resulting in
significant improvements in the boot time of the guest.

### Improved TDX support

When using the latest TDVF firmware the ACPI tables created by the VMM are now
exposed via the firmware to the guest. 

### Live migration enhancements

Live migration support has been enhanced to support migration with `virtio-mem`
based memory hotplug and the `virtio-balloon` device now supports live
migration.

### `virtio-mem` support with `vfio-user`

The use of `vfio-user` userspaces devices can now be used in conjunction with
`virtio-mem` based memory hotplug and unplug.

### AArch64 for `virtio-iommu`

A paravirtualised IOMMU can now be used on the AArch64 platform.

### Notable bug fixes

* ACPI hotplugged memory is correctly restored after a live migration or
  snapshot/restore (#3165)
* Multiple devices from the same IOMMU group can be passed through via VFIO
  (#3078 #3113)
* Live migration with large blocks of memory was buggy due to an in issue in
  the underlying crate (#3157)

### Contributors

Many thanks to everyone who has contributed to our release:

* Alyssa Ross <hi@alyssa.is>
* Bo Chen <chen.bo@intel.com>
* Henry Wang <Henry.Wang@arm.com>
* Hui Zhu <teawater@antfin.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Li Yu <liyu.yukiteru@bytedance.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* William Douglas <william.douglas@intel.com>
* Yu Li <liyu.yukiteru@bytedance.com>

# v18.0

This release has been tracked through the [v18.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/21).

### Experimental User Device (`vfio-user`) support

Experimental support for running PCI devices in userspace via `vfio-user`
has been included. This allows the use of the SPDK NVMe `vfio-user` controller
with Cloud Hypervisor. This is enabled by `--user-device` on the command line.

### Migration support for `vhost-user` devices

Devices exposed into the VM via `vhost-user` can now be migrated using the live
migration support. This requires support from the backend however the commonly
used DPDK `vhost-user` backend does support this.

### VHDX disk image support

Images using the VHDX disk image format can now be used with Cloud Hypervisor.

### Device pass through on MSHV hypervisor

When running on the MSHV hypervisor it is possible to pass through devices from
the host through to the guest (e.g with `--device`)

### AArch64 for support `virtio-mem`

The reference Linux kernel we recommend for using with Cloud Hypervisor now supports `virtio-mem` on AArch64.

### Live migration on MSHV hypervisor

Live migration is now supported when running on the MSHV hypervisor including
efficient tracking of dirty pages.

### AArch64 CPU topology support

The CPU topology (as configured through `--cpu topology=`) can now be
configured on AArch64 platforms and is conveyed through either ACPI or device
tree.

### Power button support on AArch64

Use of the ACPI power button (e.g `ch-remote --api-socket=<API socket> power-button`) 
is now supported when running on AArch64.

### Notable bug fixes

* Using two PTY outputs e.g. `--serial pty --console pty` now works correctly (#3012)
* TTY input is now always sent to the correct destination (#3005)
* The boot is no longer blocked when using a unattached PTY on the serial console (#3004)
* Live migration is now supported on AArch64 (#3049)
* Ensure signal handlers are run on the correct thread (#3069)

### Contributors

Many thanks to everyone who has contributed to our release:

* Alyssa Ross <hi@alyssa.is>
* Anatol Belski <anbelski@linux.microsoft.com>
* Arafatms <arafatms@outlook.com>
* Bo Chen <chen.bo@intel.com>
* Fazla Mehrab <akm.fazla.mehrab@vt.edu>
* Henry Wang <Henry.Wang@arm.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Jiaqi Gao <jiaqi.gao@intel.com>
* Markus Theil <markus.theil@tu-ilmenau.de>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* Yu Li <liyu.yukiteru@bytedance.com>

# v17.0

This release has been tracked through the [v17.0
project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/20).

### ARM64 NUMA support using ACPI

The support for ACPI on ARM64 has been enhanced to include support for
specifying a NUMA configuration using the existing control options.

### `Seccomp` support for MSHV backend

The `seccomp` rules have now been extended to support running against the MSHV
hypervisor backend.

### Hotplug of `macvtap` devices

Hotplug of `macvtap` devices is now supported with the file descriptor for the
network device if opened by the user and passed to the VMM. The `ch-remote`
tool supports this functionality when adding a network device.

### Improved SGX support

The SGX support has been updated to match the latest Linux kernel support and
now supports SGX provisioning and associating EPC sections to NUMA nodes.

### Inflight tracking for `vhost-user` devices

Support for handling inflight tracking of I/O requests has been added to the
`vhost-user` devices allowing recovery after device reconnection.

### Notable bug fixes

* VFIO PCI BAR calculation code now correctly handles I/O BARs (#2821).
* The VMM side of `vhost-user` devices no longer advertise the
  `VIRTIO_F_RING_PACKED` feature as they are not yet supported in the VMM
(#2833).
* On ARM64 VMs can be created with more than 16 vCPUs (#2763).

### Contributors

Many thanks to everyone who has contributed to our release:

* Anatol Belski <anbelski@linux.microsoft.com>
* Arafatms <arafatms@outlook.com>
* Bo Chen <chen.bo@intel.com>
* Fei Li <lifei.shirley@bytedance.com>
* Henry Wang <Henry.Wang@arm.com>
* Jiachen Zhang <zhangjiachen.jaycee@bytedance.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Li Hangjing <lihangjing@bytedance.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* Yukiteru <wfly1998@sina.com>

# v16.0

This release has been tracked through the [v16.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/19).

### Improved live migration support

The live migration support inside Cloud Hypervisor has been improved with the addition of the tracking of dirty pages written by the VMM to complement the tracking of dirty pages made by the guest itself. Further the internal state of the VMM now is versioned which allows the safe migration of VMs from one version of the VMM to a newer one. However further testing is required so this should be done with care. See the [live migration documentation](docs/live_migration.md) for more details.

### Improved `vhost-user` support

When using `vhost-user` to access devices implemented in different processes there is now support for reconnection of those devices in the case of a restart of the backend. In addition it is now possible to operate with the direction of the `vhost-user-net` connection reversed with the server in the VMM and the client in the backend. This is aligns with the default approach recommended by Open vSwitch.

### ARM64 ACPI and UEFI support

Cloud Hypervisor now supports using ACPI and booting from a UEFI image on ARM64. This allows the use of stock OS images without direct kernel boot.

### Notable bug fixes

* Activating fewer `virtio-net` queues than advertised is now supported. This appeared when using OVMF with an MQ enabled device (#2578).
* When using MQ with `virtio` devices Cloud Hypervisor now enforces a minimum vCPU count which ensures that the user will not see adverse guest performance (#2563).
* The KVM clock is now correctly handled during live migration / snapshot & restore.

### Removed functionality

The following formerly deprecated features have been removed:

* Support for booting with the "LinuxBoot" protocol for ELF and `bzImage`
  binaries has been deprecated. When using direct boot users should configure
  their kernel with `CONFIG_PVH=y`.

### Contributors

Many thanks to everyone who has contributed to our release including some new faces.

* Anatol Belski <anbelski@linux.microsoft.com>
* Bo Chen <chen.bo@intel.com>
* Dayu Liu <liu.dayu@zte.com.cn>
* Henry Wang <Henry.Wang@arm.com>
* Jiachen Zhang <zhangjiachen.jaycee@bytedance.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Michael Zhao <michael.zhao@arm.com>
* Mikko Ylinen <mikko.ylinen@intel.com>
* Muminul Islam <muislam@microsoft.com>
* Ren Lei <ren.lei4@zte.com.cn>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* Yi Wang <wang.yi59@zte.com.cn>

# v15.0

This release has been tracked through the [v15.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/18).

Highlights for `cloud-hypervisor` version v15.0 include:

### Version numbering and stability guarantees

This release is the first in a new version numbering scheme to represent that
we believe Cloud Hypervisor is maturing and entering a period of stability.
With this new release we are beginning our new stability guarantees:

* The API (including command line options) will not be removed or changed in a
  breaking way without a minimum of 2 releases notice. Where possible warnings
  will be given about the use of deprecated functionality and the deprecations
  will be documented in the release notes.
* Point releases will be made between individual releases where there are
  substantial bug fixes or security issues that need to be fixed.

Currently the following items are **not** guaranteed across updates:

* Snapshot/restore is not supported across different versions
* Live migration is not supported across different versions
* The following features are considered experimental and may change
  substantially between releases: TDX, SGX.

### Network device rate limiting

Building on our existing support for rate limiting block activity the network
device also now supports rate limiting. Full details of the controls are in the
[IO throttling documentation.](docs/io_throttling.md)

### Support for runtime control of `virtio-net` guest offload

The guest is now able to change the offload settings for the `virtio-net`
device. As well as providing a useful control this mitigates an issue in the
Linux kernel where the guest will attempt to reprogram the offload settings
even if they are not advertised as configurable (#2528).

### `--api-socket` supports file descriptor parameter

The `--api-socket` can now take an `fd=` parameter to specify an existing file
descriptor to use. This is particularly beneficial for frameworks that need to
programmatically control Cloud Hypervisor.

### Bug fixes

* A workaround has been put in place to mitigate a Linux kernel issues that
  results in the CPU thread spinning at 100% when using `virtio-pmem` (#2277).
* PCI BARs are now correctly aligned removing the need for the guest to
  reprogram them (#1797,#1798)
* Handle TAP interface not being writable within virtio-net (due to the buffer
  exhaustion on the host) (#2517)
* The recommended Linux kernel is now v5.12.0 as it contains a fix that
  prevents snapshot & restore working (#2535)

### Deprecations

Deprecated features will be removed in a subsequent release and users should plan to use alternatives

* Support for booting with the "LinuxBoot" protocol for ELF and `bzImage`
  binaries has been deprecated. When using direct boot users should configure
  their kernel with `CONFIG_PVH=y`. Will be removed in v16.0.

### Contributors

Many thanks to everyone who has contributed to our release including some new faces.

* Alyssa Ross <hi@alyssa.is>
* Anatol Belski <anbelski@linux.microsoft.com>
* Bo Chen <chen.bo@intel.com>
* Gaelan Steele <gbs@canishe.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>
* William Douglas <william.douglas@intel.com>

# v0.14.1

Bug fix release branched off the v0.14.0 release. The following bugs were fixed
in this release:

* CPU hotplug on Windows failed due to misreported CPU state information and
  the lack of HyperV CPUID bit enabled (#2437, #2449, #2436)
* A seccomp rule was missing that was triggered on CPU unplug (#2455)
* A bounds check in VIRTIO queue validation was erroneously generating
  DescriptorChainTooShort errors in certain circumstances (#2450, #2424)

# v0.14.0

This release has been tracked through the [0.14.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/17).

Highlights for `cloud-hypervisor` version 0.14.0 include:

### Structured event monitoring

A new option was added to the VMM `--event-monitor` which reports structured
events (JSON) over a file or file descriptor at key events in the lifecycle of
the VM. The list of events is limited at the moment but will be further
extended over subsequent releases. The events exposed form part of the Cloud
Hypervisor API surface.

### MSHV improvements

Basic support has been added for running Windows guests atop the MSHV
hypervisor as an alternative to KVM and further improvements have been made to
the MSHV support.

### Improved aarch64 platform

The aarch64 platform has been enhanced with more devices exposed to the running
VM including an enhanced serial UART.

### Updated hotplug documentation

The documentation for the hotplug support has been updated to reflect the use
of the `ch-remote` tool and to include details of `virtio-mem` based hotplug as
well as documenting hotplug of paravirtualised and VFIO devices.

### PTY control for serial and `virtio-console`

The `--serial` and `--console` parameters can now direct the console to a PTY
allowing programmatic control of the console from another process through the
PTY subsystem.

### Block device rate limiting

The block device performance can now be constrained as part of the VM
configuration allowing rate limiting. Full details of the controls are in the
[IO throttling documentation.](docs/io_throttling.md)


### Deprecations

Deprecated features will be removed in a subsequent release and users should plan to use alternatives

* Support for booting with the "LinuxBoot" protocol for ELF and `bzImage`
  binaries has been deprecated. When using direct boot users should configure
  their kernel with `CONFIG_PVH=y`.


### Contributors

Many thanks to everyone who has contributed to our 0.14.0 release including
some new faces.

Bo Chen <chen.bo@intel.com>
Henry Wang <Henry.Wang@arm.com>
Iggy Jackson <iggy@theiggy.com>
Jiachen Zhang <zhangjiachen.jaycee@bytedance.com>
Michael Zhao <michael.zhao@arm.com>
Muminul Islam <muislam@microsoft.com>
Penny Zheng <Penny.Zheng@arm.com>
Rob Bradford <robert.bradford@intel.com>
Sebastien Boeuf <sebastien.boeuf@intel.com>
Vineeth Pillai <viremana@linux.microsoft.com>
Wei Liu <liuwe@microsoft.com>
William Douglas <william.r.douglas@gmail.com>
Zide Chen <zide.chen@intel.com>

# v0.13.0

This release has been tracked through the [0.13.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/16).

Highlights for `cloud-hypervisor` version 0.13.0 include:

### Wider VFIO device support

It is now possible to use Cloud Hypervisor's VFIO support to passthrough PCI
devices that do not support MSI or MSI-X and instead rely on INTx interrupts.
Most notably this widens the support to most NVIDIA cards with the proprietary
drivers.

### Improved huge page support

Through the addition of `hugepage_size` on `--memory` it is now possible to
specify the desired size of the huge pages used when allocating the guest
memory. The user is required to ensure they have sufficient pages of the
desired size in their pool.

### MACvTAP support

It is now possible to provide file descriptors using the `fd` parameter to
`--net` which point at TAP devices that have already been opened by the user.
This aids integration with `libvirt` but also permits the use of MACvTAP
support. This is documented in dedicated [macvtap documentation](docs/macvtap-bridge.md).

### VHD disk image support

It is now possible to use VHD (fixed) disk images as well as QCOWv2 and raw
disk image with Cloud Hypervisor.

### Improved Virtio device threading

Device threads are now derived from the main VMM thread which allows more
restrictive seccomp filters to be applied to them. The threads also have a
predictable name derived from the device id.

### Clean shutdown support via synthetic power button

It is now possible to request that the guest VM shut itself down by triggering
a synthetic ACPI power button press from the VMM. If the guest is listening for
such an event (e.g. using systemd) then it will process the event and cleanly
shut down. This functionality is exposed through the HTTP API and can be
triggered via `ch-remote --api-socket=<API socket> power-button`.

### Contributors

Many thanks to everyone who has contributed to our 0.13.0 release including
some new faces.

* Bo Chen <chen.bo@intel.com>
* Mikko Ylinen <mikko.ylinen@intel.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Vineeth Pillai <viremana@linux.microsoft.com>
* Wei Liu <liuwe@microsoft.com>
* William Douglas <william.r.douglas@gmail.com>
* Xie Yongji <xieyongji@bytedance.com>

# v0.12.0

This release has been tracked through the [0.12.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/15).

Highlights for `cloud-hypervisor` version 0.12.0 include:

### ARM64 enhancements

The use of `--watchdog` is now fully supported as is the ability to reboot the
VM from within the guest when running Cloud Hypervisor on an ARM64 system.

### Removal of `vhost-user-net` and `vhost-user-block` self spawning

In order to use `vhost-user-net` or `vhost-user-block` backends the user is now
responsible for starting the backend and providing the socket for the VMM to
use. This functionality was deprecated in the last release and how now been
removed.

### Migration of `vhost-user-fs` backend

The `vhost-user-fs` backend is no longer included in Cloud Hypervisor and it is
instead hosted in [it's own
repository](https://gitlab.com/virtio-fs/virtiofsd-rs)

### Enhanced "info" API

The `vm.info` HTTP API endpoint has been extended to include the details of the
devices used by the VM including any VFIO devices used.

### Contributors

Many thanks to everyone who has contributed to our 0.12.0 release:

* Anatol Belski <anbelski@linux.microsoft.com>
* Julio Montes <julio.montes@intel.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Wei Liu <liuwe@microsoft.com>

# v0.11.0

This release has been tracked through the [0.11.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/14).

Highlights for `cloud-hypervisor` version 0.11.0 include:

### `io_uring` support by default for `virtio-block`

Provided that the host OS supports it (Linux kernel 5.8+) then `io_uring` will
be used for a significantly higher performance block device. 

### Windows Guest Support

This is the first release where we officially support Windows running as a
guest. Full details of how to setup the image and run Cloud Hypervisor with a
Windows guest can be found in the dedicated [Windows
documentation](docs/windows.md).

### `vhost-user` "Self Spawning" Deprecation

Automatically spawning a `vhost-user-net` or `vhost-user-block` backend is now
deprecated. Users of this functionality will receive a warning and should make
adjustments. The functionality will be removed in the next release.

### `virtio-mmio` Removal

Support for using the `virtio-mmio` transport, rather than using PCI, has been
removed. This has been to simplify the code and significantly
reduce the testing burden of the project.

### Snapshot/Restore support for ARM64

When running on the ARM64 architecture snapshot and restore has now been
implemented.

### Improved Linux Boot Time

The time to boot the Linux kernel has been significantly improved by the
identifying some areas of delays around PCI bus probing, IOAPIC programming and
MPTABLE issues. Full details can be seen in #1728.

### `SIGTERM/SIGINT` Interrupt Signal Handling

When the VMM process receives the `SIGTERM` or `SIGINT` signals then it will
trigger the VMM process to cleanly deallocate resources before exiting. The
guest VM will not be cleanly shutdown but the VMM process will clean up its
resources.

### Default Log Level Changed

The default logging level was changed to include warnings which should make it
easier to see potential issues. New [logging
documentation](docs/logging) was also added.

### New `--balloon` Parameter Added

Control of the setup of `virtio-balloon` has been moved from `--memory` to its
own dedicated parameter. This makes it easier to add more balloon specific
controls without overloading `--memory`.

### Experimental `virtio-watchdog` Support

Support for using a new `virtio-watchdog` has been added which can be used to
have the VMM reboot the guest if the guest userspace fails to ping the
watchdog. This is enabled with `--watchdog` and requires kernel support.

### Notable Bug Fixes

* MTRR bit was missing from CPUID advertised to guest
* "Return" key could not be used under `CMD.EXE` under Windows SAC (#1170)
* CPU identification string is now exposed to the guest
* `virtio-pmem` with`discard_writes=on` no longer marks the guest memory as
  read only so avoids excessive VM exits (#1795)
* PCI device hotplug after an unplug was fixed (#1802)
* When using the ACPI method to resize the guest memory the full reserved size
  can be used (#1803)
* Snapshot and restore followed by a second snapshot and restore now works
  correctly
* Snapshot and restore of VMs with more than 2GiB in one region now work
  correctly

### Contributors

Many thanks to everyone who has contributed to our 0.11.0 release including some new faces.

* Anatol Belski <anbelski@linux.microsoft.com>
* Bo Chen <chen.bo@intel.com>
* Daniel Verkamp <dverkamp@chromium.org>
* Henry Wang <Henry.Wang@arm.com>
* Hui Zhu <teawater@antfin.com>
* Jiangbo Wu <jiangbo.wu@intel.com>
* Josh Soref <jsoref@users.noreply.github.com>
* Julio Montes <julio.montes@intel.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* pierwill <19642016+pierwill@users.noreply.github.com>
* Praveen Paladugu <prapal@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>


# v0.10.0

This release has been tracked through the [0.10.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/13).

Highlights for `cloud-hypervisor` version 0.10.0 include:

### `virtio-block` Support for Multiple Descriptors

Some `virtio-block` device drivers may generate requests with multiple descriptors and support has been added for those drivers.

### Memory Zones

Support has been added for fine grained control of memory allocation for the guest. This includes controlling the backing of sections of guest memory, assigning to specific host NUMA nodes and assigning memory and vCPUs to specific memory nodes inside the guest. Full details of this can be found in the [memory documentation](docs/memory.md).

### `Seccomp` Sandbox Improvements

All the remaining threads and devices are now isolated within their own `seccomp` filters. This provides a layer of sandboxing and enhances the security model of `cloud-hypervisor`.

### Preliminary KVM HyperV Emulation Control

A new option (`kvm_hyperv`) has been added to `--cpus` to provide an option to toggle on KVM's HyperV emulation support. This enables progress towards booting Windows without adding extra emulated devices.

### Notable Bug Fixes

- When using `ch-remote` to resize the VM parameter now accepts the standard sizes suffices (#1596)
- `cloud-hypervisor` no longer panics when started with `--memory hotplug_method=virtio-mem` and no `hotplug_size` (#1564)
- After a reboot memory can remove when using `--memory hotplug_method=virtio-mem` (#1593)
- `--version` shows the version for released binaries (#1669)
- Errors generated by worker threads for `virtio` devices are now printed out (#1551)

### Contributors

Many thanks to everyone who has contributed to our 0.10.0 release including some new faces.

* Alyssa Ross <hi@alyssa.is>
* Amey Narkhede <ameynarkhede02@gmail.com>
* Anatol Belski <ab@php.net>
* Bo Chen <chen.bo@intel.com>
* Hui Zhu <teawater@antfin.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Wei Liu <liuwe@microsoft.com>

# v0.9.0

This release has been tracked through the [0.9.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/12).

Highlights for `cloud-hypervisor` version 0.9.0 include:

### `io_uring` Based Block Device Support

If the `io_uring` feature is enabled and the host kernel supports it then `io_uring` will be used for block devices. This results a very significant performance improvement.

### Block and Network Device Statistics

Statistics for activity of the `virtio` network and block devices is now exposed through a new `vm.counters` HTTP API entry point. These take the form of simple counters which can be used to observe the activity of the VM.

### HTTP API Responses

The HTTP API for adding devices now responds with the name that was assigned to the device as well the PCI BDF.

### CPU Topology

A `topology` parameter has been added to `--cpus` which allows the configuration of the guest CPU topology allowing the user to specify the numbers of sockets, packages per socket, cores per package and threads per core.

### Release Build Optimization

Our release build is now built with LTO (*Link Time Optimization*) which results in a ~20% reduction in the binary size.

### Hypervisor Abstraction

A new abstraction has been introduced, in the form of a `hypervisor` crate so as to enable the support of additional hypervisors beyond `KVM`.

### Snapshot/Restore Improvements

Multiple improvements have been made to the VM snapshot/restore support that was added in the last release. This includes persisting more vCPU state and in particular preserving the guest paravirtualized clock in order to avoid vCPU hangs inside the guest when running with multiple vCPUs.

### Virtio Memory Ballooning Support
 
A `virtio-balloon` device has been added, controlled through the `resize` control, which allows the reclamation of host memory by resizing a memory balloon inside the guest.

### Enhancements to ARM64 Support

The ARM64 support introduced in the last release has been further enhanced with support for using PCI for exposing devices into the guest as well as multiple bug fixes. It also now supports using an initramfs when booting.

### Intel SGX Support

The guest can now use Intel SGX if the host supports it. Details can be found in the dedicated [SGX documentation](docs/intel_sgx.md).

### `Seccomp` Sandbox Improvements

The most frequently used virtio devices are now isolated with their own `seccomp` filters. It is also now possible to pass `--seccomp=log` which result in the logging of requests that would have otherwise been denied to further aid development.

### Notable Bug Fixes

* Our `virtio-vsock` implementation has been resynced with the implementation from Firecracker and includes multiple bug fixes.
* CPU hotplug has been fixed so that it is now possible to add, remove, and re-add vCPUs (#1338)
* A workaround is now in place for when KVM reports MSRs available MSRs that are in fact unreadable preventing snapshot/restore from working correctly (#1543).
* `virtio-mmio` based devices are now more widely tested (#275).
* Multiple issues have been fixed with virtio device configuration (#1217)
* Console input was wrongly consumed by both `virtio-console` and the serial. (#1521)

### Contributors

Many thanks to everyone who has contributed to our 0.9.0 release including some new faces.

* Anatol Belski <ab@php.net>
* Bo Chen <chen.bo@intel.com>
* Dr. David Alan Gilbert <dgilbert@redhat.com>
* Henry Wang <Henry.Wang@arm.com>
* Howard Zhang <howard.zhang@arm.com>
* Hui Zhu <teawater@antfin.com>
* Jianyong Wu <jianyong.wu@arm.com>
* Jose Carlos Venegas Munoz <jose.carlos.venegas.munoz@intel.com>
* LiYa'nan <oliverliyn@gmail.com>
* Michael Zhao <michael.zhao@arm.com>
* Muminul Islam <muislam@microsoft.com>
* Praveen Paladugu <prapal@microsoft.com>
* Ricardo Koller <ricarkol@gmail.com>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Stefano Garzarella <sgarzare@redhat.com>
* Wei Liu <liuwe@microsoft.com>


# v0.8.0

This release has been tracked through the [0.8.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/10).

Highlights for `cloud-hypervisor` version 0.8.0 include:

### Experimental Snapshot and Restore Support

This release includes the first version of the snapshot and restore feature.
This allows a VM to be paused and then subsequently snapshotted. At a later
point that snapshot may be restored into a new running VM identical to the
original VM at the point it was paused.

This feature can be used for offline migration from one VM host to another, to
allow the upgrading or rebooting of the host machine transparently to the guest
or for templating the VM. This is an experimental feature and cannot be used on
a VM using passthrough (VFIO) devices. Issues with SMP have also been observed
(#1176).

### Experimental ARM64 Support

Included in this release is experimental support for running on ARM64.
Currently only `virtio-mmio` devices and a serial port are supported. Full
details can be found in the [ARM64 documentation](docs/arm64.md).

### Support for Using 5-level Paging in Guests

If the host supports it the guest is now enabled for 5-level paging (aka LA57).
This works when booting the Linux kernel with a vmlinux, bzImage or firmware
based boot. However booting an ELF kernel built with `CONFIG_PVH=y` does not
work due to current limitations in the PVH boot process.

### Virtio Device Interrupt Suppression for Network Devices

With `virtio-net` and `vhost-user-net` devices the guest can suppress
interrupts from the VMM by using the `VIRTIO_RING_F_EVENT_IDX` feature. This
can lead to an improvement in performance by reducing the number of interrupts
the guest must service.

### `vhost_user_fs` Improvements

The implementation in Cloud Hypervisor of the VirtioFS server now supports sandboxing itself with `seccomp`.


### Notable Bug Fixes

* VMs that have not yet been booted can now be deleted (#1110).
* By creating the `tap` device ahead of creating the VM it is not required to
  run the `cloud-hypervisor` binary with `CAP_NET_ADMIN` (#1273).
* Block I/O via `virtio-block` or `vhost-user-block` now correctly adheres to
  the specification and synchronizes to the underlying filesystem as required
  based on guest feature negotiation. This avoids potential data loss (#399,
  #1216).
* When booting with a large number of vCPUs then the ACPI table would be
  overwritten by the SMP `MPTABLE`. When compiled with the `acpi` feature the
  `MPTABLE` will no longer be generated (#1132).
* Shutting down VMs that have been paused is now supported (#816).
* Created socket files are deleted on shutdown (#1083).
* Trying to use passthrough devices (VFIO) will be rejected on `mmio` builds
  (#751).

### Command Line and API Changes

This is non exhaustive list of HTTP API and command line changes:

* All user visible socket parameters are now consistently called `socket`
  rather than `sock` in some cases.
* The `ch-remote` tool now shows any error message generated by the VMM
* The `wce` parameter has been removed from `--disk` as the feature is always
  offered for negotiation.
* `--net` has gained a `host_mac` option that allows the setting of the MAC
  address for the `tap` device on the host.

### Contributors

Many thanks to everyone who has contributed to our 0.8.0 release including some new faces.

* Anatol Belski <ab@php.net>
* Arron Wang <arron.wang@intel.com>
* Bo Chen <chen.bo@intel.com>
* Dr. David Alan Gilbert <dgilbert@redhat.com>
* Henry Wang <Henry.Wang@arm.com>
* Hui Zhu <teawater@antfin.com>
* LiYa'nan <oliverliyn@gmail.com>
* Michael Zhao <michael.zhao@arm.com>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Sergio Lopez <slp@redhat.com>

# v0.7.0

This release has been tracked through the [0.7.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/7).

Highlights for `cloud-hypervisor` version 0.7.0 include:

### Block, Network, Persistent Memory (PMEM), VirtioFS and Vsock hotplug

Further to our effort to support modifying a running guest we now support
hotplug and unplug of the following virtio backed devices: block, network,
pmem, virtio-fs and vsock. This functionality is available on the (default) PCI
based transport and is exposed through the HTTP API. The `ch-remote` utility
provides a CLI for adding or removing these device types after the VM has
booted. User can use the `id` parameter on the devices to choose names for
devices to ease their removal.

### Alternative `libc` Support

Cloud Hypervisor can now be compiled with the `musl` C library and this release
contains a static binary compiled using that toolchain.

### Multithreaded Multi Queued `vhost-user` Backends

The `vhost-user` backends for network and block support that are shipped by
Cloud Hypervisor have been enhanced to support multiple threads and queues to
improve throughput. These backends are used automatically if `vhost_user=true`
is passed when the devices are created.

### Initial RamFS Support

By passing the `--initramfs` command line option the user can specify a file to
be loaded into the guest memory to be used as the kernel initial filesystem.
This is usually used to allow the loading of drivers needed to be able to
access the real root filesystem but it can also be used standalone for a very
minimal image.

### Alternative Memory Hotplug: `virtio-mem`

As well as supporting ACPI based hotplug Cloud Hypervisor now supports using
the `virtio-mem` hotplug alternative. This can be controlled by the
`hotplug_method` parameter on the `--memory` command line option. It currently
requires kernel patches to be able to support it.

### `Seccomp` Sandboxing

Cloud Hypervisor now has support for restricting the system calls that the
process can use via the `seccomp` security API. This on by default and is
controlled by the `--seccomp` command line option.

### Updated Distribution Support

With the release of Ubuntu 20.04 we have added that to the list of supported
distributions and is part of our regular testing programme.

### Command Line and API Changes

This is non exhaustive list of HTTP API and command line changes

* New `id` fields added for devices to allow them to be named to ease removal.
  If no name is specified the VMM chooses one.
* Use `--memory`'s `shared` and `hugepages` controls for determining backing
  memory instead of providing a path.
* The `--vsock` parameter only takes one device as the Linux kernel only
  supports a single Vsock device. The REST API has removed the vector for this
  option and replaced it with a single optional field.
* There is enhanced validation of the command line and API provided
  configurations to ensure that the provided options are compatible e.g. that
  shared memory is in use if any attempt is made to used a `vhost-user` backed
  device.
* `ch-remote` has added `add-disk`, `add-fs`, `add-net`, `add-pmem` and
  `add-vsock` subcommands. For removal `remove-device` is used. The REST API
  has appropriate new HTTP endpoints too.
* Specifying a `size` with `--pmem` is no longer required and instead the size
  will be obtained from the file. A `discard_writes` option has also been added
  to provide the equivalent of a read-only file.
* The parameters to `--block-backend` have been changed to more closely align
  with those used by `--disk`.

### Contributors

Many thanks to everyone who has contributed to our 0.7.0 release including some new faces.

* Alejandro Jimenez <alejandro.j.jimenez@oracle.com>
* Bo Chen <chen.bo@intel.com>
* Cathy Zhang <cathy.zhang@intel.com>
* Damjan Georgievski <gdamjan@gmail.com>
* Dean Sheather <dean@coder.com>
* Eryu Guan <eguan@linux.alibaba.com>
* Hui Zhu <teawater@antfin.com>
* Jose Carlos Venegas Munoz <jose.carlos.venegas.munoz@intel.com>
* Martin Xu <martin.xu@intel.com>
* Muminul Islam <muislam@microsoft.com>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Sergio Lopez <slp@redhat.com>
* Yang Zhong <yang.zhong@intel.com>
* Yi Sun <yi.y.sun@linux.intel.com>

# v0.6.0

This release has been tracked through the [0.6.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/7).

Highlights for `cloud-hypervisor` version 0.6.0 include:

### Directly Assigned Devices Hotplug

We continued our efforts around supporting dynamically changing the guest
resources. After adding support for CPU and memory hotplug, Cloud Hypervisor
now supports hot plugging and hot unplugging directly assigned (a.k.a. `VFIO`)
devices into an already running guest. This closes the features gap for
providing a complete Kata Containers workloads support with Cloud Hypervisor.

### Shared Filesystem Improvements

We enhanced our shared filesystem support through many `virtio-fs` improvements.
By adding support for DAX, parallel processing of multiple requests, `FS_IO`,
`LSEEK` and the `MMIO` virtio transport layer to our `vhost_user_fs` daemon, we
improved our filesystem sharing performance, but also made it more stable and
compatible with other `virtio-fs` implementations.

### Block and Networking IO Self Offloading

When choosing to offload the paravirtualized block and networking I/O to an
external process (through the `vhost-user` protocol), Cloud Hypervisor now
automatically spawns its default `vhost-user-blk` and `vhost-user-net` backends
into their own, separate processes.
This provides a seamless paravirtualized I/O user experience for those who want
to run their guest I/O into separate executions contexts.

### Command Line Interface

More and more Cloud Hypervisor services are exposed through the
[Rest API](vmm/src/api/openapi/cloud-hypervisor.yaml) and thus only
accessible via relatively cumbersome HTTP calls. In order to abstract
those calls into a more user friendly tool, we created a Cloud Hypervisor
Command Line Interface (CLI) called `ch-remote`.  The `ch-remote` binary
is created with each build and available e.g. at
`cloud-hypervisor/target/debug/ch-remote` when doing a debug build.

Please check `ch-remote --help` for a complete description of all available
commands.

### PVH Boot

In addition to the traditional Linux boot protocol, Cloud Hypervisor now
supports direct kernel booting through the [PVH ABI](https://xenbits.xen.org/docs/unstable/misc/pvh.html).

### Contributors

With the 0.6.0 release, we are welcoming a few new contributors. Many thanks
to them and to everyone that contributed to this release:

* Alejandro Jimenez <alejandro.j.jimenez@oracle.com>
* Arron Wang <arron.wang@intel.com>
* Bin Liu <liubin0329@gmail.com>
* Bo Chen <chen.bo@intel.com>
* Cathy Zhang <cathy.zhang@intel.com>
* Eryu Guan <eguan@linux.alibaba.com>
* Jose Carlos Venegas Munoz <jose.carlos.venegas.munoz@intel.com>
* Liu Bo <bo.liu@linux.alibaba.com>
* Qiu Wenbo <qiuwenbo@phytium.com.cn>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Sergio Lopez <slp@redhat.com>

# v0.5.1

This is a bugfix release branched off v0.5.0. It contains the following fixes:

* Update DiskConfig to contain missing disk control features (#790) - Samuel Ortiz and Sergio Lopez
* Prevent memory overcommit via virtio-fs (#763) - Sebastien Boeuf
* Fixed error reporting for resize command - Samuel Ortiz
* Double reboot workaround (#783) - Rob Bradford
* Various CI and development tooling fixes - Sebastien Boeuf, Samuel Ortiz, Rob Bradford

# v0.5.0

This release has been tracked through the [0.5.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/6).

Highlights for `cloud-hypervisor` version 0.5.0 include:

### Virtual Machine Dynamic Resizing

With 0.4.0 we added support for CPU hot plug, and 0.5.0 adds CPU hot unplug and
memory hot plug as well. This allows to dynamically resize Cloud Hypervisor
guests which is needed for e.g. Kubernetes related use cases.
The memory hot plug implementation is based on the same framework as the CPU hot
plug/unplug one, i.e. hardware-reduced ACPI notifications to the guest.

Next on our VM resizing roadmap is the PCI devices hotplug feature.

### Multi-Queue, Multi-Threaded Paravirtualization

We enhanced our virtio networking and block support by having both devices use
multiple I/O queues handled by multiple threads. This improves our default
paravirtualized networking and block devices throughput.

### New Interrupt Management Framework

We improved our interrupt management implementation by introducing an Interrupt
Manager framework, based on the currently on-going [rust-vmm vm-device](https://github.com/rust-vmm/vm-device)
crates discussions. This move made the code significantly cleaner, and allowed
us to remove several KVM related dependencies from crates like the PCI and
virtio ones.

### Development Tools

In order to provide a better developer experience, we worked on improving our
build, development and testing tools.
Somehow similar to the excellent
[Firecracker's devtool](https://github.com/firecracker-microvm/firecracker/blob/master/tools/devtool),
we now provide a [dev_cli script](scripts/dev_cli.sh).

With this new tool, our users and contributors will be able to build and test
Cloud Hypervisor through a containerized environment.

### Kata Containers Integration

We spent some significant time and efforts debugging and fixing our integration
with the [Kata Containers](https://github.com/kata-containers) project. Cloud
Hypervisor is now a fully supported Kata Containers hypervisor, and is
integrated into the project's CI.

### Contributors

Many thanks to everyone that contributed to the 0.5.0 release:

* Bo Chen <chen.bo@intel.com>
* Cathy Zhang <cathy.zhang@intel.com>
* Qiu Wenbo <qiuwenbo@phytium.com.cn>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Sergio Lopez <slp@redhat.com>
* Yang Zhong <yang.zhong@intel.com>

# v0.4.0

This release has been tracked through the [0.4.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/4).

Highlights for `cloud-hypervisor` version 0.4.0 include:

### Dynamic virtual CPUs addition

As a way to vertically scale Cloud Hypervisor guests, we now support dynamically
adding virtual CPUs to the guests, a mechanism also known as CPU hot plug.
Through hardware-reduced ACPI notifications, Cloud Hypervisor can now add CPUs
to an already running guest and the high level operations for that process are
documented [here](docs/hotplug.md)

During the next release cycles we are planning to extend Cloud Hypervisor
hot plug framework to other resources, namely PCI devices and memory.

### Programmatic firmware tables generation

As part of the CPU hot plug feature enablement, and as a requirement for hot
plugging other resources like devices or RAM, we added support for
programmatically generating the needed ACPI tables. Through a dedicated
`acpi-tables` crate, we now have a flexible and clean way of generating those
tables based on the VMM device model and topology.

### Filesystem and block devices vhost-user backends

Our objective of running all Cloud Hypervisor paravirtualized I/O to a
vhost-user based framework is getting closer as we've added Rust based
implementations for vhost-user-blk and virtiofs backends. Together with the
vhost-user-net backend that came with the 0.3.0 release, this will form the
default Cloud Hypervisor I/O architecture.

### Guest pause and resume

As an initial requirement for enabling live migration, we added support for
pausing and resuming any VMM components. As an intermediate step towards live
migration, the upcoming guest snapshotting feature will be based on the pause
and resume capabilities.

### Userspace IOAPIC by default

As a way to simplify our device manager implementation, but also in order to
stay away from privileged rings as often as possible, any device that relies on
pin based interrupts will be using the userspace IOAPIC implementation by
default.

### PCI BAR reprogramming

In order to allow for a more flexible device model, and also support guests
that would want to move PCI devices, we added support for PCI devices BAR
reprogramming.

### New `cloud-hypervisor` organization

As we wanted to be more flexible on how we manage the Cloud Hypervisor project,
we decided to move it under a [dedicated GitHub organization](https://github.com/cloud-hypervisor).
Together with the [cloud-hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor)
project, this new organization also now hosts our [kernel](https://github.com/cloud-hypervisor/linux)
and [firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware)
repositories. We may also use it to host any rust-vmm that we'd need to
temporarily fork.
Thanks to GitHub's seamless repository redirections, the move is completely
transparent to all Cloud Hypervisor contributors, users and followers.

### Contributors

Many thanks to everyone that contributed to the 0.4.0 release:

* Cathy Zhang <cathy.zhang@intel.com>
* Emin Ghuliev <drmint80@gmail.com>
* Jose Carlos Venegas Munoz <jose.carlos.venegas.munoz@intel.com>
* Qiu Wenbo <qiuwenbo@phytium.com.cn>
* Rob Bradford <robert.bradford@intel.com>
* Samuel Ortiz <sameo@linux.intel.com>
* Sebastien Boeuf <sebastien.boeuf@intel.com>
* Sergio Lopez <slp@redhat.com>
* Wu Zongyong <wuzongyong@linux.alibaba.com>

# v0.3.0

This release has been tracked through the [0.3.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/3).

Highlights for `cloud-hypervisor` version 0.3.0 include:

### Block device offloading

We continue to work on offloading paravirtualized I/O to external processes,
and we added support for
[vhost-user-blk](https://access.redhat.com/solutions/3394851) backends.
This enables `cloud-hypervisor` users to plug a `vhost-user` based block device
like [SPDK](https://spdk.io)) into the VMM as their paravirtualized storage
backend.

### Network device backend

The previous release provided support for
[vhost-user-net](https://access.redhat.com/solutions/3394851) backends. Now we
also provide a TAP based vhost-user-net backend, implemented in Rust. Together
with the vhost-user-net device implementation, this will eventually become the
Cloud Hypervisor default paravirtualized networking architecture.

### Virtual sockets

In order to more efficiently and securely communicate between host and guest,
we added an hybrid implementation of the
[VSOCK](http://man7.org/linux/man-pages/man7/vsock.7.html) socket address
family over virtio. Credits go to the
[Firecracker](https://github.com/firecracker-microvm/firecracker/blob/master/docs/vsock.md)
project as our implementation is a copy of theirs.

### HTTP based API

In anticipation of the need to support asynchronous operations to Cloud
Hypervisor guests (e.g. resources hotplug and guest migration), we added a HTTP
based API to the VMM. The API will be more extensively documented during the
next release cycle.

### Memory mapped virtio transport

In order to support potential PCI-free use cases, we added support for the
[virtio MMIO](https://docs.oasis-open.org/virtio/virtio/v1.1/cs01/virtio-v1.1-cs01.html#x1-1440002)
transport layer. This will allow us to support simple, minimal guest
configurations that do not require a PCI bus emulation.

### Paravirtualized IOMMU

As we want to improve our nested guests support, we added support for exposing
a [paravirtualized IOMMU](docs/iommu.md) device through virtio. This allows
for a safer nested virtio and directly assigned devices support.

To add the IOMMU support, we had to make some CLI changes for Cloud Hypervisor
users to be able to specify if devices had to be handled through this virtual
IOMMU or not. In particular, the `--disk` option now expects disk paths to be
prefixed with a `path=` string, and supports an optional `iommu=[on|off]`
setting.

### Ubuntu 19.10

With the latest [hypervisor firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware),
we can now support the latest
[Ubuntu 19.10 (Eoan Ermine)](http://releases.ubuntu.com/19.10/) cloud images.

### Large memory guests

After simplifying and changing our guest address space handling, we can now
support guests with large amount of memory (more than 64GB).

# v0.2.0

This release has been tracked through the [0.2.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/2).

Highlights for `cloud-hypervisor` version 0.2.0 include:

### Network device offloading

As part of our general effort to offload paravirtualized I/O to external
processes, we added support for
[vhost-user-net](https://access.redhat.com/solutions/3394851) backends. This
enables `cloud-hypervisor` users to plug a `vhost-user` based networking device
(e.g. [DPDK](https://dpdk.org)) into the VMM as their virtio network backend.

### Minimal hardware-reduced ACPI

In order to properly implement and guest reset and shutdown, we implemented
a minimal version of the hardware-reduced ACPI specification. Together with
a tiny I/O port based ACPI device, this allows `cloud-hypervisor` guests to
cleanly reboot and shutdown.

The ACPI implementation is a `cloud-hypervisor` build time option that is
enabled by default.

### Debug I/O port

Based on the Firecracker idea of using a dedicated I/O port to measure guest
boot times, we added support for logging guest events through the
[0x80](https://www.intel.com/content/www/us/en/support/articles/000005500/boards-and-kits.html)
PC debug port. This allows, among other things, for granular guest boot time
measurements. See our [debug port documentation](docs/debug-port.md) for more
details.

### Improved direct device assignment

We fixed a major performance issue with our initial VFIO implementation: When
enabling VT-d through the KVM and VFIO APIs, our guest memory writes and reads
were (in many cases) not cached. After correctly tagging the guest memory from
`cloud-hypervisor` we're now able to reach the expected performance from
directly assigned devices.

### Improved shared filesystem

We added shared memory region with [DAX](https://www.kernel.org/doc/Documentation/filesystems/dax.txt)
support to our [virtio-fs](https://virtio-fs.gitlab.io/) shared file system.
This provides better shared filesystem IO performance with a smaller guest
memory footprint.

### Ubuntu bionic based CI

Thanks to our [simple KVM firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware)
improvements, we are now able to boot Ubuntu bionic images. We added those to
our CI pipeline.

# v0.1.0

This release has been tracked through the [0.1.0 project](https://github.com/cloud-hypervisor/cloud-hypervisor/projects/1).

Highlights for `cloud-hypervisor` version 0.1.0 include:

### Shared filesystem

We added support for the [virtio-fs](https://virtio-fs.gitlab.io/) shared file
system, allowing for an efficient and reliable way of sharing a filesystem
between the host and the `cloud-hypervisor` guest.

See our [filesystem sharing](docs/fs.md) documentation for more details on how
to use virtio-fs with `cloud-hypervisor`.

### Initial direct device assignment support

VFIO (Virtual Function I/O) is a kernel framework that exposes direct device
access to userspace. `cloud-hypervisor` uses VFIO to directly assign host
physical devices into its guest.

See our [VFIO](docs/vfio.md) documentation for more detail on how to directly
assign host devices to `cloud-hypervisor` guests.

### Userspace IOAPIC

`cloud-hypervisor` supports a so-called split IRQ chip implementation by
implementing support for the [IOAPIC](https://wiki.osdev.org/IOAPIC).
By moving part of the IRQ chip implementation from kernel space to user space,
the IRQ chip emulation does not always run in a fully privileged mode.

### Virtual persistent memory

The `virtio-pmem` implementation emulates a virtual persistent memory device
that `cloud-hypervisor` can e.g. boot from. Booting from a `virtio-pmem` device
allows to bypass the guest page cache and improve the guest memory footprint.

### Linux kernel bzImage

The `cloud-hypervisor` linux kernel loader now supports direct kernel boot from
`bzImage` kernel images, which is usually the format that Linux distributions
use to ship their kernels. For example, this allows for booting from the host
distribution kernel image.

### Console over virtio

`cloud-hypervisor` now exposes a `virtio-console` device to the guest. Although
using this device as a guest console can potentially cut some early boot
messages, it can reduce the guest boot time and provides a complete console
implementation.

The `virtio-console` device is enabled by default for the guest console.
Switching back to the legacy serial port is done by selecting
`--serial tty --console off` from the command line.

### Unit testing

We now run all unit tests from all our crates directly from our CI.

### Integration tests parallelization

The CI cycle run time has been significantly reduced by refactoring our
integration tests; allowing them to all be run in parallel.
