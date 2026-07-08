<p align="center">
  <strong>@cubesandbox/sdk</strong> — Node.js / TypeScript SDK for CubeSandbox
</p>

<p align="center">
  <a href="https://github.com/TencentCloud/CubeSandbox"><img src="https://img.shields.io/badge/CubeSandbox-GitHub-blue" alt="CubeSandbox" /></a>
  <a href="../../LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green" alt="Apache 2.0" /></a>
  <img src="https://img.shields.io/badge/Node.js-18%2B-blue" alt="Node.js 18+" />
  <img src="https://img.shields.io/badge/version-0.3.0-orange" alt="v0.3.0" />
</p>

---

`@cubesandbox/sdk` is the official Node.js / TypeScript SDK for
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox). It provides a
Promise-based interface to create sandboxes, execute code, run shell commands,
manage files, and control the full sandbox lifecycle — including pause/resume
with memory snapshot. It matches the Python SDK's surface with idiomatic
JavaScript naming (camelCase, `async`/`await`).

## Installation

```bash
npm install @cubesandbox/sdk
```

Requires Node.js 18 or newer (uses the built-in fetch/FormData/Blob and the
`undici` HTTP dispatcher). Ships dual ESM + CommonJS builds with TypeScript
type definitions.

## Quick Start

Set the required environment variables:

```bash
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Required for remote access (bypasses DNS for *.cube.app)
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
```

Run your first sandbox:

```ts
import { Sandbox } from "@cubesandbox/sdk";

const sb = await Sandbox.create();
try {
  const result = await sb.runCode("1 + 1");
  console.log(result.text); // "2"
} finally {
  await sb.kill();
}
```

With TypeScript 5.2+ / Node 20+ you can use explicit resource management so the
sandbox is destroyed automatically:

```ts
await using sb = await Sandbox.create();
const result = await sb.runCode("1 + 1");
console.log(result.text); // "2"
```

## Features

### Execute code

```ts
import { Sandbox } from "@cubesandbox/sdk";

const sb = await Sandbox.create();

// Simple expression
let result = await sb.runCode("x = 42\nx * 2");
console.log(result.text); // "84"

// Capture stdout
result = await sb.runCode('print("hello")');
console.log(result.logs.stdout); // ["hello\n"]

// Stream output in real time
await sb.runCode("for i in range(3): print(i)", {
  onStdout: (msg) => console.log("out:", msg.text),
});

await sb.kill();
```

`runCode` and `commands.run` both accept a `timeoutMs` (with `timeout` as a
millisecond alias for E2B parity; `timeoutMs` wins when both are given). Omit it
for no timeout. **The two APIs interpret it differently:**

- **`runCode`** — a pure **idle / read** timeout: the timer resets on every
  chunk received, so a long-running task that keeps producing output is never
  cut off; only a stream that goes silent for longer than `timeoutMs` aborts.
- **`commands.run`** — the same client-side idle reset **plus** the value is
  sent to envd as `Connect-Timeout-Ms`, which envd enforces as a **hard
  wall-clock deadline** for the whole command. So a command is aborted once it
  either goes silent for longer than `timeoutMs` **or** runs longer than
  `timeoutMs` in total — whichever comes first (this matches the Python SDK).
  Use a larger `timeoutMs` (or none) for genuinely long-running commands.

```ts
// runCode: aborts only if the sandbox produces no output for 30s straight.
await sb.runCode("long_running_job()", { timeoutMs: 30_000 });

// commands.run: aborts after 30s of silence OR 30s total wall-clock.
await sb.commands.run("./build.sh", { timeoutMs: 30_000 });
```

### Run shell commands

```ts
const result = await sb.commands.run("echo hello cube");
console.log(result.stdout); // "hello cube\n"
console.log(result.exitCode); // 0
```

When `user` is omitted the SDK sends requests as `root` for compatibility with
envd versions that reject process/file requests without an explicit user.

### Persistent variables within a sandbox

Variables assigned in one `runCode` call persist for the lifetime of the
sandbox — no separate context object needed:

```ts
await sb.runCode("x = 100");
const result = await sb.runCode("x + 1");
console.log(result.text); // "101"
```

### Pause & resume

```ts
const sb = await Sandbox.create();

// Pause — preserves memory snapshot, polls until state=paused
await sb.pause(); // wait=true, timeoutMs=30000 by default
await sb.pause({ wait: false }); // fire-and-forget
await sb.pause({ timeoutMs: 60000, intervalMs: 500 }); // custom poll params

// Resume by connecting — auto-resumes a paused sandbox
const sb2 = await Sandbox.connect(sb.sandboxId);
```

### Network policy

Two layers can be combined inside `network`:

