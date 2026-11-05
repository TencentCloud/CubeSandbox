# HTTP file transfer checks

`make -C cube-envd ci` discovers the download, upload, HTTP representation,
signed-file and compose cases. These launch the Cargo-built daemon and exercise
its public listener. The upload/init context regression uses an in-process HTTP
router with a private passwd fixture to control concurrent user resolution.

Coverage includes identity/gzip, metadata, conditional and Range responses,
empty files, HEAD rejection (405 with Allow: GET and POST), octet and multipart
uploads, partial writes, concatenated gzip members, malformed input, and retained
parser/header limits. Large-body and concurrent-transfer regressions check that
removed payload and eight-job limits are not restored. Signed requests cover
read/write binding, expiration, header-token priority, username and path binding.
Compose checks ordered publication, source cleanup, ownership and inherited umask.

For kernel faults and pressure, run beside a freshly built daemon in an isolated
Linux container with private mount/PID namespaces, a private writable cgroup v2
subtree, `/dev/fuse` and mount privileges. Never use a shared daemon or host
cgroups. Remove SYS_TIME from effective, bounding, inheritable and ambient
capability sets; signature expiry tests use request timestamps, not clock changes.
Set both `NO_PROXY` and `no_proxy` to `localhost,127.0.0.1,::1`.

Set `ENVD_SMOKE_URL` and `ENVD_TEST_URL` to the daemon URL, `ENVD_SMOKE_PID` to its
PID in that namespace, and `TMPDIR` to disposable data-disk storage. Start the
pressure-test daemon with umask 077. Run from the repository root:

```sh
python3 -B cube-envd/tests/file_get_failures.py
python3 -B cube-envd/tests/file_post_failures.py
python3 -B cube-envd/tests/file_get_pressure.py
python3 -B cube-envd/tests/file_post_pressure.py
python3 -B cube-envd/tests/file_compose_failures.py
```

These cover real FUSE read/write errors, ENOSPC/EDQUOT and EMFILE responses,
actual tmpfs exhaustion, inode/parent replacement, device/FIFO behavior,
blocked calls, independent request progress, disconnects, and resource cleanup.
Already blocked kernel syscalls finish or must be released by the fixture before
worker reclamation; cancellation prevents subsequent work. An accepted compose
finishes after client disconnect and cleans its temporary file and eligible
sources. Final rename/unlink retains POSIX name semantics, not an atomic
compare-inode-and-mutate guarantee.

`file_worker_failure.py` additionally requires `ENVD_WORKER_CGROUP` naming a
private, writable, daemon-only cgroup with the pids controller enabled. The
runner moves the disposable daemon into that leaf before running the script.
The test temporarily sets pids.max to the current task count, checks real worker
creation failures and health, then restores the limit and verifies recovery.

SDK checks require their dependencies installed and a disposable daemon. The
E2B checks additionally need `CUBE_FILESYSTEM_SMOKE_USER`,
`CUBE_FILESYSTEM_SMOKE_HOME` and `CUBE_FILESYSTEM_SMOKE_DIR`; the selected user's
passwd home must equal the smoke home. They change init defaults. Run with the
Python interpreter containing the SDK:

```sh
python3 -B cube-envd/tests/file_get_sdk.py
python3 -B cube-envd/tests/file_post_sdk.py
PYTHONPATH=sdk/python python3 -B cube-envd/tests/file_cube_sdk.py
```

The E2B cases exercise text/bytes/streams, octet/gzip/multipart, batch uploads and
the SDK version threshold. The repository SDK case uses its existing IP override
and checks empty/text/bytes/33 MiB read/write and overwrite. No caller code is
changed. Direct daemon checks do not certify platform proxy, image, template,
VM lifecycle or release behavior.
