# `cloud-hypervisor` debug IO port

`cloud-hypervisor` uses the [`0x80`](https://www.intel.com/content/www/us/en/support/articles/000005500/boards-and-kits.html)
I/O port to trace user defined guest events.

Whenever the guest write one byte between `0x0` and `0xF` on this particular
I/O port, `cloud-hypervisor` will log and timestamp that event at the `debug`
log level.

It is up to the guest stack to decide when and what to write to the 0x80 port
in order to signal the host about specific events and have `cloud-hypervisor`
log it.

`cloud-hypervisor` defines several debug port code ranges that should be used
for debugging specific components of the guest software stack. When logging a
write of one of those codes to the debug port, `cloud-hypervisor` adds a
pre-defined string to the logs.

| Code Range       | Component   | Log string   |
| ---------------- | ----------- | ------------ |
| `0x00` to `0x1f` | Firmware    | `Firmware`   |
| `0x20` to `0x3f` | Bootloader  | `Bootloader` |
| `0x40` to `0x5f` | Kernel      | `Kernel`     |
| `0x60` to `0x7f` | Userspace   | `Userspace`  |
| `0x80` to `0xff` | Custom      | `Custom`     |

One typical use case is guest boot time measurement and tracing. By writing
different values to the debug I/O port at different boot process steps, the
guest will have `cloud-hypervisor` generate timestamped logs of all those steps.
That provides a basic but convenient way of measuring not only the overall guest
boot time but all intermediate steps as well.

## Logging

Assuming parts of the guest software stack have been instrumented to use the
`cloud-hypervisor` debug I/O port, we may want to gather the related logs.

To do so we need to start `cloud-hypervisor` with the right debug level
(`-vvv`). It is also recommended to have it log into a dedicated file in order
to easily grep for the tracing logs (e.g.
`--log-file /tmp/cloud-hypervisor.log`):

```
./target/debug/cloud-hypervisor \
    --kernel ~/rust-hypervisor-firmware/target/target/release/hypervisor-fw \
    --disk path=~/hypervisor/images/focal-server-cloudimg-amd64.raw \
    --cpus 4 \
    --memory size=1024M \
    --rng \
    --log-file /tmp/ch-fw.log \
    -vvv
```

After booting the guest, we then have to grep for the debug I/O port traces in
the log file:

```Shell
$ grep "Debug I/O port" /tmp/ch-fw.log
cloud-hypervisor: 19.762449ms: DEBUG:vmm/src/vm.rs:510 -- [Debug I/O port: Firmware code 0x0] 0.019004 seconds
cloud-hypervisor: 403.499628ms: DEBUG:vmm/src/vm.rs:510 -- [Debug I/O port: Firmware code 0x1] 0.402744 seconds
```
