- [1. What is Cloud Hypervisor?](#1-what-is-cloud-hypervisor)
  - [Objectives](#objectives)
    - [High Level](#high-level)
    - [Architectures](#architectures)
    - [Guest OS](#guest-os)
- [2. Getting Started](#2-getting-started)
  - [Host OS](#host-os)
  - [Use Pre-built Binaries](#use-pre-built-binaries)
  - [Packages](#packages)
  - [Building from Source](#building-from-source)
  - [Booting Linux](#booting-linux)
    - [Firmware Booting](#firmware-booting)
    - [Custom Kernel and Disk Image](#custom-kernel-and-disk-image)
      - [Building your Kernel](#building-your-kernel)
      - [Disk image](#disk-image)
      - [Booting the guest VM](#booting-the-guest-vm)
- [3. Status](#3-status)
  - [Hot Plug](#hot-plug)
  - [Device Model](#device-model)
  - [Roadmap](#roadmap)
- [4. Relationship with _Rust VMM_ Project](#4-relationship-with-rust-vmm-project)
  - [Differences with Firecracker and crosvm](#differences-with-firecracker-and-crosvm)
- [5. Community](#5-community)
  - [Contribute](#contribute)
  - [Slack](#slack)
  - [Mailing list](#mailing-list)
  - [Security issues](#security-issues)

# 1. What is Cloud Hypervisor?

Cloud Hypervisor is an open source Virtual Machine Monitor (VMM) that runs on
top of the [KVM](https://www.kernel.org/doc/Documentation/virtual/kvm/api.txt)
hypervisor and the Microsoft Hypervisor (MSHV).

The project focuses on running modern, _Cloud Workloads_, on specific, common,
hardware architectures. In this case _Cloud Workloads_ refers to those that are
run by customers inside a Cloud Service Provider. This means modern operating
systems with most I/O handled by
paravirtualised devices (e.g. _virtio_), no requirement for legacy devices, and
64-bit CPUs.

Cloud Hypervisor is implemented in [Rust](https://www.rust-lang.org/) and is
based on the [Rust VMM](https://github.com/rust-vmm) crates.

## Objectives

### High Level

- Runs on KVM or MSHV
- Minimal emulation
- Low latency
- Low memory footprint
- Low complexity
- High performance
- Small attack surface
- 64-bit support only
- CPU, memory, PCI hotplug
- Machine to machine migration

### Architectures

Cloud Hypervisor supports the `x86-64` and `AArch64` architectures. There are
minor differences in functionality between the two architectures
(see [#1125](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/1125)).

### Guest OS

Cloud Hypervisor supports `64-bit Linux` and Windows 10/Windows Server 2019.

# 2. Getting Started

The following sections describe how to build and run Cloud Hypervisor on the
`x86-64` platform. For getting started on the `AArch64` platform, please refer
to the
[AArch64 documentation](docs/arm64.md).

## Host OS

For required KVM functionality the minimum host kernel version is 4.11.  For
adequate performance the minimum recommended host kernel version is 5.6. The
majority of the CI currently tests with kernel version 5.15.

## Use Pre-built Binaries

The recommended approach to getting started with Cloud Hypervisor is by using a
pre-built binary. Binaries are available for the [latest
release](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/latest).
Use `cloud-hypervisor-static` for `x86-64` or `cloud-hypervisor-static-aarch64`
for `AArch64` platform.

## Packages

For convenience, packages are also available targeting some popular Linux
distributions. This is thanks to the [Open Build
Service](https://build.opensuse.org). The [OBS
README](https://github.com/cloud-hypervisor/obs-packaging) explains how to
enable the repository in a supported Linux distribution and install Cloud Hypervisor
and accompanying packages. Please report any packaging issues in the
[obs-packaging](https://github.com/cloud-hypervisor/obs-packaging) repository.

## Building from Source

Please see the [instructions for building from source](docs/building.md) if you
do not wish to use the pre-built binaries.

## Booting Linux

The instructions below are for the `x86-64` platform. For `AArch64` please see
the [AArch64 specific documentation](docs/arm64.md).

Cloud Hypervisor supports direct kernel boot (if the kernel is built with PVH
support) or booting via a firmware (either [Rust Hypervisor
Firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware) or an
edk2 UEFI firmware called `CLOUDHV`.)

Binary builds of the firmware files are available for the latest release of
[Rust Hyperivor
Firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/latest)
and [our edk2
repository](https://github.com/cloud-hypervisor/edk2/releases/latest)

The choice of firmware depends on your guest OS choice; some experimentation
may be required.

### Firmware Booting

Cloud Hypervisor supports booting disk images containing all needed components
to run cloud workloads, a.k.a. cloud images. 

The following sample commands will download an Ubuntu Cloud image, converting
it into a format that Cloud Hypervisor can use and a firmware to boot the image
with.

```shell
$ wget https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img
$ qemu-img convert -p -f qcow2 -O raw focal-server-cloudimg-amd64.img focal-server-cloudimg-amd64.raw
$ wget https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw
```

The Ubuntu cloud images do not ship with a default password so it necessary to
use a `cloud-init` disk image to customise the image on the first boot. A basic
`cloud-init` image is generated by this [script](scripts/create-cloud-init.sh).
This seeds the image with a default username/password of `cloud/cloud123`. It
is only necessary to add this disk image on the first boot.

```shell
$ sudo setcap cap_net_admin+ep ./cloud-hypervisor
$ ./create-cloud-init.sh
$ ./cloud-hypervisor \
	--kernel ./hypervisor-fw \
	--disk path=focal-server-cloudimg-amd64.raw path=/tmp/ubuntu-cloudinit.img \
	--cpus boot=4 \
	--memory size=1024M \
	--net "tap=,mac=,ip=,mask="
```

If access to the firmware messages or interaction with the boot loader (e.g.
GRUB) is required then it necessary to switch to the serial console instead of
`virtio-console`.

```shell
$ ./cloud-hypervisor \
	--kernel ./hypervisor-fw \
	--disk path=focal-server-cloudimg-amd64.raw path=/tmp/ubuntu-cloudinit.img \
	--cpus boot=4 \
	--memory size=1024M \
	--net "tap=,mac=,ip=,mask=" \
	--serial tty \
	--console off
```

### Custom Kernel and Disk Image

#### Building your Kernel

Cloud Hypervisor also supports direct kernel boot into a `vmlinux` ELF kernel (compiled with PVH support). In order to support development there is a custom branch; however provided the required options are enabled any recent kernel will suffice.

To build the kernel:

```shell
# Clone the Cloud Hypervisor Linux branch
$ git clone --depth 1 https://github.com/cloud-hypervisor/linux.git -b ch-5.15.12 linux-cloud-hypervisor
$ pushd linux-cloud-hypervisor
# Use the cloud-hypervisor kernel config to build your kernel
$ wget https://raw.githubusercontent.com/cloud-hypervisor/cloud-hypervisor/main/resources/linux-config-x86_64
$ cp linux-config-x86_64 .config
$ KCFLAGS="-Wa,-mx86-used-note=no" make bzImage -j `nproc`
$ popd
```

The `vmlinux` kernel image will then be located at
`linux-cloud-hypervisor/arch/x86/boot/compressed/vmlinux.bin`.

#### Disk image

For the disk image the same Ubuntu image as before can be used. This contains
an `ext4` root filesystem.

```shell
$ wget https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img
$ qemu-img convert -p -f qcow2 -O raw focal-server-cloudimg-amd64.img focal-server-cloudimg-amd64.raw
```

#### Booting the guest VM

These sample commands boot the disk image using the custom kernel whilst also
supplying the desired kernel command line.

```shell
$ sudo setcap cap_net_admin+ep ./cloud-hypervisor
$ ./create-cloud-init.sh
$ ./cloud-hypervisor \
	--kernel ./linux-cloud-hypervisor/arch/x86/boot/compressed/vmlinux.bin \
	--disk path=focal-server-cloudimg-amd64.raw path=/tmp/ubuntu-cloudinit.img \
	--cmdline "console=hvc0 root=/dev/vda1 rw" \
	--cpus boot=4 \
	--memory size=1024M \
	--net "tap=,mac=,ip=,mask="
```

If earlier kernel messages are required the serial console should be used instead of `virtio-console`.

```cloud-hypervisor \
	--kernel ./linux-cloud-hypervisor/arch/x86/boot/compressed/vmlinux.bin \
	--console off \
	--serial tty \
	--disk path=focal-server-cloudimg-amd64.raw \
	--cmdline "console=ttyS0 root=/dev/vda1 rw" \
	--cpus boot=4 \
	--memory size=1024M \
	--net "tap=,mac=,ip=,mask="
```

# 3. Status

Cloud Hypervisor is under active development. The following stability
guarantees are currently made:

* The API (including command line options) will not be removed or changed in a
  breaking way without a minimum of 2 major releases notice. Where possible
  warnings will be given about the use of deprecated functionality and the
  deprecations will be documented in the release notes.

* Point releases will be made between individual releases where there are
  substantial bug fixes or security issues that need to be fixed. These point
  releases will only include bug fixes.

Currently the following items are **not** guaranteed across updates:

* Snapshot/restore is not supported across different versions
* Live migration is not supported across different versions
* The following features are considered experimental and may change
  substantially between releases: TDX, vfio-user, vDPA.

Further details can be found in the [release documentation](docs/releases.md).

As of 2022-10-13, the following cloud images are supported:

- [Ubuntu Bionic](https://cloud-images.ubuntu.com/bionic/current/) (bionic-server-cloudimg-amd64.img)
- [Ubuntu Focal](https://cloud-images.ubuntu.com/focal/current/) (focal-server-cloudimg-amd64.img)
- [Ubuntu Jammy](https://cloud-images.ubuntu.com/jammy/current/) (jammy-server-cloudimg-amd64.img )
- [Fedora 36](https://fedora.mirrorservice.org/fedora/linux/releases/36/Cloud/x86_64/images/) (Fedora-Cloud-Base-36-1.5.x86_64.raw.xz)

Direct kernel boot to userspace should work with a rootfs from most
distributions although you may need to enable exotic filesystem types in the
reference kernel configuration (e.g. XFS or btrfs.)

## Hot Plug

Cloud Hypervisor supports hotplug of CPUs, passthrough devices (VFIO),
`virtio-{net,block,pmem,fs,vsock}` and memory resizing. This
[document](docs/hotplug.md) details how to add devices to a running VM.

## Device Model

Details of the device model can be found in this
[documentation](docs/device_model.md).

## Roadmap

The project roadmap is tracked through a [GitHub
project](https://github.com/orgs/cloud-hypervisor/projects/6).

# 4. Relationship with _Rust VMM_ Project

In order to satisfy the design goal of having a high-performance,
security-focused hypervisor the decision was made to use the
[Rust](https://www.rust-lang.org/) programming language. The language's strong
focus on memory and thread safety makes it an ideal candidate for implementing
VMMs.

Instead of implementing the VMM components from scratch, Cloud Hypervisor is
importing the [Rust VMM](https://github.com/rust-vmm) crates, and sharing code
and architecture together with other VMMs like e.g. Amazon's
[Firecracker](https://firecracker-microvm.github.io/) and Google's
[crosvm](https://chromium.googlesource.com/chromiumos/platform/crosvm/).

Cloud Hypervisor embraces the _Rust VMM_ project's goals, which is to be able
to share and re-use as many virtualization crates as possible.

## Differences with Firecracker and crosvm

A large part of the Cloud Hypervisor code is based on either the Firecracker or
the crosvm project's implementations. Both of these are VMMs written in Rust
with a focus on safety and security, like Cloud Hypervisor.

The goal of the Cloud Hypervisor project differs from the aforementioned
projects in that it aims to be a general purpose VMM for _Cloud Workloads_ and
not limited to container/serverless or client workloads.

The Cloud Hypervisor community thanks the communities of both the Firecracker
and crosvm projects for their excellent work.

# 5. Community

The Cloud Hypervisor project follows the governance, and community guidelines
described in the [Community](https://github.com/cloud-hypervisor/community)
repository.

## Contribute

The project strongly believes in building a global, diverse and collaborative
community around the Cloud Hypervisor project. Anyone who is interested in
[contributing](CONTRIBUTING.md) to the project is welcome to participate.

Contributing to a open source project like Cloud Hypervisor covers a lot more
than just sending code. Testing, documentation, pull request
reviews, bug reports, feature requests, project improvement suggestions, etc,
are all equal and welcome means of contribution. See the
[CONTRIBUTING](CONTRIBUTING.md) document for more details.

## Slack

Get an [invite to our Slack channel](https://join.slack.com/t/cloud-hypervisor/shared_invite/enQtNjY3MTE3MDkwNDQ4LWQ1MTA1ZDVmODkwMWQ1MTRhYzk4ZGNlN2UwNTI3ZmFlODU0OTcwOWZjMTkwZDExYWE3YjFmNzgzY2FmNDAyMjI)
and [join us on Slack](https://cloud-hypervisor.slack.com/).

## Mailing list

Please report bugs using the [GitHub issue
tracker](https://github.com/cloud-hypervisor/cloud-hypervisor/issues) but for
broader community discussions you may use our [mailing
list](https://lists.cloudhypervisor.org/g/dev/).

## Security issues

Please contact the maintainers listed in the MAINTAINERS.md file with security issues.
