# How to use virtio-fs

In the context of virtualization, it is always convenient to be able to share a
directory from the host with the guest.

__virtio-fs__, also known as __vhost-user-fs__ is a virtual device defined by
the VIRTIO specification which allows any VMM to perform filesystem sharing.

## Pre-requisites

### The daemon

This virtual device relies on the _vhost-user_ protocol, which assumes the
backend (device emulation) is handled by a dedicated process running on the
host. This daemon is called __virtiofsd__ and needs to be present on the host.

_Build virtiofsd_
```bash
git clone https://gitlab.com/virtio-fs/virtiofsd
pushd virtiofsd
cargo build --release
sudo setcap cap_sys_admin+epi target/release/virtiofsd
```

_Create shared directory_
```bash
mkdir /tmp/shared_dir
```
_Run virtiofsd_
```bash
./virtiofsd \
    --log-level debug \
    --socket-path=/tmp/virtiofs \
    --shared-dir=/tmp/shared_dir \
    --cache=never \
    --thread-pool-size=$N
```

The `cache=never` option is the default when using `virtiofsd` with
Cloud Hypervisor. This prevents from using the host page cache, reducing the
overall footprint on host memory. This increases the maximum density of virtual
machines that can be launched on a single host.

The `cache=always` option will allow the host page cache to be used, which can
result in better performance for the guest's workload at the cost of increasing
the footprint on host memory.

The `thread-pool-size` option controls how many IO threads are spawned. For
very fast storage like NVMe spawning enough worker threads is critical to
getting an acceptable performance compared to native.

### Kernel support

Modern Linux kernels (at least v5.10) have support for virtio-fs. Use of older
kernels, with additional patches, are not supported.

## How to share directories with cloud-hypervisor

### Start the VM

Once the daemon is running, the option `--fs` from Cloud Hypervisor needs
to be used.

Both direct kernel boot and EFI firmware can be used to boot a VM with
virtio-fs, given that the cloud image contains a recent enough kernel.

Correct functioning of `--fs` requires `--memory shared=on` to facilitate
interprocess memory sharing.

Assuming you have `focal-server-cloudimg-amd64.raw` and `vmlinux` on your
system, here is the Cloud Hypervisor command you need to run:
```bash
./cloud-hypervisor \
    --cpus boot=1 \
    --memory size=1G,shared=on \
    --disk path=focal-server-cloudimg-amd64.raw \
    --kernel vmlinux \
    --cmdline "console=hvc0 root=/dev/vda1 rw" \
    --fs tag=myfs,socket=/tmp/virtiofs,num_queues=1,queue_size=512
```

### Mount the shared directory

The last step is to mount the shared directory inside the guest, using the
`virtiofs` filesystem type.

```bash
mkdir mount_dir
mount -t virtiofs myfs mount_dir/
```

The `tag` needs to be consistent with what has been provided through the
Cloud Hypervisor command line, which happens to be `myfs` in this example.

## DAX feature

Given the DAX feature is not stable yet from a daemon standpoint, it is not
available in Cloud Hypervisor.
