# Virtual IOMMU

## Rationales

Having the possibility to expose a virtual IOMMU to the guest can be
interesting to support specific use cases. That being said, it is always
important to keep in mind a virtual IOMMU can impact the performance of the
attached devices, which is the reason why one should be careful when enabling
this feature.

### Protect nested virtual machines

The first reason why one might want to expose a virtual IOMMU to the guest is
to increase the security regarding the memory accesses performed by the virtual
devices (VIRTIO devices), on behalf of the guest drivers.

With a virtual IOMMU, the VMM stands between the guest driver and its device
counterpart, validating and translating every address before to try accessing
the guest memory. This is standard interposition that is performed here by the
VMM.

The increased security does not apply for a simple case where we have one VM
per VMM. Because the guest cannot be trusted, as we always consider it could
be malicious and gain unauthorized privileges inside the VM, preventing some
devices from accessing the entire guest memory is pointless.

But let's take the interesting case of nested virtualization, and let's assume
we have a VMM running a first layer VM. This L1 guest is fully trusted as the
user intends to run multiple VMs from this L1. We can end up with multiple L2
VMs running on a single L1 VM. In this particular case, and without exposing a
virtual IOMMU to the L1 guest, it would be possible for any L2 guest to use the
device implementation from the host VMM to access the entire guest L1 memory.
The virtual IOMMU prevents from this kind of trouble as it will validate the
addresses the device is authorized to access.

### Achieve VFIO nested

Another reason for having a virtual IOMMU is to allow passing physical devices
from the host through multiple layers of virtualization. Let's take as example
a system with a physical IOMMU running a VM with a virtual IOMMU. The
implementation of the virtual IOMMU is responsible for updating the physical
DMA Remapping table (DMAR) everytime the DMA mapping changes. This must happen
through the VFIO framework on the host as this is the only userspace interface
to interact with a physical IOMMU.

Relying on this update mechanism, it is possible to attach physical devices to
the virtual IOMMU, which allows these devices to be passed from L1 to another
layer of virtualization.

## Why virtio-iommu?

The Cloud Hypervisor project decided to implement the brand new virtio-iommu
device in order to provide a virtual IOMMU to its users. The reason being the
simplicity brought by the paravirtualization solution. By having one side
handled from the guest itself, it removes the complexity of trapping memory
page accesses and shadowing them. This is why the project will not try to
implement a full emulation of a physical IOMMU.

## Pre-requisites

### Kernel

Since virtio-iommu has landed partially into the version 5.3 of the Linux
kernel, a special branch is needed to get things working with Cloud Hypervisor.
By partially, we are talking about x86 specifically, as it is already fully
functional for ARM architectures.

## Usage

In order to expose a virtual IOMMU to the guest, it is required to create a
virtio-iommu device and expose it through the ACPI IORT table. This can be
simply achieved by attaching at least one device to the virtual IOMMU.

The way to expose to the guest a specific device as sitting behind this IOMMU
is to explicitly tag it from the command line with the option `iommu=on`.

Not all devices support this extra option, and the default value will always
be `off` since we want to avoid the performance impact for most users who don't
need this.

Refer to the command line `--help` to find out which device support to be
attached to the virtual IOMMU.

Below is a simple example exposing the `virtio-blk` device as attached to the
virtual IOMMU:

```bash
./cloud-hypervisor \
    --cpus boot=1 \
    --memory size=512M \
    --disk path=focal-server-cloudimg-amd64.raw,iommu=on \
    --kernel custom-vmlinux \
    --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
```

From a guest perspective, it is easy to verify if the device is protected by
the virtual IOMMU. Check the directories listed under
`/sys/kernel/iommu_groups`:

```bash
ls /sys/kernel/iommu_groups
0
```

In this case, only one IOMMU group should be created. Under this group, it is
possible to find out the b/d/f of the device(s) part of this group.

```bash
ls /sys/kernel/iommu_groups/0/devices/
0000:00:03.0
```

And you can validate the device is the one we expect running `lspci`:

```bash
lspci
00:00.0 Host bridge: Intel Corporation Device 0d57
00:01.0 Unassigned class [ffff]: Red Hat, Inc. Device 1057
00:02.0 Unassigned class [ffff]: Red Hat, Inc. Virtio console
00:03.0 Mass storage controller: Red Hat, Inc. Virtio block device
00:04.0 Unassigned class [ffff]: Red Hat, Inc. Virtio RNG
```

### Work with FDT on AArch64

On AArch64 architecture, the virtual IOMMU can still be used even if ACPI is not
enabled. But the effect is different with what the aforementioned test showed.

When ACPI is disabled, virtual IOMMU is supported through Flattened Device Tree
(FDT). In this case, the guest kernel can not tell which device should be
IOMMU-attached and which should not. No matter how many devices you attached to
the virtual IOMMU by setting `iommu=on` option, all the devices on the PCI bus
will be attached to the virtual IOMMU (except the IOMMU itself). Each of the
devices will be added into a IOMMU group.

