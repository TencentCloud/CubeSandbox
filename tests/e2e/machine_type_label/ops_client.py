# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Thin CubeOps HTTP client used by the machine-type label E2E suite."""

from __future__ import annotations

import json
import time
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen

MACHINE_TYPE_KEY = "cube.cubesandbox.io/machine-type"
ISOLATION_KEY = "cube.cloud.tencentcloud.com/scheduling-disabled"


class OpsError(RuntimeError):
    def __init__(self, status: int, body: str) -> None:
        super().__init__(f"HTTP {status}: {body}")
        self.status = status
        self.body = body


class OpsClient:
    def __init__(self, base_url: str, username: str, password: str, timeout: float = 30.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.username = username
        self.password = password
        self.timeout = timeout
        self._token: str | None = None

    # ---- transport -----------------------------------------------------
    def _request(self, method: str, path: str, body: dict | None = None, *, auth: bool = True):
        url = f"{self.base_url}{path}"
        data = json.dumps(body).encode() if body is not None else None
        req = Request(url, data=data, method=method)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if auth and self._token:
            req.add_header("Authorization", f"Bearer {self._token}")
        try:
            with urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read().decode()
                return resp.status, (json.loads(raw) if raw else None)
        except HTTPError as exc:
            raise OpsError(exc.code, exc.read().decode()) from exc

    # ---- auth ----------------------------------------------------------
    def login(self) -> None:
        status, payload = self._request(
            "POST",
            "/api/v1/auth/login",
            {"username": self.username, "password": self.password},
            auth=False,
        )
        if status != 200 or not payload or not payload.get("accessToken"):
            raise OpsError(status, json.dumps(payload))
        self._token = payload["accessToken"]

    # ---- nodes ---------------------------------------------------------
    def list_nodes(self) -> list[dict]:
        _, payload = self._request("GET", "/api/v1/nodes")
        return payload or []

    def get_node(self, node_id: str) -> dict:
        _, payload = self._request("GET", f"/api/v1/nodes/{quote(node_id, safe='')}")
        return payload or {}

    def labels(self, node_id: str) -> dict[str, str]:
        return self.get_node(node_id).get("labels") or {}

    def wait_for_labels(
        self,
        node_id: str,
        expected: dict[str, str],
        timeout: float = 15.0,
        interval: float = 0.25,
    ) -> dict[str, str]:
        """Poll until every expected key/value is visible, then return the set.

        Label writes are eventually consistent across the database and the
        Redis snapshot; an immediate follow-up read can return the old value.
        """
        deadline = time.time() + timeout
        labels: dict[str, str] = {}
        while time.time() < deadline:
            labels = self.labels(node_id)
            if all(labels.get(k) == v for k, v in expected.items()):
                return labels
            time.sleep(interval)
        missing = {k: v for k, v in expected.items() if labels.get(k) != v}
        raise AssertionError(
            f"labels not applied within {timeout}s: expected {missing}, got {labels}"
        )

    def operations(self, node_id: str, limit: int = 20) -> list[dict]:
        _, payload = self._request(
            "GET", f"/api/v1/nodes/{quote(node_id, safe='')}/operations?limit={limit}"
        )
        return payload or []

    # ---- label writes ---------------------------------------------------
    def set_labels(self, node_id: str, labels: dict[str, str]) -> None:
        self._request("PATCH", f"/api/v1/nodes/{quote(node_id, safe='')}/labels", {"labels": labels})

    def replace_labels(self, node_id: str, labels: dict[str, str]) -> None:
        self._request("PUT", f"/api/v1/nodes/{quote(node_id, safe='')}/labels", {"labels": labels})

    def delete_label(self, node_id: str, key: str) -> None:
        # Keys contain '/', so the key travels as a query parameter.
        self._request(
            "DELETE",
            f"/api/v1/nodes/{quote(node_id, safe='')}/labels?key={quote(key, safe='')}",
        )

    # ---- machine types (catalog + membership; not the generic label API) --
    def ensure_machine_type(self, name: str, display_name: str = "") -> None:
        try:
            self._request(
                "POST",
                "/api/v1/machine-types",
                {"name": name, "displayName": display_name or name},
            )
        except OpsError as exc:
            if exc.status != 409:
                raise

    def bind_machine_type(self, node_id: str, name: str) -> None:
        self.ensure_machine_type(name)
        self._request(
            "PUT",
            f"/api/v1/nodes/{quote(node_id, safe='')}/machine-type",
            {"machineType": name},
        )

    def clear_machine_type(self, node_id: str) -> None:
        self._request("DELETE", f"/api/v1/nodes/{quote(node_id, safe='')}/machine-type")

    # ---- isolation (separate operation owns this key) -------------------
    def isolate(self, node_id: str) -> None:
        self._request("PUT", f"/api/v1/nodes/{quote(node_id, safe='')}/isolation", {})

    def unisolate(self, node_id: str) -> None:
        self._request("DELETE", f"/api/v1/nodes/{quote(node_id, safe='')}/isolation")

    # ---- internal (no auth, cluster-local only) -------------------------
    def internal_nodes(self) -> list[dict]:
        _, payload = self._request("GET", "/internal/v1/nodes", auth=False)
        return payload or []
