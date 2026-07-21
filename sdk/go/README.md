# cubesandbox Go SDK

Go SDK for [CubeSandbox](https://github.com/TencentCloud/CubeSandbox). It matches the current Python SDK surface: sandbox lifecycle, code execution, commands, PTY (interactive terminal), filesystem operations (read, write, list, stat, exists, remove, rename, mkdir, watch), snapshots, clone, rollback, and L7 egress policy.

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

## PTY (interactive terminal)

`sb.Pty()` opens a real pseudo-terminal for interactive programs that need a TTY — shells/REPLs, full-screen tools (`vim`, `top`), or agent-driven terminals. It mirrors the Python/Node `sandbox.pty` surface.

```go
pty := sb.Pty()

// Start an interactive login shell (80x24). Optionally set opts.User / opts.Cwd.
handle, err := pty.Create(ctx, cubesandbox.PtySize{Rows: 24, Cols: 80}, cubesandbox.PtyCreateOptions{})
if err != nil {
	panic(err)
}

// Drive it: send input, resize the window.
_ = handle.SendStdin(ctx, []byte("echo hi && stty size\n"))
_ = handle.Resize(ctx, cubesandbox.PtySize{Rows: 40, Cols: 120})

// Consume output with Wait's callback OR by ranging handle.Output() — not both,
// they share one stream. Wait blocks until the shell exits (or you call
// handle.Kill / handle.Disconnect) and returns the exit code.
code, err := handle.Wait(func(chunk []byte) {
	os.Stdout.Write(chunk)
})
fmt.Println("pty exited with", code, err)
```

Reattach to a still-running PTY from elsewhere with `pty.Connect(ctx, pid, ...)`, or control one by PID without a handle:

```go
handle, _ := pty.Create(ctx, cubesandbox.PtySize{Rows: 24, Cols: 80}, cubesandbox.PtyCreateOptions{})
handle.Disconnect() // detach without killing; the shell keeps running

again, _ := pty.Connect(ctx, handle.PID(), cubesandbox.PtyConnectOptions{})
_ = pty.SendStdin(ctx, again.PID(), []byte("ls\n"))
killed, _ := pty.Kill(ctx, again.PID()) // false if the PID already exited
_ = killed
```

| Method | Description |
|---|---|
| `Pty.Create(ctx, size, opts)` | Start `/bin/bash -i -l` with a PTY; seeds `TERM`/`LANG`/`LC_ALL` (overridable via `opts.Envs`). Streaming `process.Process/Start`. |
| `Pty.Connect(ctx, pid, opts)` | Reattach to a running PTY. Streaming `process.Process/Connect`. |
| `Pty.Kill(ctx, pid)` | `SIGKILL` a PTY; returns `false` (not an error) if the PID was not found. |
| `Pty.SendStdin(ctx, pid, data)` | Write bytes to the PTY master (`SendInput`). |
| `Pty.Resize(ctx, pid, size)` | Resize the window (`Update`). |
| `handle.Output()` | Channel of raw output chunks; closed when the stream ends. |
| `handle.Wait(onData)` | Block until exit, return the exit code; surfaces envd errors (e.g. `signal: killed`). |
| `handle.Disconnect()` | Stop receiving output without killing the PTY. |
| `handle.PID()` / `ExitCode()` / `ErrorMessage()` | PTY process ID; exit code (returns `0, false` until known); and envd end error. |
| `handle.Kill` / `SendStdin` / `Resize` | Per-handle shortcuts that target this PTY's PID. |

Consume output via **either** `Output()` **or** `Wait(onData)`, not both — they share one stream.

`PtyCreateOptions.Timeout` / `PtyConnectOptions.Timeout` (default 60s, `<= 0` uses the default) is both sent to envd as `Connect-Timeout-Ms` and enforced client-side as an idle abort that resets on every received frame; on expiry `Wait` returns an "idle" timeout error.

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
URL:  <CUBE_PROXY_SCHEME>://49983-<sandboxID>.<CUBE_SANDBOX_DOMAIN>/<envd-endpoint>
TCP:  <CUBE_PROXY_NODE_IP>:<CUBE_PROXY_PORT_HTTP>
Host: 49983-<sandboxID>.<CUBE_SANDBOX_DOMAIN>
```

The host prefix is the sandbox's internal service port: `49983` (envd) for
commands/filesystem/files/PTY, and `49999` (Jupyter) for the code interpreter
(`RunCode` / `/execute`).

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
