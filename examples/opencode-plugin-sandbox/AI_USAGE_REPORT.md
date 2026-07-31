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

### 5.7 Four defects surfaced by the repository's automated review

The repository runs an AI review on every PR. It found four real defects that
neither the offline suite nor manual reading had caught, and one documentation
contradiction. All were confirmed against the actual code before being fixed —
the review was treated as a lead to verify, not as a verdict to accept.

**State preservation did not work at all (highest impact).** The wrapper ran the
user command through `bash -c`, which starts a *child* shell. A `cd` or `export`
performed by the command mutated only that child and was gone by the time the
state block ran, so the captured state always reported the wrapper's own cwd and
environment. "Stateful" was a headline property of this integration, and the
documented examples (`cd /workspace/demo` then `pwd`) could not have reproduced.

Confirmed with two minimal scripts before changing anything:

```
bash -c variant:   command printed /tmp/statetest/demo
                   wrapper shell still at /tmp/statetest      <- state lost
eval variant:      command printed /tmp/statetest/demo
                   wrapper shell now at /tmp/statetest/demo   <- state kept
```

Fixed by running the command with `eval` in the current shell. Re-verified
end to end: after `cd /workspace/demo && export TOKEN=abc123`, the state block
reports both the new cwd and `TOKEN=abc123`.

Root cause of the miss is worth naming: the offline suite only exercises the
JavaScript hook, and `build_wrapper` was never executed. The gap was already
listed as "end-to-end usage not verified" in this report, and this is precisely
the defect that gap was hiding.

**The idempotence guard was a silent host escape (security).** The guard was
`command.includes("exec_backend.py")`, placed *before* the passthrough and
fail-closed checks. So `ls exec_backend.py`, `echo x # exec_backend.py`, and
`touch /tmp/pwned; echo exec_backend.py` all skipped redirection and ran on the
host with no isolation and no log entry — the exact failure this plugin exists
to prevent, arrived at through the guard meant to make it safe.

Replaced with a structural check: parse the command with POSIX single-quote
semantics and require the exact six-token shape this plugin emits, with the
backend path matching our resolved absolute path. Five escape cases were added
to the test suite.

**The session lock could release a lock it did not own.** `__exit__` unlinked
the lock file unconditionally. When `__enter__` gave up after its 90 s timeout,
`self.fd` is `None` and the lock is still held by a live peer; unlinking it let a
third call acquire concurrently and defeated the serialisation. Now `__exit__`
returns early unless the lock was actually acquired. Crashed holders remain
covered by the existing stale-lock reclaim.

**The installer's copy fallback installed a broken plugin.** When symlinking is
unavailable the installer copied only the plugin, but the plugin resolves the
backend at `<plugin-dir>/../exec_backend.py`, which was never installed in that
mode. Every `bash` call would then fail closed, and the documented "reinstall"
remedy reproduced the same state. The fallback now copies the backend to the
matching location and aborts (removing the plugin again) if it cannot. Uninstall
removes the copy, guarded by a content check so a user's own file is never
deleted.

**Documentation contradiction.** The README summary table and diagram claimed
"one MicroVM per session" while the code creates one per call and the known
limitations section said so correctly. The summary and diagram now match the
code.

One further wording fix came out of the same review: the comment on
`resolveSessionId` claimed the fallback "only costs sandbox reuse granularity".
That understated it — if every known session key is missing, all sessions share
one state file and lock, so cwd and environment bleed between them. Still not a
host escape, but worth stating accurately.

### 5.9 A second review round, and one fix that was wrong

The automated review ran again after the first round of fixes and found four
more issues. Two are worth recording in detail because they say something about
the first round.

**The `eval` fix from §5.7 was incomplete.** Replacing `bash -c` with `eval`
made `cd` and `export` visible to the state block, which was the reported
defect. But `eval` runs in the wrapper's own shell, so a command ending in
`exit` — a plausible thing for an agent to write — terminated that shell before
the state block and exit-code line ran. The host then found no sentinel and
reported success for a command that had failed:

```
eval variant, command `echo before; exit 3`:
  stdout : before
  state  : (missing)
  rc     : 0     <- wrong; the command exited 3
```

The correct shape is a subshell with an `EXIT` trap: `eval` still runs where its
mutations are observable, the trap fires however the subshell ends, and an
`exit` inside only ends the subshell. Verified across four cases:

```
plain command      rc=0  state captured  cwd=demo  TOKEN=v1
ends with exit 3   rc=3  state captured  cwd=demo  TOKEN=v2
failing command    rc=1  state captured
exec echo ...      no sentinel  <- see below
```

`exec` remains outside any shell-level wrapper: it replaces the process image
and discards traps with it. Rather than claim coverage, the backend keeps the
previous session state when no sentinel arrives (stale rather than wrong) and
this is now listed as a known limitation.

The lesson is narrower than "test more". The first fix was verified against the
exact scenario in the report — `cd` then `pwd` — and passed. It was not verified
against the adjacent shapes of the same operation. A fix to control flow should
be checked against the ways control flow can end, not only the case reported.

**The default passthrough list was a host-execution escape hatch.** The list
contained `git`, `gh`, `opencode`, justified as protecting the developer's
workflow — the sandbox has its own filesystem, so `git commit` there commits the
wrong repository. That reasoning is sound but it was applied to the wrong threat
model. Matching is on the leading token, so allowing `git` allows anything git
can be made to do, and git can be made to run arbitrary shell:

```console
$ git -c alias.probe='!echo PWNED_VIA_GIT_ALIAS' probe
PWNED_VIA_GIT_ALIAS
```

