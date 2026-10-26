# Cloud Hypervisor Hot Plug

Currently Cloud Hypervisor supports hot plugging of CPUs devices (x86 only), PCI devices and memory resizing.

## Kernel support

For hotplug on Cloud Hypervisor ACPI GED support is needed. This can either be achieved by turning on `CONFIG_ACPI_REDUCED_HARDWARE_ONLY` 
or by using this kernel patch (available in 5.5-rc1 and later): https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/patch/drivers/acpi/Makefile?id=ac36d37e943635fc072e9d4f47e40a48fbcdb3f0

## CPU Hot Plug

Extra vCPUs can be added and removed from a running `cloud-hypervisor` instance. This is controlled by two mechanisms:

1. Specifying a number of maximum potential vCPUs that is greater than the number of default (boot) vCPUs.
2. Making a HTTP API request to the VMM to ask for the additional vCPUs to be added.

To use CPU hotplug start the VM with the number of max vCPUs greater than the number of boot vCPUs, e.g.

```shell
$ pushd $CLOUDH
$ sudo setcap cap_net_admin+ep ./cloud-hypervisor/target/release/cloud-hypervisor
$ ./cloud-hypervisor/target/release/cloud-hypervisor \
	--kernel custom-vmlinux.bin \
	--cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
	--disk path=focal-server-cloudimg-amd64.raw \
	--cpus boot=4,max=8 \
	--memory size=1024M \
	--net "tap=,mac=,ip=,mask=" \
	--rng \
	--api-socket=/tmp/ch-socket
$ popd
```

Notice the addition of `--api-socket=/tmp/ch-socket` and a `max` parameter on `--cpus boot=4,max=8`.

To ask the VMM to add additional vCPUs then use the resize API:

```shell
./ch-remote --api-socket=/tmp/ch-socket resize --cpus 8
```

The extra vCPU threads will be created and advertised to the running kernel. The kernel does not bring up the CPUs immediately and instead the user must "online" them from inside the VM:

```shell
root@ch-guest ~ # lscpu | grep list:
On-line CPU(s) list:             0-3
Off-line CPU(s) list:            4-7
root@ch-guest ~ # echo 1 | tee /sys/devices/system/cpu/cpu[4,5,6,7]/online
1
root@ch-guest ~ # lscpu | grep list:
On-line CPU(s) list:             0-7
```

After a reboot the added CPUs will remain.

Removing CPUs works similarly by reducing the number in the "desired_vcpus" field of the reisze API. The CPUs will be automatically offlined inside the guest so there is no need to run any commands inside the guest:

```shell
./ch-remote --api-socket=/tmp/ch-socket resize --cpus 2
```

As per adding CPUs to the guest, after a reboot the VM will be running with the reduced number of vCPUs.

## Memory Hot Plug

### ACPI method

Extra memory can be added from a running `cloud-hypervisor` instance. This is controlled by two mechanisms:

1. Allocating some of the guest physical address space for hotplug memory.
2. Making a HTTP API request to the VMM to ask for a new amount of RAM to be assigned to the VM. In the case of expanding the memory for the VM the new memory will be hotplugged into the running VM, if reducing the size of the memory then change will take effect after the next reboot.

To use memory hotplug start the VM specifying some size RAM in the `hotplug_size` parameter to the memory configuration. Not all the memory specified in this parameter will be available to hotplug as there are spacing and alignment requirements so it is recommended to make it larger than the hotplug RAM needed.

Because the ACPI method is the default, there is no need to add the extra option `hotplug_method=acpi`.

```shell
$ pushd $CLOUDH
$ sudo setcap cap_net_admin+ep ./cloud-hypervisor/target/release/cloud-hypervisor
$ ./cloud-hypervisor/target/release/cloud-hypervisor \
	--kernel custom-vmlinux.bin \
	--cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
	--disk path=focal-server-cloudimg-amd64.raw \
	--cpus boot=4,max=8 \
	--memory size=1024M,hotplug_size=8192M \
	--net "tap=,mac=,ip=,mask=" \
	--rng \
	--api-socket=/tmp/ch-socket
$ popd
```

Before issuing the API request it is necessary to run the following command inside the VM to make it automatically online the added memory:

```shell
root@ch-guest ~ # echo online | sudo tee /sys/devices/system/memory/auto_online_blocks
```

To ask the VMM to expand the RAM for the VM:

```shell
./ch-remote --api-socket=/tmp/ch-socket resize --memory 3G
```

The new memory is now available to use inside the VM:

```shell
free -h
              total        used        free      shared  buff/cache   available
Mem:          3.0Gi        71Mi       2.8Gi       0.0Ki        47Mi       2.8Gi
Swap:          32Mi          0B        32Mi
```

Due to guest OS limitations is is necessary to ensure that amount of memory added (between currently assigned RAM and that which is desired) is a multiple of 128MiB.

The same API can also be used to reduce the desired RAM for a VM but the change will not be applied until the VM is rebooted.

