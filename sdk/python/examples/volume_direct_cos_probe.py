"""Direct volume-content probe against a real COS-backed volume.

This is a verification harness, NOT a production gateway and NOT a substitute for
a real volume-content service:

- It starts a loopback HTTP server that serves ``/volumecontent/{volumeID}/...``
  by reading objects from Tencent Cloud COS under ``volumes/<volumeID>/``, using
  the same routes and return shapes as the E2B contract.
- It then drives the current ``cubesandbox`` SDK against that loopback server, so
  read_file/list/get_info/exists exercise real COS bytes without creating any
  sandbox, VM or container.
- It prints the raw requests both SDKs would send, for side-by-side review.

Security: credentials are read from ``volume-cos.conf`` and never printed. The
probe writes one uniquely named object under the supplied volume prefix and
deletes it on exit; it never destroys the volume or deletes ``.keep``. The
loopback server binds to 127.0.0.1 and accepts one fixed generated token. This
does not validate tenant isolation, symlink confinement, server-side leases or
production Bearer-token issuance.

Usage (on the machine that can reach COS):

    pip install cos-python-sdk-v5
    export COS_VOLUME_ID=<existing-cos-volume-id>
    cd sdk/python
    python3 examples/volume_direct_cos_probe.py

Optional environment variables:
    CUBE_COS_CONFIG, PROBE_TOKEN, PROBE_KEEP_OBJECT=1
"""

from __future__ import annotations

import hashlib
import json
import os
import posixpath
import sys
import threading
import uuid
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, quote, urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from cubesandbox import Volume, VolumeEntryStat, VolumeFileType, VolumePathNotFoundException  # noqa: E402
from cubesandbox._volume_content import validate_volume_path  # noqa: E402

CONFIG_PATH = Path(os.environ.get("CUBE_COS_CONFIG")
                   or "/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf")
VOLUME_ID = os.environ.get("COS_VOLUME_ID") or os.environ.get("CUBE_COS_VOLUME_ID") or ""
TOKEN = os.environ.get("PROBE_TOKEN") or "probe-" + uuid.uuid4().hex
KEEP = os.environ.get("PROBE_KEEP_OBJECT") == "1"
PROBE_PREFIX = "sdk-direct-probe/"
PAYLOAD = b"\x00\xffcos-direct-\xe4\xb8\xad\xe6\x96\x87\n"


def load_conf() -> dict[str, str]:
    """Read COS credentials without echoing or logging their values."""
    if not CONFIG_PATH.is_file():
        raise SystemExit(f"COS config not found: {CONFIG_PATH}")
    values: dict[str, str] = {}
    for raw in CONFIG_PATH.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip().strip("\"'")
    placeholders = {"<your-secret-id>", "<your-secret-key>", "mybucket-1250000000"}
    for key in ("SECRET_ID", "SECRET_KEY", "BUCKET", "REGION"):
        value = values.get(key, "")
        if not value or "<" in value or ">" in value or value in placeholders:
            raise SystemExit(
                f"COS config {key} is empty or a placeholder; no request was sent. "
                f"Edit {CONFIG_PATH} on the server, do not paste credentials into chat."
            )
    return values


def cos_client(values: dict[str, str]):
    try:
        from qcloud_cos import CosConfig, CosS3Client
    except ImportError as exc:
        raise SystemExit("cos-python-sdk-v5 missing; run: python3 -m pip install cos-python-sdk-v5") from exc
    client = CosS3Client(CosConfig(
        Region=values["REGION"], SecretId=values["SECRET_ID"],
        SecretKey=values["SECRET_KEY"], Scheme="https",
    ))
    return client, values["BUCKET"]


def seed_object(client, bucket: str) -> tuple[str, str]:
    relative_key = PROBE_PREFIX + "file-" + uuid.uuid4().hex + ".bin"
    cos_key = f"volumes/{VOLUME_ID}/" + relative_key
    client.put_object(Bucket=bucket, Key=cos_key, Body=PAYLOAD)
    return cos_key, relative_key