Verified against real git before changing anything. Since the model's commands
are the untrusted input, a default list containing `git` re-opened the host path
this integration closes. The default is now empty, opting in is explicit, and
both the code comment and the docs state that listed commands must be treated as
fully trusted with host privileges.

Two smaller items from the same round: fail-closed was not applied to unreadable
payloads (a non-string `command`, or a bash call with no `args`, returned quietly
and reached the host — now throws), and the Chinese README diagram still said
"one MicroVM per session" because §5.7's fix landed only in the English file.
The second is a plain process failure: a bilingual repository needs both files
changed in the same edit, and checking only the file named in the review is not
enough.

Tests grew from 21 to 25 with the passthrough default inverted and the new
fail-closed cases covered.

### 5.10 A third round: the same fix missed in more places

The review ran a third time and moved to "approve with minor changes". Its main
finding was uncomfortable: the passthrough default had been inverted in the code
and documented correctly in the integration guide, but **five other places still
described the old default**, including two spots in each README and
`.env.example`. Each README therefore contradicted itself — its config table
said the default was empty while its prose said `git` runs on the host.

That is the same failure mode as §5.9's Chinese-diagram miss, one round later
and wider. The lesson taken from §5.9 was "change both language files". The
actual lesson is more general: **after inverting a default, grep the whole
example for the old value rather than editing the places the review named.**
Applied here, that turns up `.env.example`, which no review round had mentioned
and which is the file a user copies before reading anything else.

Four smaller items in the same round:

- The backend module docstring still said each session maps to one MicroVM. Same
  stale claim as §5.7, surviving in the file a reviewer opens first. Reworded,
  and the practical consequence spelled out: environment and cwd carry over as
  replayed strings, but filesystem changes do not, so a directory created by one
  call is gone by the next.
- `exec` lost the exit code. §5.9 claimed the exit code was returned unchanged
  when no sentinel arrived; in fact `split_output` returned 0, so `exec false`
  was reported as success. The guest now appends `__CUBE_RC__=<returncode>` when
  the wrapper produced none, and `split_output` reads the exit code from the body
  as well as from after the state block. This was a documentation claim that the
  code did not support — worse than an undocumented gap, because it invited trust.
- The concurrency claim overstated the lock. `SessionLock` proceeds *unlocked*
  after its timeout, and the default command timeout is longer than the lock
  timeout, so concurrent same-session calls can routinely overlap. The code
  comment was honest about this; the property table said "serialised". Both
  READMEs now say best-effort and state what happens on timeout.
- The idempotence guard could be spoofed. It pinned the backend path and the
  flags but not the interpreter or the session id. Since `read` is deliberately
  unsandboxed, a model can read the plugin and reproduce the six-token shape.
  Now every fixed position is pinned, including the session id derived from the
  same call, so a replayed rewrite from another session is redirected rather
  than trusted. Covered by a new assertion.

Tests: 25.

### 5.11 A fourth round: a documented mode that could not run

The fourth review round reported that `--reset`, documented in both READMEs as

```bash
python3 exec_backend.py --session <session-id> --reset
```

exits with `error: the following arguments are required: --command`. The
`--reset` branch existed and was correct; `--command` was declared
`required=True`, so argparse rejected the invocation before that branch could be
reached. Reproduced exactly as documented before changing anything.

This is a different class of miss from the previous three rounds. Those were
stale prose describing code that had moved on. This one is code that never
worked in any version, in a mode both language variants document. Nothing in the
offline suite touches `main()` — it tests the JavaScript hook — and no manual
run had used `--reset`, because during development state was cleared by deleting
the file directly. A code path that only a user would take was therefore never
taken.

The correction: `--command` is no longer `required`, the two modes are validated
against each other after parsing, and `--command` without `--reset` still errors
with a clear message.

Three smaller findings from the same round:

The plugin's own header still claimed one MicroVM per session. Round three fixed
this in the backend docstring and both READMEs; the plugin header was missed. It
now also states the consequence, that filesystem changes do not carry over.

A missing exit-code marker was reported as success. `split_output` defaulted `rc`
to `0`, so if neither the EXIT trap nor the guest fallback emitted a marker —
truncated output, a crashed guest interpreter, a transport problem — OpenCode was
told the command succeeded while nothing was known about it. That is precisely
the silent-success failure this integration exists to remove, reintroduced by a
default value. `split_output` now returns `None` and the caller fails explicitly.

The test named "backend script exists next to the plugin" did not check
existence. It asserted `path.basename(BACKEND) === "exec_backend.py"`, which a
constant-derived path satisfies unconditionally: the test would have passed on a
checkout with the backend deleted. It now calls `fs.existsSync` and `statSync`,
which is what the plugin does at call time. A test that cannot fail is worse than
no test, because it occupies the space where a real check would be noticed as
missing.

Also made the `--timeout` default reject non-integers with an argparse error
instead of a `ValueError` traceback, and corrected `.env.example`, which said to
copy the file to `.env` and source it — nothing in the example reads `.env`
files, and exporting the variables after OpenCode has started has no effect on
the running instance.

Tests: 25.

### 5.12 Command-line parsing that missed the real output format

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
- 25/25 offline assertions pass

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
node tests/test_plugin.mjs        # expect 25/25, exit 0

# Against a real deployment
export CUBE_TEMPLATE_ID=tpl-xxxxxxxx
python3 exec_backend.py --session demo --command 'uname -r'
# expect the sandbox guest kernel, not your own
```
