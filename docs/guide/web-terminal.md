---
title: Web Terminal
---

# Web Terminal

Web Terminal opens an interactive shell in a running sandbox container from the CubeSandbox Dashboard. The browser connects through the deployed WebUI nginx and CubeOps; it never connects directly to CubeMaster, Cubelet, or containerd.

## Requirements

- Sign in to the Dashboard with an account authorized to open terminals.
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

CubeOps owns the Web Terminal runtime defaults in `CubeOps/internal/config/config.go`. The deployment layer intentionally does not repeat those values. When no deployment override is present, CubeOps uses its built-in defaults; an operator can add a supported `CUBE_TERMINAL_*` variable to `.one-click.env` to override a specific setting.

For example, `CUBE_TERMINAL_ALLOWED_ORIGINS` accepts a comma-separated list of additional exact trusted `http://` or `https://` origins. The same-origin Dashboard does not require an entry. Keep all other settings absent unless an operator has an explicit reason to override the application default.

Cubelet also enforces built-in limits of 100 sessions per node, 10 per sandbox, and 5 per container. The browser cannot raise these limits.

## Deploy And Verify Web Terminal

The following workflow uses the one-click release bundle to bring up
CubeSandbox and verify the complete browser-to-container terminal path. Use a
Linux host with KVM, systemd, and Docker. Keep deployment configuration and
credentials specific to that host; do not copy a production `.env` into a new
installation.

### Prepare the release bundle

```bash
tar -xzf cube-sandbox-one-click-<version>.tar.gz
cd cube-sandbox-one-click-<version>
cp env.example .env
chmod 600 .env
```

Set only the deployment values required by the target host, such as its node
address and WebUI port. Keep passwords and deployment credentials out of
shell history and command arguments.

### Install and verify the services

```bash
sudo ./install.sh
systemctl is-active cube-sandbox-cubeops.service
systemctl is-active cube-sandbox-cubemaster.service
systemctl is-active cube-sandbox-cubelet.service
systemctl is-active cube-sandbox-webui.service
curl -fsS http://127.0.0.1:12088/health
```

The expected result is four `active` responses and HTTP 200 from `/health`.
Open the deployed WebUI endpoint in a browser; do not use the Vite development
server.

### Create a sandbox and open its terminal

1. Sign in to the Dashboard with an account authorized to open terminals. If
   the deployment still uses documented bootstrap credentials, immediately
   use the normal password-change flow.
2. If the deployment has no `READY` template and running sandbox, follow
   [Quick Start: Create a Template](./quickstart.md#step-3-create-a-template),
   then create a sandbox from that template in the Dashboard.
3. Open **Sandboxes**, find the running sandbox, and select **Open terminal**
   from the list or detail view.
4. Wait for **Connected** and confirm that the terminal surface is non-empty.

A paused or stopped target remains disabled. If template preparation or
sandbox creation fails, resolve that lifecycle problem before testing the
terminal rather than using a different sandbox.

### Verify terminal interaction

In the terminal, run:

```text
printf 'WEB_TERMINAL_HOST=%s\n' "$(hostname)"
ls --color=auto
stty size
top
```

Press `Ctrl+C` to stop `top` and confirm that the same shell remains usable.
Run `ping -c 3 127.0.0.1` to verify ordinary command input and output. Resize
the terminal window and run `stty size` again; the reported dimensions should
change. Close the terminal normally when finished.

Delete the sandbox through the Dashboard when it is no longer needed. To stop
and remove a one-click installation, run:

```bash
sudo ./down.sh
```

Do not include credentials, grants, cookies, authorization headers, internal
tokens, terminal payloads, or database passwords in troubleshooting output or
shared screenshots.

### Shared Internal Token

CubeOps and CubeMaster must receive the same `CUBE_TERMINAL_INTERNAL_TOKEN`.

- One-click generates the token when `CUBE_TERMINAL_INTERNAL_TOKEN` is empty, moves it to `/usr/local/services/cubetoolbox/.terminal-internal-token`, requires root ownership with mode `0400` or `0600`, and removes it from the shared runtime environment file.
- This change does not add Helm/Kubernetes or Terraform/TKE terminal-token wiring. Those deployment modes must supply the same token to CubeOps and CubeMaster through their existing secret-management process if support is added later.

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

### A terminal request returns 401 or 403

- `401`: sign in again; the current login session may be missing or expired.
- `403` from the grant request: confirm that the account is authorized to open a terminal for the target sandbox.
- `403` during WebSocket upgrade: verify the browser Origin matches the Dashboard scheme, host, and port, or is present in the additional allowed-origin list.

### The WebSocket does not return 101

The one-click nginx source at `deploy/one-click/webui/nginx.conf` must contain the exact terminal location, Upgrade/Connection headers, `Host $http_host`, disabled buffering, and a 7200-second read timeout. The live generated file is `/usr/local/services/cubetoolbox/webui/nginx.generated.conf`.

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
