---
title: Web Terminal
---

# Web Terminal

The **Web Terminal** gives you an interactive shell inside a running sandbox straight from the Dashboard — no SSH, no SDK, no CLI. It is the fastest way to look around a micro-VM when something misbehaves.

> ⏱ Takes ~4 minutes to read. Afterwards you can debug any running sandbox from a browser tab.

## 1. Opening a terminal

There are two entry points, both of which only light up while the sandbox state is `running`:

| Where | What to click |
| --- | --- |
| **Sandboxes** list (`/sandboxes`) | The terminal icon in the row's action column |
| **Sandbox detail** (`/sandboxes/<id>`) | The **Terminal** button in the header |

A dialog opens with a full `xterm.js` terminal running `/bin/bash -i -l` as `root` inside the sandbox. Colours, cursor keys, and full-screen programs (`top`, `vim`, `htop`) all work, and resizing the dialog resizes the shell.

::: tip The sandbox must be running
A paused, pausing, or terminated sandbox has no envd to talk to, so the entry point is disabled and the backend refuses the request with `409 Conflict`. Resume the sandbox first.
:::

## 2. What happens under the hood

The terminal reuses the **existing envd PTY data plane** — the same one the Python/Node/Go SDKs use — rather than adding a new RPC anywhere in the cluster:

```
Browser (xterm.js)
   │  wss://<dashboard>/opsapi/v1/terminal/ws?ticket=<one-time ticket>
   ▼
CubeOps :3010            ← JWT auth, ticket issuance, session registry, audit log
   │  Connect-JSON over HTTP (streaming Start/Connect + unary SendInput/Update/SendSignal)
   ▼
CubeProxy ──► envd :49983 inside the sandbox (process.Process PTY API)
```

Two requests are involved:

1. `POST /opsapi/v1/sdk/sandboxes/<id>/terminal` — authenticated with your normal login JWT. CubeOps checks that the sandbox exists and is running, then returns a **one-time ticket** valid for 30 seconds.
2. `GET /opsapi/v1/terminal/ws?ticket=…` — the WebSocket upgrade. The ticket is consumed on first use.

The ticket exists because browsers cannot attach an `Authorization` header to a WebSocket handshake. Putting the login JWT in the query string would leak a long-lived credential into access logs and browser history; a 30-second, single-use, separately-scoped ticket cannot be replayed and is useless against any other endpoint.

## 3. Sessions, idle timeout, and reconnects

CubeOps keeps a registry of live terminal sessions:

- **Multiple terminals per sandbox** are allowed. Each gets its own PTY and its own PID; they do not share scrollback or history.
- **Closing the dialog kills the shell.** The frontend sends an explicit close frame, and CubeOps sends `SIGKILL` to the PTY.
- **A dropped connection does not.** If the network blips or you close the laptop lid, the shell keeps running and the dialog offers a **Reconnect** button, which reattaches to the same PID via envd's `Connect` RPC. Your session — including anything running in the foreground — is still there.
- **Idle sessions are reaped.** After 30 minutes without keyboard input, CubeOps closes the WebSocket and kills the PTY. Configure this with `terminal_idle_timeout` in `ops.yaml` or the `CUBE_OPS_TERMINAL_IDLE_TIMEOUT` environment variable (Go duration syntax, e.g. `15m`).

::: warning Idle terminals do not keep a sandbox alive
Keyboard input refreshes the sandbox's `last_active` timestamp because the traffic flows through CubeProxy. A terminal that is merely *open* sends no such traffic, so `cube-lifecycle-manager` may still auto-pause the sandbox underneath it. The terminal will report the broken stream when that happens.
:::

## 4. Auditing

Every session start and end is written to the CubeOps log (`/data/log/CubeOps/cubeops-req.log` by default) as a structured line:

```
terminal_session_start sessionID=<uuid> sandboxID=<id> username=<user> clientIP=<ip> pid=<pid> reconnect=false
terminal_session_end   sessionID=<uuid> sandboxID=<id> username=<user> clientIP=<ip> pid=<pid> reason=client_close durationMs=48213
```

`reason` is one of `client_close` (user closed the terminal), `pty_exit` (the shell exited), `detached` (connection lost, PTY kept for reconnect), `idle_timeout` (reaped by the sweeper), `stream_error`, or `reconnect_failed`.

Grep for `terminal_session_` to reconstruct who opened a shell into which sandbox, from where, and for how long.

## 5. Deployment requirements

The WebSocket needs an nginx location that forwards the `Upgrade` header and does not time out after five minutes. The one-click deployment ships this by default:

```nginx
location = /opsapi/v1/terminal/ws {
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 7206s;
    proxy_send_timeout 7206s;
    proxy_buffering off;

    rewrite ^/opsapi/(.*)$ /api/$1 break;
    proxy_pass http://cube-ops:3010;
}
```

If you run a custom reverse proxy in front of the Dashboard, replicate this location. Without it the handshake fails with `400 Bad Request`, or the terminal silently dies after `proxy_read_timeout`.

For local development, `npm run dev` already proxies the upgrade (`ws: true` on the `/opsapi` entry in `vite.config.ts`).

## 6. Known limitations

- **One shell per PTY, one PTY per request.** A sandbox is a single micro-VM running a single envd, so there is no "pick a container" selector — the concept does not exist in Cube Sandbox.
- **Authorization is coarse.** Any authenticated Dashboard user can open a terminal into any sandbox, with `root` inside it. There is no per-sandbox ownership model yet. Treat Dashboard login as equivalent to root on every sandbox in the cluster, and restrict it accordingly (see [Authentication](./authentication.md)).
- **Scrollback lives in the browser.** It is capped at 5000 lines and is lost when the dialog closes. Use [Sandbox Logs](./sandbox-logs.md) for anything you need to keep.
- **No file upload/download.** Use the SDK's filesystem API for moving files in and out.

## 7. Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| The terminal button is greyed out | The sandbox is not in `running` state — resume it first |
| `409 Conflict` when opening | Same, but the state changed between page refresh and click |
| Dialog opens, then "Connection failed" | nginx is missing the WebSocket location (see §5), or CubeProxy cannot reach the sandbox |
| Terminal dies after ~5 minutes idle | `proxy_read_timeout` is still at the 300s default on your reverse proxy |
| "The shell is no longer available" on reconnect | The PTY was killed — the sandbox was paused, or the idle sweeper reaped it |
| Garbled full-screen programs | The dialog was resized while the connection was down; reconnect to resync the size |
