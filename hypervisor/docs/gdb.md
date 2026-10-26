# GDB Support

This feature allows remote guest debugging using GDB. Note that this feature is only supported on x86_64/KVM.

To enable debugging with GDB, build with the `guest_debug` feature enabled:

```bash
cargo build --features guest_debug
```

To use the `--gdb` option, specify the Unix Domain Socket with `--path` that Cloud Hypervisor will use to communicate with the host's GDB:

```bash
./cloud-hypervisor \
    --kernel hypervisor-fw \
    --disk path=bionic-server-cloudimg-amd64.raw \
    --cpus boot=1 \
    --memory size=1024M \
    --net "tap=,mac=,ip=,mask=" \
    --console off \
    --serial tty \
    --gdb path=/tmp/ch-gdb-sock
```

Cloud Hypervisor will listen for GDB on the host side before starting the guest.
On the host side, connect to the GDB remote server as follows:

```bash
gdb -q
(gdb) target remote /tmp/ch-gdb-sock
Remote debugging using /tmp/ch-gdb-sock
warning: No executable has been specified, and target does not support
determining executable automatically. Try using the "file" command.
0x000000000011217e in ?? ()
```

You can set up to four hardware breakpoints using the x86 debug register:

```bash
(gdb) hb *0x1121b7
Hardware assisted breakpoint 1 at 0x1121b7
(gdb) c
Continuing.

Breakpoint 1, 0x00000000001121b7 in ?? ()
(gdb)
```
