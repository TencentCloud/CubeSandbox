# WebUI Terminal Login

CubeSandbox WebUI can open an interactive shell for a running sandbox directly from the sandbox list or detail page. The terminal uses a browser WebSocket to CubeAPI, and CubeAPI bridges that session to the sandbox EnvD PTY process API. It does not introduce a separate execution backend.

## Requirements

- The sandbox must exist and be in the `running` state.
- The browser must be able to reach CubeAPI through the same origin used by WebUI.
- If WebUI login is enabled, the current WebUI session token is required.
- If CubeAPI auth callback is enabled, the terminal WebSocket must include an API key or bearer token so CubeAPI can call the same auth callback path.
- Production deployments should expose WebUI/CubeAPI over HTTPS so the terminal uses WSS.

## Open a Terminal

1. Open WebUI.
2. Go to **Sandboxes**.
3. Find a running sandbox and click **Open terminal**.
4. Run a command such as:

```bash
ls
top
ping -c 1 127.0.0.1
```

The terminal supports ANSI colors, cursor control, paste, scrollback, and resize. The panel can be expanded to fullscreen and its font size can be adjusted from the toolbar.

If CubeAPI receives multiple container records for the sandbox, the terminal
panel shows a **Container** selector. Changing the selected container opens a
new terminal session for that container. The sandbox list opens the default
container because the list API does not include per-container records.

## State and Permission Checks

The **Open terminal** button is disabled when a sandbox is paused, pausing, or otherwise not running. CubeAPI also validates the target sandbox after the WebSocket connects, so direct WebSocket attempts against a non-running sandbox are rejected.

CubeAPI records structured audit logs for terminal session open and close events. The log fields include:

- `actor`
- `sandbox_id`
- `session_id`
- `container_id`

## WebSocket Protocol

The browser connects to:

```text
/cubeapi/v1/sandboxes/{sandboxID}/terminal
```

The browser sends JSON messages:

```json
{ "type": "input", "data": "ls\n" }
{ "type": "resize", "rows": 32, "cols": 120 }
{ "type": "close" }
```

CubeAPI sends JSON messages:

```json
{ "type": "status", "status": "ready", "sessionId": "...", "pid": 123 }
{ "type": "output", "data": "<base64 terminal bytes>" }
{ "type": "error", "message": "..." }
{ "type": "exit", "code": 0 }
```

`output.data` is base64-encoded so ANSI and binary terminal bytes are preserved.

## Known Limitations

- Explicit container selection is available when the sandbox detail API returns container metadata. The sandbox list opens the default container.
- Reconnect opens a new terminal session. It does not reattach to a previously disconnected PTY yet.
- The terminal inherits the sandbox/container permission boundary and network policy. It is not intended to bypass CubeEgress or sandbox isolation.
- Idle sessions are closed by CubeAPI after the configured idle timeout.

## Local Validation

1. Start the local CubeSandbox deployment and WebUI.
2. Create or resume a sandbox.
3. Open WebUI and click **Open terminal** for the running sandbox.
4. Run `ls` and verify output appears.
5. Run a command with ANSI output, for example `printf '\033[31mred\033[0m\n'`, and verify the color renders.
6. Resize the terminal panel and verify interactive commands redraw correctly.
7. Close the terminal and check CubeAPI logs for `terminal.session.closed`.

This flow should be possible to complete in under 30 minutes on a working local deployment.