- **L3/L4** — `allowOut` / `denyOut` lists of CIDRs or hostnames.
- **L7** — `rules` for host / path / SNI matching, audit, and credential
  injection using the typed `Rule` / `Match` / `Action` / `Inject` shapes.

```ts
import { Sandbox, type Rule } from "@cubesandbox/sdk";

const rules: Rule[] = [
  {
    name: "deepseek_api",
    match: {
      scheme: "https",
      host: "api.deepseek.com",
      method: ["POST"],
      path: "/v1/chat",
      sni: "api.deepseek.com",
    },
    action: {
      allow: true,
      audit: "metadata",
      inject: [
        { header: "Authorization", format: "Bearer ${SECRET}", secret: "sk_xxxxxxxx" },
      ],
    },
  },
];

const sb = await Sandbox.create({
  network: { allowOut: ["172.67.0.0/16"], rules },
});
await sb.runCode("import requests; requests.post('https://api.deepseek.com/v1/chat')");
```

Rules are evaluated **first-match-wins** in list order. Credential injection
only runs on HTTPS requests where SNI and Host match (server-enforced).

#### E2B per-host request transforms (compat shape)

For drop-in compatibility with E2B's per-host request transforms,
`network.rules` also accepts a host-keyed mapping. Each `transform.headers`
entry is converted into a CubeEgress L7 rule that injects the same headers on
outbound HTTPS requests to that host:

```ts
const sb = await Sandbox.create({
  network: {
    // The host must still be referenced via allowOut — registering a rule
    // alone does not grant egress.
    allowOut: ["api.example.com"],
    denyOut: ["0.0.0.0/0"],
    rules: {
      "api.example.com": [{ transform: { headers: { "X-Header": "Content" } } }],
    },
  },
});
```

Pass either a list of `Rule` (typed) **or** a host-keyed mapping (E2B-shaped)
on a single `create` call — not both.

### Filesystem

```ts
// Read & write
await sb.files.write("/tmp/hello.txt", "Hello, world!");
console.log(await sb.files.read("/tmp/hello.txt")); // "Hello, world!"

// Batch write
await sb.files.writeFiles([
  { path: "/tmp/a.txt", data: "aaa" },
  { path: "/tmp/b.txt", data: new Uint8Array([98, 98, 98]) }, // bytes also accepted
]);

// Directory operations
await sb.files.makeDir("/tmp/mydir");
const entries = await sb.files.list("/tmp");
const info = await sb.files.stat("/tmp/hello.txt");
console.log(await sb.files.exists("/tmp/hello.txt")); // true
await sb.files.rename("/tmp/hello.txt", "/tmp/renamed.txt");
await sb.files.remove("/tmp/renamed.txt");

// Watch for changes
const watcher = await sb.files.watchDir("/tmp");
for await (const event of watcher) {
  console.log(event.name, event.type); // e.g. "a.txt" "EVENT_TYPE_CREATE"
  break;
}
watcher.close();
```

### PTY (interactive terminal)

Start an interactive login shell over a pseudo-terminal, stream its raw output,
and drive it with keystrokes / resizes — mirrors the Python SDK's `sandbox.pty`.

```ts
// Start a PTY (defaults to `/bin/bash -i -l`, TERM=xterm-256color)
const handle = await sb.pty.create({ rows: 24, cols: 80 });
console.log("pty pid:", handle.pid);

// Feed it input (string or bytes)
await handle.sendStdin("echo hello\n");
await handle.resize({ rows: 40, cols: 120 });

// Stream raw output as it arrives (each chunk is a Uint8Array)
const decoder = new TextDecoder();
for await (const chunk of handle) {
  process.stdout.write(decoder.decode(chunk));
  if (/* your exit condition */ false) break;
}

// …or drain to completion with a callback and get the exit code
const exitCode = await handle.wait((chunk) => process.stdout.write(chunk));

// Detach without killing (reattach later with sb.pty.connect(pid))
handle.disconnect();
const reattached = await sb.pty.connect(handle.pid);

// Ad-hoc control by pid without holding a handle
await sb.pty.sendStdin(handle.pid, "exit\n");
await sb.pty.resize(handle.pid, { rows: 50, cols: 132 });
await sb.pty.kill(handle.pid); // → boolean (false if it no longer exists)
```

### Snapshots, clone, rollback

```ts
const snap = await sb.createSnapshot(); // POST /sandboxes/:id/snapshots

const { snapshots, nextToken } = await Sandbox.listSnapshots({
  sandboxId: sb.sandboxId,
  limit: 100,
});

await Sandbox.deleteSnapshot(snap.snapshotId); // DELETE /templates/:id

await sb.rollback(snap.snapshotId); // POST /sandboxes/:id/rollback

const clones = await sb.clone(3, { concurrency: 3 });
```

