---
title: Web Terminal
---

# Web Terminal

The Dashboard can open an interactive TTY for a running sandbox without exposing the container runtime or a node port.

This is an ops-only path: the Dashboard authenticates to CubeOps, CubeOps validates the target and relays the WebSocket to CubeMaster, and CubeAPI is not involved.

## Use it

1. Open **Sandboxes** and select a running sandbox.
2. Click **Open terminal** on the detail page.
3. If the sandbox has multiple containers, select the target container before connecting.
4. Click **Connect**. The terminal supports ANSI output, cursor control, scrollback, copy/paste, resize, fullscreen, disconnect, and reconnect.

Pausing, deleting, or otherwise stopping the sandbox prevents new sessions. Closing the panel actively closes the WebSocket and cleans up its exec process. A session that has no terminal input, output, or resize activity for 30 minutes is closed automatically; keepalive frames do not extend that limit.

## Deployment

One-click deployment generates `CUBE_TERMINAL_GATEWAY_TOKEN` in `.one-click.env`. The Helm chart creates a shared secret and injects it into CubeOps and CubeMaster. For a manual deployment, generate at least 32 random bytes and configure the same value on both services:

```bash
CUBE_TERMINAL_GATEWAY_TOKEN=<random-secret>
```

When the Dashboard is served from an origin other than CubeOps' host, configure a comma-separated exact allowlist:

```bash
CUBE_TERMINAL_ALLOWED_ORIGINS=https://dashboard.example.com
```

Production deployments must terminate TLS so the browser uses `wss://`. Do not expose CubeMaster's internal terminal endpoint; it accepts only the shared gateway credential and is intended for the control plane network.

Cubelet creates containerd terminal FIFOs under `/data/cubelet/fifo` by default. Set `CUBELET_TERMINAL_FIFO_DIR` when that directory is not writable in your deployment.

Terminal grants are held in CubeOps memory. The chart defaults to one CubeOps replica; if you scale CubeOps horizontally, keep the session-creation request and its following WebSocket upgrade on the same replica (for example, with sticky routing).

## Security and operations

The authenticated session-creation request resolves the requested container against the sandbox and issues a 60-second, single-use grant. The WebSocket handshake requires the grant, an HttpOnly/SameSite binding cookie, the `cube-terminal.v1` subprotocol, and an allowed `Origin`. Grants are not placed in URLs or logs.

CubeOps limits pending and active sessions per principal, per sandbox, and globally. Structured logs record grant issuance, denied handshakes, session open, and session close with session, principal, sandbox, and container identifiers. Never log terminal input or output because it may contain secrets.

## Troubleshooting

- **Open terminal is disabled:** the sandbox is not running.
- **403/401 during WebSocket upgrade:** verify the browser origin, login state, proxy cookie forwarding, and HTTPS configuration.
- **503 when creating a session:** verify that CubeOps and CubeMaster share `CUBE_TERMINAL_GATEWAY_TOKEN`.
- **Immediate disconnect:** verify CubeMaster-to-Cubelet connectivity and that the selected container is still running.
