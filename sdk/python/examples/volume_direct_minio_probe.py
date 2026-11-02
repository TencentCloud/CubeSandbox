"""Direct volume-content probe against a real MinIO-backed volume.

This is a verification harness, NOT a production gateway and NOT a substitute for
a real volume-content service:

- It starts a loopback HTTP server that serves ``/volumecontent/{volumeID}/...``
  by proxying objects from MinIO's S3 API, using the same routes and return
  shapes as the E2B contract.
- It then drives the current ``cubesandbox`` SDK against that loopback server, so
  read_file/list/get_info/exists exercise real MinIO bytes without creating any
  sandbox, VM or container.
- It finally prints the raw requests both SDKs would send, for side-by-side review.

Security: it uses the existing ``volume-s3.conf`` credentials only to read/write
one uniquely named probe object under the ``sdk-direct-probe/`` prefix, and
deletes that object on exit. It never deletes the bucket or any other object.
The loopback server binds to 127.0.0.1 and accepts one fixed token supplied by
the caller. This does not validate tenant isolation, symlink confinement,
server-side leases or credential issuance.

Usage (on the machine that can reach MinIO):

    cd sdk/python
    python3 examples/volume_direct_minio_probe.py

Optional environment variables:
    MINIO_ENDPOINT / MINIO_ACCESS_KEY_ID / MINIO_SECRET_ACCESS_KEY / MINIO_BUCKET
    MINIO_REGION, PROBE_TOKEN, PROBE_KEEP_OBJECT=1
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import posixpath
import re
import sys
import threading
import uuid
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, quote, urlsplit, urlencode

import httpx

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from cubesandbox import Volume, VolumeEntryStat, VolumeFileType, VolumePathNotFoundException  # noqa: E402
from cubesandbox._volume_content import validate_volume_path  # noqa: E402

ENDPOINT = (os.environ.get("MINIO_ENDPOINT") or "http://9.135.79.34:9000").rstrip("/")
ACCESS_KEY = os.environ.get("MINIO_ACCESS_KEY_ID") or "cubeminio"
SECRET_KEY = os.environ.get("MINIO_SECRET_ACCESS_KEY") or ""
BUCKET = os.environ.get("MINIO_BUCKET") or "cube-volumes"
REGION = os.environ.get("MINIO_REGION") or "us-east-1"
TOKEN = os.environ.get("PROBE_TOKEN") or "probe-" + uuid.uuid4().hex
KEEP = os.environ.get("PROBE_KEEP_OBJECT") == "1"
PROBE_PREFIX = "sdk-direct-probe/"
PAYLOAD = b"\x00\xffprobe-\xe4\xb8\xad\xe6\x96\x87\n"
TIMEOUT = 20.0


def load_conf_credentials() -> tuple[str, str, str, str]:
    """Reuse the deployed s3 plugin config when env vars are absent."""
    endpoint, access, secret, bucket = ENDPOINT, ACCESS_KEY, SECRET_KEY, BUCKET
    if secret:
        return endpoint, access, secret, bucket
    conf = Path("/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-s3.conf")
    if conf.is_file():
        for line in conf.read_text().splitlines():
            match = re.match(r"^(ACCESS_KEY_ID|SECRET_ACCESS_KEY|BUCKET|ENDPOINT|REGION)='?([^']*)'?$", line.strip())
            if not match:
                continue
            key, value = match.group(1), match.group(2)
            if key == "ACCESS_KEY_ID":
                access = value
            elif key == "SECRET_ACCESS_KEY":
                secret = value
            elif key == "BUCKET":
                bucket = value
            elif key == "ENDPOINT":
                endpoint = value
            elif key == "REGION":
                globals()["REGION"] = value
    return endpoint.rstrip("/"), access, secret, bucket


def sign(key: bytes, value: str) -> bytes:
    return hmac.new(key, value.encode("utf-8"), hashlib.sha256).digest()


def signed_request(method: str, key: str, *, content: bytes = b"", content_type: str = "application/octet-stream",
                   query: dict[str, str] | None = None) -> httpx.Response:
    """Minimal AWS SigV4 request against MinIO's path-style S3 API."""
    now = datetime.now(timezone.utc)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    scope = f"{now.strftime('%Y%m%d')}/{REGION}/s3/aws4_request"
    host = urlsplit(ENDPOINT).netloc
    canonical_headers = f"host:{host}\nx-amz-content-sha256:{hashlib.sha256(content).hexdigest()}\nx-amz-date:{amz_date}\n"
    params = dict(query or {})
    canonical_query = urlencode({k: params[k] for k in sorted(params)})
    canonical_request = "\n".join([
        method, f"/{BUCKET}/{quote(key)}", canonical_query, canonical_headers,
        "host;x-amz-content-sha256;x-amz-date", hashlib.sha256(content).hexdigest(),
    ])
    signing_key = sign(sign(sign(sign(b"AWS4" + SECRET_KEY.encode(), now.strftime("%Y%m%d")), REGION), "s3"), "aws4_request")
    signature = hmac.new(signing_key, f"AWS4-HMAC-SHA256\n{amz_date}\n{scope}\n{hashlib.sha256(canonical_request.encode()).hexdigest()}".encode(), hashlib.sha256).hexdigest()
    url = f"{ENDPOINT}/{BUCKET}/{quote(key)}" + (f"?{canonical_query}" if canonical_query else "")
    return httpx.request(
        method, url, content=content or None, timeout=TIMEOUT,
        headers={
            "Host": host,
            "X-Amz-Content-Sha256": hashlib.sha256(content).hexdigest(),
            "X-Amz-Date": amz_date,
            "Authorization": f"AWS4-HMAC-SHA256 Credential={ACCESS_KEY}/{scope}, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature={signature}",
            **({"Content-Type": content_type} if content else {}),
        },
    )


