---
title: Unreleased Changes
---

# Unreleased Changes

## Go SDK

- Connect streaming calls now report a routing diagnostic for HTML pages, 404 text pages, and invalid Connect frames such as JSON or whole-response gzip payloads. HTTP 200 responses labeled `text/html` are rejected before reading the stream body; other non-200 text errors retain the server's message. Non-200 HTML and 404 text error messages are truncated to 200 characters before appending the diagnostic hint.
- Connect calls report routing errors for HTML error pages. The non-JSON `text/*` heuristic applies only to 404 responses. Filesystem RPCs (`Files.List`/`Stat`/`Exists`/`Remove`/`Rename`/`MakeDir`) also detect HTML routing pages returned with HTTP 200. Routing errors clear the not-found classifications, while 401/403 responses keep `ErrAuthentication`. JSON API error documents keep their classification even when labeled `text/*`. See the [Connect stream routing guide](../guide/troubleshooting/sdk-connect-stream-routing.md).
- `Pty.Kill` now returns an error for a 404 HTML routing page. A genuine process-not-found response still returns `(false, nil)`.
- Connect stream diagnostics for unfollowed redirects include only the truncated redirect path.
- `RunCode` uses a separate endpoint and is not covered by this change. Refs [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759).
