# Memory

Cloud Hypervisor has many ways to expose memory to the guest VM. This document
aims to explain what Cloud Hypervisor is capable of and how it can be used to
meet the needs of very different use cases.

## Basic Parameters

`MemoryConfig` or what is known as `--memory` from the CLI perspective is the
easiest way to get started with Cloud Hypervisor.

```rust
struct MemoryConfig {
    size: u64,
    mergeable: bool,
    hotplug_method: HotplugMethod,
    hotplug_size: Option<u64>,
    hotplugged_size: Option<u64>,
    shared: bool,
    hugepages: bool,
    hugepage_size: Option<u64>,
    prefault: bool,
    thp: bool
    zones: Option<Vec<MemoryZoneConfig>>,
}
```

```
--memory <memory>	Memory parameters "size=<guest_memory_size>,mergeable=on|off,shared=on|off,hugepages=on|off,hugepage_size=<hugepage_size>,hotplug_method=acpi|virtio-mem,hotplug_size=<hotpluggable_memory_size>,hotplugged_size=<hotplugged_memory_size>,prefault=on|off,thp=on|off" [default: size=512M,thp=on]
```

### `size`

Size of the RAM in the guest VM.

This option is mandatory when using the `--memory` parameter.

Value is an unsigned integer of 64 bits.

_Example_

```
--memory size=1G
```

### `mergeable`

Specifies if the pages from the guest RAM must be marked as _mergeable_. In
case this option is `true` or `on`, the pages will be marked with `madvise(2)`
to let the host kernel know which pages are eligible for being merged by the
KSM daemon.

This option can be used when trying to reach a higher density of VMs running
on a single host, as it will reduce the amount of memory consumed by each VM.

By default this option is turned off.

_Example_

```
--memory size=1G,mergeable=on
```

### `hotplug_method`

Selects the way of adding and/or removing memory to/from a booted VM.

Possible values are `acpi` and `virtio-mem`. Default value is `acpi`.

_Example_

```
--memory size=1G,hotplug_method=acpi
```

### `hotplug_size`

Amount of memory that can be dynamically added to the VM.

Value is an unsigned integer of 64 bits. A value of 0 is invalid.

_Example_

```
--memory size=1G,hotplug_size=1G
```

### `hotplugged_size`

Amount of memory that will be dynamically added to the VM at boot. This option
allows for starting a VM with a certain amount of memory that can be reduced
during runtime.

This is only valid when the `hotplug_method` is `virtio-mem` as it does not
make sense for the `acpi` use case. When using ACPI, the memory can't be
resized after it has been extended.

This option is only valid when `hotplug_size` is specified, and its value can't
exceed the value of `hotplug_size`.

Value is an unsigned integer of 64 bits. A value of 0 is invalid.

_Example_

```
--memory size=1G,hotplug_method=virtio-mem,hotplug_size=1G,hotplugged_size=512M
```

### `shared`

Specifies if the memory must be `mmap(2)` with `MAP_SHARED` flag.

By sharing a memory mapping, one can share the guest RAM with other processes
running on the host. One can use this option when running vhost-user devices
as part of the VM device model, as they will be driven by standalone daemons
needing access to the guest RAM content.

By default this option is turned off, which results in performing `mmap(2)`
with `MAP_PRIVATE` flag.

If `hugepages=on` then the value of this field is ignored as huge pages always
requires `MAP_SHARED`.

_Example_

```
--memory size=1G,shared=on
```

### `hugepages` and `hugepage_size`

Specifies if the memory must be created and `mmap(2)` with `MAP_HUGETLB` and size
flags. This performs a memory mapping relying on the specified huge page size.
If no huge page size is supplied the system's default huge page size is used.

By using hugepages, one can improve the overall performance of the VM, assuming
the guest will allocate hugepages as well. Another interesting use case is VFIO
as it speeds up the VM's boot time since the amount of IOMMU mappings are
reduced.

The user is responsible for ensuring there are sufficient huge pages of the
specified size for the VMM to use. Failure to do so may result in strange VMM
behaviour, e.g. error with `ReadKernelImage` is common. If there is a strange
error with `hugepages` enabled, just disable it or check whether there are enough
huge pages.

If `hugepages=on` then the value of `shared` is ignored as huge pages always
requires `MAP_SHARED`.

By default this option is turned off.

_Example_

