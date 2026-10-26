# How to test Vhost-user net with OpenVSwitch/DPDK

The purpose of this document is to illustrate how to test vhost-user-net
in cloud-hypervisor with OVS/DPDK as the backend. This document was
tested with Open vSwitch v2.13.1, DPDK v19.11.3, and Cloud Hypervisor
v15.0 on Ubuntu 20.04.1 (host kernel v5.4.0).

## Framework

It's a simple test to validate the communication between two virtual machine, connecting them to vhost-user ports respectively provided by `OVS/DPDK`.
```
             +----+----------+          +-------------+-----------+-------------+          +----------+----+
             |    |          |          |             |           |             |          |          |    |
             |    |vhost-user|----------| vhost-user  |   ovs     | vhost-user  |----------|vhost-user|    |
             |    |net device|          | port 1      |           | port 2      |          |net device|    |
             |    |          |          |             |           |             |          |          |    |
             |    +----------+          +-------------+-----------+-------------+          +----------+    |
             |               |          |                                       |          |               |
             |vm1            |          |                  dpdk                 |          |           vm2 |
             |               |          |                                       |          |               |
          +--+---------------------------------------------------------------------------------------------+--+
          |  |                                       hugepages                                             |  |
          |  +---------------------------------------------------------------------------------------------+  |
          |                                                                                                   |
          |                                              host                                                 |
          |                                                                                                   |
          +---------------------------------------------------------------------------------------------------+
```
## Prerequisites

Prior to running the test, the following steps need to be performed.
- Enable hugepages
- Install DPDK
- Install OVS

Here is a good reference for setting up OVS with DPDK from scratch:
https://docs.openvswitch.org/en/latest/intro/install/dpdk/.

On Ubuntu systems (18.04 or newer), the OpenVswitch-DPDK package can be
easily installed with:
```bash
sudo apt-get update
sudo apt-get install openvswitch-switch-dpdk
sudo update-alternatives --set ovs-vswitchd /usr/lib/openvswitch-switch-dpdk/ovs-vswitchd-dpdk
```
## Test
The test runs with multiple queue (MQ) support enabled, using 2 pairs of
TX/RX queues defined for both OVS and the virtual machine. Here are the
detailed instructions.

_Setup OVS_

Here is an example how to configure a basic OpenVswitch using DPDK:
```bash
# load the ovs kernel module
modprobe openvswitch
sudo service openvswitch-switch start
ovs-vsctl init
ovs-vsctl set Open_vSwitch . other_config:dpdk-init=true
# run on core 0-3 only
ovs-vsctl set Open_vSwitch . other_config:dpdk-lcore-mask=0xf
# allocate 2G huge pages (to NUMA 0 only)
ovs-vsctl set Open_vSwitch . other_config:dpdk-socket-mem=1024
# run PMD (Pull Mode Driver) threads on core 0-3 only
ovs-vsctl set Open_vSwitch . other_config:pmd-cpu-mask=0xf
sudo service openvswitch-switch restart
# double check the configurations
ovs-vsctl list Open_vSwitch
```

Here is an example how to create a bridge and add two DPDK ports to it
(for later use via Cloud Hypervisor):
```bash
# create a bridge
ovs-vsctl add-br ovsbr0 -- set bridge ovsbr0 datapath_type=netdev
# create two DPDK ports and add them to the bridge
ovs-vsctl add-port ovsbr0 vhost-user1 -- set Interface vhost-user1 type=dpdkvhostuser
ovs-vsctl add-port ovsbr0 vhost-user2 -- set Interface vhost-user2 type=dpdkvhostuser
# set the number of rx queues
ovs-vsctl set Interface vhost-user1 options:n_rxq=2
ovs-vsctl set Interface vhost-user2 options:n_rxq=2
```

_Launch the VMs_

VMs run in client mode. They connect to the socket created by the `dpdkvhostuser` backend.
```bash
# From one terminal. We need to give the cloud-hypervisor binary the NET_ADMIN capabilities for it to set TAP interfaces up on the host.
./cloud-hypervisor \
        --cpus boot=2 \
        --memory size=512M,hugepages=on,shared=true \
        --kernel vmlinux \
        --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
        --disk path=focal-server-cloudimg-amd64.raw   \
        --net mac=52:54:00:02:d9:01,vhost_user=true,socket=/var/run/openvswitch/vhost-user1,num_queues=4

# From another terminal. We need to give the cloud-hypervisor binary the NET_ADMIN capabilities for it to set TAP interfaces up on the host.
./cloud-hypervisor \
        --cpus boot=2 \
        --memory size=512M,hugepages=on,shared=true \
        --kernel vmlinux \
        --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
        --disk path=focal-server-cloudimg-amd64.raw   \
        --net mac=52:54:20:11:C5:02,vhost_user=true,socket=/var/run/openvswitch/vhost-user2,num_queues=4
```

_Setup VM1_
```bash
# From inside the guest
sudo ip addr add 172.100.0.1/24 dev ens2
sudo ip link set up dev ens2
```

_Setup VM2_
```bash
# From inside the guest
sudo ip addr add 172.100.0.2/24 dev ens2
sudo ip link set up dev ens2
```

_Ping VM1 from VM2_
```bash
# From inside the guest
sudo ping 172.100.0.1
```

_Ping VM2 from VM1_
```bash
# From inside the guest
sudo ping 172.100.0.2
```

__Result:__ At this point, VM1 and VM2 can ping each other successfully. We can now run `iperf3` test.

_Run VM1 as server_
```bash
# From inside the guest
iperf3 -s -p 4444
```

_Run VM2 as client_
```bash
# From inside the guest
iperf3 -c 172.100.0.1 -t 30 -p 4444 &
```
