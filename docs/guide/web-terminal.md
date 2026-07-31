---
title: Web Terminal
---

# Web Terminal

Web Terminal opens an interactive shell in a running sandbox container from the CubeSandbox Dashboard. The browser connects through the deployed WebUI nginx and CubeOps; it never connects directly to CubeMaster, Cubelet, or containerd.

## Requirements

- Sign in to the Dashboard with an account whose current role is `admin`.
- The sandbox and selected container must be running.
- Open the deployed Dashboard endpoint. The Vite development server is useful for frontend work, but it is not a production Web Terminal endpoint.
- For production access, terminate HTTPS at the Dashboard or an approved reverse proxy so the terminal uses WSS.

The terminal button is disabled for paused or stopped sandboxes. This is only an early UI check: CubeOps and Cubelet validate the target state again before starting a shell.

## Open And Use A Terminal

1. Open **Sandboxes**.
2. Use the terminal icon in a running sandbox row, or open the sandbox detail page and select **Open terminal**.
3. Wait for the status to change from **Connecting** to **Connected**.
4. If the sandbox has multiple containers, use the container selector to start a separate terminal tab for the required running container. Non-running containers remain disabled.
5. Use **New session** to open another independent shell. Closing one tab does not close the other tabs.

The toolbar provides font sizes from 12 to 20 and browser fullscreen mode. The selected font size is stored locally under the non-sensitive key `cube.terminal.fontSize`.

### Input, Copy, And Paste

- Type normally. The terminal sends UTF-8 input to the selected shell.
- `Ctrl+C` is sent to the TTY and interrupts the foreground command, such as `ping` or `top`, without closing the shell.
- Select terminal text before copying. `Ctrl+Shift+C` / `Ctrl+Shift+V` are the usual terminal copy and paste shortcuts; on macOS use the corresponding Command shortcuts. Browser clipboard permissions and security policy still apply.
- Avoid pasting credentials into a shell. CubeSandbox does not write terminal payloads to its audit tables or application logs, but the shell, its history, or a command you run may record them.

## Connection And Session Lifecycle

The browser receives a short-lived, single-use grant from CubeOps and presents it only in the WebSocket subprotocol. The grant does not enter the URL, DOM, browser storage, or query cache. The user's JWT and cookies are not copied into the WebSocket URL or subprotocol.

If the transport fails unexpectedly, Cubelet keeps the same shell detached for 30 seconds by default. The UI retries after approximately 1, 2, and 4 seconds, using the same session ID and last received byte offset. A successful resume keeps the same shell process and replays buffered output. If the grace period expires, the UI reports `SESSION_LOST` and offers a new session.

Normal close, shell exit, sandbox pause/stop, idle timeout, maximum lifetime, and service drain end the old shell. These terminal states do not silently reconnect to a new shell.

## Configuration Reference

One-click deployments expose the variables in `deploy/one-click/env.example`. Helm deployments expose equivalent values under `terminal` in `deploy/kubernetes/chart/values.yaml`.