Memory and CPU resizing can be combined together into the same HTTP API request.

### virtio-mem method

Extra memory can be added and removed from a running Cloud Hypervisor instance. This is controlled by two mechanisms:

1. Allocating some of the guest physical address space for hotplug memory.
2. Making a HTTP API request to the VMM to ask for a new amount of RAM to be assigned to the VM.

To use memory hotplug start the VM specifying some size RAM in the `hotplug_size` parameter along with `hotplug_method=virtio-mem` to the memory configuration.

```shell
$ pushd $CLOUDH
$ sudo setcap cap_net_admin+ep ./cloud-hypervisor/target/release/cloud-hypervisor
$ ./cloud-hypervisor/target/release/cloud-hypervisor \
	--kernel custom-vmlinux.bin \
	--cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
	--disk path=focal-server-cloudimg-amd64.raw \
	--memory size=1024M,hotplug_size=8192M,hotplug_method=virtio-mem \
	--net "tap=,mac=,ip=,mask=" \
	--api-socket=/tmp/ch-socket
$ popd
```

To ask the VMM to expand the RAM for the VM (request is in bytes):

```shell
./ch-remote --api-socket=/tmp/ch-socket resize --memory 3G
```

The new memory is now available to use inside the VM:

```shell
free -h
              total        used        free      shared  buff/cache   available
Mem:          3.0Gi        71Mi       2.8Gi       0.0Ki        47Mi       2.8Gi
Swap:          32Mi          0B        32Mi
```

The same API can also be used to reduce the desired RAM for a VM. It is important to note that reducing RAM size might only partially work, as the guest might be using some of it.

## PCI Device Hot Plug

Extra PCI devices can be added and removed from a running `cloud-hypervisor` instance. This is controlled by making a HTTP API request to the VMM to ask for the additional device to be added, or for the existing device to be removed.

Note: On AArch64 platform, PCI device hotplug can only be achieved using ACPI. Please refer to the [documentation](arm64.md#uefi-booting) for more information.

To use PCI device hotplug start the VM with the HTTP server.

```shell
$ sudo setcap cap_net_admin+ep ./cloud-hypervisor/target/release/cloud-hypervisor
$ ./cloud-hypervisor/target/release/cloud-hypervisor \
	--kernel custom-vmlinux.bin \
	--cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
	--disk path=focal-server-cloudimg-amd64.raw \
	--cpus boot=4 \
	--memory size=1024M \
	--net "tap=,mac=,ip=,mask=" \
	--api-socket=/tmp/ch-socket
```

Notice the addition of `--api-socket=/tmp/ch-socket`.

### Add VFIO Device

To ask the VMM to add additional VFIO device then use the `add-device` API.

```shell
./ch-remote --api-socket=/tmp/ch-socket add-device path=/sys/bus/pci/devices/0000:01:00.0/
```

### Add Disk Device

To ask the VMM to add additional disk device then use the `add-disk` API.

```shell
./ch-remote --api-socket=/tmp/ch-socket add-disk path=/foo/bar/cloud.img
```

### Add Fs Device

To ask the VMM to add additional fs device then use the `add-fs` API.

```shell
./ch-remote --api-socket=/tmp/ch-socket add-fs tag=myfs,socket=/foo/bar/virtiofs.sock
```

### Add Net Device

To ask the VMM to add additional network device then use the `add-net` API.

```shell
./ch-remote --api-socket=/tmp/ch-socket add-net tap=chtap0
```

### Add Pmem Device

To ask the VMM to add additional PMEM device then use the `add-pmem` API.

```shell
./ch-remote --api-socket=/tmp/ch-socket add-pmem file=/foo/bar.cloud.img
```

### Add Vsock Device

To ask the VMM to add additional vsock device then use the `add-vsock` API.

```shell
./ch-remote --api-socket=/tmp/ch-socket add-vsock cid=3,socket=/foo/bar/vsock.sock
```

### Common Across All PCI Devices

The extra PCI device will be created and advertised to the running kernel. The new device can be found by checking the list of PCI devices.

```shell
root@ch-guest ~ # lspci
00:00.0 Host bridge: Intel Corporation Device 0d57
00:01.0 Unassigned class [ffff]: Red Hat, Inc. Virtio console (rev 01)
00:02.0 Mass storage controller: Red Hat, Inc. Virtio block device (rev 01)
00:03.0 Unassigned class [ffff]: Red Hat, Inc. Virtio RNG (rev 01)
```

After a reboot the added PCI device will remain.

### Remove PCI device

Removing a PCI device works the same way for all kind of PCI devices. The unique identifier related to the device must be provided. This identifier can be provided by the user when adding the new device, or by default Cloud Hypervisor will assign one.

```shell
./ch-remote --api-socket=/tmp/ch-socket remove-device _disk0
```

As per adding a PCI device to the guest, after a reboot the VM will be running without the removed PCI device.
