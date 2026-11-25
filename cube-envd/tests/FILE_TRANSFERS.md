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

The upload bridge also has deterministic Body-frame tests (no TCP-frame
assumptions). Run them with:

```sh
cargo test --manifest-path cube-envd/Cargo.toml --locked --lib filesystem::upload::tests
```

They cover empty/interleaved frames, whole-frame identity, 32 KiB and 128 KiB
boundaries and tails, a prefix before EOF, Body errors versus normal EOF,
unexpected channel closure, backpressure, file-side early failure, and request
cancellation waking a blocking reader. The shared write loop is checked with
partial writes, EINTR, zero progress, EIO/ENOSPC/EDQUOT and cancellation after a
short write; each syscall receives at most 128 KiB.

Owner drop probes verify the lifetime of the actual backing allocation, including
three independent 4 MiB owners simultaneously retained by producer, queue and
consumer (12 MiB). Completion/failure/cancellation release those references.
Queue capacity 1 is a message count, **not** a byte or backing-memory cap.
Transport prefetch, decoder/parser buffers and other requests add further memory.

The producer forwards each nonempty `Bytes` frame once without a payload copy.
Raw uploads write those bytes directly in the existing file task, capped at
128 KiB per syscall; gzip/multipart retain their 32 KiB streaming Read buffers.
Multipart candidate search reuses the locked `memchr` dependency; suffix/padding
validation and cross-read retention remain in the parser. Regression inputs
include false boundary candidates, binary data, CRLF/LF and one-byte reads.
Decoders/parsers and the kernel may still copy data. Keep throughput measurements
on the final uninstrumented musl release artifact, with complete readback outside
timing, and keep strace diagnostics separate. These regression tests establish
streaming and ownership behavior; they do not establish a throughput guarantee.

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
short writes, one-shot EINTR, zero-byte write results, actual tmpfs exhaustion,
inode/parent replacement, device/FIFO behavior,
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
python3 -B cube-envd/tests/file_sdk.py -v
PYTHONPATH=sdk/python python3 -B cube-envd/tests/filesystem_cube_sdk.py
```

Select `FileTransferSDK.test_download` or `FileTransferSDK.test_upload` to rerun
one case. The E2B cases exercise text/bytes/streams, octet/gzip/multipart, batch uploads and
the SDK version threshold. The repository SDK case uses its existing IP override
and checks empty/text/bytes/33 MiB read/write and overwrite in the same entry
point as metadata and streaming watch. Platform proxy, image, template, VM
lifecycle and release behavior require separate validation.
