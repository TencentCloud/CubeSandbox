# CPU

Cloud Hypervisor has many options when it comes to the creation of virtual
CPUs. This document aims to explain what Cloud Hypervisor is capable of and
how it can be used to meet the needs of very different use cases.

## Options

`CpusConfig` or what is known as `--cpus` from the CLI perspective is the way
to set vCPUs options for Cloud Hypervisor.

```rust
struct CpusConfig {
    boot_vcpus: u8,
    max_vcpus: u8,
    topology: Option<CpuTopology>,
    kvm_hyperv: bool,
    max_phys_bits: u8,
    affinity: Option<Vec<CpuAffinity>>,
    features: CpuFeatures,
}
```

```
--cpus boot=<boot_vcpus>,max=<max_vcpus>,topology=<threads_per_core>:<cores_per_die>:<dies_per_package>:<packages>,kvm_hyperv=on|off,max_phys_bits=<maximum_number_of_physical_bits>,affinity=<list_of_vcpus_with_their_associated_cpuset>,features=<list_of_features_to_enable>
```

### `boot`

Number of vCPUs present at boot time.

This option allows to define a specific number of vCPUs to be present at the
time the VM is started. This option is mandatory when using the `--cpus`
parameter. If `--cpus` is not specified, this option takes the default value
of `1`, starting the VM with a single vCPU.

Value is an unsigned integer of 8 bits.

_Example_

```
--cpus boot=2
```

### `max`

Maximum number of vCPUs.

This option defines the maximum number of vCPUs that can be assigned to the VM.
In particular, this option is used when looking for CPU hotplug as it lets the
provide an indication about how many vCPUs might be needed later during the
runtime of the VM.
For instance, if booting the VM with 2 vCPUs and a maximum of 6 vCPUs, it means
up to 4 vCPUs can be added later at runtime by resizing the VM.

The value must be greater than or equal to the number of boot vCPUs.
The value is an unsigned integer of 8 bits.

By default this option takes the value of `boot`, meaning vCPU hotplug is not
expected and can't be performed.

_Example_

```
--cpus max=3
```

### `topology`

Topology of the guest platform.

This option gives the user a way to describe the exact topology that should be
exposed to the guest. It can be useful to describe to the guest the same
topology found on the host as it allows for proper usage of the resources and
is a way to achieve better performances.

The topology is described through the following structure:

```rust
struct CpuTopology {
    threads_per_core: u8,
    cores_per_die: u8,
    dies_per_package: u8,
    packages: u8,
}
```

or the following syntax through the CLI:

```
topology=<threads_per_core>:<cores_per_die>:<dies_per_package>:<packages>
```

By default the topology will be `1:1:1:1`.

_Example_

```
--cpus boot=2,topology=1:1:2:1
```

### `kvm_hyperv`

Enable KVM Hyper-V emulation.

When turned on, this option relies on KVM to emulate the synthetic interrupt
controller (SynIC) along with synthetic timers expected by a Windows guest.
A Windows guest usually runs on top of Microsoft Hyper-V, therefore expects
these synthetic devices to be present. That's why KVM provides a way to emulate
them and avoids failures running a Windows guest with Cloud Hypervisor.

By default this option is turned off.

_Example_

```
--cpus kvm_hyperv=on
```

### `max_phys_bits`

Maximum size for guest's addressable space.

This option defines the maximum number of physical bits for all vCPUs, which
sets a limit for the size of the guest's addressable space. This is mainly
useful for debug purpose.

The value is an unsigned integer of 8 bits.

_Example_

```
--cpus max_phys_bits=40
```

### `affinity`

Affinity of each vCPU.

This option gives the user a way to provide the host CPU set associated with
each vCPU. It is useful for achieving CPU pinning, ensuring multiple VMs won't
affect the performance of each other. It might also be used in the context of
NUMA as it is way of making sure the VM can run on a specific host NUMA node.
In general, this option is used to increase the performances of a VM depending
on the host platform and the type of workload running in the guest.

The affinity is described through the following structure:

```rust
struct CpuAffinity {
    vcpu: u8,
    host_cpus: Vec<u8>,
}
```

or the following syntax through the CLI:

```
affinity=[<vcpu_id1>@[<host_cpu_id1>, <host_cpu_id2>], <vcpu_id2>@[<host_cpu_id3>, <host_cpu_id4>]]
```

The outer brackets define the list of vCPUs. And for each vCPU, the inner
brackets attached to `@` define the list of host CPUs the vCPU is allowed to
run onto.

Multiple values can be provided to define each list. Each value is an unsigned
integer of 8 bits.

For instance, if one needs to run vCPU 0 on host CPUs from 0 to 4, the syntax
using `-` will help define a contiguous range with `affinity=0@[0-4]`. The
same example could also be described with `affinity=0@[0,1,2,3,4]`.

A combination of both `-` and `,` separators is useful when one might need to
describe a list containing host CPUs from 0 to 99 and the host CPU 255, as it
could simply be described with `affinity=0@[0-99,255]`.

As soon as one tries to describe a list of values, `[` and `]` must be used to
demarcate the list.

By default each vCPU runs on the entire host CPU set.

_Example_

```
--cpus boot=3,affinity=[0@[2,3],1@[0,1]]
```

In this example, assuming the host has 4 CPUs, vCPU 0 will run exclusively on
host CPUs 2 and 3, while vCPU 1 will run exclusively on host CPUs 0 and 1.
Because nothing is defined for vCPU 2, it can run on any of the 4 host CPUs.

### `features`

Set of CPU features to enable.

This option allows the user to enable a set of CPU features that are disabled
by default otherwise.

The currently available feature set is: `amx`.

The `amx` feature will enable the x86 extension adding hardware units for
matrix operations (int and float dot products). The goal of the extension is to
provide performance enhancements for these common operations.

_Example_

```
--cpus features=amx
```

In this example the amx CPU feature will be enabled for the VMM.
