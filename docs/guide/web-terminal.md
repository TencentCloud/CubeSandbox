# Web Terminal

The Cube Sandbox WebUI provides an interactive terminal for running sandbox containers, allowing you to open a shell session directly from your browser.

## Quick Start

1. Navigate to **Sandboxes** in the sidebar
2. Find a running sandbox and click the **Terminal** icon (or **Open Terminal** from the sandbox detail page)
3. A terminal panel opens with a `/bin/bash -l` shell inside the default container
4. Type commands normally — the terminal supports ANSI colors, scrolling, copy/paste, and window resize

## Features

- **Full interactive shell**: `/bin/bash -l` with PTY allocation, supporting curses-based tools like `top`, `vim`, and `htop`
- **Multi-container support**: If your sandbox has multiple containers, use the dropdown in the toolbar to select which one to connect to
- **ANSI color & cursor support**: Full terminal emulation via xterm.js with 256-color support
- **Resize**: Drag the terminal panel or toggle fullscreen — the PTY dimensions sync automatically
- **Scrollback**: 5000 lines of scrollback buffer; scroll with mouse wheel or touchpad
- **Copy & paste**: Select text and use `Ctrl+Shift+C` to copy, `Ctrl+Shift+V` to paste
- **Font size**: Adjust with the `+`/`-` buttons in the toolbar (10px–24px)
- **Reconnection**: Automatically retries on transient network failures (up to 3 attempts with exponential backoff)

## Keyboard Shortcuts

| Shortcut | Action |
|----------|--------|
| `Ctrl+Shift+C` | Copy selected text |
| `Ctrl+Shift+V` | Paste from clipboard |
| `Ctrl+L` | Clear terminal screen |

## Permissions

Terminal access requires authentication when the platform's auth callback is configured. The terminal inherits the same authentication model as the WebUI — the session token from login is passed as a query parameter on the WebSocket upgrade.

Terminal access is only available for sandboxes in the **running** state. Paused or deleted sandboxes cannot be connected to.

## Idle Timeout

Terminal sessions are automatically closed after 30 minutes of inactivity (no keyboard input). This timeout is configurable via the `TERMINAL_IDLE_TIMEOUT_SECS` environment variable on CubeAPI.

```bash
# Set a 1-hour idle timeout
TERMINAL_IDLE_TIMEOUT_SECS=3600
```

## Known Limitations

- **No session persistence**: Closing the browser tab terminates the terminal session. The shell process inside the container is also terminated.
- **No sandbox pause survivability**: Pausing a sandbox closes all active terminal sessions. Resuming the sandbox requires opening a new terminal.
- **Single shell per terminal**: Each terminal panel opens one shell process. Open multiple panels for concurrent shells in the same sandbox.
- **envd dependency**: Terminal functionality requires envd to be running inside the sandbox (port 49983). All Cube Sandbox templates include envd by default.

## Architecture

```
Browser (xterm.js)
  │  wss://<host>/cubeapi/v1/sandboxes/{id}/terminal?token=...
  ▼
CubeAPI (WebSocket upgrade + proxy)
  │  HTTP POST → CubeProxy → envd process.Process/Connect
  ▼
envd (inside sandbox VM)
  │  PTY → /bin/bash -l
  ▼
Container shell
```

## Audit Logging

Each terminal session creates structured audit log entries:

- **Session start**: Logged with `event_type=terminal_session_start`, including sandbox ID, container name, user, and session ID
- **Session end**: Logged with `event_type=terminal_session_end`, including close reason (`client_disconnect`, `idle_timeout`, `sandbox_destroyed`, etc.) and session duration

Terminal I/O content (stdin, stdout, stderr) is **never** logged. Only session metadata is recorded.