| Environment variable | Helm value | Default | Meaning |
| --- | --- | --- | --- |
| `CUBE_TERMINAL_ENABLED` | `terminal.enabled` | `true` | Enables terminal grants and the WebSocket gateway when the shared internal token is present. |
| `CUBE_TERMINAL_ALLOWED_ORIGINS` | `terminal.allowedOrigins` | empty | Additional exact trusted `http://` or `https://` origins. Same-origin Dashboard requests are accepted without an entry. |
| `CUBE_TERMINAL_GRANT_TTL_SECONDS` | `terminal.grantTTLSeconds` | `60` | Lifetime of an unconsumed one-time grant; values above 60 are rejected. |
| `CUBE_TERMINAL_HANDSHAKE_TIMEOUT_SECONDS` | `terminal.handshakeTimeoutSeconds` | `10` | Maximum time allowed to establish the terminal relay. |
| `CUBE_TERMINAL_PING_INTERVAL_SECONDS` | `terminal.pingIntervalSeconds` | `20` | WebSocket ping interval. |
| `CUBE_TERMINAL_PONG_TIMEOUT_SECONDS` | `terminal.pongTimeoutSeconds` | `10` | Time to wait for transport liveness after a ping. |
| `CUBE_TERMINAL_WRITE_DEADLINE_SECONDS` | `terminal.writeDeadlineSeconds` | `10` | Per-write deadline for a slow terminal consumer. |
| `CUBE_TERMINAL_IDLE_TIMEOUT_MINUTES` | `terminal.idleTimeoutMinutes` | `30` | User idle timeout. Only stdin resets it; output, resize, ping, and pong do not. |
| `CUBE_TERMINAL_MAX_LIFETIME_HOURS` | `terminal.maxLifetimeHours` | `8` | Absolute lifetime of a shell, including active shells. |
| `CUBE_TERMINAL_RECONNECT_GRACE_SECONDS` | `terminal.reconnectGraceSeconds` | `30` | Detached resume window. Set to `0` to disable resume. |
| `CUBE_TERMINAL_REPLAY_BUFFER_BYTES` | `terminal.replayBufferBytes` | `262144` | Maximum detached output retained in memory for replay (256 KiB). |
| `CUBE_TERMINAL_MAX_FRAME_BYTES` | `terminal.maxFrameBytes` | `65536` | Maximum inbound WebSocket frame size (64 KiB). |
| `CUBE_TERMINAL_STDIN_QUEUE_FRAMES` | `terminal.stdinQueueFrames` | `8` | Bounded stdin queue depth. |
| `CUBE_TERMINAL_STDOUT_PENDING_BYTES` | `terminal.stdoutPendingBytes` | `262144` | Maximum pending stdout before slow-consumer handling (256 KiB). |
| `CUBE_TERMINAL_MAX_SESSIONS_PER_USER` | `terminal.maxSessionsPerUser` | `5` | Active sessions allowed for one user. |
| `CUBE_TERMINAL_MAX_SESSIONS_PER_REPLICA` | `terminal.maxSessionsPerReplica` | `200` | Active connections allowed for one CubeOps replica. |
| `CUBE_TERMINAL_DRAIN_TIMEOUT_SECONDS` | `terminal.drainTimeoutSeconds` | `30` | Graceful CubeOps shutdown window. |

Cubelet also enforces built-in limits of 100 sessions per node, 10 per sandbox, and 5 per container. The browser cannot raise these limits.

## 30-Minute Deployment-To-Terminal Reproduction

This checklist is for a disposable Linux one-click deployment. Use a task-owned
release bundle and a task-owned deployment host. Do not reuse an operator's
production `.env`, credentials, or sandbox IDs.

