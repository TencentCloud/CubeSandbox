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

# Template tried first when CreateOptions.MCP is set and TemplateID is empty;
# CUBE_TEMPLATE_ID is used if it does not exist.
export CUBE_MCP_TEMPLATE_ID=mcp-gateway
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
png, err := sb.Files().ReadBytes(ctx, "/tmp/chart.png")
err = sb.Files().Write(ctx, "/tmp/hello.txt", []byte("hi"))

// Execute all filesystem operations as a specific sandbox user.
rootFiles := sb.Files().ForUser("root")
content, err = rootFiles.Read(ctx, "/root/hello.txt")
err = rootFiles.Write(ctx, "/root/hello.txt", []byte("hi"))

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
| `ForUser(user)` | Return an immutable view that runs all filesystem operations as `user` |
| `Read(ctx, path)` | Download file content as text via `GET /files` (e2b `format=text`) |
| `ReadBytes(ctx, path)` | Download raw bytes (e2b `format=bytes`) |
| `ReadStream(ctx, path)` | Stream download; caller must `Close` (e2b `format=stream`) |
| `Write(ctx, path, data)` | Upload via `POST /files` (octet-stream, multipart fallback) |
| `WriteFrom(ctx, path, src)` | Stream an `io.Reader` in one multipart upload; caller owns the reader |
| `WriteFiles(ctx, entries)` | Batch write, stops on first error, returns count |
| `List(ctx, path)` | List directory entries via `ListDir` RPC |
| `Stat(ctx, path)` | File/directory metadata via `Stat` RPC |
| `Exists(ctx, path)` | `true` if path exists (Stat + 404 check) |
| `MakeDir(ctx, path)` | Create directory via `MakeDir` RPC |
| `Rename(ctx, old, new)` | Move/rename via `Move` RPC |
| `Remove(ctx, path)` | Delete file or directory via `Remove` RPC |
| `WatchDir(ctx, path)` | Stream filesystem events (Connect streaming) |

### Streaming uploads

Use `WriteFrom` when the content is already available as an `io.Reader`, such
as a local file. It uses fixed-size buffering instead of loading the entire file:

```go
file, err := os.Open("artifact.tar.gz")
if err != nil {
	panic(err)
}
defer file.Close()

if err := sb.Files().WriteFrom(ctx, "/tmp/artifact.tar.gz", file); err != nil {
	panic(err)
}
```

`WriteFrom` uses `POST /files` with a streaming multipart body, including for
empty files, and supports `ForUser`. It uses one demand-driven reader goroutine,
a 32 KiB transfer buffer and multipart framing, rather than a file-sized buffer.
The endpoint and any proxy must accept unknown-length bodies (chunked transfer
for HTTP/1.1). If an older endpoint does not support multipart uploads, the call
fails without replaying the reader. Consider `Write([]byte)`, which tries an
octet-stream upload before its multipart fallback, if buffering is acceptable.

The SDK consumes the source once, never closes it, and does not retry a rejected
upload or follow redirects. These guarantees concern SDK behavior; proxy retry
policies are deployment-specific. The bundled Nginx configuration allows up to two upstream attempts
(`proxy_next_upstream_tries 2`) but does not enable `non_idempotent`: by default, a POST already sent to
an upstream is not retried on another server. See Nginx's
[retry rules](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_next_upstream).

The bundled [CubeProxy configuration](../../CubeProxy/nginx.conf) sets
`client_max_body_size 256M` for the entire request body, including multipart
framing. Exceeding this limit produces HTTP 413; a 256 MiB file itself exceeds it
once framing is added. Default Nginx request buffering remains enabled, so large
uploads can consume proxy temporary disk space before forwarding. Request-body
temporary files use `client_body_temp_path`; `proxy_temp_path /data/` configures
upstream **response** temporary files, not upload storage. Deployment overrides
may change limits, buffering, paths and retries; `WriteFrom` does not change them.

With the standard `http.Transport`, including wrappers that preserve its trace
hooks, the SDK checks source consumption and the final request-write result.
An early 2xx does not override an observed failure writing the multipart footer.
Custom transports without tracing remain responsible for their write-error
contract; reported `WroteRequest` errors are preserved.

> **Transport requirement:** a transport or registered protocol handler reporting
> both `GotConn` and `WroteHeaders` must complete the lifecycle with `WroteRequest`.
> Wrappers must preserve the request context and the complete trace lifecycle.
> A missing callback produces an error, not an assumed successful upload.

