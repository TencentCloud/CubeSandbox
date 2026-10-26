# HOWTO VirtioFS rootfs

A quick guide for using virtiofs as a cloud-hypervisor guest's rootfs (i.e.
with no root block device). This document is a quick getting started guide.
There are many more steps to take to make this a production ready, secure
setup.

## Prerequisites

1. virtiofsd from the qemu project
   * We are using the Qemu version for now
   * There is a Rust version being worked on that may be a better option in the future
   * Part of the qemu-system-common package on Ubuntu
   * Part of the qemu-common package on Fedora
2. cloud-hypervisor - the newer the better, but I tested with 0.12
3. a rootfs - This howto uses an alpine rootfs available here:
   * https://dl-cdn.alpinelinux.org/alpine/v3.13/releases/x86_64/alpine-minirootfs-3.13.2-x86_64.tar.gz
   * Others should work

## To create the VM rootfs

```bash
mkdir rootfs/
cd rootfs
# this needs sudo to be able to set root permissions on fs components
sudo tar -xf /path/to/alpine-minirootfs-3.13.1-x86_64.tar.gz
# this will get created when the VM actually boots by the dhcp client
# but we need it in the chroot to download packages
sudo cp /etc/resolv.conf etc/
# the alpine mini rootfs is meant for docker containers, we need a few extra
# things for a working rootfs
sudo chroot $PWD apk add openrc busybox-initscripts
# we are using the paravirt console in cloud-hypervisor, so enable it in init
# append it after the other console since it doesn't work just appending it
sudo sed -i '/vt100/a \n# paravirt console\nhvc0::respawn:/sbin/getty -L hvc0 115200 vt100' etc/inittab
# set no password for root user... you obviously don't want to do this for
# any sort of production setup
sudo sed -i 's/root:!::0:::::/root:::0:::::/' etc/shadow
# set up init scripts
for i in acpid crond
    sudo ln -sf /etc/init.d/$i etc/runlevels/default/$i
end
for i in bootmisc hostname hwclock loadkmap modules networking swap sysctl syslog urandom
    sudo ln -sf /etc/init.d/$i etc/runlevels/boot/$i
end

for i in killprocs mount-ro savecache
    sudo ln -sf /etc/init.d/$i etc/runlevels/shutdown/$i
end

for i in devfs dmesg hwdrivers mdev
    sudo ln -sf /etc/init.d/$i etc/runlevels/sysinit/$i
end
# setup network config
echo 'auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
' | sudo tee etc/network/interfaces

```

## To run the VM

```bash
# starting in the directory above rootfs
sudo virtiofsd --socket-path=$PWD/virtiofs-rootfs.sock --shared-dir=$PWD/rootfs --cache=never &
sudo cloud-hypervisor \
    --cpus boot=1,max=1 \
    --kernel vmlinux \
    --fs tag=/dev/root,socket=$PWD/virtiofs-rootfs.sock \
    --memory size=2G,shared=on \
    --cmdline "console=hvc0 rootfstype=virtiofs root=/dev/root ro debug" \
    --api-socket $PWD/ch.sock \
    --rng \
    --net ...
```

Note: an important part of the above is the `tag=/dev/root` and
`root=/dev/root` parts. For whatever reason, it would only work with that as
the tag.

Note: another important bit is that the memory is shared. This is required for
virtiofs

## Message from the author

If you find any issues or have suggestions, feel free to reach out to @iggy on
the cloud-hypervisor slack. Also if this works for you, I'd like to know as
well. It would also be nice to get steps for preparing other distribution root
filesystems.