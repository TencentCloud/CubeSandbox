# How to create a custom Ubuntu image

In the context of adding more utilities to the Ubuntu cloud image being used
for integration testing, this quick guide details how to achieve the proper
modification of an official Ubuntu cloud image.

## Create the image

Let's go through the steps on how to extend an official Ubuntu image. These
steps can be applied to other distributions (with a few changes regarding
package management).

### Get latest Ubuntu cloud image

```bash
wget https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img
```

### Check the file format is QCOW2

```bash
file focal-server-cloudimg-amd64.img
focal-server-cloudimg-amd64.img: QEMU QCOW2 Image (v2), 2361393152 bytes
```

### Convert QCOW2 into RAW

```bash
qemu-img convert -p -f qcow2 -O raw focal-server-cloudimg-amd64.img focal-server-cloudimg-amd64.raw
```

### Identify the Linux partition

The goal is to mount the image rootfs so that it can be modified as needed.
That's why we need to identify where the Linux filesystem partition is located
in the image.

```bash
sudo fdisk -l focal-server-cloudimg-amd64.raw
Disk focal-server-cloudimg-amd64.raw: 2.2 GiB, 2361393152 bytes, 4612096 sectors
Units: sectors of 1 * 512 = 512 bytes
Sector size (logical/physical): 512 bytes / 512 bytes
I/O size (minimum/optimal): 512 bytes / 512 bytes
Disklabel type: gpt
Disk identifier: A1171ABA-2BEA-4218-A467-1B2B607E5953

Device                             Start     End Sectors  Size Type
focal-server-cloudimg-amd64.raw1  227328 4612062 4384735  2.1G Linux filesystem
focal-server-cloudimg-amd64.raw14   2048   10239    8192    4M BIOS boot
focal-server-cloudimg-amd64.raw15  10240  227327  217088  106M EFI System

Partition table entries are not in disk order.
```

### Mount the Linux partition

```bash
mkdir -p /mnt
sudo mount -o loop,offset=$((227328 * 512)) focal-server-cloudimg-amd64.raw /mnt
```

### Set up DNS

The next step describes changing the root directory to the rootfs contained by 
the cloud image. For DNS to work in the root directory, you will need to first bind-mount 
the host `/etc/resolv.conf` onto the mounted linux partition of the cloud image.

```bash
sudo mount -o bind /etc/resolv.conf /mnt/etc/resolv.conf
```

### Change root directory

Changing the root directory will allow us to install new packages to the rootfs
contained by the cloud image.

```bash
sudo chroot /mnt
mount -t proc proc /proc
mount -t devpts devpts /dev/pts
```

### Install needed packages

In the context Cloud Hypervisor's integration tests, we need several utilities.
Here is the way to install them for a Ubuntu image. This step is specific to
Ubuntu distributions.

```bash
apt update
apt install fio iperf iperf3 socat stress
```

### Remove counterproductive packages

* snapd:

This prevents snapd from trying to mount squashfs filesystem when the kernel
might not support it. This might be the case when the image is used with direct
kernel boot. This step is specific to Ubuntu distributions.

* pollinate:

Remove this package which can fail and lead to the SSH daemon failing to start.
See #2113 for details.

```bash
apt remove --purge snapd pollinate
```


### Cleanup the image

Leave no trace in the image before unmounting its content.

```bash
umount /dev/pts
umount /proc
history -c
exit
umount /mnt/etc/resolv.conf
umount /mnt
```

### Rename the image

Renaming is important to identify this is a modified image.

```bash
mv focal-server-cloudimg-amd64.raw focal-server-cloudimg-amd64-custom-$(date "+%Y%m%d")-0.raw
```

The `-0` is the revision and is only necessary to change if multiple images are
updated on the same day.

### Create QCOW2 from RAW

Last step is to create the QCOW2 image back from the modified image.

```bash
qemu-img convert -p -f raw -O qcow2 focal-server-cloudimg-amd64-custom-$(date "+%Y%m%d")-0.raw focal-server-cloudimg-amd64-custom-$(date "+%Y%m%d")-0.qcow2
```

## Switch CI to use the new image

### Upload to Azure storage

The next step is to update both images (QCOW2 and RAW) stored as part of the
Azure storage account, replacing them with the newly created ones. This will
make these new images available from the integration tests. This is usually
achieved through the web interface.

### Update integration tests

Last step is about updating the integration tests to work with this new image.
The key point is to identify where the Linux filesystem partition is located,
as we might need to update the direct kernel boot command line, replacing
`/dev/vda1` with the appropriate partition number.

Update all references to the previous image name to the new one.
