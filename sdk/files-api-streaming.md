# Files API and Streaming Request Body Notes

This note summarizes the behavior of the CubeSandbox Python SDK file API,
the E2B SDK reference behavior, and the known risk around streaming request
bodies in the current `IPOverrideTransport` implementation.

Related issue: https://github.com/TencentCloud/CubeSandbox/issues/498

## Scope

The issue reports two symptoms:

1. `sandbox.files.read()` / `sandbox.files.write()` may fail with
   `httpx.RequestNotRead`.
2. Direct envd `/files` access may return `502` in some deployments.

These are related to file content transfer, not filesystem metadata RPCs.
Process APIs and metadata RPCs can work while `/files` still fails.

## CubeSandbox Python SDK Behavior

`cubesandbox` reads and writes file content through envd's HTTP files API:

- `files.read(path)` -> `GET /files?path=...&username=...`
- `files.write(path, data)` -> `POST /files?path=...&username=...`

The current implementation first tries `application/octet-stream` upload and
falls back to `multipart/form-data` when the server rejects the request:

```python
resp = client.post(
    f"http://{sandbox.get_host(49983)}/files",
    params={"path": path, "username": effective_user},
    headers={"Content-Type": "application/octet-stream"},
    content=body,
)

if resp.status_code >= 400:
    resp = client.post(
        f"http://{sandbox.get_host(49983)}/files",
        params={"path": path, "username": effective_user},
        files={"file": (path, body)},
    )
```

When `CUBE_PROXY_NODE_IP` is set, data-plane requests use
`IPOverrideTransport` to connect to the proxy node IP while preserving the
virtual sandbox host header.

## `RequestNotRead` Risk

The current `IPOverrideTransport` rebuilds each request and reads
`request.content`:

```python
proxied = httpx.Request(
    method=request.method,
    url=url,
    headers=...,
    content=request.content,
)
```

This is unsafe for streaming request bodies. In `httpx`, `request.content`
is only available after the request body has been read into memory. For
multipart uploads, file objects, generators, or other stream-backed request
bodies, accessing `request.content` can raise:

```text
httpx.RequestNotRead: Attempted to access streaming request content, without having called `read()`.
```

Impact:

- `content=b"..."` uploads usually work because the body is already in memory.
- Multipart uploads created via `files=...` can fail.
- File-object uploads are especially likely to be stream-backed.
- GET requests usually have no body, but the transport should not rely on
  `request.content` for correctness.

## E2B SDK Reference Behavior

E2B's Python SDK supports file data as:

```python
data: str | bytes | IO
```

For newer envd versions, E2B uses octet-stream upload. It converts file-like
objects to bytes before sending:

```python
content = to_upload_body(file_data, gzip)
client.post("/files", content=content, ...)
```

For older envd versions, E2B uses multipart upload:

```python
client.post("/files", files=httpx_files, ...)
```

When a binary `IOBase` file object is passed to `files=...`, httpx can stream
the multipart body. E2B's transport does not read `request.content`; it passes
the original request to `httpx.HTTPTransport.handle_request()`. That is why E2B
does not hit this specific `RequestNotRead` failure.

## `/files` 502 Is a Separate Layer

If direct access like this returns `502`, the request has already bypassed the
SDK transport:

```python
httpx.get(
    f"http://{proxy_node_ip}:{proxy_port}/files",
    params={"path": path, "username": "root"},
    headers={"Host": f"49983-{sandbox_id}.cube.app"},
)
```

In that case, fixing `IPOverrideTransport` is not enough. A direct `502` points
to the CubeProxy -> envd data-plane path, for example:

- envd `/files` handler failure
- proxy upstream connection reset or invalid response
- missing or incorrect sandbox port mapping
- template/envd version behavior mismatch
- deployment-specific CubeProxy or network configuration

Process APIs can still work because they use a different envd route
(`/process.Process/Start`) on the same port.

## Recommended SDK Fix

Avoid reading `request.content` in `IPOverrideTransport`.

Minimum safe fix:

```python
body = request.read()
proxied = httpx.Request(
    method=request.method,
    url=url,
    headers=...,
    content=body,
)
```

This avoids `RequestNotRead`, but it buffers the whole body in memory.

Preferred fix:

- Preserve the original request stream semantics.
- Route TCP connections to `CUBE_PROXY_NODE_IP:CUBE_PROXY_PORT_HTTP`.
- Preserve the original virtual `Host` header.
- Do not force stream-backed request bodies into `request.content`.

Add regression tests for:

- `POST /files` with `content=b"hello"`
- `POST /files` with `files={"file": (...)}`
- `POST /files` with a binary file object
- `GET /files` with proxy IP override enabled

## Operational Debug Checklist

For deployments reporting direct `/files` 502:

1. Verify process API works through the same host:
   `Host: 49983-<sandbox-id>.cube.app`.
2. Verify direct `/files` without the SDK:
   `GET /files?path=...&username=root`.
3. Check CubeProxy error logs for upstream connect/reset/invalid response.
4. Check envd logs inside the sandbox or guest VM.
5. Confirm template envd version and whether `/files` is expected to be
   supported.
6. Test both upload formats:
   - `multipart/form-data`
   - `application/octet-stream`

## Current Interpretation

The `RequestNotRead` part is a real SDK risk whenever request bodies are
stream-backed. The direct `/files` 502 part is environment-dependent and should
be debugged separately at the CubeProxy/envd layer.
