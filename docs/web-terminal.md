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
# Optional; defaults to four active sessions per sandbox and CubeAPI process.
TERMINAL_MAX_SESSIONS_PER_SANDBOX=4
```

The CubeMaster terminal WebSocket stays disabled when this value is empty.
Keep CubeMaster on a private network; browsers connect only to CubeAPI through
the existing HTTPS/WSS proxy. CubeMaster intentionally accepts the CubeAPI
server-to-server WebSocket without using browser Origin as an authorization
signal, so its terminal endpoint must never be exposed directly.

## Authentication and auditing

The browser passes its existing WebUI session token using the WebSocket
subprotocol header, never in a URL. Terminal access is disabled when the WebUI
session store is unavailable. An absent, expired, or invalid session is rejected
before the WebSocket is upgraded. Existing API-key/Bearer authentication
middleware also protects the route when an auth callback is configured.

CubeAPI emits `terminal.session.open` and `terminal.session.close` audit events
with the operator, sandbox ID, and container ID. The URL sandbox ID must match
the first protocol frame, and `open` is emitted only after that frame reaches
CubeMaster; failed upgrades and backend connection attempts do not create an
open audit record. Each terminal is a separate Cubelet TTY process, so multiple
sessions and sandboxes remain isolated. CubeAPI rejects sessions above
`TERMINAL_MAX_SESSIONS_PER_SANDBOX` with HTTP 429. The limit is process-local,
so multiply it by the number of CubeAPI replicas when sizing a deployment.

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
