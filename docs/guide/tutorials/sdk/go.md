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
- Perform complete filesystem operations and watch directories.
- Create snapshots, roll back, and clone concurrently.
- Manage persistent volumes and host-directory mounts.
- Pause and reconnect to sandboxes.
- Configure node placement, remote proxy access, and L3/L4 or L7 egress policy.

For detailed method descriptions and integration-test instructions, see the [Go SDK README](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/go/README.md).
