# Real-cluster Web Terminal evidence

`terminal-evidence.sh` drives the deployed WebUI/nginx origin and writes raw
evidence outside the repository. It is intentionally opt-in: the script never
uses localhost as the product endpoint and never creates cloud resources or
template/runtime rows directly. The explicit `--direct-db-task-users` mode is a
test-only exception for deployments where the operator has authorized two exact
task-owned authentication rows.

## Safety model

- A unique lowercase task ID names every run and every created sandbox.
- The raw root defaults below `/data/cubelet`; repository paths and broad roots
  are rejected.
- Browser cache/config stay in the full task-owned run root. Chromium `TMPDIR`
  uses a short, task-ID-derived `/data/cubelet/c7e/<sha-prefix>/<phase>` path so
  its Unix singleton socket stays within the platform length limit; that exact
  phase path is removed after each browser process closes.
- Two-user runs use two distinct mode-0400/0600 JSON files and two independent
  browser contexts. The files must have different inodes and usernames. The
  dev-only public-hint mode can supply the primary credential, but a complete
  real run still requires `--secondary-credential-file` or direct task-user mode.
- `--direct-db-task-users` generates two random passwords and bcrypt hashes
  without printing them, writes credentials below the external raw root with
  mode 0600, and sends SQL to MySQL only through stdin. Cleanup checks that both
  users have zero open terminal sessions, revokes/removes their exact refresh
  and grant rows, retains closed terminal-session audit rows, removes only the
  two exact user rows, verifies zero counts, then deletes both credential files.
- Grant/JWT/Cookie/internal-token values, complete headers, terminal output,
  `token_hash`, full journals, profiles, traces, and videos are never review
  artifacts.
- A failure trap first saves bounded service/browser state, then removes only
  exact sandbox IDs and the exact commented firewall rule for this run. Cleanup
  waits for normal network convergence, then uses the supported idempotent
  network-agent release API only for a still-present recorded task ID.
- The idle test backs up Cubelet's complete live TOML with its metadata, changes
  only the cubebox terminal `idle_timeout_minutes` field, restarts only Cubelet,
  and restores the exact backup even when a later step fails.
- `--require-multi-container` converts unavailable multi-container coverage into
  a hard failure. A pass requires two live selector targets, distinct container
  IDs, and task-owned `primary`/`sidecar` role markers from real PTYs.

## Invocation

```bash
scripts/terminal-evidence/terminal-evidence.sh --help

scripts/terminal-evidence/terminal-evidence.sh \
  --preflight-only \
  --endpoint http://192.0.2.10 \
  --task-id issue643-example-01 \
  --credential-file /data/cubelet/issue643-example-01/credential.json

scripts/terminal-evidence/terminal-evidence.sh \
  --run-real \
  --endpoint http://192.0.2.10 \
  --task-id issue643-example-01 \
  --credential-file /data/cubelet/issue643-example-01/credential-a.json \
  --secondary-credential-file /data/cubelet/issue643-example-01/credential-b.json \
  --template-id tpl-task-owned-multi-container \
  --require-multi-container

# Only with explicit authorization to create exact task-owned auth rows:
scripts/terminal-evidence/terminal-evidence.sh \
  --run-real \
  --endpoint http://192.0.2.10 \
  --task-id issue643-example-02 \
  --direct-db-task-users \
  --template-id tpl-task-owned-multi-container \
  --require-multi-container
```

The credential file is JSON:

```json
{"username":"<admin-user>","password":"<password>"}
```

Keep both files outside git and mode 0600. Do not place the values on the command
line. For an explicitly disposable development deployment whose login page
already displays a default hint, `--allow-public-login-hint` is available for
the primary login when a distinct secondary credential is also supplied; by
itself it does not satisfy the two-user real-run gate.

Run fixture tests with:

```bash
bash scripts/terminal-evidence/tests/test.sh
```
