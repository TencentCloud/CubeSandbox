# Seccomp filtering

As a means to harden Cloud Hypervisor's security, the project leverages seccomp
filtering.

## What is seccomp filtering

A seccomp filter is a way for a process to tell the kernel which system calls
are authorized.
In case this process calls into a prohibited system call, the kernel will kill
the process right away.

## How does it apply to Cloud Hypervisor

Cloud Hypervisor is a multi threaded application. It spawns dedicated threads
for virtual CPUs, virtio devices and HTTP server, along with the main thread
representing the VMM.

Each of these threads has a limited scope of what it is expected to perform,
which is why different filters are applied to each of them.

Release builds enable seccomp filtering by default because security should not
be optional. Debug builds disable filtering by default so that newly introduced
system calls do not terminate the process while developers investigate them;
startup logs make this exception visible. The default debug mode does not log
blocked system calls because it installs no filter. Use `--seccomp log` when the
host audit setup is available and syscall logging is desired. In either build
type, `--seccomp process` explicitly enables the production default.

For development and debugging purposes, one might want to disable filtering or
log the faulty system call explicitly.

### Disabling seccomp filters

Append `--seccomp false` to Cloud Hypervisor's command line to prevent seccomp
filtering from being applied.

### Logging prohibited system calls

In the context of debug, one alternative to disabling seccomp filtering is to
log faulty system calls that would have caused the application to be killed by
the kernel.

Append `--seccomp log` to Cloud Hypervisor's command line to enable faulty
system calls to be logged.

The kernel running on the host machine must have the `audit` parameter enabled.
If this is not the case, update kernel boot options by appending `audit=1`.

Unauthorized system calls will be logged to the journal similarly to the
following example

```
type=SECCOMP msg=audit(1423263412.694:7878): auid=1000 uid=1000 gid=1000 ses=3 subj=unconfined_u:unconfined_r:cloud_hypervisor:s0-s0:c0.c1023 pid=1193 comm="cloud-hypervisor" exe="/usr/bin/cloud-hypervisor" sig=0 arch=c000003e syscall=47 compat=0 ip=0x7f4f63982604 code=0x50000
```

Provided `ausyscall` has been installed on the host, the system call can be
identified with

```
$ ausyscall 47
recvmsg
```

### Further debug with `strace`

One more way of debugging seccomp related issues is to use the `strace` tool as
it will log every system call issued by the process. It is important to use
`-f` option in order to trace each and every thread belonging to the process.

```
strace --decode-pids=comm -f ./cloud-hypervisor ...
```