def probe_key(name: str) -> str:
    return PROBE_PREFIX + name


def seed_object() -> str:
    key = probe_key("file-" + uuid.uuid4().hex + ".bin")
    response = signed_request("PUT", key, content=PAYLOAD)
    if response.status_code not in (200, 201):
        raise SystemExit(f"seed object failed: HTTP {response.status_code} {response.text[:200]}")
    return key


def delete_object(key: str) -> None:
    if KEEP:
        return
    try:
        signed_request("DELETE", key)
    except Exception as exc:  # pragma: no cover - cleanup best effort
        print(f"warn: cleanup failed for {key}: {exc}")


class GatewayHandler(BaseHTTPRequestHandler):
    object_key: str = ""

    def log_message(self, *args):
        pass

    def reply(self, status: int, body: bytes = b"", content_type: str = "application/json") -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def unauthorized(self) -> bool:
        supplied = self.headers.get("Authorization") or ""
        expected = f"Bearer {TOKEN}"
        if supplied != expected:
            print(f"gateway: rejected auth (received {supplied[:24]!r}, expected prefix 'Bearer ')")
            self.reply(401, json.dumps({"message": "unauthorized"}).encode())
            return True
        return False

    def entry(self, path: str, size: int, kind: str = "file") -> dict:
        stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        return {
            "name": posixpath.basename(path.rstrip("/")) or "/", "type": kind, "path": path, "size": size,
            "mode": 0o644, "uid": 0, "gid": 0, "atime": stamp, "mtime": stamp, "ctime": stamp,
        }

    def serve(self, path: str, *, size: int, kind: str = "file") -> None:
        self.reply(200, json.dumps(self.entry(path, size, kind)).encode())

    def do_GET(self):
        if self.unauthorized():
            return
        location = urlsplit(self.path)
        params = parse_qs(location.query)
        path = validate_volume_path(params["path"][0])
        assert location.path.startswith("/volumecontent/"), location.path
        endpoint = location.path.rsplit("/", 1)[-1]
        object_path = "/" + self.object_key
        if endpoint == "dir":
            directory = "/" + posixpath.dirname(self.object_key).strip("/")
            if path == directory:
                entry = self.entry(object_path, len(PAYLOAD), kind="file")
                self.reply(200, json.dumps([entry]).encode())
                return
            self.reply(404, b"{}")
            return
        if path != object_path:
            self.reply(404, b"{}")
            return
        if endpoint == "file":
            response = signed_request("GET", self.object_key)
            if response.status_code != 200:
                self.reply(502, json.dumps({"message": f"minio HTTP {response.status_code}"}).encode())
                return
            self.reply(200, response.content, "application/octet-stream")
            return
        if endpoint == "path":
            self.serve(path, size=len(PAYLOAD))
            return
        self.reply(404, b"{}")


