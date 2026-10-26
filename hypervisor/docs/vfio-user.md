# Cloud Hypervisor VFIO-user HOWTO

VFIO-user is an *experimental* protocol for allowing devices to be implemented in another process and communicate over a socket; ie.e VFIO-user is to VFIO as virtio is to vhost-user.

The protocol is documented here: https://github.com/nutanix/libvfio-user/blob/master/docs/vfio-user.rst

The Cloud Hypervisor support for such devices is *experimental*. Not all Cloud Hypervisor functionality is supported in particular: virtio-mem and iommu are not supported.

## Usage

The `--user-device socket=<path>` parameter is used to create a vfio-user device when creating the VM specifying the socket to connect to. The device can also be hotplugged with `ch-remote add-user-device socket=<path>`.

## Example (GPIO device)

There is a simple GPIO device included in the libvfio-user repository: https://github.com/nutanix/libvfio-user#gpio

Run the example from the libvfio-user repository:

```sh
rm /tmp/vfio-user.sock
./build/dbg/samples/gpio-pci-idio-16 -v /tmp/vfio-user.sock &
```

Start Cloud Hypervisor:

```sh
target/debug/cloud-hypervisor \
    --memory size=1G,shared=on \
    --disk path=~/images/focal-server-cloudimg-amd64.raw \
    --kernel ~/src/linux/vmlinux \
    --cmdline "root=/dev/vda1 console=hvc0" \
    --user-device socket=/tmp/vfio-user.sock 
```

Inside the VM you can test the device with:

```sh
cat /sys/class/gpio/gpiochip480/base > /sys/class/gpio/export
for ((i=0;i<12;i++)); do cat /sys/class/gpio/OUT0/value; done
```

## Example (NVMe device)

Use SPDK: https://github.com/spdk/spdk

Compile with `./configure --with-vfio-user`

Create an NVMe controller listening on a vfio-user socket with a simple AIO block device in spdk.
More details of configuring SPDK bdev can be viewed in [SPDK bdev](https://spdk.io/doc/bdev.html).
More details of setting SPDK NVMe-oF target can be viewed in [SDPK NVMe-oF tgt](https://spdk.io/doc/nvmf.html).

```sh
sudo scripts/setup.sh
rm ~/images/test-disk.raw
truncate ~/images/test-disk.raw -s 128M
mkfs.ext4  ~/images/test-disk.raw
sudo killall ./build/bin/nvmf_tgt
sudo ./build/bin/nvmf_tgt -i 0 -e 0xFFFF -m 0x1 &
sleep 2
sudo ./scripts/rpc.py nvmf_create_transport -t VFIOUSER
sudo rm -rf /tmp/nvme-vfio-user
sudo mkdir -p /tmp/nvme-vfio-user
sudo ./scripts/rpc.py bdev_aio_create ~/images/test-disk.raw test 512
sudo ./scripts/rpc.py nvmf_create_subsystem nqn.2019-07.io.spdk:cnode -a -s test
sudo ./scripts/rpc.py nvmf_subsystem_add_ns nqn.2019-07.io.spdk:cnode test
sudo ./scripts/rpc.py nvmf_subsystem_add_listener nqn.2019-07.io.spdk:cnode -t VFIOUSER -a /tmp/nvme-vfio-user -s 0
sudo chown $USER.$USER -R /tmp/nvme-vfio-user
```

Start Cloud Hypervisor:

```sh
target/debug/cloud-hypervisor \
    --memory size=1G,shared=on \
    --disk path=~/images/focal-server-cloudimg-amd64.raw \
    --kernel ~/src/linux/vmlinux \
    --cmdline "root=/dev/vda1 console=hvc0" \
    --user-device socket=/tmp/nvme-vfio-user/cntrl 
```
