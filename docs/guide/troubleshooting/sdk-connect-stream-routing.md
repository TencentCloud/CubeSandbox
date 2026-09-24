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

## Environment

- Go SDK Connect streaming calls through CubeProxy
- Data-plane settings: `CUBE_PROXY_NODE_IP`, `CUBE_PROXY_PORT_HTTP`, and `CUBE_SANDBOX_DOMAIN`

## Root Cause

The SDK data-plane requests must reach the CubeProxy stream endpoint. If `CUBE_PROXY_NODE_IP` is unset, points to the wrong node, or the proxy route is replaced by a web service or gateway, the response is not Connect framed data.

## Resolution

1. Set `CUBE_PROXY_NODE_IP` to the IP address of the CubeProxy node reachable from the client:

   ```bash
   export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>
   ```

2. Confirm that `CUBE_PROXY_PORT_HTTP` and the sandbox domain match the CubeProxy configuration.
3. Check that the request is sent to the data-plane endpoint and that an intermediate gateway does not rewrite or compress the stream response.
4. Retry the SDK call. The diagnostic includes the endpoint hint for HTML pages and 404 text pages; other text errors retain the server's original message.

For remote clients, verify firewall rules and routing from the client to the CubeProxy node. When the SDK runs on the CubeProxy host, use its local node address as appropriate.

## References

- Issue tracking: [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)