As a result, the directory content of `/sys/kernel/iommu_groups` would be:

```bash
ls /sys/kernel/iommu_groups/0/devices/
0000:00:02.0
ls /sys/kernel/iommu_groups/1/devices/
0000:00:03.0
ls /sys/kernel/iommu_groups/2/devices/
0000:00:04.0
```

## Faster mappings

By default, the guest memory is mapped with 4k pages and no huge pages, which
causes the virtual IOMMU device to be asked for 4k mappings only. This
configuration slows down the setup of the physical IOMMU as an important number
of requests need to be issued in order to create large mappings.

One use case is even more impacted by the slowdown, the nested VFIO case. When
passing a device through a L2 guest, the VFIO driver running in L1 will update
the DMAR entries for the specific device. Because VFIO pins the entire guest
memory, this means the entire mapping of the L2 guest need to be stored into
multiple 4k mappings. Obviously, the bigger the L2 guest RAM is, the longer the
update of the mappings will last. There is an additional problem happening in
this case, if the L2 guest RAM is quite large, it will require a large number
of mappings, which might exceed the VFIO limit set on the host. The default
value is 65536, which can simply be reached with a 256MiB sized RAM.

The way to solve both problems, the slowdown and the limit being exceeded, is
to reduce the amount of requests to describe those same large mappings. This
can be achieved by using 2MiB pages, known as huge pages. By seeing the guest
RAM as larger pages, and because the virtual IOMMU device supports it, the
guest will require less mappings, which will prevent the limit from being
exceeded, but also will take less time to process them on the host. That's
how using huge pages as much as possible can speed up VM boot time.

### Basic usage

Let's look at an example of how to run a guest with huge pages.

First, make sure your system has enough pages to cover the entire guest RAM:
```bash
# This example creates 4096 hugepages
echo 4096 > /proc/sys/vm/nr_hugepages
```

Next step is simply to create the VM. Two things are important, first we want
the VM RAM to be mapped on huge pages by backing it with `/dev/hugepages`. And
second thing, we need to create some huge pages in the guest itself so they can
be consumed.

```bash
./cloud-hypervisor \
    --cpus boot=1 \
    --memory size=8G,hugepages=on \
    --disk path=focal-server-cloudimg-amd64.raw \
    --kernel custom-vmlinux \
    --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw hugepagesz=2M hugepages=2048" \
    --net tap=,mac=,iommu=on
```

### Nested usage

Let's now look at the specific example of nested virtualization. In order to
reach optimized performances, the L2 guest also need to be mapped based on
huge pages. Here is how to achieve this, assuming the physical device you are
passing through is `0000:00:01.0`.

```bash
./cloud-hypervisor \
    --cpus boot=1 \
    --memory size=8G,hugepages=on \
    --disk path=focal-server-cloudimg-amd64.raw \
    --kernel custom-vmlinux \
    --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw kvm-intel.nested=1 vfio_iommu_type1.allow_unsafe_interrupts rw hugepagesz=2M hugepages=2048" \
    --device path=/sys/bus/pci/devices/0000:00:01.0,iommu=on
```

Once the L1 VM is running, unbind the device from the default driver in the
guest, and bind it to VFIO (it should appear as `0000:00:04.0`).

```bash
echo 0000:00:04.0 > /sys/bus/pci/devices/0000\:00\:04.0/driver/unbind
echo 8086 1502 > /sys/bus/pci/drivers/vfio-pci/new_id
echo 0000:00:04.0 > /sys/bus/pci/drivers/vfio-pci/bind
```

Last thing is to start the L2 guest with the huge pages memory backend.

```bash
./cloud-hypervisor \
    --cpus boot=1 \
    --memory size=4G,hugepages=on \
    --disk path=focal-server-cloudimg-amd64.raw \
    --kernel custom-vmlinux \
    --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
    --device path=/sys/bus/pci/devices/0000:00:04.0
```

### Dedicated IOMMU PCI segments

To facilitate hotplug of devices that require being behind an IOMMU it is
possible to mark entire PCI segments as behind the IOMMU.

This is accomplished through `--platform
num_pci_segments=<number_of_segments>,iommu_segments=<range of segments>` or
via the equivalents in `PlatformConfig` for the API.

e.g.

```bash
./cloud-hypervisor \
    --api-socket=/tmp/api \
    --cpus boot=1 \
    --memory size=4G,hugepages=on \
    --disk path=focal-server-cloudimg-amd64.raw \
    --kernel custom-vmlinux \
    --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
    --platform num_pci_segments=2,iommu_segments=1
```

This adds a second PCI segment to the platform behind the IOMMU. A VFIO device
requiring the IOMMU then may be hotplugged:

e.g.

```bash
./ch-remote --api-socket=/tmp/api add-device path=/sys/bus/pci/devices/0000:00:04.0,iommu=on,pci_segment=1
```

Devices that cannot be placed behind an IOMMU (e.g. lacking an `iommu=` option)
cannot be placed on the IOMMU segments.

