# cubesandbox Go SDK

Go SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox). It matches the current Python SDK surface: sandbox lifecycle, code execution, commands, and file reads only.

## Install

```bash
go get github.com/tencentcloud/CubeSandbox/sdk/go
```

## Configuration

```bash
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Optional remote data-plane access.
export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>
export CUBE_PROXY_PORT_HTTP=80
export CUBE_PROXY_SCHEME=http
export CUBE_SANDBOX_DOMAIN=cube.app
```

`NewConfigFromEnv` also accepts `E2B_API_URL` and `E2B_API_KEY`; `CUBE_API_URL` and `CUBE_API_KEY` take precedence.
`CUBE_PROXY_SCHEME` supports `http` and `https`; when omitted, port `443` defaults to `https` and other ports default to `http`.

## Create And Run Code

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

	sb, err := client.Create(ctx, cubesandbox.CreateOptions{})
	if err != nil {
		panic(err)
	}
	defer sb.Kill(ctx)

	exec, err := sb.RunCode(ctx, "x = 41\nx + 1", cubesandbox.RunCodeOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Println(exec.Text)
}
```

## Commands

```go
result, err := sb.Commands().Run(ctx, "echo hello", cubesandbox.CommandOptions{})
if err != nil {
	panic(err)
}
fmt.Println(result.Stdout, result.Stderr, result.ExitCode)
```

`Commands.Run` starts `/bin/bash -l -c <command>` through envd's `process.Process/Start` API and returns stdout, stderr, and the `EndEvent` exit code. Callers are still responsible for treating untrusted shell input carefully.

## Files

```go
content, err := sb.Files().Read(ctx, "/etc/hosts")
```

`Files.Read` downloads content through envd's `GET /files?path=...` file API.

## Pause And Connect

```go
wait := true
if err := sb.Pause(ctx, cubesandbox.PauseOptions{Wait: &wait}); err != nil {
	panic(err)
}

resumed, err := client.Connect(ctx, sb.SandboxID)
if err != nil {
	panic(err)
}
_ = resumed
```

`Sandbox.Resume` is available for compatibility but deprecated; prefer `Client.Connect`.

## Network Policy

```go
denyInternet := false
sb, err := client.Create(ctx, cubesandbox.CreateOptions{
	AllowInternetAccess: &denyInternet,
	Network: cubesandbox.NetworkOptions{
		AllowOut: []string{"151.101.0.0/16"},
		DenyOut:  []string{"0.0.0.0/0"},
	},
})
```

## Host Directory Mount

```go
sb, err := client.Create(ctx, cubesandbox.CreateOptions{
	Metadata: map[string]string{
		"hostdir-mount": `[{"hostPath":"/data/shared","mountPath":"/mnt/data"}]`,
	},
})
```

## Remote Proxy

When `CUBE_PROXY_NODE_IP` is set, data-plane requests connect directly to that IP and port while preserving the virtual sandbox host:

```text
URL:  <CUBE_PROXY_SCHEME>://49999-<sandboxID>.<CUBE_SANDBOX_DOMAIN>/<envd-endpoint>
TCP:  <CUBE_PROXY_NODE_IP>:<CUBE_PROXY_PORT_HTTP>
Host: 49999-<sandboxID>.<CUBE_SANDBOX_DOMAIN>
```

You can also set it directly:

```go
cfg := cubesandbox.Config{
	APIURL:         "http://10.0.0.1:3000",
	TemplateID:     "tpl-xxxxxxxx",
	ProxyNodeIP:    "10.0.0.1",
	ProxyPortHTTP:  80,
	ProxyScheme:    "http",
	SandboxDomain:  "cube.app",
}
client := cubesandbox.NewClient(cfg)
```

## Integration Tests

Unit tests do not require a live service:

```bash
go test ./...
```

Live integration tests are behind the `integration` build tag. They require `CUBE_API_URL`, auto-discover a READY template from `/templates` when `CUBE_TEMPLATE_ID` is unset, and use `CUBE_PROXY_NODE_IP` for remote data-plane proxying when needed.

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
export CUBE_PROXY_PORT_HTTP=80
export CUBE_PROXY_SCHEME=http
export CUBE_SANDBOX_DOMAIN=cube.app
go test -tags=integration -run Integration -count=1 ./...
```