### 0-5 minutes: prepare the bundle

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
chmod 600 .env
```

Set only the deployment values required by the target host, such as the node
address and WebUI port. Keep database passwords and administrator credentials
out of shell history and out of the command line. Use the operator-provided
administrator credential through the deployment's normal secure input path.

### 5-20 minutes: install and verify the services

```bash
sudo ./install.sh
systemctl is-active cube-sandbox-cubeops.service
systemctl is-active cube-sandbox-cubemaster.service
systemctl is-active cube-sandbox-cubelet.service
systemctl is-active cube-sandbox-webui.service
curl -fsS http://127.0.0.1:12088/health
```

The expected result is four `active` responses and HTTP 200 from `/health`.
Open the configured WebUI endpoint from a non-localhost browser. Do not use a
Vite development server for this check.

### 20-25 minutes: sign in and open a terminal

1. Sign in with an administrator account whose current role is `admin`.
2. Open **Sandboxes** and select a sandbox that is already running.
3. Select **Open terminal** from the list or detail view.
4. Wait for **Connected** and confirm that the terminal surface is non-empty.

The expected result is a real xterm surface with a shell prompt. A paused or
stopped target must remain disabled.

### 25-30 minutes: run `top` and clean up

In the terminal, run:

```text
printf 'WEB_TERMINAL_HOST=%s\n' "$(hostname)"
top
```

Press `Ctrl+C` to return to the shell, then close the terminal normally. The
expected result is a live `top` screen, a return to the same shell, and no
automatic reconnect after the normal close.

For a disposable one-click host, finish with:

```bash
sudo ./down.sh
```

For a shared host, delete only the exact task-owned sandbox through the normal
Dashboard/API workflow and leave the host services running.

### Shared Internal Token

CubeOps and CubeMaster must receive the same `CUBE_TERMINAL_INTERNAL_TOKEN`.

- One-click generates the token when `CUBE_TERMINAL_INTERNAL_TOKEN` is empty, moves it to `/usr/local/services/cubetoolbox/.terminal-internal-token`, requires root ownership with mode `0400` or `0600`, and removes it from the shared runtime environment file.
- Helm creates and reuses a Secret by default. Set `terminal.existingSecret` and `terminal.secretKey` to use an operator-managed Secret, especially with an external control plane.
- Terraform accepts `TENCENTCLOUD_TERMINAL_INTERNAL_TOKEN` or generates a value. Protect the generated `.env`, resolved variables, and Terraform state with mode restrictions, encrypted storage, and limited backend access.

Never place the token in a command argument, URL, log, screenshot, or committed values file.

## Security Model

- The browser-facing route is `/opsapi/v1/terminal/ws` on the same origin as the Dashboard.
- Production deployments should use HTTPS/WSS. Configure `CUBE_TERMINAL_ALLOWED_ORIGINS` only when a separate trusted WebUI origin is required.
- nginx must preserve the browser Host including a non-default port with `Host $http_host`; otherwise strict Origin validation rejects the upgrade.
- Grants expire after 60 seconds by default, are consumed atomically once, and are bound to the user, sandbox, container, and session operation.
- The shell inherits the container's existing user, capabilities, mounts, namespaces, seccomp policy, and network boundaries. Web Terminal does not grant SSH-like host access or additional container privileges.
- Audit data contains user, target, timestamps, close reason, exit code, byte counters, and resume count. It does not contain terminal input/output or the raw grant.

## Troubleshooting

### The terminal button is disabled

Confirm that both the sandbox and target container are running. A paused target is intentionally disabled in the UI and rejected by the backend with `TARGET_NOT_RUNNING`.

### The grant request returns 401 or 403

- `401`: sign in again and confirm the account has the current `admin` role.
- `403` during WebSocket upgrade: verify the browser Origin matches the Dashboard scheme, host, and port, or is present in the additional allowed-origin list.

### The WebSocket does not return 101

All three nginx sources must contain the exact terminal location, Upgrade/Connection headers, `Host $http_host`, disabled buffering, and a 7200-second read timeout:

1. One-click: `deploy/one-click/webui/nginx.conf`. The live generated file is `/usr/local/services/cubetoolbox/webui/nginx.generated.conf`.
2. Helm: `deploy/kubernetes/chart/templates/_helpers.tpl`, rendered into the WebUI ConfigMap.
3. Terraform: `deploy/one-click/terraform/tencentcloud/tke-addons.tf`; `create.sh` normally copies the canonical one-click template to `webui-nginx.conf` before rendering the `cube-webui-nginx-conf` ConfigMap.

Keep the ordinary `/opsapi/` REST location at its 300-second timeout and preserve the existing `/sandbox/` WebSocket route. A successful health or REST request does not prove that the terminal upgrade headers and long timeout are active.

### The terminal disconnects near five minutes

Inspect the effective nginx configuration, not only the source template. The exact `/opsapi/v1/terminal/ws` location must use `proxy_read_timeout 7200s`; the ordinary `/opsapi/` block remains `300s`.

### The shell cannot resume

Resume is available only for unexpected transport loss within the configured grace period. User close, shell exit, sandbox transition, service drain, buffer overflow, or Cubelet restart ends the old session. Start a new session after `SESSION_LOST`.

### Service checks

For one-click deployments, inspect only bounded current state unless you intentionally need logs:

```bash
systemctl show cube-sandbox-cubeops.service cube-sandbox-cubemaster.service \
  -p Id -p ActiveState -p SubState --no-pager
curl -fsS http://127.0.0.1:3010/health
```

Do not print the protected environment file or terminal Secret while troubleshooting.

## Known Limitations

- A Cubelet restart does not restore an existing terminal session.
- TTY mode merges stderr into stdout; there is no independent stderr stream.
- Detached resume is limited to 30 seconds by default and 256 KiB of buffered output.
- Web Terminal does not provide SSH, SFTP, file upload/download, terminal recording, or collaborative input.
- Clipboard behavior is controlled by the browser and operating system; CubeSandbox does not bypass clipboard permission policy.
