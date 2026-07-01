# cubesandbox Go SDK

Go SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox). It matches the current Python SDK surface: sandbox lifecycle, code execution, commands, filesystem operations (read, write, list, stat, exists, remove, rename, mkdir, watch), snapshots, clone, rollback, and L7 egress policy.

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
	defer client.Close()

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

`Client.Close` releases local idle HTTP connections held by the SDK client. It does not pause or destroy remote sandboxes.
Use `Sandbox.Kill` to destroy a sandbox, or `Sandbox.Pause` when you want to keep the sandbox for a later `Client.Connect`.
When `WithHTTPClient` is used with a shared `*http.Client`, `Client.Close` also closes that shared client's idle connections.

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
// Read & write
content, err := sb.Files().Read(ctx, "/etc/hosts")
err = sb.Files().Write(ctx, "/tmp/hello.txt", []byte("hi"))

// Batch write
n, err := sb.Files().WriteFiles(ctx, []cubesandbox.WriteEntry{
	{Path: "/tmp/a.txt", Data: []byte("aaa")},
	{Path: "/tmp/b.txt", Data: []byte("bbb")},
})

// Directory operations
entries, err := sb.Files().List(ctx, "/tmp")
entry, err := sb.Files().Stat(ctx, "/tmp/hello.txt")
exists, err := sb.Files().Exists(ctx, "/tmp/hello.txt")
entry, err = sb.Files().MakeDir(ctx, "/tmp/mydir")
entry, err = sb.Files().Rename(ctx, "/tmp/old.txt", "/tmp/new.txt")
err = sb.Files().Remove(ctx, "/tmp/hello.txt")

// Watch for changes
watcher, err := sb.Files().WatchDir(ctx, "/tmp")
if err != nil {
	panic(err)
}
defer watcher.Close()
for ev := range watcher.Events {
	fmt.Println(ev.Name, ev.Type) // e.g. "hello.txt" "EVENT_TYPE_CREATE"
}
```

| Method | Description |
|---|---|
| `Read(ctx, path)` | Download file content via `GET /files` |
| `Write(ctx, path, data)` | Upload via `POST /files` (octet-stream, multipart fallback) |
| `WriteFiles(ctx, entries)` | Batch write, stops on first error, returns count |
| `List(ctx, path)` | List directory entries via `ListDir` RPC |
| `Stat(ctx, path)` | File/directory metadata via `Stat` RPC |
| `Exists(ctx, path)` | `true` if path exists (Stat + 404 check) |
| `MakeDir(ctx, path)` | Create directory via `MakeDir` RPC |
| `Rename(ctx, old, new)` | Move/rename via `Move` RPC |
| `Remove(ctx, path)` | Delete file or directory via `Remove` RPC |
| `WatchDir(ctx, path)` | Stream filesystem events (Connect streaming) |

## Snapshots, Clone, Rollback

```go
snap, err := sb.CreateSnapshot(ctx, "") // POST /sandboxes/:id/snapshots

snaps, nextToken, err := client.ListSnapshots(ctx, cubesandbox.ListSnapshotsOptions{
	SandboxID: sb.SandboxID,
	Limit:     100,
})

err = client.DeleteSnapshot(ctx, snap.SnapshotID) // DELETE /templates/:id

_, err = sb.Rollback(ctx, snap.SnapshotID) // POST /sandboxes/:id/rollback

clones, err := sb.Clone(ctx, cubesandbox.CloneOptions{N: 3, Concurrency: 3})
```

`Clone` snapshots the sandbox, creates `N` sandboxes from it (capped by `Concurrency`), then deletes the ephemeral snapshot. If any create fails, all successful siblings are killed and the first error is returned. `Rollback` restarts the sandbox process and drops pooled data-plane connections so the next call reconnects.

## L7 Egress Policy

```go
sb, err := client.Create(ctx, cubesandbox.CreateOptions{
	Network: cubesandbox.NetworkOptions{
		Rules: []cubesandbox.Rule{{
			Name:  "github-api",
			Match: cubesandbox.Match{Host: "api.github.com", Scheme: "https"},
			Action: cubesandbox.Action{
				Allow:  true,
				Audit:  "metadata",
				Inject: []cubesandbox.Inject{{Header: "Authorization", Secret: "token", Format: "Bearer ${SECRET}"}},
			},
		}},
	},
})
```

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
