# Live Migration

This document gives two examples of how to use the live migration
support in Cloud Hypervisor:

1. local migration - migrating between two VMs running on the same
   machine;
1. nested-vm migration - migrating between two nested VMs whose host VMs
   are running on the same machine.

## Local Migration (Suitable for Live Upgrade of VMM)
Launch the source VM (on the host machine):
```bash
$ target/release/cloud-hypervisor
    --kernel ~/workloads/vmlinux \
    --disk path=~/workloads/focal.raw \
    --cpus boot=1 --memory size=1G,shared=on \
    --cmdline "root=/dev/vda1 console=ttyS0"  \
    --serial tty --console off --api-socket=/tmp/api1
```

Launch the destination VM from the same directory (on the host machine):
```bash
$ target/release/cloud-hypervisor --api-socket=/tmp/api2
```

Get ready for receiving migration for the destination VM (on the host machine):
```bash
$ target/release/ch-remote --api-socket=/tmp/api2 receive-migration unix:/tmp/sock
```

Start to send migration for the source VM (on the host machine):
```bash
$ target/release/ch-remote --api-socket=/tmp/api1 send-migration --local unix:/tmp/sock
```

When the above commands completed, the source VM should be successfully
migrated to the destination VM. Now the destination VM is running while
the source VM is terminated gracefully.

## Nested-VM Migration

Launch VM 1 (on the host machine) with an extra virtio-blk device for
exposing a guest image for the nested source VM:
> Note: the example below also attached an additional virtio-blk device
> with a dummy image for testing purpose (which is optional).
```bash
$ head -c 1M < /dev/urandom > tmp.img # create a dummy image for testing
$ sudo /target/release/cloud-hypervisor \
        --serial tty --console off \
        --cpus boot=1 --memory size=512M \
        --kernel vmlinux \
        --cmdline "root=/dev/vda1 console=ttyS0"  \
        --disk path=focal-1.raw path=focal-nested.raw path=tmp.img\
        --net ip=192.168.101.1
```

Launch VM 2 (on the host machine) with an extra virtio-blk device for
exposing the same guest image for the nested destination VM:
```bash
$ sudo /target/release/cloud-hypervisor \
        --serial tty --console off \
        --cpus boot=1 --memory size=512M \
        --kernel vmlinux \
        --cmdline "root=/dev/vda1 console=ttyS0"  \
        --disk path=focal-2.raw path=focal-nested.raw path=tmp.img\
        --net ip=192.168.102.1
```

Launch the nested source VM (inside the guest OS of the VM 1) :
```bash
vm-1:~$ sudo ./cloud-hypervisor \
        --serial tty --console off \
        --memory size=128M \
        --kernel vmlinux \
        --cmdline "console=ttyS0 root=/dev/vda1" \
        --disk path=/dev/vdb path=/dev/vdc \
        --api-socket=/tmp/api1 \
        --net ip=192.168.100.1
vm-1:~$ # setup the guest network if needed
vm-1:~$ sudo ip addr add 192.168.101.2/24 dev ens4
vm-1:~$ sudo ip link set up dev ens4
vm-1:~$ sudo ip r add default via 192.168.101.1
```
Optional: Run the guest workload below (on the guest OS of the nested source VM),
which performs intensive virtio-blk operations. Now the console of the nested
source VM should repeatedly print `"equal"`, and our goal is migrating
this VM and the running workload without interruption.
```bash
#/bin/bash

# On the guest OS of the nested source VM

input="/dev/vdb"
result=$(md5sum $input)
tmp=$(md5sum $input)

while  [[ "$result" == "$tmp" ]]
do
    echo "equal"
    tmp=$(md5sum $input)
done

echo "not equal"
echo "result = $result"
echo "tmp = $tmp"
```

Launch the nested destination VM (inside the guest OS of the VM 2):
```bash
vm-2:~$ sudo ./cloud-hypervisor --api-socket=/tmp/api2
vm-2:~$ # setup the guest network with the following commands if needed
vm-2:~$ sudo ip addr add 192.168.102.2/24 dev ens4
vm-2:~$ sudo ip link set up dev ens4
vm-2:~$ sudo ip r add default via 192.168.102.1
vm-2:~$ ping 192.168.101.2 # This should succeed
```
> Note: If the above ping failed, please check the iptables rule on the
> host machine, e.g. whether the policy for the `FORWARD` chain is set
> to `DROP` (which is the default setting configured by Docker).

Get ready for receiving migration for the nested destination VM (inside
the guest OS of the VM 2):
```bash
vm-2:~$ sudo ./ch-remote --api-socket=/tmp/api2 receive-migration unix:/tmp/sock2
vm-2:~$ sudo socat TCP-LISTEN:6000,reuseaddr UNIX-CLIENT:/tmp/sock2
```

Start to send migration for the nested source VM (inside the guest OS of
the VM 1):
```bash
vm-1:~$ sudo socat UNIX-LISTEN:/tmp/sock1,reuseaddr TCP:192.168.102.2:6000
vm-1:~$ sudo ./ch-remote --api-socket=/tmp/api1 send-migration unix:/tmp/sock1
```

When the above commands completed, the source VM should be successfully
migrated to the destination VM without interrupting our testing guest
workload. Now the destination VM is running the testing guest workload
while the source VM is terminated gracefully.
