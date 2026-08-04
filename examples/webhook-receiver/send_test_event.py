#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Send a signed sample CubeSandbox lifecycle event to the example receiver."""

from __future__ import annotations

import hashlib
import hmac
import http.client
import json
import os
import uuid
from datetime import datetime, timezone
from urllib.parse import urlsplit


def main() -> None:
    url = os.getenv("WEBHOOK_TEST_URL", "http://127.0.0.1:8088/webhook")
    secret = os.getenv("WEBHOOK_SECRET", "")
    payload = {
        "timestamp": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "level": "info",
        "event": "sandbox.created",
        "sandbox_id": "sandbox-example",
        "template_id": "template-example",
    }
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    headers = {
        "Content-Type": "application/json",
        "X-CubeSandbox-Event": payload["event"],
        "X-CubeSandbox-Delivery": str(uuid.uuid4()),
    }
    if secret:
        digest = hmac.new(secret.encode("utf-8"), body, hashlib.sha256).hexdigest()
        headers["X-CubeSandbox-Signature"] = f"sha256={digest}"

    target = urlsplit(url)
    if target.scheme not in {"http", "https"} or not target.hostname:
        raise ValueError("WEBHOOK_TEST_URL must be an http or https URL")
    connection_type = (
        http.client.HTTPSConnection
        if target.scheme == "https"
        else http.client.HTTPConnection
    )
    connection = connection_type(
        target.hostname,
        target.port,
        timeout=5,
    )
    path = target.path or "/"
    if target.query:
        path = f"{path}?{target.query}"
    try:
        connection.request("POST", path, body=body, headers=headers)
        response = connection.getresponse()
        status = response.status
        response.read()
    finally:
        connection.close()
    if not 200 <= status < 300:
        raise RuntimeError(f"receiver returned HTTP {status}")
    print(f"receiver returned HTTP {status}")


if __name__ == "__main__":
    main()
