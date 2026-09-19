# cube-envd API

[中文](cube-envd-api_zh.md) · [Build and usage](../README.md)

This reference describes the guest-facing interfaces registered by cube-envd.
The default listen port is **49983**. Sandbox and template creation belong to
CubeSandbox's platform API, not this daemon; see the [Python SDK](../../sdk/python).

## Contents

- [Connection and authentication](#connection-and-authentication)
- [HTTP endpoints](#http-endpoints)
- [Process RPC](#process-rpc)
- [Filesystem RPC](#filesystem-rpc)
- [Connect encoding and streaming](#connect-encoding-and-streaming)
- [Errors and limits](#errors-and-limits)
- [Protocol sources and examples](#protocol-sources-and-examples)

## Connection and authentication

Set `ENVD_URL` to the reachable envd endpoint of your sandbox. Through CubeProxy,
this normally uses the hostname returned by `sandbox.get_host(49983)`, with the
scheme and proxy port configured for your deployment. If using a proxy-node IP,
preserve that sandbox hostname in `Host`. A CubeAPI URL is not an envd URL.

The examples use curl and a deployment-supplied token:

```bash
export ENVD_URL='https://envd.example.com'  # Replace with your sandbox endpoint.
export ENVD_ACCESS_TOKEN='replace-with-the-sandbox-envd-token'
```

| Header or query | Meaning |
| --- | --- |
| `X-Access-Token` | Daemon access token installed through `/init`. Required on protected endpoints once a token is set. |
| `Authorization: Basic ...` | Select a guest user for Process/Filesystem RPCs. The username is used; the password is not a login credential. For example, `curl --user root:`. |
| `username` query parameter | Select a guest user for `GET /files` and `POST /files`. |
| `username` JSON field | Select a guest user for `POST /files/compose`. |

When no explicit execution user is supplied, operations use the daemon's current
default user, initially `root`. A selected username must exist in the guest.
File HTTP handlers use their query/body username, not Basic authentication, to
select the execution identity. A Basic username, when supplied, is still validated
by the common middleware. Platform API keys and proxy traffic tokens are separate
from `X-Access-Token`; use the SDK's configured routing and credentials for normal
sandbox access.

`GET /health` does not require the daemon token. `/init` validates token changes
itself. `GET /files` and `POST /files` accept either the daemon token or a signed
URL. `/files/compose` does not accept signed-URL authentication. If no daemon
token has been installed, the token check is inactive; this does not describe
any authentication enforced by the surrounding platform.

### Signed file URLs

The `/files` query supports `path`, `username`, `signature`, and optional
`signature_expiration` (Unix seconds, signed 64-bit decimal integer).

The signature is:

```text
input = path + ":" + operation + ":" + username + ":" + accessToken
input = input + ":" + decimalExpiration   # Only when expiration is supplied.
signature = "v1_" + base64_without_padding(SHA256(UTF8(input)))
```

`operation` is `read` for GET and `write` for POST. Base64 uses the standard
alphabet, not the URL-safe alphabet; URL-encode the query values. `path` and
`username` are the decoded query strings before filesystem path expansion, with
an omitted value treated as an empty string. This is SHA-256 of the concatenated
input, not HMAC. An expiration earlier than the current Unix second is rejected.

A nonempty `X-Access-Token` header takes precedence over a signature. An incorrect
header token is rejected even if the URL has a valid signature. Malformed
`signature_expiration` produces HTTP 400, including when header authentication
is used. Missing, invalid or expired credentials produce HTTP 401.

## HTTP endpoints

| Method | Path | Request | Successful response |
| --- | --- | --- | --- |
| GET | `/health` | No body. | 204, empty body. |
| POST | `/init` | Initialization JSON. | 204, empty body. |
| GET | `/envs` | No body. | 200, JSON environment map. |
| GET | `/metrics` | No body. | 200, JSON metrics object. |
| GET | `/files` | File query and optional range/conditional headers. | 200 file bytes; 206 for a satisfied range; 304 for an unchanged representation. |
| POST | `/files` | Raw bytes or multipart file parts. | 200, JSON array of written-file summaries. |
| POST | `/files/compose` | Destination and ordered source paths. | 200, JSON destination summary. |

Unsupported methods return 405 after applicable readiness/authentication checks.
In particular, HEAD is not a substitute for GET on `/health`, `/files`, `/envs`
or `/metrics`. CORS preflight uses OPTIONS with `Access-Control-Request-Method`
and returns 204; a successful preflight does not mean that the target endpoint
implements every CORS-advertised method.

### GET /health

Returns 204 when the daemon's readiness checks pass, otherwise 503. A successful
response has an empty body and `Cache-Control: no-store`.

```bash
curl -sS -o /dev/null -w '%{http_code}\n' "$ENVD_URL/health"
```

### POST /init

This is a platform initialization interface, normally called by the runtime.
It changes guest state, rather than creating a sandbox. JSON fields are:

| Field | Type | Effect |
| --- | --- | --- |
| `envVars` | Object mapping strings to strings | Merge values into the daemon environment used by subsequent operations. |
| `accessToken` | Nonempty string | Install or validate the daemon token. |
| `defaultUser` | String | Set the default execution user when nonempty. |
| `defaultWorkdir` | String | Set the default working directory when nonempty. |
| `caBundle` | String | Supply CA material for guest certificate setup. |
| `hyperloopIP` | String | Supply the guest event-forwarding address. |
| `timestamp` | Timestamp string with timezone | Apply initialization ordering and attempt to set the guest clock. |
| `volumeMounts` | Array of objects | Each object has `nfs_target` and `path` strings for guest NFS setup. |

Fields are optional, but omission is not a way to bypass an installed token.
Once a token exists, a request must supply the matching body token or satisfy
the platform's MMDS token-hash validation for a change/reset. The header token
alone does not authorize an omitted or different body token. Unauthorized token
changes return 401. An older initialization timestamp is ignored; equal instants
can be applied again. Initialization may have side effects before later work
fails, so an error is not a promise of transactional rollback.

Example body for a platform managing an isolated guest:

```json
{
  "envVars": {"APP_MODE": "demo"},
  "accessToken": "example-guest-token",
  "defaultUser": "user",
  "defaultWorkdir": "/tmp"
}
```

The body limit is 256 KiB and the JSON nesting limit is 64. Oversized input
returns 413; excessive nesting, malformed JSON or timestamps return 400. Keep clock and mount
initialization inside the guest's isolation boundary.

### GET /envs and GET /metrics

Both use ordinary daemon-token authorization and `Cache-Control: no-store`.
`/envs` returns the current daemon environment, not a particular child's complete
environment after its per-process overrides.

```bash
curl -sS -H "X-Access-Token: $ENVD_ACCESS_TOKEN" "$ENVD_URL/envs"
curl -sS -H "X-Access-Token: $ENVD_ACCESS_TOKEN" "$ENVD_URL/metrics"
```

| Metrics field | Type / unit | Meaning |
| --- | --- | --- |
| `ts` | Integer, Unix seconds | Sample time. |
| `cpu_count` | Integer | CPUs visible in the guest. |
| `cpu_used_pct` | Number, percent | CPU busy ratio from successive daemon samples; the first sample uses counters since boot. |
| `mem_total`, `mem_used`, `mem_cache` | Integer, bytes | Guest memory totals, usage and cache. |
| `mem_total_mib`, `mem_used_mib` | Integer, MiB | Memory values divided by 1024². |
| `disk_total`, `disk_used` | Integer, bytes | Root filesystem capacity and usage based on available blocks. |

These are guest resource metrics, not the daemon's RSS. Sampling failure returns 500.

### GET /files

Use `path` for the guest file and optional `username` for its execution user.
The response is file content, not a JSON wrapper. Downloads support gzip
negotiation, byte ranges including multipart ranges, and HTTP modification-time
conditions. Unsatisfied ranges return 416; a failed precondition can return 412.

```bash
curl -sS --fail --get "$ENVD_URL/files" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  --data-urlencode 'path=/tmp/envd-api.txt' \
  --data-urlencode 'username=root' --output downloaded.txt
```

`/tmp/envd-api.txt` is a path inside the sandbox; `downloaded.txt` is written in
the caller's current directory. Missing files return 404. Paths may be absolute
or relative to the selected guest user's home; `~` and `~/...` refer to that
home. `~otheruser` expansion is not supported.

### POST /files

| Content-Type | Body and destination |
| --- | --- |
| `application/octet-stream` | Raw file bytes. The `path` query parameter is required. |
| `multipart/form-data` or `multipart/mixed` | File parts with `Content-Disposition: form-data; name="file"; filename="..."`. Use each filename as its destination, or supply `path` to override it. |

`Content-Encoding: gzip` is supported for the request body. Uploading truncates
an existing target; parent directories are created as needed. Uploads are not a
transaction: errors or disconnects can leave truncated/partial files or earlier
successful multipart writes. There is no daemon-specific total upload-size cap;
disk space, permissions and parser/resource limits still apply.

```bash
printf 'hello from the API\n' > upload.txt
curl -sS --fail -X POST "$ENVD_URL/files?path=%2Ftmp%2Fenvd-api.txt&username=root" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/octet-stream' --data-binary @upload.txt
```

Example response body:

```json
[{"name":"envd-api.txt","path":"/tmp/envd-api.txt","type":"file"}]
```

For compatibility, this successful response contains JSON but uses
`Content-Type: text/plain; charset=utf-8`. It is a file-summary array, not a
protobuf `EntryInfo` response.

### POST /files/compose

| Field | Type | Meaning |
| --- | --- | --- |
| `destination` | Nonempty string | Guest destination path. |
| `source_paths` | Nonempty array of strings | Guest source files, concatenated in this order. |
| `username` | Optional string | Guest execution user. |

```json
{
  "destination": "/tmp/combined.txt",
  "source_paths": ["/tmp/part-1.txt", "/tmp/part-2.txt"],
  "username": "root"
}
```

After writing a temporary file, the handler publishes it by rename and attempts
to delete the consumed, unchanged source files. Source cleanup is best-effort;
a source observed to have been replaced is retained. An accepted compose can
finish after client disconnect. The 200 response is a single file-summary object
with `name`, `path` and `type: "file"`, using `Content-Type: application/json`.
This endpoint uses `X-Access-Token`, not the `/files` signature query.

## Process RPC

All methods use **POST** under `/process.Process/`. Types and field names below
use ProtoJSON notation; the source schema is [process.proto](../proto/process.proto).

| Method | Request fields | Response | Shape |
| --- | --- | --- | --- |
| `List` | `{}` | `processes: ProcessInfo[]` | Unary. |
| `Start` | `process: ProcessConfig`, optional `pty`, `tag`, `stdin` | `event: ProcessEvent` | Server stream. |
| `Connect` | `process: ProcessSelector` | `event: ProcessEvent` | Server stream. |
| `Update` | `process: ProcessSelector`, optional `pty` | `{}` | Unary; update PTY dimensions. |
| `SendInput` | `process: ProcessSelector`, `input: ProcessInput` | `{}` | Unary. |
| `StreamInput` | A sequence of `start`, `data`, or `keepalive` messages | `{}` | Client stream. |
| `SendSignal` | `process: ProcessSelector`, `signal` | `{}` | Unary. |
| `CloseStdin` | `process: ProcessSelector` | `{}` | Unary. |

### Process types

| Type / field | Type | Meaning |
| --- | --- | --- |
| `ProcessConfig.cmd` | String | Executable; shell syntax requires an explicit shell such as `/bin/sh` with `args: ["-c", "..."]`. |
| `ProcessConfig.args` | String array | Arguments following the executable. |
| `ProcessConfig.envs` | String map | Per-process environment overrides. |
| `ProcessConfig.cwd` | Optional string | Working directory; otherwise use the initialized default, then the guest user's home. Relative paths are home-relative. |
| `StartRequest.pty.size` | Object with `cols`, `rows` uint32 | Request a PTY and its dimensions. |
| `StartRequest.tag` | Optional string | Selector label; use the returned PID for an unambiguous process identity. |
| `StartRequest.stdin` | Optional boolean | Keep a non-PTY stdin pipe open; omission defaults to true. SDK defaults may differ. |
| `ProcessSelector` | One of `pid: uint32` or `tag: string` | Select an existing managed process. |
| `ProcessInfo` | `config`, `pid`, optional `tag` | Item returned by List. |
| `ProcessInput` | One of `stdin: bytes` or `pty: bytes` | Base64 text in JSON; for example `"aGVsbG8K"` means `hello` followed by newline. |
| `signal` | Enum | `SIGNAL_SIGTERM` (15) or `SIGNAL_SIGKILL` (9); unspecified/unsupported values are rejected. |

Start and Connect emit an initial `event.start` with the PID, output events,
then a terminal process event. A process nonzero exit is not by itself an RPC
transport failure; read `event.end.exitCode` and the other end fields.

| Process event | Payload |
| --- | --- |
| `start` | `pid: uint32`. |
| `data` | One of `stdout`, `stderr`, `pty`, each containing bytes. |
| `end` | `exitCode: sint32`, `exited: bool`, `status: string`, optional `error: string`. |
| `keepalive` | Empty object; not command output. |

Connect subscribes to an existing process; it is not a persisted replay API.
Disconnecting an output subscriber does not by itself terminate the process.
SendSignal explicitly terminates it. CloseStdin signals EOF only for non-PTY
processes; for PTYs send Ctrl+D (`0x04`, Base64 `BA==`) through PTY input.

For StreamInput, first send `{"start":{"process":{"pid":123}}}`, then
`{"data":{"input":{"stdin":"aGVsbG8K"}}}` messages. The sequence preserves
input order. Closing the RPC input stream does not replace CloseStdin.

`Connect-Timeout-Ms` on Start controls process lifetime for ordinary positive
durations and also the response subscription deadline. On Connect it limits
only that subscription. SendInput, StreamInput and CloseStdin may wait for a
pipe and do not use a valid Connect timeout as a write-cancellation guarantee.
`Keepalive-Ping-Interval` for Start/Connect is in seconds, default 90.

### Unary example

```bash
curl -sS --fail -X POST "$ENVD_URL/process.Process/List" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  --user root: --data '{}'
```

## Filesystem RPC

All methods use **POST** under `/filesystem.Filesystem/`. File content read/write
uses HTTP `/files`; there are no `ReadFile` or `WriteFile` RPCs in this service.
The source schema is [filesystem.proto](../proto/filesystem.proto).

| Method | Request fields | Response | Behavior |
| --- | --- | --- | --- |
| `Stat` | `path: string` | `entry: EntryInfo` | Get metadata, including final symlink information. |
| `MakeDir` | `path: string` | `entry: EntryInfo` | Create a directory and needed parents; an existing target directory is a conflict. |
| `Move` | `source: string`, `destination: string` | `entry: EntryInfo` | Move/rename an entry. |
| `ListDir` | `path: string`, `depth: uint32` | `entries: EntryInfo[]` | List descendants; omitted/zero depth is treated as 1. |
| `Remove` | `path: string` | `{}` | Remove a file or recursively remove a directory; an absent path succeeds. |
| `WatchDir` | `path: string`, `recursive: bool` | WatchDirResponse stream | Watch until cancellation or a terminal error. |
| `CreateWatcher` | `path: string`, `recursive: bool` | `watcherId: string` | Create a polling watcher. |
| `GetWatcherEvents` | `watcherId: string` | `events: FilesystemEvent[]` | Drain queued events; an empty queue returns no events. |
| `RemoveWatcher` | `watcherId: string` | `{}` | Release a polling watcher. |

Paths refer to the guest filesystem under the selected user's permissions.
Absolute paths remain absolute; relative paths and `~/...` are home-relative.
An empty path uses the initialized default workdir when present, otherwise the
user's home. Destructive operations protect the filesystem root.

### EntryInfo

| Field | Proto type / JSON form | Meaning |
| --- | --- | --- |
| `name` | String | Entry name. |
| `type` | FileType enum | `FILE_TYPE_FILE`, `FILE_TYPE_DIRECTORY`, or `FILE_TYPE_SYMLINK`; zero is `FILE_TYPE_UNSPECIFIED`. |
| `path` | String | Resolved entry path. |
| `size` | int64 / decimal string | Size in bytes. |
| `mode` | uint32 / number | Numeric mode information. |
| `permissions` | String | Human-readable permissions. |
| `owner`, `group` | String | Owner and group information. |
| `modifiedTime` | protobuf Timestamp / string | Modification time in timestamp notation. |
| `symlinkTarget` | Optional string | Target text for a symlink. |

### Directory events

`FilesystemEvent` has `name: string` and `type: EventType`. EventType values are
`EVENT_TYPE_CREATE`, `EVENT_TYPE_WRITE`, `EVENT_TYPE_REMOVE`, `EVENT_TYPE_RENAME`
and `EVENT_TYPE_CHMOD` (zero is `EVENT_TYPE_UNSPECIFIED`). Names are relative to
the watched directory. Events describe filesystem notifications, not exactly
one event per user action or a durable change journal.

WatchDirResponse is one of `start: {}`, `filesystem: FilesystemEvent`, or
`keepalive: {}`. Polling clients call CreateWatcher, repeatedly call
GetWatcherEvents, then call RemoveWatcher to release resources. A second poll
does not replay events drained by the first. Streaming clients must consume the
Connect end-of-stream record to distinguish normal termination from an error.

### Unary example

```bash
curl -sS --fail -X POST "$ENVD_URL/filesystem.Filesystem/Stat" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  --user root: --data '{"path":"/tmp/envd-api.txt"}'
```

## Connect encoding and streaming

| Call shape | Connect Content-Type | Body |
| --- | --- | --- |
| Unary | `application/json` or `application/proto` | A single JSON object or protobuf message. |
| Server/client streaming | `application/connect+json` or `application/connect+proto` | Length-prefixed message envelopes. |

Use `Connect-Protocol-Version: 1`. JSON uses lowerCamelCase field names, enum
names, Base64 for bytes, decimal strings for int64, and timestamp strings.
Default-valued fields may be omitted. A protobuf `oneof` permits only one of its
alternatives. Proto field names remain available in the linked schemas.

gRPC and binary gRPC-Web media types are also routed for these methods, with
protobuf or JSON payloads (`application/grpc`, `application/grpc+proto`,
`application/grpc+json`, and corresponding `application/grpc-web` variants).
They use their own framing and terminal status conventions; do not decode them
as Connect envelopes. This reference's RPC examples use Connect, matching the
SDK's RPC calls. File content transfer uses the HTTP endpoints above.

Each Connect stream envelope is:

```text
1-byte flags | 4-byte unsigned big-endian payload length | payload bytes
```

Flag `0x00` denotes an uncompressed message; bit `0x01` denotes compression.
The `0x02` end-of-stream envelope contains JSON metadata and an optional
`error` object, even for protobuf streams. A process `event.end` is a domain
event and is different from this transport end-of-stream envelope. Check both:
HTTP 200 alone does not imply that a streaming RPC succeeded.

Unary gzip uses `Content-Encoding` / `Accept-Encoding`; Connect stream gzip
uses `Connect-Content-Encoding` / `Connect-Accept-Encoding`. The compression
flag applies to the message payload. Unsupported encodings and media types are
rejected rather than interpreted as raw JSON.

For example, this creates an uncompressed Start request envelope in the caller's
current directory and saves the framed response for a Connect decoder:

```bash
python3 - <<'PY' > start-request.bin
import json
import struct
import sys

request = {"process": {"cmd": "/bin/sh", "args": ["-c", "printf hello"]}, "stdin": False}
payload = json.dumps(request).encode("utf-8")
sys.stdout.buffer.write(struct.pack(">BI", 0, len(payload)) + payload)
PY
curl -sS --fail -X POST "$ENVD_URL/process.Process/Start" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/connect+json' -H 'Connect-Protocol-Version: 1' \
  --user root: --data-binary @start-request.bin --output start-response.bin
```

Decoded message payloads include `{"event":{"start":{"pid":123}}}` and
`{"event":{"data":{"stdout":"aGVsbG8="}}}`. PID and event boundaries vary;
do not assume one output event or treat the `.bin` response as plain JSON.
For ordinary command/file use, prefer the SDK example in the
[template and SDK guide](usage.md).

## Errors and limits

Connect unary errors use string codes, for example:

```json
{"code":"invalid_argument","message":"cwd '/missing' does not exist"}
```

Streaming RPC errors use the terminal envelope's `error` member. Common codes
include `invalid_argument`, `unauthenticated`, `permission_denied`, `not_found`,
`already_exists`, `failed_precondition`, `resource_exhausted`, `unimplemented`,
`internal`, `unknown`, `unavailable`, `canceled`, and `deadline_exceeded`.
Some process I/O failures use `unknown` for compatibility. Treat error messages
as diagnostics rather than stable identifiers.

HTTP file errors and common token rejection instead use a numeric HTTP code:

```json
{"code":404,"message":"not found"}
```

These responses use `application/json; charset=utf-8`. Token middleware can
return this HTTP error before a request reaches an RPC handler. `/init` errors,
malformed query binding, and unknown routes may return plain text or an empty
body; there is no universal JSON error envelope for every HTTP response.

There is no daemon-specific total RPC-message or uploaded-file size cap. Parser
constraints and operating-system resources still apply; `/init` has the explicit
256 KiB / 64-level limits described above. Before readiness or during failure,
ordinary requests can receive 503. Authentication and readiness checks may
precede an endpoint's own validation. Full-volume or permission errors, and
connection termination after response headers, must be handled by callers.

## Protocol sources and examples

| Reference | Purpose |
| --- | --- |
| [Process schema](../proto/process.proto) | All Process message and enum definitions. |
| [Filesystem schema](../proto/filesystem.proto) | All Filesystem message and enum definitions. |
| [HTTP routing and handlers](../src/transport/rest.rs) | HTTP response and content-type behavior. |
| [Authentication](../src/transport/auth.rs) | Token and signed-file handling. |
| [Maintained SDK acceptance](../../tests/e2e/sdk_compat/README.md#selected-envd-acceptance) | Runnable health, command, file and template scenarios. |
| [File transfer tests](../tests/FILE_TRANSFERS.md) | Range, multipart, signatures, compose and error coverage. |

Only registered production routes and the two production protobuf services are
covered here. Test-only conformance services are not part of the normal API.