def run_gateway(key: str) -> tuple[ThreadingHTTPServer, str]:
    handler = type("BoundHandler", (GatewayHandler,), {"object_key": key})
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, f"http://127.0.0.1:{server.server_port}"


def verify(volume: Volume, path: str) -> None:
    expected = hashlib.sha256(PAYLOAD).digest()
    data = volume.read_file(path, format="bytes")
    assert isinstance(data, bytes), type(data)
    assert hashlib.sha256(data).digest() == expected, "bytes read mismatch"
    stream = volume.read_file(path, format="stream")
    digest = hashlib.sha256()
    try:
        for chunk in stream:
            digest.update(chunk)
    finally:
        close = getattr(stream, "close", None)
        if close:
            close()
    assert digest.digest() == expected, "stream read mismatch"
    info = volume.get_info(path)
    assert isinstance(info, VolumeEntryStat), type(info)
    assert info.size == len(PAYLOAD), info.size
    assert info.type is VolumeFileType.FILE, info.type
    assert isinstance(info.mtime, datetime), type(info.mtime)
    assert volume.exists(path) is True
    assert volume.list(posixpath.dirname(path) or "/", depth=1)
    try:
        volume.get_info("/definitely-missing-" + uuid.uuid4().hex)
    except VolumePathNotFoundException:
        pass
    else:
        raise AssertionError("missing path must raise VolumePathNotFoundException")
    assert volume.exists("/definitely-missing-" + uuid.uuid4().hex) is False


def print_requests(url: str, volume_id: str, path: str) -> None:
    encoded = quote(path, safe="")
    directory = quote(posixpath.dirname(path) or "/", safe="")
    base = f"{url}/volumecontent/{volume_id}"
    print("\n=== E2B / cubesandbox raw request shape (identical) ===")
    print(f"GET {base}/file?path={encoded}  [Authorization: Bearer <token>]")
    print(f"GET {base}/dir?path={directory}&depth=1  [Authorization: Bearer <token>]")
    print(f"GET {base}/path?path={encoded}  [Authorization: Bearer <token>]")
    print("\ncurl (direct read):")
    print(f"curl -i '{base}/file?path={encoded}' -H 'Authorization: Bearer <token>'")
    print("\ncurl (MinIO object, for comparison):")
    print(f"curl -i '{ENDPOINT}/{BUCKET}/{quote(PROBE_PREFIX)}'  # listing requires SigV4")


def main() -> int:
    global ENDPOINT, ACCESS_KEY, SECRET_KEY, BUCKET
    ENDPOINT, ACCESS_KEY, SECRET_KEY, BUCKET = load_conf_credentials()
    if not SECRET_KEY:
        print("set MINIO_SECRET_ACCESS_KEY or keep /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-s3.conf")
        return 2
    key = seed_object()
    server, url = run_gateway(key)
    print(f"probe: bucket={BUCKET} endpoint={ENDPOINT} key={key} token={TOKEN}")
    print(f"probe: gateway={url}; raw reads:")
    print(f"  curl -i '{url}/volumecontent/{BUCKET}/file?path=%2F{quote(key)}' -H 'Authorization: Bearer {TOKEN}'")
    try:
        volume = Volume(BUCKET, "minio-probe", TOKEN)
        volume._config.volume_api_url = url
        path = "/" + key
        verify(volume, path)
        print(f"OK: direct read from MinIO bucket={BUCKET} key={key}")
        print(f"gateway={url} (loopback shim, not the production service)")
        print_requests(url, BUCKET, path)
        return 0
    finally:
        server.shutdown()
        server.server_close()
        delete_object(key)


if __name__ == "__main__":
    raise SystemExit(main())