```
--memory size=1G,hugepages=on,hugepage_size=2M
```

### `prefault`

Specifies if the memory must be `mmap(2)` with `MAP_POPULATE` flag.

By triggering prefault, one can allocate all required physical memory and create
its page tables while calling `mmap`. With physical memory allocated, the number
of page faults will decrease during running, and performance will also improve.

Note that boot of VM will be slower with `prefault` enabled because of allocating
physical memory and creating page tables in advance, and physical memory of the
specified size will be consumed quickly.

This option only takes effect at boot of VM. There is also a `prefault` option in
restore and its choice will overwrite `prefault` in memory.

By default this option is turned off.

_Example_

```
--memory size=1G,prefault=on
```

### `thp`

Specifies if private anonymous memory for the guest (i.e. `shared=off` and no
backing file) should be labelled `MADV_HUGEPAGE` with `madvise(2)` indicating
to the kernel that this memory may be backed with huge pages transparently.

The use of transparent huge pages can improve the performance of the guest as
there will fewer virtualisation related page faults. Unlike using
`hugepages=on` a specific number of huge pages do not need to be allocated by
the kernel.

By default this option is turned on.

_Example_

```
--memory size=1G,thp=on
```

## Advanced Parameters

`MemoryZoneConfig` or what is known as `--memory-zone` from the CLI perspective
is a power user parameter. It allows for a full description of the guest RAM,
describing how every memory region is backed and exposed to the guest.

```rust
struct MemoryZoneConfig {
    id: String,
    size: u64,
    file: Option<PathBuf>,
    shared: bool,
    hugepages: bool,
    hugepage_size: Option<u64>,
    host_numa_node: Option<u32>,
    hotplug_size: Option<u64>,
    hotplugged_size: Option<u64>,
    prefault: bool,
}
```

```
--memory-zone <memory-zone>	User defined memory zone parameters "size=<guest_memory_region_size>,file=<backing_file>,shared=on|off,hugepages=on|off,hugepage_size=<hugepage_size>,host_numa_node=<node_id>,id=<zone_identifier>,hotplug_size=<hotpluggable_memory_size>,hotplugged_size=<hotplugged_memory_size>,prefault=on|off"
```

This parameter expects one or more occurences, allowing for a list of memory
zones to be defined. It must be used with `--memory size=0`, clearly indicating
that the memory will be described through advanced parameters.

Each zone is given a list of options which we detail through the following
sections.

### `id`

Memory zone identifier. This identifier must be unique, otherwise an error will
be returned.

This option is useful when referring to a memory zone previously created. In
particular, the `--numa` parameter can associate a memory zone to a specific
NUMA node based on the memory zone identifier.

This option is mandatory when using the `--memory-zone` parameter.

Value is a string.

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G
```

### `size`

Size of the memory zone.

This option is mandatory when using the `--memory-zone` parameter.

Value is an unsigned integer of 64 bits.

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G
```

### `file`

Path to the file backing the memory zone. This can be either a file or a
directory. In case of a file, it will be opened and used as the backing file
for the `mmap(2)` operation. In case of a directory, a temporary file with no
hard link on the filesystem will be created. This file will be used as the
backing file for the `mmap(2)` operation.

This option can be particularly useful when trying to back a part of the guest
RAM with a well known file. In the context of the snapshot/restore feature, and
if the provided path is a file, the snapshot operation will not perform any
copy of the guest RAM content for this specific memory zone since the user has
access to it and it would duplicate data already stored on the current
filesystem.

Value is a string.

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G,file=/foo/bar
```

### `shared`

Specifies if the memory zone must be `mmap(2)` with `MAP_SHARED` flag.

By sharing a memory zone mapping, one can share part of the guest RAM with
other processes running on the host. One can use this option when running
vhost-user devices as part of the VM device model, as they will be driven
by standalone daemons needing access to the guest RAM content.

If `hugepages=on` then the value of this field is ignored as huge pages always
requires `MAP_SHARED`.

By default this option is turned off, which result in performing `mmap(2)`
with `MAP_PRIVATE` flag.

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G,shared=on
```

### `hugepages` and `hugepage_size`

Specifies if the memory must be created and `mmap(2)` with `MAP_HUGETLB` and size
flags. This performs a memory mapping relying on the specified huge page size.
If no huge page size is supplied the system's default huge page size is used.

