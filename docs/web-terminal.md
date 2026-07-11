# WebUI interactive terminal

The sandbox details page exposes **Open terminal** only when CubeMaster reports
at least one running, user workload container. The terminal runs inside that
existing container; it does not mount a Docker socket or grant host access.

## Deployment

CubeAPI and CubeMaster must be upgraded together. Set the same randomly
generated value in both services before starting them:

```yaml
# CubeMaster/conf.yaml
common:
  terminal_gateway_token: replace-with-a-long-random-secret
```

```bash
# CubeAPI environment
TERMINAL_GATEWAY_TOKEN=replace-with-a-long-random-secret
```

The CubeMaster terminal WebSocket stays disabled when this value is empty.
Keep CubeMaster on a private network; browsers connect only to CubeAPI through
the existing HTTPS/WSS proxy.

## Authentication and auditing

The browser passes its existing WebUI session token using the WebSocket
subprotocol header, never in a URL. When the CubeAPI database is configured,
an absent, expired, or invalid session is rejected before the WebSocket is
upgraded. Existing API-key/Bearer authentication middleware also protects the
route when an auth callback is configured.

CubeAPI emits `terminal.session.open` and `terminal.session.close` audit events
with the operator and sandbox ID. Each terminal is a separate Cubelet TTY
process, so multiple sessions and sandboxes remain isolated.

## Validation checklist

1. Open a running sandbox with a container target in WebUI.
2. Run `ls`, `top`, and `ping`; verify ANSI output and copy/paste.
3. Resize the dialog and verify the shell's terminal size changes.
4. Open two terminals against different sandboxes and verify output does not
   cross over.
5. Pause a sandbox and verify its terminal button is disabled; try an expired
   WebUI session and verify the WebSocket is rejected.

Known limitation: automatic reconnect intentionally creates a new TTY session;
it cannot resume an exited shell process.
