# Run cube-envd locally

[中文](usage_zh.md) · [Component overview](../README.md)

The component builds a standalone Linux daemon. Image publishing, template
selection, one-click installation and SDK/platform E2E integration are separate
follow-up work; this change does not install or select a daemon for the platform.

## Build and start

Run from the repository root, in a disposable Linux guest or container whose
files and processes may be changed by the API. Use the component's
[isolated test environment](../README.md#tests) for the complete suite.

```bash
make -C cube-envd build
cube-envd/target/debug/cube-envd -version
cube-envd/target/debug/cube-envd cube-version
cube-envd/target/debug/cube-envd -commit
cube-envd/target/debug/cube-envd -isnotfc -port 49983
```

If `CARGO_TARGET_DIR` is set, use its `debug/cube-envd` executable instead.
The last command runs in the foreground; use another terminal in the same
environment for the requests below. `-isnotfc` disables Firecracker MMDS/log
export tasks. Writable cgroup v2 enables envd process grouping; initialization
failure logs a warning and uses the existing no-op fallback.

## Health and file operations

```bash
export NO_PROXY=localhost,127.0.0.1,::1
export no_proxy="$NO_PROXY"
curl -fsS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:49983/health
# 204

printf 'hello from cube-envd\n你好\n' | curl -fsS \
  -X POST -H 'Content-Type: application/octet-stream' --data-binary @- \
  'http://127.0.0.1:49983/files?path=/tmp/envd-example.txt&username=root'
curl -fsS 'http://127.0.0.1:49983/files?path=/tmp/envd-example.txt&username=root'
```

These requests target the newly launched daemon directly. They do not validate
CubeProxy, template readiness, SDK routing or image lifecycle. If `/init` has
configured an access token, include its `X-Access-Token` header. See the
[API reference](cube-envd-api.md) for initialization, Process/PTY streams,
Filesystem RPC, gzip/multipart, signed file requests and error behavior.

Stop the foreground daemon with Ctrl-C after the check and remove the example
file in the disposable environment. To run process, file and protocol
regression tests, see the [isolated test instructions](../README.md#tests).