def read_object(client, bucket: str, cos_key: str) -> bytes:
    response = client.get_object(Bucket=bucket, Key=cos_key)
    body = response["Body"]
    try:
        return body.get_raw_stream().read()
    finally:
        close = getattr(body, "close", None)
        if close:
            close()


def delete_object(client, bucket: str, cos_key: str) -> None:
    if KEEP:
        return
    try:
        client.delete_object(Bucket=bucket, Key=cos_key)
    except Exception as exc:  # pragma: no cover - cleanup best effort
        print(f"warn: cleanup failed for {cos_key}: {exc}")


class GatewayHandler(BaseHTTPRequestHandler):
    client: object
    bucket: str
    cos_key: str
    relative_key: str
    volume_id: str

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
        if self.headers.get("Authorization") != f"Bearer {TOKEN}":
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
        assert location.path.startswith(f"/volumecontent/{self.volume_id}/"), location.path
        endpoint = location.path.rsplit("/", 1)[-1]
        object_path = "/" + self.relative_key
        if endpoint == "dir":
            directory = "/" + posixpath.dirname(self.relative_key).strip("/")
            if path == directory:
                self.reply(200, json.dumps([self.entry(object_path, len(PAYLOAD))]).encode())
            else:
                self.reply(404, b"{}")
            return
        if path != object_path:
            self.reply(404, b"{}")
            return
        if endpoint == "file":
            try:
                data = read_object(self.client, self.bucket, self.cos_key)
            except Exception as exc:
                self.reply(502, json.dumps({"message": f"COS read failed: {type(exc).__name__}"}).encode())
                return
            self.reply(200, data, "application/octet-stream")
            return
        if endpoint == "path":
            self.serve(path, size=len(PAYLOAD))
            return
        self.reply(404, b"{}")


def run_gateway(client, bucket: str, cos_key: str, relative_key: str) -> tuple[ThreadingHTTPServer, str]:
    handler = type("BoundHandler", (GatewayHandler,), {
        "client": client, "bucket": bucket, "cos_key": cos_key,
        "relative_key": relative_key, "volume_id": VOLUME_ID,
    })
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


def print_requests(url: str, path: str) -> None:
    encoded = quote(path, safe="")
    directory = quote(posixpath.dirname(path) or "/", safe="")
    base = f"{url}/volumecontent/{VOLUME_ID}"
    print("\n=== E2B / cubesandbox raw request shape (identical) ===")
    print(f"GET {base}/file?path={encoded}  [Authorization: Bearer <token>]")
    print(f"GET {base}/dir?path={directory}&depth=1  [Authorization: Bearer <token>]")
    print(f"GET {base}/path?path={encoded}  [Authorization: Bearer <token>]")
    print("\ncurl (direct read):")
    print(f"curl -i '{base}/file?path={encoded}' -H 'Authorization: Bearer <token>'")


def main() -> int:
    values = load_conf()
    if not VOLUME_ID:
        print("set COS_VOLUME_ID (or CUBE_COS_VOLUME_ID) to an existing COS volume")
        return 2
    client, bucket = cos_client(values)
    cos_key, relative_key = seed_object(client, bucket)
    server, url = run_gateway(client, bucket, cos_key, relative_key)
    path = "/" + relative_key
    print(f"probe: bucket={bucket} region={values['REGION']} volume={VOLUME_ID} key={cos_key} token={TOKEN}")
    print(f"probe: gateway={url}; raw read:")
    print(f"  curl -i '{url}/volumecontent/{VOLUME_ID}/file?path={quote(path, safe='')}' -H 'Authorization: Bearer {TOKEN}'")
    try:
        volume = Volume(VOLUME_ID, "cos-probe", TOKEN)
        volume._config.volume_api_url = url
        verify(volume, path)
        print(f"OK: direct read from COS volume={VOLUME_ID} key={cos_key}")
        print(f"gateway={url} (loopback shim, not the production service)")
        print_requests(url, path)
        return 0
    finally:
        server.shutdown()
        server.server_close()
        delete_object(client, bucket, cos_key)


if __name__ == "__main__":
    raise SystemExit(main())
