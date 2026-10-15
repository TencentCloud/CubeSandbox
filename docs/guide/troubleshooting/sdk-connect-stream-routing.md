---
title: Go SDK Connect Stream Returns an HTML or Text Response
author: tokove
date: 2026-09-24
tags:
  - sdk
  - networking
  - cube-proxy
lang: en-US
---

# Go SDK Connect Stream Returns an HTML or Text Response

## Symptom

Go SDK calls such as `Commands.Run`, `Files.WatchDir`, or `Pty.Create` fail with a message about an HTML/text response, an invalid Connect envelope, or a message that is too large. A browser login page, gateway error page, JSON response, or compressed response may be returned instead of a Connect stream.

Known limitation: `RunCode` uses the separate Jupyter `/execute` endpoint, which is not covered by this diagnostic yet — a misrouted 200 HTML page fed to its stream parser surfaces as an empty result with no error (see [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)).

## Environment

- Go SDK Connect streaming calls through CubeProxy
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