By using hugepages, one can improve the overall performance of the VM, assuming
the guest will allocate hugepages as well. Another interesting use case is VFIO
as it speeds up the VM's boot time since the amount of IOMMU mappings are
reduced.

The user is responsible for ensuring there are sufficient huge pages of the
specified size for the VMM to use. Failure to do so may result in strange VMM
behaviour, e.g. error with `ReadKernelImage` is common. If there is a strange
error with `hugepages` enabled, just disable it or check whether there are enough
huge pages.

If `hugepages=on` then the value of `shared` is ignored as huge pages always
requires `MAP_SHARED`.

By default this option is turned off.

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G,hugepages=on,hugepage_size=2M
```

### `host_numa_node`

Node identifier of a node present on the host. This option will let the user
pick a specific NUMA node from which the memory must be allocated. After the
memory zone is `mmap(2)`, the NUMA policy for this memory mapping will be
applied through `mbind(2)`, relying on the provided node identifier. If the
node does not exist on the host, the call to `mbind(2)` will fail.

This option is useful when trying to back a VM memory with a specific type of
memory from the host. Assuming a host has two types of memory, with one slower
than the other, each related to a distinct NUMA node, one could create a VM
with slower memory accesses by backing the entire guest RAM from the furthest
NUMA node on the host.

This option also gives the opportunity to create a VM with non uniform memory
accesses as one could define a first memory zone backed by fast memory, and a
second memory zone backed by slow memory.

Value is an unsigned integer of 32 bits.

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G,host_numa_node=0
```

### `hotplug_size`

Amount of memory that can be dynamically added to the memory zone. Since
`virtio-mem` is the only way of resizing a memory zone, one must specify
the `hotplug_method=virtio-mem` to the `--memory` parameter.

Value is an unsigned integer of 64 bits. A value of 0 is invalid.

_Example_

```
--memory size=0,hotplug_method=virtio-mem
--memory-zone id=mem0,size=1G,hotplug_size=1G
```

### `hotplugged_size`

Amount of memory that will be dynamically added to a memory zone at VM's boot.
This option allows for starting a VM with a certain amount of memory that can
be reduced during runtime.

This is only valid when the `hotplug_method` is `virtio-mem` as it does not
make sense for the `acpi` use case. When using ACPI, the memory can't be
resized after it has been extended.

This option is only valid when `hotplug_size` is specified, and its value can't
exceed the value of `hotplug_size`.

Value is an unsigned integer of 64 bits. A value of 0 is invalid.

_Example_

```
--memory size=0,hotplug_method=virtio-mem
--memory-zone id=mem0,size=1G,hotplug_size=1G,hotplugged_size=512M
```

### `prefault`

Specifies if the memory must be `mmap(2)` with `MAP_POPULATE` flag.

By triggering prefault, one can allocate all required physical memory and create
its page tables while calling `mmap`. With physical memory allocated, the number
of page faults will decrease during running, and performance will also improve.

Note that boot of VM will be slower with `prefault` enabled because of allocating
physical memory and creating page tables in advance, and physical memory of the
specified size will be consumed quickly.

This option only takes effect at boot of VM. There is also a `prefault` option in
restore and its choice will overwrite `prefault` in memory.

