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

`ForUser` also applies to this method. The SDK sends one multipart request and
never retries it or follows redirects, since the reader may not be replayable.
The caller retains ownership of the reader. The endpoint and proxy must accept
unknown-length request bodies (chunked transfer for HTTP/1.1).

With the standard HTTP transport, the method also waits for the request-write
result. Custom transports remain responsible for reporting their own write
errors; HTTP trace hooks are not required for them.

The earlier of the context deadline and an injected HTTP client's `Timeout`
bounds the full operation, including the request-body completion wait. An
arbitrary blocked source `Read` must be unblocked by the caller or support
cancellation itself; ensure it has exited before reusing the source after an
error. Uploads are not atomic: an error can leave a partial or truncated
destination file. Existing `Write([]byte)` behavior is unchanged.

For detailed method descriptions and integration-test instructions, see the [Go SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/go/README.md).
