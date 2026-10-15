---
title: Unreleased Changes
---

# Unreleased Changes

## Go SDK

- Connect streaming calls now report a routing diagnostic for HTML pages, 404 text pages, and invalid Connect frames such as JSON or whole-response gzip payloads. HTTP 200 responses labeled `text/html` are rejected before reading the stream body; other non-200 text errors retain the server's message. Non-200 HTML and 404 text error messages are truncated to 200 characters before appending the diagnostic hint.
- On Connect streaming calls, any 404 whose body is HTML or a non-JSON `text/*` page is treated as a routing error and no longer matches `errors.Is` for any sentinel (`ErrSandboxNotFound`, `ErrTemplateNotFound`, `ErrVolumeNotFound`). A JSON body — including a JSON API error document served with a `text/*` content type — keeps its classification. Check `CUBE_PROXY_NODE_IP` and the [Connect stream routing guide](../guide/troubleshooting/sdk-connect-stream-routing.md) when this diagnostic appears.
- `RunCode` uses a separate endpoint and is not covered by this change. Refs [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759).