`clone` snapshots the sandbox, creates `n` sandboxes from it (capped by
`concurrency`), then deletes the ephemeral snapshot. If any create fails, all
successful siblings are killed and the first error is re-thrown.

### List & health check

```ts
console.log(await Sandbox.health()); // { status: "ok", sandboxes: 4 }
console.log(await Sandbox.list()); // list of running sandboxes
console.log(await Sandbox.listV2()); // v2 API (supports filtering)
```

### Templates

```ts
import { Template } from "@cubesandbox/sdk";

const templates = await Template.list();
const build = await Template.build({ image: "python:3.11-slim" });
const detail = await Template.get("tpl-xxxxxxxxxxxxxxxxxxxxxxxx");
await Template.delete("tpl-xxx");
```

## Configuration

| Environment Variable | Required | Default | Description |
|---|:---:|---|---|
| `CUBE_API_URL` | | `http://127.0.0.1:3000` | CubeAPI management plane address (defaults to localhost) |
| `CUBE_API_KEY` | remote | — | Management-plane API key → `Authorization: Bearer` (falls back to `E2B_API_KEY`; omitted when unset) |
| `CUBE_TEMPLATE_ID` | ✅ | — | Template ID for sandbox creation |
| `CUBE_PROXY_NODE_IP` | remote | — | CubeProxy node IP, bypasses DNS for `*.cube.app` |
| `CUBE_PROXY_PORT_HTTP` | | `80` | CubeProxy HTTP port |
| `CUBE_PROXY_SCHEME` | | `http` | Data-plane scheme; normalized to `http` / `https` (unknown values → `https` when port is 443, else `http`) |
| `CUBE_SANDBOX_DOMAIN` | | `cube.app` | Sandbox domain suffix |

You can also pass a `Config` (or plain options object) directly:

```ts
import { Config, Sandbox } from "@cubesandbox/sdk";

const config = new Config({
  apiUrl: "http://10.0.0.1:3000",
  templateId: "tpl-xxxxxxxxxxxxxxxxxxxxxxxx",
  proxyNodeIp: "10.0.0.1",
});
const sb = await Sandbox.create({ config });
console.log((await sb.runCode("2 ** 10")).text); // "1024"
```

Or, for convenience, pass the connection settings inline on `create` (handy
when talking to different CubeAPIs from the same process):

```ts
const sb = await Sandbox.create({
  apiUrl: process.env.CUBE_API_URL,
  templateId: process.env.CUBE_TEMPLATE_ID, // alias for `template`
  proxyNodeIp: process.env.CUBE_PROXY_NODE_IP,
});
```

Precedence for these settings is: **inline field > `config` > `CUBE_*` env
var**. Inline fields accepted by `create`: `apiUrl`, `proxyNodeIp`,
`proxyPort`, `proxyScheme`, `sandboxDomain`, `requestTimeoutMs`, and
`templateId` (alias for `template`).

## API Reference

### `Sandbox` — static methods

| Method | Description |
|---|---|
| `Sandbox.create(options?)` | `POST /sandboxes` — create a new sandbox |
| `Sandbox.connect(sandboxId, options?)` | `POST /sandboxes/:id/connect` — connect (auto-resumes if paused) |
| `Sandbox.list(config?)` | `GET /sandboxes` — list running sandboxes (v1) |
| `Sandbox.listV2(config?)` | `GET /v2/sandboxes` — list sandboxes (v2) |
| `Sandbox.health(config?)` | `GET /health` — service health check |
| `Sandbox.listSnapshots(options?)` | `GET /snapshots` — list snapshots (paginated) |
| `Sandbox.deleteSnapshot(snapshotId, options?)` | `DELETE /templates/:id` — delete a snapshot |

### `Sandbox` — instance methods

| Method | Description |
|---|---|
| `sb.runCode(code, options?)` | `POST /execute` — execute code, returns `Execution` |
| `sb.getInfo()` | `GET /sandboxes/:id` — get sandbox state and metadata |
| `sb.pause(options?)` | `POST /sandboxes/:id/pause` — pause sandbox |
| `sb.resume(timeout?)` | `POST /sandboxes/:id/resume` — resume (deprecated, use `connect`) |
| `sb.kill()` | `DELETE /sandboxes/:id` — destroy sandbox |
| `sb.createSnapshot(name?)` | `POST /sandboxes/:id/snapshots` — create a snapshot |
| `sb.rollback(snapshotId)` | `POST /sandboxes/:id/rollback` — roll back to a snapshot |
| `sb.clone(n?, options?)` | Snapshot + create ×n + cleanup |
| `sb.getHost(port)` | Return virtual hostname `{port}-{id}.{domain}` |
| `sb.commands` / `sb.files` / `sb.pty` | Shell / filesystem / PTY namespaces (see below) |

