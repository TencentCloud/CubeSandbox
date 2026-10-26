# virtiofsd

A [virtio-fs](https://virtio-fs.gitlab.io/) vhost-user device daemon
written in Rust.

## Building from sources

### Requirements

This project depends on
[libcap-ng](https://people.redhat.com/sgrubb/libcap-ng/) and
[libseccomp](https://github.com/seccomp/libseccomp). You can obtain
those dependencies by building them for their respective sources, or
by installing the correspondent development packages from your
distribution, if available:

- Fedora/CentOS/RHEL
```shell
dnf install libcap-ng-devel libseccomp-devel
```

- Debian/Ubuntu
```shell
apt install libcap-ng-dev libseccomp-dev
```

### Compiling

virtiofsd is written in Rust, so you will have to install [Rust](https://www.rust-lang.org/learn/get-started)
in order to compile it, and it uses [cargo](https://doc.rust-lang.org/cargo/) to manage the
project and its dependencies.
After installing Rust, you can compile it to a binary by running:

```shell
cargo build --release
```

## CI-built binaries

Every time new code is merged, the CI pipeline will upload a debug binary
of virtiofsd. It is intended to be an accessible way for anyone to
download and test virtiofsd without needing a Rust toolchain installed.

The debug binary is built only for x86\_64 Linux-based systems.

[Click here to download the latest build](
https://gitlab.com/virtio-fs/virtiofsd/-/jobs/artifacts/main/download?job=publish)

## Contributing
See [CONTRIBUTING.md](CONTRIBUTING.md)

## Usage
This program must be run as the root user or as a "fake" root inside a
user namespace (see [Running as non-privileged user](#running-as-non-privileged-user)).

The program drops privileges where possible during startup,
although it must be able to create and access files with any uid/gid:

* The ability to invoke syscalls is limited using `seccomp(2)`.
* Linux `capabilities(7)` are dropped. virtiofsd only retains the following capabilities:
`CAP_CHOWN`, `CAP_DAC_OVERRIDE`, `CAP_FOWNER`, `CAP_FSETID`, `CAP_SETGID`, `CAP_SETUID`,
`CAP_MKNOD`, `CAP_SETFCAP`
(and `CAP_DAC_READ_SEARCH` if `--inode-file-handles` is used).

```shell
virtiofsd [FLAGS] [OPTIONS] --fd <fd>|--socket-path <socket-path> --shared-dir <shared-dir>
```
#### Flags
```shell
-h, --help
```
Prints help information.

```shell
-V, --version
```
Prints version information.

```shell
--syslog
```
Log to syslog. Default: stderr.

```shell
--print-capabilities
```
Print vhost-user.json backend program capabilities and exit.

```shell
--allow-direct-io
```
Honor the `O_DIRECT` flag passed down by guest applications.

```shell
--announce-submounts
```
Tell the guest which directories are mount points.
If multiple filesystems are mounted in the shared directory,
virtiofsd passes inode IDs directly to the guest, and because such IDs
are unique only on a single filesystem, it is possible that the guest
will encounter duplicates if multiple filesystems are mounted in the
shared directory.
`--announce-submounts` solves that problem because it reports a different
device number for every submount it encounters.

In addition, when running with `--announce-submounts`, the client sends one
`SYNCFS` request per submount that is to be synced, so virtiofsd
will call `syncfs()` on each submount.
On the other hand, when running without `--announce-submounts`,
the client only sends a `SYNCFS` request for the root mount,
this may lead to data loss/corruption.

```shell
--no-killpriv-v2
```
Disable `KILLPRIV V2` support.
This is required if the shared directory is an NFS file system.
`KILLPRIV V2` support is disabled by default.

```shell
--killpriv-v2
```
Enable `KILLPRIV V2` support. It is disabled by default.

```shell
--no-readdirplus
```
Disable support for `READDIRPLUS` operations.

```shell
--writeback
```
Enable writeback cache.

```shell
--xattr
```
Enable support for extended attributes.

```shell
--posix-acl
```
Enable support for posix ACLs (implies --xattr).

```shell
--security-label
```
Enable support for security label (SELinux).

```shell
--preserve-noatime
```
Always preserve `O_NOATIME`.

By default virtiofsd will implicitly clean up `O_NOATIME` to prevent potential
permission errors when it does not have the right capabilities to access all
the exported files (typically when running as unprivileged user and with
`--sandbox none`, that means it won't have the `CAP_FOWNER` capability set).

The option `--preserve-noatime` can be used to override this behavior and
preserve the `O_NOATIME` flag specified by the client.

#### Options
```shell
--shared-dir <shared-dir>
```
Shared directory path.

```shell
--tag <tag>
```
The tag that the virtio device advertises.

Setting this option will enable advertising of VHOST_USER_PROTOCOL_F_CONFIG.
However, the vhost-user frontend of your hypervisor may not negotiate this
feature and (or) ignore this value. Notably, QEMU currently (as of 8.1) ignores
the CONFIG feature. QEMU versions from 7.1 to 8.0 will crash while attempting to
log a warning about not supporting the feature.

```shell
--socket-group <socket-group>
```
Name of group for the vhost-user socket.

```shell
--socket-path <socket-path>
```
vhost-user socket path.

```shell
--fd <fd>
```
File descriptor for the listening socket.

```shell
--log-level <log-level>
```
Log level (error, warn, info, debug, trace, off).

Default: info.

```shell
--thread-pool-size <thread-pool-size>
```
Maximum thread pool size. A value of "0" disables the pool.

Default: 0.

```shell
--rlimit-nofile <rlimit-nofile>
```
Set maximum number of file descriptors.
If the soft limit is greater than 1M  or `--rlimit-nofile=0`  is passed
as parameter, the maximum number of file descriptors is not changed.

Default: min(1000000, `/proc/sys/fs/nr_open`).

```shell
--modcaps=<modcaps>
```
Modify the list of capabilities, e.g., `--modcaps=+sys_admin:-chown`.
Although it is not mandatory, it is recommended to always use the `=` sign,
in other case, this will fail  `--modcaps -mknod`, because it will be
interpreted as two options, instead of the intended `--modcaps=-mknod`.

```shell
--sandbox <sandbox>
```
Sandbox mechanism to isolate the daemon process (namespace, chroot, none).

- **namespace**: The program switches into a new file system
namespace (`namespaces(7)`) and invokes `pivot_root(2)` to make the shared directory
tree its root. A new mount (`mount_namespaces(7)`), pid (`pid_namespaces(7)`) and
net namespace (`network_namespaces(7)`) is also created to isolate the process.

- **chroot**: The program invokes `chroot(2)` to make the shared
directory tree its root. This mode is intended for container environments where
the container runtime has already set up the namespaces and the program does
not have permission to create namespaces itself.

- **none**: Do not isolate the daemon (not recommended).

Both **namespace** and **chroot** sandbox modes prevent "file system escapes"
due to symlinks and other file system objects that might lead to files outside
the shared directory.

Default: namespace.

```shell
--seccomp <seccomp>
```
Action to take when seccomp finds a not allowed syscall (none, kill, log, trap).

Default: kill.

```shell
--cache <cache>
```
The caching policy the file system should use (auto, always, metadata, never).

Default: auto.

```shell
--allow-mmap
```

For shared directories with `--cache={metadata, never}`, allow files contained in the shared directory to be `mmap`'d.

```shell
--inode-file-handles=<inode-file-handles>
```
When to use file handles to reference inodes instead of `O_PATH` file descriptors (never, prefer, mandatory).

- **never**: Never use file handles, always use `O_PATH` file descriptors.

- **prefer**: Attempt to generate file handles, but fall back to `O_PATH` file descriptors where the underlying
  filesystem does not support file handles or `CAP_DAC_READ_SEARCH` is not available.
  Useful when there are various different filesystems under the shared directory and some of them do not support file handles.

- **mandatory**: Always use file handles.
  It will fail if the underlying filesystem does not support file handles or `CAP_DAC_READ_SEARCH` is not available.

Using file handles reduces the number of file descriptors virtiofsd keeps open, which is not only helpful
with resources, but may also be important in cases where virtiofsd should only have file descriptors open
for files that are open in the guest, e.g. to get around bad interactions with NFS's silly renaming
(see [NFS FAQ, Section D2: "What is a "silly rename"?"](http://nfs.sourceforge.net/)).

Default: never.

```shell
--xattrmap <xattrmap>
```
Add custom rules for translating extended attributes between host and guest (e.g., `:map::user.virtiofs.:`).
For additional details please see [Extended attribute mapping](doc/xattr-mapping.md).

```shell
--uid-map=:namespace_uid:host_uid:count:
```
When running virtiofsd as non-root, map a range of UIDs from host to namespace.
In order to use this option, the range of subordinate user IDs must have been set up via
`subuid(5)`. virtiofsd uses `newuidmap(1)` for non-trivial cases, that requires a valid subuid,
to do the mapping. If this option is not provided, virtiofsd will set up a 1-to-1 mapping for current uid.

namespace_uid: Beginning of the range of UIDs inside the user namespace.
host_uid: Beginning of the range of UIDs outside the user namespace.
count: Length of the ranges (both inside and outside the user namespace).

For instance, let's assume the invoking UID is 1000 and the content of /etc/subuid is: 1000:100000:65536,
which creates 65536 subuids starting at 100000, i.e. the (inclusive) range [100000, 165535], belonging to the actual UID 1000.
This range can be mapped to the UIDs [0, 65535] in virtiofsd’s user namespace (i.e. as seen in the guest) via --uid-map=:0:100000:65536:.
Alternatively, you can simply map your own UID to a single UID in the namespace:
For example, --uid-map=:0:1000:1: would map UID 1000 to root’s UID in the namespace (and thus the guest).

```shell
--gid-map=:namespace_gid:host_gid:count:
```
When running virtiofsd as non-root, map a range of GIDs from host to namespace.
In order to use this option, the range of subordinate group IDs must have been set up via
`subgid(5)`. virtiofsd uses `newgidmap(1)` for non-trivial cases, that requires a valid subgid,
to do the mapping. If this option is not provided, virtiofsd will set up a 1-to-1 mapping for current gid.

namespace_gid: Beginning of the range of GIDs inside the user namespace.
host_gid: Beginning of the range of GIDs outside the user namespace.
count: Length of the ranges (both inside and outside the user namespace).

For instance, let's assume the invoking GID is 1000 and the content of /etc/subgid is: 1000:100000:65536,
which creates 65536 subgids starting at 100000, i.e. the (inclusive) range [100000, 165535], belonging to the actual GID 1000.
This range can be mapped to the GIDs [0, 65535] in virtiofsd’s user namespace (i.e. as seen in the guest) via --gid-map=:0:100000:65536:.
Alternatively, you can simply map your own GID to a single GID in the namespace:
For example, --gid-map=:0:1000:1: would map GID 1000 to root’s GID in the namespace (and thus the guest).

```shell
--migration-mode=<find-paths>
```
Defines how to perform migration, i.e. how to represent the internal state to the destination
instance, and how to obtain that representation.  Note that (when using QEMU) **QEMU version 8.2**
or newer is required to use virtio-fs migration.

virtiofsd internally holds references to all inodes indexed or opened by the guest.  During
migration, these references need to be transferred to the destination; how that is done is
determined with this switch:

- **find-paths**: Iterate through the shared directory (exhaustive search) to find paths for all
  inodes held by the source instance (relative to the shared directory), transfer these paths to the
  destination, and let the destination instance open these paths.  This allows migration without
  requiring special privileges, and regardless of whether source and destination use the same shared
  directory; but it is expensive in terms of I/O (DFS through the shared directory) and is
  vulnerable to third parties changing metadata in the shared directory while migration is ongoing
  (e.g. renaming, unlinking, removing permissions), which can potentially lead to data loss and/or
  corruption.

This parameter is ignored on the destination side of migration.

```shell
--migration-on-error=<abort|guest-error>
```
Controls how to respond to errors during migration.

During migration, some inodes that the guest has indexed or opened may turn out not to be
migrateable: Either the source instance cannot construct instructions on how the destination
instance may be able to find/open some inode, or the destination instance finds itself unable to
follow those instructions.  In all cases, the destination instance is notified of these inodes, and
then decides what to do depending on the value of this parameter:

- **abort**: Whenever the destination instance sees any such error, it returns a hard error to the
  vhost-user front-end (e.g. QEMU), which aborts migration.  Execution is to continue on the source
  VM.

- **guest-error**: Migration is allowed to finish, but all affected inodes are marked as invalid.
  The guest will not be able to access any such inode, receiving only errors.

Note that this parameter is to be used purely for the destination instance; its value is ignored on
the source side of migration.

```shell
--migration-verify-handles
```
Ensure that the migration destination opens the very same inodes as the source.  This only works if
source and destination are to use the same shared directory on the same filesystem.

On migration, the source instance informs the destination instance of all inodes the guest has
indexed or opened, and has the destination re-open them.  This switch makes the source generate a
file handle for each such inode, and send it to the destination, allowing the destination to
re-generate the same file handle for the inode it has opened and verify that it is equal, proving it
is the same inode.

(File handles are per-filesystem unique identifiers for inodes that, besides the inode ID, also
include a generation ID to protect against inode ID reuse.)

Using this option protects against external parties renaming or replacing inodes while migration is
ongoing, which, without this option, can lead to data loss or corruption, so it should always be
used when other processes besides virtiofsd have write access to the shared directory.  However,
again, it only works if both source and destination use the same shared directory; though in the
case of network filesystems, this does not require them to run on the same host.

This parameter is ignored on the destination side of migration.

```shell
--migration-confirm-paths
```
Double-check the identity of inodes right before switching over to the destination, potentially
making migration more resilient when third parties have write access to the shared directory.

When representing migrated inodes by their paths relative to the shared directory, double-check
during switch-over to the destination that each path still matches the respective inode.  If a path
does not match, try to correct by consulting the respective symbolic link in */proc/self/fd*.

Note that this option requires accessing each inode indexed or opened by the guest once during the
switch-over stage of migration, when both the source and destination VM are paused, so can prolong
that phase for an indeterminate amount of time.

This parameter is ignored on the destination side of migration.

### Examples
Export `/mnt` on vhost-user UNIX domain socket `/tmp/vfsd.sock`:

```shell
host# virtiofsd --socket-path=/tmp/vfsd.sock --shared-dir /mnt \
        --announce-submounts --inode-file-handles=mandatory &

host# qemu-system \
        -blockdev file,node-name=hdd,filename=<your image> \
        -device virtio-blk,drive=hdd \
        -chardev socket,id=char0,path=/tmp/vfsd.sock \
        -device vhost-user-fs-pci,queue-size=1024,chardev=char0,tag=myfs \
        -object memory-backend-memfd,id=mem,size=4G,share=on \
        -numa node,memdev=mem \
        -accel kvm -m 4G

guest# mount -t virtiofs myfs /mnt
```

See [FAQ](#faq) for adding virtiofs config to an existing qemu command-line.

### Running as non-privileged user
When run without root, virtiofsd requires a user namespace (see `user_namespaces(7)`)
to be able to switch between arbitrary user/group IDs within the guest.
virtiofsd will fail in a user namespace where UIDs/GIDs have not been mapped
(i.e., `uid_map` and `gid_map` files have not been written).
There are many options to run virtiofsd inside a user namespace.
For instance:

Let's assume the invoking UID and GID is 1000 and the content of both `/etc/subuid`
and `/etc/subgid` are:
```
1000:100000:65536
```

Using `podman-unshare(1)` the user namespace will be configured so that the invoking user's UID
and primary GID (i.e., 1000) appear to be UID 0 and GID 0, respectively.
Any ranges which match that user and group in `/etc/subuid` and `/etc/subgid` are also
mapped in as themselves with the help of the `newuidmap(1)` and `newgidmap(1)` helpers:

```shell
host$ podman unshare -- virtiofsd --socket-path=/tmp/vfsd.sock --shared-dir /mnt \
        --announce-submounts --sandbox chroot &
```

Using `lxc-usernsexec(1)`, we could leave the invoking user outside the mapping, having
the root user inside the user namespace mapped to the user and group 100000:

```shell
host$ lxc-usernsexec -m b:0:100000:65536 -- virtiofsd --socket-path=/tmp/vfsd.sock \
        --shared-dir /mnt --announce-submounts --sandbox chroot &
```

In order to have the same behavior as `podman-unshare(1)`, we need to run

```shell
host$ lxc-usernsexec -m b:0:1000:1 -m b:1:100000:65536 -- virtiofsd --socket-path=/tmp/vfsd.sock \
        --shared-dir /mnt --announce-submounts --sandbox chroot &
```

We could also select `--sandbox none` instead of `--sandbox chroot`.

#### Limitations
- Within the guest, it is not possible to create block or char device nodes in the shared directory.

- virtiofsd can't use file handles (`--inode-file-handles` requires `CAP_DAC_READ_SEARCH`),
  so a large number of file descriptors is required.
  Additionally, on NFS, not using file handles may result in a hidden file lingering after some file is deleted
  (see [NFS FAQ, Section D2: "What is a "silly rename"?"](http://nfs.sourceforge.net/)).

- virtiofsd will not be able to increase `RLIMIT_NOFILE`.

## FAQ
- How to read-only-share a directory that cannot be modified within the guest?
To accomplish this you need to export a read-only mount point, for instance,
exporting `share`:

```shell
mkdir ro-share
mount -o bind,ro share ro-share
virtiofsd --shared-dir ro-share ...
```

- How to share multiple directories with the same virtiofsd?
Currently, virtiofsd only supports sharing a single directory,
but it is possible to use submounts to achieve this, for instance,
exporting `share0`, `share1`:

```shell
mkdir -p share/{sh0,sh1}
mount -o bind share0 share/sh0
mount -o bind share1 share/sh1
virtiofsd --announce-submounts --shared-dir share ...
```
Note the use of `--announce-submounts` to prevent data loss/corruption.

- How to add virtiofs devices to an existing qemu command-line:

  If `-object memory-backend-memfd,id=mem` and either `-numa node,memdev=mem`
  or a `memory-backend=mem` property in the `-machine` option
  have not already been added to the command, add them.

  If a different memory backend is already configured then it should be changed
  to `memory-backend-memfd`.

  `-object memory-backend-memfd` **must** have the option `share=on`
  and `size=` **must** match the memory size defined by `-m`.

  For each virtiofs device mount add a
  `-chardev socket,id=${MATCHING_ID},path=${VIRTIOFSD_SOCKET_PATH}` and
  `-device vhost-user-fs-pci,queue-size=1024,chardev=${MATCHING_ID},tag=${VIRTIOFS_TAG}`
  substituting appropriate values for the shell-style variables.

## SELinux Support
One can enable support for SELinux by running virtiofsd with option
"--security-label". But this will try to save guest's security context
in xattr security.selinux on host and it might fail if host's SELinux
policy does not permit virtiofsd to do this operation.

Hence, it is recommended to remap guest's "security.selinux" xattr to say
"trusted.virtiofs.security.selinux" on host. Add following option to
command line.

"--xattrmap=:map:security.selinux:trusted.virtiofs.:"

This will make sure that guest and host's SELinux xattrs on same file
remain separate and not interfere with each other. And will allow both
host and guest to implement their own separate SELinux policies.

Setting trusted xattr on host requires CAP_SYS_ADMIN. So one will need
add this capability to daemon. Add following option to command line.

"--modcaps=+sys_admin"

trusted xattrs are not namespaced. So virtiofsd needs to have CAP_SYS_ADMIN
in init_user_ns. IOW, one should not be using user namespaces and virtiofsd
should run with CAP_SYS_ADMIN.

Giving CAP_SYS_ADMIN increases the risk on system. Now virtiofsd is more
powerful and if gets compromised, it can do lot of damage to host system.
So keep this trade-off in my mind while making a decision.
