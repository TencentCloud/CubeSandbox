# Filesystem component checks

`make -C cube-envd ci` discovers the Rust unary, protobuf/error, context,
streaming and polling cases. Unary and watcher integration cases launch the
Cargo-built daemon. Context and lifecycle-transition tests use the real HTTP router in process;
context tests supply a private passwd fixture. Polling acceptance covers 72
concurrent watchers, create and write events for 1024 files, and recursive
tree changes. Resource assertions inspect the actual daemon PID.

The Python checks run beside a newly built daemon in an isolated Linux
container with a private writable cgroup v2 subtree, private mount/PID
namespaces, `/dev/fuse`, and mount privileges. Never run mount/race fixtures
against a shared daemon or host cgroups. Drop SYS_TIME from all capability
sets. Set `NO_PROXY` and `no_proxy` to `localhost,127.0.0.1,::1`.

For tests that attach to a running daemon, set `ENVD_SMOKE_URL` to its URL and
`ENVD_SMOKE_PID` and `WATCH_DAEMON_PID` to its PID in that namespace.
Set `TMPDIR` to disposable space on the data disk. Run from the repository root:

```sh
python3 -B cube-envd/tests/filesystem_compat.py
python3 -B cube-envd/tests/filesystem_object_safety.py
ENVD_TEST_BINARY="${CARGO_TARGET_DIR:-cube-envd/target}/debug/cube-envd" \
  python3 -B cube-envd/tests/filesystem_kernel_races.py -v
```

The race entry point starts a fresh daemon per creation, listing and mutation
scenario and passes its loopback URL to the scenario in a separate process.

These exercise real RPC and kernel behavior for inotify event ordering, bound
root aliases, mount traversal and partial failure, FUSE lookup failures and
replacement races.
The mount traversal case checks its local sentinel directly because file
content transfer uses the separate HTTP `/files` interface.

For fault/cleanup checks, compile the test-only shim with
`gcc -shared -fPIC cube-envd/tests/watch_faults.c -ldl -o "$TMPDIR/watch_faults.so"`,
launch the daemon with `LD_PRELOAD="$TMPDIR/watch_faults.so"`, then run
`python3 -B cube-envd/tests/watch_failures.py`. This covers real FUSE rejection,
injected inotify installation/read/overflow errors, an unread subscriber,
independent requests and descriptor/thread cleanup. Injected overflow is not
proof of actual kernel queue exhaustion. The shim is never linked into envd.

`PYTHONPATH=sdk/python python3 -B cube-envd/tests/filesystem_cube_sdk.py`
uses the repository CubeSandbox SDK and its existing IP override configuration
to exercise unary calls, streaming watch, uploads and downloads against the daemon.
Install the SDK dependencies first. This is not platform/proxy acceptance.

E2B SDK checks require an installed E2B Python SDK and its dependencies, a `user`
account, and a disposable user/home configured through
`CUBE_FILESYSTEM_SMOKE_USER`, `CUBE_FILESYSTEM_SMOKE_HOME` and
`CUBE_FILESYSTEM_SMOKE_DIR`. The home must be inside the smoke directory.
Run `filesystem_unary_sdk.py` and `watch_sdk.py` with that SDK's Python.
These scripts change init defaults; use a disposable daemon. Watch checks
exercise high-level polling and generated streaming RPC in JSON and protobuf.

`filesystem_root_alias.py` additionally requires a read-only container root;
it tests the symlink/`..` alias without risking writable root entries.
Start a disposable daemon there and give both the daemon and test a writable
private temporary directory, for example with Docker `--tmpfs /tmp`.
Run `python3 -B cube-envd/tests/filesystem_root_alias.py` separately.
The ignored Rust snapshot server fixtures require an explicit isolated VM
harness. Ordinary component checks do not certify snapshot/restore behavior.
