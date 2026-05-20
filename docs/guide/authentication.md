# Authentication

By default, Cube API Server allows all requests without any authentication. To enable authentication, start the server with an auth callback URL. When configured, every incoming request is validated by forwarding the credential header to your callback service — Cube API Server acts purely as a passthrough proxy for the auth decision.

## Enabling Authentication

Pass `--auth-callback-url` at startup, or set the equivalent environment variable:

```bash
# CLI flag
./cube-api --auth-callback-url https://your-auth-service/verify

# Or via environment variable
export AUTH_CALLBACK_URL=https://your-auth-service/verify
./cube-api
```

When `AUTH_CALLBACK_URL` is not set (the default), all requests are allowed without any credential check.

## How It Works

When a request arrives, Cube API Server:

1. Extracts the credential from the request header (`Authorization: Bearer` takes priority over `X-API-Key`).
2. Forwards a `POST` request to the callback URL with the credential header, the original request path, **and the HTTP method**.
3. If the callback returns **HTTP 200**, the request is allowed through.
4. Any other status code causes the request to be rejected with **HTTP 401 Unauthorized**.

```
Client ──→ Cube API Server
                │
                ├─ extract credential (Bearer / API Key)
                ├─ capture method (GET / POST / DELETE / PATCH …)
                │
                └─ POST → your auth service
                                │
                       200 ─────┤──→ allow request
                    non-200 ────┘──→ 401 Unauthorized
```

## Sending Credentials from the SDK

The E2B SDK passes the value of `E2B_API_KEY` as `Authorization: Bearer <key>` on every request.

```bash
export E2B_API_KEY=your-actual-api-key
```

You can also send `X-API-Key` directly if your integration does not use the E2B SDK:

```
X-API-Key: your-actual-api-key
```

Both formats are accepted. `Authorization: Bearer` takes priority if both are present.

## Callback Request Format

Cube API Server sends a `POST` to your callback URL with the following headers:

| Header | Value |
|--------|-------|
| `Authorization` | `Bearer <token>` — present when the client used Bearer auth |
| `X-API-Key` | `<key>` — present when the client used API Key auth |
| `X-Request-Path` | The original request path (e.g. `/templates/my-tmpl`) |
| `X-Request-Method` | The HTTP method of the original request (e.g. `GET`, `DELETE`) |

The two credential headers are mutually exclusive. Your callback receives whichever one the client sent.

::: warning Validate both path **and** method
Multiple HTTP methods are mounted on the same path — for example, `/templates/:id` handles `GET` (read), `POST` (rebuild), `DELETE` (delete), and `PATCH` (update). A callback that only whitelists by path cannot distinguish a read from a destructive operation: a caller with read-only access could escalate to delete or overwrite a template.

Always check **both** `X-Request-Path` and `X-Request-Method` in your callback.
:::

### Example callback (Python/FastAPI)

```python
from fastapi import FastAPI, Request
from fastapi.responses import Response

app = FastAPI()

VALID_KEYS = {"secret-key-1", "secret-key-2"}

# Define which methods each key is allowed to use per path prefix.
# Always check BOTH path and method — the same path (e.g. /templates/:id)
# serves GET (read), DELETE, POST (rebuild), and PATCH (update).
READ_METHODS = {"GET", "HEAD"}
WRITE_METHODS = {"POST", "DELETE", "PATCH", "PUT"}

READONLY_KEYS = {"readonly-key-1"}
FULL_ACCESS_KEYS = {"secret-key-1", "secret-key-2"}

@app.post("/verify")
async def verify(request: Request):
    path = request.headers.get("X-Request-Path", "")
    method = request.headers.get("X-Request-Method", "").upper()

    # Extract credential (Bearer takes priority)
    key = None
    auth = request.headers.get("Authorization", "")
    if auth.startswith("Bearer "):
        key = auth.removeprefix("Bearer ").strip()
    else:
        key = request.headers.get("X-API-Key", "")

    if not key:
        return Response(status_code=401)

    if key in FULL_ACCESS_KEYS:
        return {}                           # 200 → allow all

    if key in READONLY_KEYS:
        if method in READ_METHODS:
            return {}                       # 200 → allow reads
        return Response(status_code=403)   # deny writes/deletes

    return Response(status_code=401)
```

## Error Responses

| Scenario | HTTP Status |
|----------|-------------|
| No credential provided | `401 Unauthorized` |
| Callback returned non-200 | `401 Unauthorized` |
| Callback unreachable | `500 Internal Server Error` |
