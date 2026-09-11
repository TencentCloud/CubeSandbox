# Guest startup and auxiliary service tests

The daemon retains Process cgroup setup, startup commands and supervision, and
adds a `socats` cgroup for loopback TCP forwarding. It scans listening IPv4/IPv6
loopback sockets and forwards them through `169.254.0.21`; it does not configure
that guest address or install socat. Missing helper executables are logged and
retried. Mandatory cgroup configuration fails before commands or listening.

Firecracker mode polls MMDS using the token handshake, projects sandbox/template
identifiers into runtime environment and `/run/e2b` markers, and exports JSON logs
to the metadata collector. Successful init requests trigger metadata refresh.
`-isnotfc` disables these background MMDS/log services, while retaining forwarding.
The exporter retries transport failures and attempts a 500 ms shutdown flush;
logs are not durable, and shutdown can discard undelivered entries. Collector HTTP
status handling remains the source behavior: a received HTTP response completes
that send. No release or external integration certification is implied.

`GET /metrics` reports the existing CPU, memory, disk and timestamp fields, with
JSON and `Cache-Control: no-store`. Sampling failure returns 500. `/metrics` and
`/envs` use ordinary token authorization; unsupported methods, including HEAD,
return 405 after authorization.

## Execution

Use the root pinned Rust toolchain, read-only source mounts, a persistent Cargo
cache and a data-disk target directory. Before any tests, arrange a **private,
writable cgroup v2 mount** with CPU/memory controllers delegated and all runner
processes moved below its root. Never bind a host cgroup mount writable. Run with
SYS_TIME absent from effective, bounding, inheritable and ambient capability
sets, and set both `NO_PROXY` and `no_proxy` to `localhost,127.0.0.1,::1`.

`cargo test --test startup_services` runs the public daemon fixtures and a
supervision check through the real server listener. Python fixtures create their
own network and mount namespaces, then install link-local addresses only there.
They require Python 3, socat, mount/unmount and namespace privileges. Use an init
process in the validation container to reap orphaned, terminated helper children.
IPv6 socket checks explicitly skip when the kernel reports EAFNOSUPPORT; the IPv6
scanner test still runs. A fixture is not a real MMDS/Firecracker integration.

The production idle timeout takes over ten minutes, so run it explicitly against
the newly built daemon in the same isolated validation environment:

```sh
ENVD_TEST_BINARY="$CARGO_TARGET_DIR/debug/cube-envd" \
  python3 cube-envd/tests/guest_idle.py -v
```

This uses actual elapsed time without changing clocks or exposing a timeout
configuration: an idle connection closes at 640 seconds while a 650-second
Process stream and an unused connection remain usable. Short Cargo unit tests
also cover overlapping requests, final response completion, errors and disconnect.

Run the complete component checks with:

```sh
make -C cube-envd ci CARGO_TARGET_DIR=/data/cubelet/dev-cache/envd-rust/targets/current
```
