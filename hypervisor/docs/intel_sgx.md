# Intel SGX

Intel® Software Guard Extensions (Intel® SGX) is an Intel technology designed
to increase the security of application code and data. Cloud Hypervisor supports
SGX virtualization through KVM. Because SGX is built on hardware features that
cannot be emulated in software, virtualizing SGX requires support in KVM and in
the host kernel. The required Linux and KVM changes can be found in the
[KVM SGX Tree](https://github.com/intel/kvm-sgx).

Utilizing SGX in the guest requires a kernel/OS with SGX support, e.g. a kernel
built using the [SGX Linux Development Tree](https://git.kernel.org/pub/scm/linux/kernel/git/jarkko/linux-sgx.git)
or the [KVM SGX Tree](https://github.com/intel/kvm-sgx). Running KVM SGX as the
guest kernel allows nested virtualization of SGX.

For more information about SGX, please refer to the [SGX Homepage](https://software.intel.com/sgx).

For more information about SGX SDK and how to test SGX, please refer to the
following [instructions](https://github.com/intel/linux-sgx).

## Cloud Hypervisor support

Assuming the host exposes `/dev/sgx_vepc`, we can pass SGX enclaves through
the guest.

In order to use SGX enclaves within a Cloud Hypervisor VM, we must define one
or several Enclave Page Cache (EPC) sections. Here is an example of a VM being
created with 2 EPC sections, the first one being 64MiB with pre-allocated
memory, the second one being 32MiB with no pre-allocated memory.

```bash
./cloud-hypervisor \
    --cpus boot=1 \
    --memory size=1G \
    --disk path=focal-server-cloudimg-amd64.raw \
    --kernel vmlinux \
    --cmdline "console=ttyS0 console=hvc0 root=/dev/vda1 rw" \
    --sgx-epc id=epc0,size=64M,prefault=on id=epc1,size=32M,prefault=off
```

Once booted, and assuming your guest kernel contains the patches from the
[KVM SGX Tree](https://github.com/intel/kvm-sgx), you can validate SGX devices
have been correctly created under `/dev/sgx`:

```bash
ls /dev/sgx*
/dev/sgx_enclave  /dev/sgx_provision  /dev/sgx_vepc
```

From this point, it is possible to run any SGX application from the guest, as
it will access `/dev/sgx_enclave` device to create dedicated SGX enclaves.

Note: There is only one contiguous SGX EPC region, which contains all SGX EPC
sections. This region is exposed through ACPI and marked as reserved through
the e820 table. It is treated as yet another device, which means it should
appear at the end of the guest address space.
