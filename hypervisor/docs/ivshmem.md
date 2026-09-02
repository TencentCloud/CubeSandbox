# Inter-VM shared memory device

The Inter-VM shared memory device (ivshmem) is designed to share a memory
region between a guest and the host. In order for all guests to be able to
pick up the shared memory area, it is modeled as a PCI device exposing said
memory to the guest as a PCI BAR.

Device Specification is
at https://www.qemu.org/docs/master/specs/ivshmem-spec.html.

Now we support setting a backend file to share data between host and guest.
In other words, we only support ivshmem-plain and ivshmem-doorbell is not
supported yet.

## Usage

`--ivshmem`, an optional argument, can be passed to enable ivshmem device.
This argument takes a file as a `path` value and a file size as a `size` value.

```
--ivshmem <ivshmem>  device backend file "path=</path/to/a/file>,size=<file_size/must=2^n>";
```

## Example

Create a file with a size bigger than passed to `cloud-hypervisor`:

```
truncate -s 1M /tmp/ivshmem.data
```

Start application to mmap the file data to a memory region:

```
./cloud-hypervisor \
    --api-socket /tmp/cloud-hypervisor.sock \
    --kernel vmlinux \
    --disk path=focal-server-cloudimg-amd64.raw \
    --cpus boot=4 \
    --memory size=1024M \
    --ivshmem path=/tmp/ivshmem.data,size=1M
```

Insmod a ivshmem device driver to enable the device. The file data will be
mmapped to the PCI `bar2` of ivshmem device,
guest can r/w data by accessing this memory.

A simple example of ivshmem driver can get from:
https://github.com/lisongqian/clh-linux/commits/ch-6.12.8-ivshmem

The host process can r/w this data by remmaping the `/tmp/ivshmem.data`.