By default this option is turned off.

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G,prefault=on
```

## NUMA settings

`NumaConfig` or what is known as `--numa` from the CLI perspective has been
introduced to define a guest NUMA topology. It allows for a fine description
about the CPUs and memory ranges associated with each NUMA node. Additionally
it allows for specifying the distance between each NUMA node.

```rust
struct NumaConfig {
    guest_numa_id: u32,
    cpus: Option<Vec<u8>>,
    distances: Option<Vec<NumaDistance>>,
    memory_zones: Option<Vec<String>>,
    sgx_epc_sections: Option<Vec<String>>,
}
```

```
--numa <numa>	Settings related to a given NUMA node "guest_numa_id=<node_id>,cpus=<cpus_id>,distances=<list_of_distances_to_destination_nodes>,memory_zones=<list_of_memory_zones>,sgx_epc_sections=<list_of_sgx_epc_sections>"
```

### `guest_numa_id`

Node identifier of a guest NUMA node. This identifier must be unique, otherwise
an error will be returned.

This option is mandatory when using the `--numa` parameter.

Value is an unsigned integer of 32 bits.

_Example_

```
--numa guest_numa_id=0
```

### `cpus`

List of virtual CPUs attached to the guest NUMA node identified by the
`guest_numa_id` option. This allows for describing a list of CPUs which
must be seen by the guest as belonging to the NUMA node `guest_numa_id`.

One can use this option for a fine grained description of the NUMA topology
regarding the CPUs associated with it, which might help the guest run more
efficiently.

Multiple values can be provided to define the list. Each value is an unsigned
integer of 8 bits.

For instance, if one needs to attach all CPUs from 0 to 4 to a specific node,
the syntax using `-` will help define a contiguous range with `cpus=0-4`. The
same example could also be described with `cpus=[0,1,2,3,4]`.

A combination of both `-` and `,` separators is useful when one might need to
describe a list containing all CPUs from 0 to 99 and the CPU 255, as it could
simply be described with `cpus=[0-99,255]`.

As soon as one tries to describe a list of values, `[` and `]` must be used to
demarcate the list.

_Example_

```
--cpus boot=8
--numa guest_numa_id=0,cpus=[1-3,7] guest_numa_id=1,cpus=[0,4-6]
```

### `distances`

List of distances between the current NUMA node referred by `guest_numa_id`
and the destination NUMA nodes listed along with distances. This option let
the user choose the distances between guest NUMA nodes. This is important to
provide an accurate description of the way non uniform memory accesses will
perform in the guest.

One or more tuple of two values must be provided through this option. The first
value is an unsigned integer of 32 bits as it represents the destination NUMA
node. The second value is an unsigned integer of 8 bits as it represents the
distance between the current NUMA node and the destination NUMA node. The two
values are separated by `@` (`value1@value2`), meaning the destination NUMA
node `value1` is located at a distance of `value2`. Each tuple is separated
from the others with `,` separator.

As soon as one tries to describe a list of values, `[` and `]` must be used to
demarcate the list.

For instance, if one wants to define 3 NUMA nodes, with each node located at
different distances, it can be described with the following example.

_Example_

```
--numa guest_numa_id=0,distances=[1@15,2@25] guest_numa_id=1,distances=[0@15,2@20] guest_numa_id=2,distances=[0@25,1@20]
```

### `memory_zones`

List of memory zones attached to the guest NUMA node identified by the
`guest_numa_id` option. This allows for describing a list of memory ranges
which must be seen by the guest as belonging to the NUMA node `guest_numa_id`.

This option can be very useful and powerful when combined with `host_numa_node`
option from `--memory-zone` parameter as it allows for creating a VM with non
uniform memory accesses, and let the guest know about it. It allows for
exposing memory zones through different NUMA nodes, which can help the guest
workload run more efficiently.

Multiple values can be provided to define the list. Each value is a string
referring to an existing memory zone identifier. Values are separated from
each other with the `,` separator.

As soon as one tries to describe a list of values, `[` and `]` must be used to
demarcate the list.

Note that a memory zone must belong to a single NUMA node. The following
configuration is incorrect, therefore not allowed:
`--numa guest_numa_id=0,memory_zones=mem0 guest_numa_id=1,memory_zones=mem0`

_Example_

```
--memory size=0
--memory-zone id=mem0,size=1G id=mem1,size=1G id=mem2,size=1G
--numa guest_numa_id=0,memory_zones=[mem0,mem2] guest_numa_id=1,memory_zones=mem1
```

### `sgx_epc_sections`

List of SGX EPC sections attached to the guest NUMA node identified by the
`guest_numa_id` option. This allows for describing a list of SGX EPC sections
which must be seen by the guest as belonging to the NUMA node `guest_numa_id`.

Multiple values can be provided to define the list. Each value is a string
referring to an existing SGX EPC section identifier. Values are separated from
each other with the `,` separator.

As soon as one tries to describe a list of values, `[` and `]` must be used to
demarcate the list.

_Example_

```
--sgx-epc id=epc0,size=32M id=epc1,size=64M id=epc2,size=32M
--numa guest_numa_id=0,sgx_epc_sections=epc1 guest_numa_id=1,sgx_epc_sections=[epc0,epc2]
```

### PCI bus

Cloud Hypervisor supports only one PCI bus, which is why it has been tied to
the NUMA node 0 by default. It is the user responsibility to organize the NUMA
nodes correctly so that vCPUs and guest RAM which should be located on the same
NUMA node as the PCI bus end up on the NUMA node 0.
