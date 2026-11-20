---
title: SDK Connect Stream Returns an HTML or Text Response
author: tokove
date: 2026-09-24
tags:
  - sdk
  - networking
  - cube-proxy
lang: en-US
---

# SDK Connect Stream Returns an HTML or Text Response

## Symptom

SDK calls that read a Connect stream fail with a message about an HTML/text response, an invalid Connect envelope, or a message that is too large:

- Go SDK: `Commands.Run`, `Files.WatchDir`, `Pty.Create`, `Pty.Connect`
- Python SDK: `commands.run`, `files.watch_dir`, `pty.create`, `pty.connect`
- Node SDK: `commands.run`, `files.watchDir`, `pty.create`, `pty.connect`

Typical messages are `received HTML instead of a Connect response`, `response is not a Connect envelope (first 5 bytes: ...)`, or an HTTP error followed by `response may be an HTML/text page instead of a Connect stream`. SDK versions without this diagnostic report `Connect stream message too large: 558124867 bytes` instead: the HTML prefix `<!DOC` read as an envelope header. A browser login page, gateway error page, JSON response, or compressed response may be returned instead of a Connect stream.

Known limitations: `RunCode` uses the separate Jupyter `/execute` endpoint, which is not covered by this diagnostic yet — a misrouted 200 HTML page fed to its stream parser surfaces as an empty result with no error (see [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)). The plain-HTTP `Files.Read`/`Files.Write` paths are not covered either: a misrouted 200 HTML page is returned as the file's contents by `readFile`, and `writeFile` treats any status below 400 as success. The same holds for the Python SDK: `files.read` and `files.write` use plain `/files` HTTP requests, so a misrouted 200 HTML page is returned as the file contents by `files.read`. The Node SDK has the same gaps: `runCode` is not covered, `files.read` returns a 200 HTML page as the file contents, and `files.write` treats any status below 400 as success.

In the Node SDK the message starts with the call that failed: `command failed: ...` for `commands.run`, `WatchDir failed: ...` for `files.watchDir`, and `Start failed: ...` / `Connect failed: ...` for `pty.create` / `pty.connect`. Server text quoted in a message is cut to 200 bytes for an HTML page, an error body is read up to 64 KiB (longer bodies end with ` [truncated]`), and control characters in it are replaced with U+FFFD.

## Environment

- Go, Python and Node SDK Connect streaming calls through CubeProxy
- Data-plane settings: `CUBE_PROXY_NODE_IP`, `CUBE_PROXY_PORT_HTTP`, and `CUBE_SANDBOX_DOMAIN`

## Root Cause

The SDK data-plane requests must reach the CubeProxy stream endpoint. When `CUBE_PROXY_NODE_IP` is set, it must point at a reachable CubeProxy node; when it is unset, the client must be able to resolve `*.<CUBE_SANDBOX_DOMAIN>` to CubeProxy. If neither holds — or the route is taken over by a web service or gateway — the response is not Connect-framed data.

## Resolution

1. If `CUBE_PROXY_NODE_IP` is set, make sure it points at the IP address of the CubeProxy node reachable from the client:

   ```bash
   export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>
   ```

   If it is unset, make sure `*.<CUBE_SANDBOX_DOMAIN>` resolves to CubeProxy from the client instead.

2. Confirm that `CUBE_PROXY_PORT_HTTP` and the sandbox domain match the CubeProxy configuration.
3. Check that the request is sent to the data-plane endpoint and that an intermediate gateway does not rewrite or compress the stream response.
4. Retry the SDK call. The diagnostic includes the endpoint hint for HTML pages and 404 text pages; other text errors retain the server's original message.

For remote clients, verify firewall rules and routing from the client to the CubeProxy node. When the SDK runs on the CubeProxy host, use its local node address as appropriate.

## References

- Issue tracking: [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)
