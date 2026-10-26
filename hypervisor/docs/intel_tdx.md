# Intel TDX

Intel® Trust Domain Extensions (Intel® TDX) is an Intel technology designed to
isolate virtual machines from the VMM, hypervisor and any other software on the
host platform.

For more information about TDX technical aspects, design and specification
please refer to the
[TDX Homepage](https://www.intel.com/content/www/us/en/developer/articles/technical/intel-trust-domain-extensions.html).

The required Linux changes for the host side can be found in the
[KVM TDX tree](https://github.com/intel/tdx/tree/kvm) while the changes for
the guest side can be found in the [Guest TDX tree](https://github.com/intel/tdx/tree/guest).

The TDVF firmware can be found in the
[EDK2 staging project](https://github.com/tianocore/edk2-staging/tree/TDVF).

The TDShim firmware can be found in the
[Confidential Containers project](https://github.com/confidential-containers/td-shim).

## Cloud Hypervisor support

First, you must be running on a machine with TDX enabled in hardware, and
with the host OS compiled from the [KVM TDX tree](https://github.com/intel/tdx/tree/kvm).

Cloud Hypervisor can run TDX VM (Trust Domain) by loading a TD firmware,
which will then load the guest kernel from the image. The image must be custom
as it must include a kernel built from the [Guest TDX tree](https://github.com/intel/tdx/tree/guest).

### TDVF

The firmware can be built as follows:

```bash
git clone https://github.com/tianocore/edk2-staging.git
cd edk2-staging
git checkout origin/TDVF
git submodule update --init --recursive
make -C BaseTools
source ./edksetup.sh
build -p OvmfPkg/OvmfCh.dsc -a X64 -t GCC5 -b RELEASE
```

If debug logs are needed, here is the alternative command:

```bash
build -p OvmfPkg/OvmfCh.dsc -a X64 -t GCC5 -D DEBUG_ON_SERIAL_PORT=TRUE
```

On the Cloud Hypervisor side, all you need is to build the project with the
`tdx` feature enabled:

```bash
cargo build --features tdx
```

And run a TDX VM by providing the firmware previously built, along with the
guest image containing the TDX enlightened kernel. The latest image
`td-guest-rhel8.5.raw` contains `console=hvc0` on the kernel boot parameters,
meaning it will be printing guest kernel logs to the `virtio-console` device.

```bash
./cloud-hypervisor \
    --platform tdx=on
    --firmware edk2-staging/Build/OvmfCh/RELEASE_GCC5/FV/OVMF.fd \
    --cpus boot=1 \
    --memory size=1G \
    --disk path=tdx_guest_img
```

And here is the alternative command when looking for debug logs from the
firmware:

```bash
./cloud-hypervisor \
    --platform tdx=on
    --firmware edk2-staging/Build/OvmfCh/DEBUG_GCC5/FV/OVMF.fd \
    --cpus boot=1 \
    --memory size=1G \
    --disk path=tdx_guest_img \
    --serial file=/tmp/ch_serial \
    --console tty
```

### TDShim

This is a lightweight version of the TDVF, written in Rust and designed for
direct kernel boot, which is useful for containers use cases.

You can find the instructions for building the firmware directly from the
project [documentation](https://github.com/confidential-containers/td-shim/tree/staging#how-to-build).

And run a TDX VM by providing the firmware previously built, along with a guest
kernel built from the [Guest TDX tree](https://github.com/intel/tdx/tree/guest).
The appropriate kernel boot options must be provided through the `--cmdline`
option as well.

```bash
./cloud-hypervisor \
    --platform tdx=on
    --firmware tdshim \
    --kernel bzImage \
    --cmdline "root=/dev/vda3 console=hvc0 rw"
    --cpus boot=1 \
    --memory size=1G \
    --disk path=tdx_guest_img
```

### Guest kernel limitations

#### Serial ports disabled

The latest guest kernel that can be found in the latest image
`td-guest-rhel8.5.raw` disabled the support for serial ports. This means adding
`console=ttyS0` will have no effect and will not print any log from the guest.

#### PCI hotplug through ACPI

Unless you run the guest kernel with the parameter `tdx_disable_filter`, ACPI
devices responsible for handling PCI hotplug (PCI hotplug controller, PCI
Express Bus and Generic Event Device) will not be allowed, therefore the
corresponding drivers will not be loaded and the PCI hotplug feature will not
be supported.
