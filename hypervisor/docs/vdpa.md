# Virtio Data Path Acceleration

vDPA aims at achieving bare-metal performance for devices passed into a virtual
machine. It is an alternative to VFIO, as it provides a simpler solution for
achieving migration.

It is a kernel framework introduced recently to handle devices complying with
the VIRTIO specification on their data-path, while the control path is vendor
specific. In practice, virtqueues are accessed directly through DMA mechanism
between the hardware and the guest. The control path is accessed through the
vDPA framework, being exposed through the vhost interface as a vhost-vdpa
device.

Because DMA accesses between device and guest are going through virtqueues,
migration can be achieved without requiring device's driver to implement any
specific migration support. In case of VFIO, each vendor is expected to provide
an implementation of the VFIO migration framework, complicating things as it
must be done for each and every device's driver.

The official [website](https://vdpa-dev.gitlab.io/) contains some extensive
documentation on the topic.

## Usage

`VdpaConfig` (known as `--vdpa` from the CLI perspective) contains the list of
parameters available for the vDPA device.

```rust
struct VdpaConfig {
    path: PathBuf,
    num_queues: usize,
    id: Option<String>,
    pci_segment: u16,
}
```

```
--vdpa <vdpa>	vDPA device "path=<device_path>,num_queues=<number_of_queues>,iommu=on|off,id=<device_id>,pci_segment=<segment_id>"
```

### `path`

Path of the vDPA device. Usually `/dev/vhost-vdpa-X`.

This parameter is mandatory.

Value is a string.

_Example_

```
--vdpa path=/dev/vhost-vdpa-0
```

### `num_queues`

Number of virtqueues supported by the vDPA device.

This parameter is optional.

Value is an unsigned integer set to `1` by default.

_Example_

```
--vdpa path=/dev/vhost-vdpa-0,num_queues=2
```

### `id`

Identifier of the vDPA device.

This parameter is optional. If provided, it must be unique across the entire
virtual machine.

Value is a string.

_Example_

```
--vdpa path=/dev/vhost-vdpa-0,id=vdpa0
```

### `pci_segment`

PCI segment number to which the vDPA device should be attached to.

This parameter is optional.

Value is an unsigned integer of 16 bits set to `0` by default.

_Example_

```
--vdpa path=/dev/vhost-vdpa-0,pci_segment=1
```

## Example with vDPA block simulator

The vDPA framework provides a simulator with both `virtio-block` and
`virtio-net` implementations. This is very useful for testing vDPA when we
don't have access to the specific hardware.

Given the host kernel has the appropriate modules available, let's load them
all:

```
sudo modprobe vdpa
sudo modprobe vhost_vdpa
sudo modprobe vdpa_sim
sudo modprobe vdpa_sim_blk
```

Given you have the `iproute2/vdpa` tool installed, let's now create the
`virtio-block` vDPA device:

```sh
sudo vdpa dev add name vdpa-blk1 mgmtdev vdpasim_blk
sudo chown $USER:$USER /dev/vhost-vdpa-0
sudo chmod 660 /dev/vhost-vdpa-0
```

Increase the maximum locked memory to ensure setting up IOMMU mappings will
succeed:

```sh
ulimit -l unlimited
```

Start Cloud Hypervisor:

```sh
cloud-hypervisor \
    --cpus boot=1 \
    --memory size=1G,hugepages=on \
    --disk path=focal-server-cloudimg-amd64.raw \
    --kernel vmlinux \
    --cmdline "root=/dev/vda1 console=hvc0" \
    --vdpa path=/dev/vhost-vdpa-0,num_queues=1
```

The `virtio-block` device backed by the vDPA simulator can be found as
`/dev/vdb` in the guest:

```
cloud@cloud:~$ lsblk
NAME    MAJ:MIN RM  SIZE RO TYPE MOUNTPOINT
nullb0  252:0    0  250G  0 disk 
vda     254:0    0  2.2G  0 disk 
├─vda1  254:1    0  2.1G  0 part /
├─vda14 254:14   0    4M  0 part 
└─vda15 254:15   0  106M  0 part /boot/efi
vdb     254:16   0  128M  0 disk
```