Use a context deadline or an injected HTTP client's `Timeout` to bound the full
operation with the standard transport; the earlier bound applies. If neither is
set, successful-response completion has a fallback limit of `Config.RequestTimeout`
(default 30 seconds), starting when the response is received. This bounds both
remaining body consumption and the final write callback. It does not impose a
30-second limit on the upload before a response arrives. Custom transports must
honor request cancellation. The SDK does not change a shared client's settings.

> **Blocked sources:** cancellation or closing the request body releases the
> transport's read and lets the call return with the standard transport, even
> before a response arrives. It cannot terminate an arbitrary source `Read`
> (for example, a stalled filesystem mount). One reader goroutine and its buffer
> can remain until the source owner unblocks that read. Use a cancelable source,
> and ensure the pending read has exited before reusing the source after an error.

Uploads are not atomic or resumable; an error can leave a partial or truncated
file. Non-2xx responses prefer the HTTP rejection and available server message.
Error bodies are read up to 64 KiB plus one truncation-detection byte, then closed
without an unbounded drain; oversized messages are marked `[truncated]`.
An incomplete final UTF-8 character is dropped at the truncation boundary. This
shared limit also applies to other SDK HTTP errors: oversized JSON is returned
as a truncated raw prefix, so `message`/`detail` extraction and keyword-based
error classification may differ from a complete error response.
During successful-response completion, deadline and observed source/write errors
are returned. Go 1.22 legacy transport timeouts may expose `net.Error.Timeout()`
without wrapping `context.DeadlineExceeded`, matching the standard client.

For a local allocation comparison, run from `sdk/go`:

```bash
go test -run '^$' -bench '^BenchmarkFilesWrite' -benchtime=3x -benchmem .
```

The `Write` baseline separates preloaded `[]byte` from materializing a reader,
and octet-stream success from multipart fallback. Preloaded input allocation is
outside the timed region. Results include the local HTTP test server; they are
not production throughput or peak-memory measurements.

## Snapshots, Clone, Rollback

```go
snap, err := sb.CreateSnapshot(ctx, "") // POST /sandboxes/:id/snapshots

snaps, nextToken, err := client.ListSnapshots(ctx, cubesandbox.ListSnapshotsOptions{
	SandboxID: sb.SandboxID,
	Limit:     100,
})

deleted, err := client.DeleteSnapshot(ctx, snap.SnapshotID) // DELETE /templates/:id?snapshot_only=true
// `deleted` is false when the snapshot no longer exists; a name may be passed
// instead of an ID. Only snapshots are matched, never templates; several
// same-named snapshots return an error (HTTP 409), so use the `snap-` ID.

_, err = sb.Rollback(ctx, snap.SnapshotID) // POST /sandboxes/:id/rollback

clones, err := sb.Clone(ctx, cubesandbox.CloneOptions{N: 3, Concurrency: 3})

// One server-side call: the backend snapshots sb once and derives N copies.
forks, err := sb.Fork(ctx, cubesandbox.ForkOptions{Count: cubesandbox.IntPtr(3)})
```

`DeleteSnapshot` accepts a snapshot ID or name and returns `(deleted bool, err error)`; `deleted` is `false` when the snapshot does not exist. `Clone` snapshots the sandbox, creates `N` sandboxes from it (capped by `Concurrency`), then deletes the ephemeral snapshot. If any create fails, all successful siblings are killed and the first error is returned. `Rollback` restarts the sandbox process and drops pooled data-plane connections so the next call reconnects.

`Fork` returns exactly `Count` entries — each a `ForkResult` with the derived `Sandbox` or that fork's error — so partial success keeps copies without failing the call (unlike `Clone`). `Count` is 1..100, default 1 (`IntPtr` for a value, nil = default); `Timeout` is a `time.Duration` idle TTL per fork. The temporary snapshot is server-managed.

## Volumes

Persistent volumes survive sandbox lifecycles and are mounted at creation via `VolumeMounts`.

```go
// Driver is optional: when empty the backend uses its first configured
// volume plugin (e2b compatible). Name is optional too — when empty the
// server generates a UUID used as both name and volume ID.
volume, err := client.CreateVolume(ctx, cubesandbox.CreateVolumeOptions{
	Name:   "my-data",
	Driver: "cos",
}) // POST /volumes

volumes, err := client.ListVolumes(ctx)         // GET /volumes (no tokens)
volume, err = client.GetVolume(ctx, "my-data")  // GET /volumes/:id (includes token)

sb, err := client.Create(ctx, cubesandbox.CreateOptions{
	TemplateID: "base",
	VolumeMounts: []cubesandbox.VolumeMount{
		{Name: "my-data", Path: "/workspace"},
		{Name: "shared-cache", Path: "/cache", ReadOnly: true},
	},
})

err = client.DeleteVolume(ctx, "my-data") // DELETE /volumes/:id
```

