# Go SDK

The CubeSandbox Go SDK covers sandbox lifecycle management, code execution, commands, PTY sessions, filesystem operations, snapshots, cloning, rollback, volumes, and network policy.

## Installation

```bash
go get github.com/tencentcloud/CubeSandbox/sdk/go
```

## Configuration

```bash
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Optional remote data-plane access
export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>
export CUBE_PROXY_PORT_HTTP=80
export CUBE_PROXY_SCHEME=http
export CUBE_SANDBOX_DOMAIN=cube.app
```

`NewConfigFromEnv` also reads `E2B_API_URL` and `E2B_API_KEY`. Cube-specific variables take precedence.

## Quick start

```go
package main

import (
    "context"
    "fmt"

    cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

func main() {
    ctx := context.Background()
    client := cubesandbox.NewClient(cubesandbox.NewConfigFromEnv())
    defer client.Close()

    sandbox, err := client.Create(ctx, cubesandbox.CreateOptions{})
    if err != nil {
        panic(err)
    }
    defer sandbox.Kill(ctx)

    result, err := sandbox.RunCode(ctx, "x = 41\nx + 1", cubesandbox.RunCodeOptions{})
    if err != nil {
        panic(err)
    }
    fmt.Println(result.Text)
}
```

`Client.Close` releases local idle HTTP connections; it does not destroy remote sandboxes. Use `Sandbox.Kill` for cleanup or `Sandbox.Pause` to preserve a sandbox for later reconnection.

## Main capabilities

- Execute code and shell commands.
- Create, reconnect to, resize, and control interactive PTYs.
- Perform complete filesystem operations, stream file uploads and downloads, and watch directories.
- Create snapshots, roll back, and clone concurrently.
- Manage persistent volumes and host-directory mounts.
- Pause and reconnect to sandboxes.
- Configure node placement, remote proxy access, and L3/L4 or L7 egress policy.

For detailed method descriptions and integration-test instructions, see the [Go SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/go/README.md).

## Streaming file uploads

`Files.WriteFrom(ctx, path, src io.Reader)` uploads from a reader without
buffering the entire file. For example, after opening a local artifact with
`os.Open` (import `os`):

```go
file, err := os.Open("artifact.tar.gz")
if err != nil {
    panic(err)
}
defer file.Close()

if err := sandbox.Files().WriteFrom(ctx, "/tmp/artifact.tar.gz", file); err != nil {
    panic(err)
}
```

`ForUser` also applies. The SDK consumes the reader once, never closes it, and
does not retry a rejected upload or follow redirects. It uses one demand-driven
reader goroutine, a 32 KiB buffer and multipart framing. Endpoints and proxies
must accept unknown-length bodies (chunked transfer for HTTP/1.1). For older
endpoints without multipart support, consider `Write([]byte)` and its initial
octet-stream request if buffering is acceptable; `WriteFrom` does not replay.

With the standard HTTP transport and trace-preserving wrappers, the SDK waits
for the final request-write result. **A custom transport reporting `GotConn` and
`WroteHeaders` must also report `WroteRequest`.** Missing callbacks produce errors.
Without tracing, custom transports remain responsible for their write errors.

Use a context deadline or injected HTTP client `Timeout` to bound the full
operation with the standard transport; the earlier bound applies. If neither is
set, completion after a successful response uses `Config.RequestTimeout`
(default 30 seconds), including remaining body consumption and the write
callback. This fallback starts at the response, not the start of the upload.
Custom transports must honor cancellation.

**A blocked source may retain one reader goroutine and its buffer after the call
returns.** Cancellation releases the transport's read but cannot interrupt an
arbitrary source `Read`, such as one on a stalled mount. The owner must unblock
that read and wait for it to exit before reusing the source. Uploads are not
atomic; failures can leave partial or truncated files.

CubeProxy defaults to buffering requests, potentially using temporary disk space.
The 256 MiB body limit includes multipart framing; exceeding it produces HTTP 413.
Request-body temporary files use `client_body_temp_path`; `proxy_temp_path /data/`
is for responses. SDK retry guarantees do not govern proxies: the bundled Nginx
configuration does not enable `non_idempotent`, so it normally does not retry a
POST already sent to an upstream. Deployment overrides can change these rules.
Error responses are limited to 64 KiB plus one detection byte and marked when
truncated, dropping an incomplete final UTF-8 character. This shared SDK limit
can change field extraction and error classification for oversized responses.
See the Go SDK README for the full transport contract and limitations.
