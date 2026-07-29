# WebUI interactive terminal

The sandbox details page exposes **Open terminal** for a running sandbox. The
terminal is an operational capability served entirely by **CubeOps**: it opens
an interactive login shell inside the sandbox through the in-guest **envd**
agent's `process.Process` PTY API — the same envd path CubeOps already uses to
run commands. There is no dedicated terminal RPC on CubeMaster or Cubelet, and
CubeAPI is not involved.

```text
WebUI / xterm.js
      │  authenticated WebSocket ( /opsapi/v1/sdk/sandboxes/<id>/terminal/ws )
      ▼
CubeOps (JWT auth · audit · session limit · frame/idle limits)
      │  envd Connect stream ( process.Process: Start / SendInput / Update / SendSignal )
      ▼
envd (in-guest) → PTY → workload shell
```

## Deployment

No extra secret or cross-service token is required. CubeOps reaches envd exactly
as it already does for command execution: over the sandbox proxy using the
`<envd-port>-<sandboxId>.<sandbox_domain>` host, so `sandbox_domain` must be set
correctly for CubeOps (it defaults to the same value used elsewhere).

The only deployment requirement is that the ingress in front of CubeOps forwards
the WebSocket upgrade for `/opsapi/`. The bundled WebUI nginx configs
(`deploy/one-click/webui/nginx.conf`, the Helm chart, and the Terraform TKE
addon) already set `Upgrade`/`Connection` on that location.

## Authentication and auditing

A browser cannot set an `Authorization` header on a WebSocket upgrade, so the
existing CubeOps JWT access token is carried as the second
`Sec-WebSocket-Protocol` value (the first is the fixed `cube-terminal` marker),
never in the URL. CubeOps verifies the token before the socket is upgraded and
rejects an absent, expired, or invalid token with HTTP 401. Because the token
lives in the WebUI origin's storage and is unreachable cross-origin, this also
defeats cross-site WebSocket hijacking — the token is the authorization
boundary, so `Origin` is not enforced.

CubeOps emits `terminal.session.open` and `terminal.session.close` audit log
events with the operator (from the JWT) and the sandbox ID. Each session is an
independent envd PTY process, so multiple sessions and sandboxes stay isolated.
On disconnect CubeOps sends `SIGKILL` to the PTY so a lingering shell cannot
block the sandbox.

Limits (process-local; multiply by the number of CubeOps replicas when sizing):

- Up to 4 concurrent sessions per sandbox; further sessions get HTTP 429.
- 64 KiB maximum WebSocket frame.
- 30 minute idle timeout.

The current WebUI session is a cluster-administrator identity. CubeSandbox does
not yet attach an owner or tenant to a sandbox, so this endpoint does not claim
per-sandbox tenant authorization. Do not expose the cluster-global WebUI to
mutually untrusted tenants; a deployment that adds sandbox ownership must enforce
it before the upgrade.

## Validation checklist

1. Open a running sandbox in the WebUI and click **Open terminal**.
2. Run `id`, `ls`, `top`; verify ANSI output, cursor interaction, and copy/paste.
3. Resize the dialog and verify `stty size` reflects the new rows/cols.
4. Open two terminals against different sandboxes and verify output does not
   cross over.
5. Pause a sandbox and verify its terminal button is disabled; sign out (or use
   an expired token) and verify the WebSocket is rejected before upgrade.

Known limitation: automatic reconnect intentionally creates a new PTY session;
it cannot resume an exited shell process.