For opaque plugin input, use the separate creation method (requires support in
CubeAPI, every serving CubeMaster, and the plugin; existing `CreateVolume` calls are unchanged):

```go
volume, err := client.CreateVolumeWithPrivateData(ctx, cubesandbox.CreateVolumeWithPrivateDataOptions{
	Name:        "my-data",
	Driver:      "my-storage",
	PrivateData: `{"bucket":"my-bucket","prefix":"work/","cred_ref":"account-7"}`,
}) // POST /v2/volumes
```

The `private_data` create API requires CubeAPI, every serving CubeMaster, and
Cubelet at **0.8.0 or newer**, a plugin that supports this protocol, and the Go
SDK at **0.8.0 or newer**.
`PrivateData` must be valid UTF-8, limited to 1024 bytes, contain no NUL, and
contain no characters outside the BMP. Invalid input is rejected before any
HTTP request. Valid input is passed unchanged.
Use credential references, never plaintext credentials. Empty strings are
forwarded; API versions without `/v2/volumes` return an error without a legacy fallback.
The returned `VolumeInfo` works with the existing query, delete and mount APIs.

Upgrade order: check or upgrade the Controller/Node plugins for Create input A
and Attach/Destroy state B, upgrade **all CubeMasters serving volume operations**,
then deploy the new CubeAPI and begin using this method. A new API connected to
an old Master can return 201 while silently dropping A; the old Master also omits
B on Destroy. The SDK does not detect this mixed-version deployment. Keep compatible
Masters serving existing A/B-dependent volumes during any rollback. See the
[plugin upgrade guide](../../docs/guide/volume-plugin.md#private_data-upgrade-order).

Deleting a volume does not auto-detach it: destroy the sandboxes using it first, otherwise the server refuses with an error matching `ErrVolumeInUse`. A delete of a missing volume matches `ErrVolumeNotFound`, so idempotent cleanup can ignore that case:

```go
if err := client.DeleteVolume(ctx, "my-data"); err != nil {
	switch {
	case errors.Is(err, cubesandbox.ErrVolumeInUse):
		// still mounted — destroy the sandboxes using it first
	case errors.Is(err, cubesandbox.ErrVolumeNotFound):
		// already gone
	default:
		return err
	}
}
```

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

## Is Running

`Sandbox.IsRunning` checks the control-plane `state` first: `paused`/`pausing`
return `false` without touching the data plane (so a probe never auto-resumes
the sandbox). For `running`/`unknown` it probes envd `GET /health` on port
49983, returning `false` on 404/502/503/410 and on transport errors, raising
only on server errors such as 403/500.

```go
ok, err := sb.IsRunning(ctx)
if err != nil {
	panic(err)
}
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

## MCP Gateway (Preview)

Set `MCP` to start an E2B-compatible MCP gateway inside the sandbox. It needs a template that ships `mcp-gateway`; when `TemplateID` is empty, the alias `mcp-gateway` is tried first (override with `CUBE_MCP_TEMPLATE_ID` or `Config.MCPTemplateID`), then `CUBE_TEMPLATE_ID` if that template does not exist. An error matching `ErrTemplateNotFound` is returned when neither exists. A failed start returns an error wrapping `ErrMCPGateway` and kills the sandbox. See [MCP Gateway](../../docs/guide/mcp-gateway.md).

```go
sb, err := client.Create(ctx, cubesandbox.CreateOptions{
	MCP: cubesandbox.MCPServers{"duckduckgo": map[string]any{}},
})
if err != nil {
	return err
}
url := sb.GetMCPURL()               // streamable HTTP endpoint
token, err := sb.GetMCPToken(ctx)   // send as "Authorization: Bearer <token>"
```

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

Customize the Host forwarded to user services with a per-sandbox template:

```go
maskRequestHost := "localhost:${PORT}"
sb, err := client.Create(ctx, cubesandbox.CreateOptions{
	Network: cubesandbox.NetworkOptions{
		MaskRequestHost: &maskRequestHost,
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

To verify streaming uploads through the configured CubeProxy and envd:

```bash
go test -tags=integration -run '^TestIntegrationFilesWriteFrom$' -v -count=1 -timeout=5m .
```

The selected template must provide `root` and `nobody`, as required by the
filesystem user-isolation tests. This test uploads empty and binary files,
truncates an existing file with an empty upload, checks a destination containing
URL-special characters, and verifies ownership for both users. It reads back each
destination specified by the `path` query (the multipart filename is `upload`)
and checks its size and SHA-256. A skip due to an unset `CUBE_API_URL` / `E2B_API_URL`
is not a successful live verification.
