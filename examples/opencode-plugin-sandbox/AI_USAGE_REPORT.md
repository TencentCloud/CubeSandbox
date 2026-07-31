# AI Usage Self-Report — OpenCode Plugin Integration

Task: [#644](https://github.com/TencentCloud/CubeSandbox/issues/644) —
CubeSandbox integration with Claude Code / CodeBuddy / OpenCode.
Sub-direction claimed: **OpenCode**.

This report records how an AI assistant was used, and — more importantly — the
points where its output was wrong and had to be corrected against the real
system. Statements that were never verified are marked as such.

## 1. Tools and division of labour

| Tool | Used for |
|---|---|
| Large-language-model coding assistant | Reading upstream source, drafting code and docs, generating test cases |
| Real CubeSandbox v0.6.0 deployment | Every behavioural claim in the docs |
| Node.js / Python | Running the test suite and inspecting bytes |
| `bash` | End-to-end verification of shell quoting |

The AI produced first drafts. Everything that ended up in this PR was checked
against either the upstream repository at a pinned commit or a running
deployment. Nothing was accepted because it looked plausible.

## 2. Research phase

The starting question was not "how do I write this integration" but "what has
already been rejected, and why". That reframing came from reading the issue's
PR history rather than the issue text.

Findings that shaped the design:

- Roughly twenty PRs reference #644; none had been merged at the time of
  writing.
- Maintainer feedback on a closed PR described running the agent directly inside
  the sandbox as *"essentially just running any application inside the
  sandbox"* — i.e. redundant.
- Another closed PR was described as *"not particularly creative"*, with a
  hook-based PR named as the counter-example.
- Several PRs were closed after a request for **screenshots and logs from a real
  cluster** went unanswered.

The AI's initial instinct was the common approach: build a Dockerfile, install
OpenCode inside it, drive it from a host script. Reading the maintainer's own
words ruled that out before any code was written.

Cross-checking the three existing OpenCode PRs confirmed all of them ship
`Dockerfile` + `run_opencode.py` + `resume_opencode.py`, and none uses the
plugin mechanism. The `tool.execute.before` hook was then verified in the
[OpenCode plugin documentation](https://opencode.ai/docs/plugins/) — it exists,
`output.args` is mutable, and throwing blocks the call.

## 3. Comparison phase

Two designs were weighed explicitly:

| | Agent inside sandbox | Redirect `bash` only |
|---|---|---|
| Isolation of shell commands | Yes | Yes |
| Agent can edit the real project | **No** — works on a copy | Yes |
| Access to git credentials / editor state | Lost | Kept |
| Maintainer's stated view | "redundant" | hook approach called impressive |

The second was chosen. The reasoning is written into the docs rather than left
implicit, because the trade-off — `read`/`write`/`edit` stay on the host — is a
real limitation a reader deserves to see stated plainly.

## 4. Verification phase

A real deployment was built specifically for this work: CubeSandbox v0.6.0 on
Ubuntu 24.04.4 (WSL2), `/data/cubelet` on XFS with `reflink=1`, 14 systemd units
active, a template in `READY` state.

The verification that matters most is the kernel comparison:

```
sandbox guest kernel : 6.6.1199-0009-03_2.0.1
host kernel          : 6.18.33.2-microsoft-standard-WSL2
```

Different kernels. A container sharing the host kernel would report an identical
version, so this single observation distinguishes real virtualisation from
namespace isolation. Every performance figure quoted in the docs (~1.0 s to
create a sandbox, ~2.4 s for a full cycle) comes from this deployment.

## 5. Corrections made to AI output

This is the substantive part of the report. Each item is a case where the AI's
output was accepted on first read and later proven wrong by running it.

### 5.1 Module system mismatch — would have broken at load time

The draft plugin used CommonJS `require()` together with ESM `export`, and
`__dirname`.

OpenCode loads plugins as ES modules. Actually importing the file produced
`ReferenceError: require is not defined`; `__dirname` does not exist in ESM
either. **The plugin would have failed for every user on first load.**

Fixed by switching to `import` and deriving the directory from
`fileURLToPath(import.meta.url)`.

Lesson: `node --check` validates syntax, not module semantics. It passed. Only
a real `import` caught this.

### 5.2 Configuration variable silently ignored

The draft read the interpreter once at module scope:

```js
const PYTHON = process.env.CUBE_OPENCODE_PYTHON || "python3";
```

OpenCode keeps a plugin resident for the whole session, so a value exported
after the editor started would be ignored. On Ubuntu 24.04 — where PEP 668
forces a virtualenv — this is precisely the documented setup path, so the
documented instructions would not have worked.

Fixed by reading the variable per call.

### 5.3 A test assertion that was logically incapable of passing

The AI's injection test asserted that after stripping escape sequences, the
payload should be absent:

```js
assert.ok(!/;\s*rm -rf \/tmp\/pwned/.test(cmd.replace(/'\\''/g, "")));
```

This is nonsense. The payload is preserved *as data* — that is the whole point
of quoting it. Removing the escape sequences leaves exactly the payload text, so
the assertion can never pass, no matter how correct the code is.

Rewritten to assert the property that actually matters: parse the rewritten
command the way a POSIX shell would and check the original text lands in exactly
one argv element.

Implementing that parser exposed a second subtlety. The POSIX idiom for
embedding a quote inside a single-quoted string is four characters — quote,
backslash, quote, quote — so a parser that only handles quoted runs and ignores
backslash escapes decodes it incorrectly. The first version of the parser had
that bug and produced a false failure.

### 5.4 A false alarm caused by the debugging method

While investigating 5.3, a debug script written via a bash heredoc appeared to
show that `shellQuote` emitted three quotes instead of the correct escape
sequence — suggesting a serious bug in the plugin.

It was wrong. **The heredoc had consumed one backslash level**, so the debug
script under test was not the code under test.

Reading the source file's actual code points settled it:

```
... 34,39,92,92,39,39,34 ...     (92 = backslash)
```

The source was correct all along. The investigation method was faulty.

Verified afterwards with real `bash` argv parsing:

```
[--command]
[echo A'; touch /tmp/PWNED_MARKER; echo ']
```

The payload occupies exactly one argument, and `/tmp/PWNED_MARKER` was never
created — the `touch` did not execute.

Lesson: when debugging escaping, do not add another escaping layer. Write test
files with a tool that does not reinterpret backslashes, or compare code points.

### 5.5 Docs claimed a feature that was not implemented

An early draft stated that one MicroVM is reused per session. Reading the
backend showed each call creates a fresh sandbox; only the *state* (cwd, env) is
carried across calls.

The docs now say so, and "a MicroVM per call, not per session" is listed first
under Known limitations. Claiming VM reuse would have been the kind of statement
a reviewer catches immediately by reading the code.

### 5.6 Command-line parsing that missed the real output format

Not part of the deliverable, but worth recording: a helper script used to drive
the deployment failed to parse `cubemastercli` output because the AI-generated
regex expected `key: value` and `"key":"value"`, while the CLI actually emits
`job_id=<uuid> template_id=tpl-<hex>`. The template had in fact been built
successfully; only the parser was broken.

## 6. What is verified and what is not

**Verified on a real deployment**

- CubeSandbox v0.6.0 installs and runs on WSL2 with loopback XFS + reflink
- Template creation reaches `READY`; sandbox creation and `run_code` succeed
- Guest kernel differs from host kernel
- All timings quoted in the docs
- Plugin loads as an ES module; hook rewrites `bash` and leaves other tools alone
- Quote escaping survives real `bash` argv parsing
- 19/19 offline assertions pass

**Not verified**

- End-to-end use inside a live OpenCode session. The hook contract, the rewrite,
  and the backend were each exercised directly, but the full editor loop was
  not. The session-id key name in particular is probed defensively for this
  reason.
- Behaviour across multiple OpenCode versions. Only the documented hook
  contract was relied on.
- Concurrent load. The lock file logic is unit-tested in isolation, not under
  real parallel calls from the editor.
- Long-running commands near the timeout boundary.
- Multi-node CubeSandbox clusters. Single-node only.

## 7. How AI was and was not useful

**Genuinely useful**

- Reading the upstream repository quickly: source layout, template pipeline,
  DNS handling, service topology
- Drafting boilerplate — argument parsing, error messages, doc scaffolding
- Enumerating edge cases worth testing (empty command, idempotence, malicious
  session id)

**Where it was unreliable**

- Environment assumptions it could not see: the module system, PEP 668, WSL's
  `resolv.conf` rewriting
- Assertion logic that looks like a test but cannot fail meaningfully (5.3)
- Filling gaps with plausible-sounding behaviour rather than flagging them
  (5.5) — the most dangerous failure mode, because the output reads as
  confident

**Process that caught the errors**

Every draft was executed, not reviewed. Section 5 lists five defects; none was
visible by reading the code, and four were found by running it. The remaining
one was found by reading bytes after a debugging method produced a false result.

## 8. Reproducing the checks

```bash
# Offline: no deployment, no network, no npm install
cd examples/opencode-plugin-sandbox
node tests/test_plugin.mjs        # expect 19/19, exit 0

# Against a real deployment
export CUBE_TEMPLATE_ID=tpl-xxxxxxxx
python3 exec_backend.py --session demo --command 'uname -r'
# expect the sandbox guest kernel, not your own
```