### `sb.files` — Filesystem

| Method | Description |
|---|---|
| `read(path, options?)` | Download file content via `GET /files` |
| `write(path, data, options?)` | Upload via `POST /files` (octet-stream, multipart fallback) |
| `writeFiles(files, options?)` | Batch write, stops on first error, returns count |
| `list(path)` | List directory entries |
| `stat(path)` | File/directory metadata |
| `exists(path)` | `true` if path exists (stat + 404 check) |
| `makeDir(path)` | Create directory |
| `rename(old, new)` | Move/rename |
| `remove(path)` | Delete file or directory |
| `watchDir(path)` | Stream filesystem events → async-iterable `Watcher` |

### `sb.commands` — Shell commands

| Method | Description |
|---|---|
| `commands.run(cmd, options?)` | Run a shell command over envd's Connect process API; returns `{ stdout, stderr, exitCode }`. Options: `user`, `cwd`, `envs`, `timeoutMs`/`timeout` (see [timeout semantics](#execute-code)) |

### `sb.pty` — Pseudo-terminals

| Method | Description |
|---|---|
| `pty.create(size, options?)` | Start a PTY (`/bin/bash -i -l`) → `PtyHandle`. Options: `user`, `cwd`, `envs`, `timeoutMs` (envd deadline + client idle abort, default 60000) |
| `pty.connect(pid, options?)` | Reattach to a running PTY → `PtyHandle` |
| `pty.kill(pid, requestTimeoutMs?)` | `SIGKILL` a PTY → `boolean` (`false` if not found) |
| `pty.sendStdin(pid, data, requestTimeoutMs?)` | Write input (string or bytes) to a PTY |
| `pty.resize(pid, size, requestTimeoutMs?)` | Resize a running PTY |

`PtyHandle` is an async-iterable of `Uint8Array` output chunks with: `pid`,
`exitCode`, `error`, `wait(onData?)` → exit code, `disconnect()` (detach without
killing), and per-handle `kill()` / `sendStdin(data)` / `resize(size)`.

### `Template` — static methods

| Method | Description |
|---|---|
| `Template.list(options?)` | `GET /templates` — list all templates |
| `Template.get(templateId, options?)` | `GET /templates/:id` — template detail + build history |
| `Template.build(options)` | `POST /templates` — build a new template from a container image |
| `Template.rebuild(templateId, options?)` | `POST /templates/:id` — rebuild an existing template |
| `Template.getBuildStatus(templateId, buildId, options?)` | `GET /templates/:id/builds/:buildId/status` — poll build status |
| `Template.getBuildLogs(templateId, buildId, options?)` | `GET /templates/:id/builds/:buildId/logs` — fetch build logs |
| `Template.delete(templateId, options?)` | `DELETE /templates/:id` — delete a template permanently |

### `Execution` object

| Property | Type | Description |
|---|---|---|
| `.text` | `string \| undefined` | Final expression value (main result) |
| `.logs.stdout` | `string[]` | All stdout lines |
| `.logs.stderr` | `string[]` | All stderr lines |
| `.error` | `ExecutionError \| null` | Exception info if execution failed |
| `.results` | `Result[]` | All result events |

## DNS bypass (remote access)

When running outside the CubeSandbox node, `*.cube.app` cannot be resolved by
the OS DNS. Set `CUBE_PROXY_NODE_IP` to route all data-plane connections
directly to that IP while preserving the virtual `Host` header for CubeProxy
routing — the Node equivalent of `curl --resolve host:port:ip`, implemented
with a custom `undici` dispatcher.

```
Without CUBE_PROXY_NODE_IP:
  SDK → OS DNS (*.cube.app) → CubeProxy

With CUBE_PROXY_NODE_IP:
  SDK → TCP direct to CUBE_PROXY_NODE_IP:80
        Host: 49999-{sandboxID}.cube.app (preserved for routing)
```

When `CUBE_PROXY_SCHEME=https`, the TLS SNI / certificate name is also pinned
to the virtual `*.cube.app` host (not the proxy IP), so CubeProxy's TLS routing
and certificate validation keep working through the IP override.

## Development

```bash
npm install
npm run build       # tsup → dist (ESM + CJS + .d.ts)
npm run typecheck   # tsc --noEmit
npm test            # vitest (unit tests, no live service required)
```

Live integration tests are gated behind `CUBE_RUN_INTEGRATION=1` and require a
running CubeAPI:

```bash
export CUBE_RUN_INTEGRATION=1
export CUBE_API_URL=http://<your-cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-node-ip>
npm test
```

## License

Apache-2.0 © 2026 Tencent Inc.
