# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Shared helpers for the CubeSandbox Deno runtime examples."""

from __future__ import annotations

import os
import shlex
import time
from pathlib import Path
from typing import Any

import requests
from dotenv import load_dotenv

APP_DIR = "/workspace/deno-app"
APP_PORT = 8000
ENVD_PORT = 49983
APP_PID_FILE = "/tmp/cube-deno-app.pid"
APP_LOG_FILE = "/tmp/cube-deno-app.log"
DENO_CACHE_DIR = "/home/user/.cache/deno"
TRAFFIC_TOKEN_HEADER = "e2b-traffic-access-token"


def load_environment() -> None:
    """Load a neighboring .env without overriding the caller's environment."""
    load_dotenv(Path(__file__).with_name(".env"), override=False)


def required(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise SystemExit(f"Missing required environment variable: {name}")
    return value


def sandbox_create_options(template_id: str, timeout: int) -> dict[str, Any]:
    """Build the secure-by-default options shared by both examples."""
    return {
        "template": template_id,
        "timeout": timeout,
        "allow_internet_access": False,
        "network": {"allow_public_traffic": False},
    }


def sandbox_identifier(sandbox: Any) -> str:
    value = getattr(sandbox, "sandbox_id", None)
    if not isinstance(value, str) or not value:
        raise RuntimeError("CubeSandbox SDK returned no sandbox_id")
    return value


def public_url(sandbox: Any, path: str = "/health", port: int = APP_PORT) -> str:
    return f"https://{sandbox.get_host(port)}{path}"


def traffic_headers(sandbox: Any) -> dict[str, str]:
    """Return the CubeProxy access token required by restricted sandboxes."""
    token = getattr(sandbox, "traffic_access_token", None)
    if not isinstance(token, str) or not token:
        raise RuntimeError(
            "CubeSandbox returned no traffic_access_token; confirm that "
            "network.allow_public_traffic is false and the control plane supports "
            "restricted public access"
        )
    return {TRAFFIC_TOKEN_HEADER: token}


def tls_verify() -> str | bool:
    """Use a configured CA bundle while keeping TLS verification enabled."""
    return (
        os.environ.get("REQUESTS_CA_BUNDLE") or os.environ.get("SSL_CERT_FILE") or True
    )


def ensure_success(result: Any, action: str) -> Any:
    if getattr(result, "exit_code", 1) != 0:
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise RuntimeError(
            f"{action} failed (exit={getattr(result, 'exit_code', None)}):\n"
            f"stdout:\n{stdout}\nstderr:\n{stderr}"
        )
    return result


def run_checked(
    sandbox: Any,
    command: str,
    *,
    action: str,
    cwd: str = APP_DIR,
    timeout: float = 120,
    user: str = "user",
) -> Any:
    result = sandbox.commands.run(
        command,
        cwd=cwd,
        timeout=timeout,
        user=user,
    )
    return ensure_success(result, action)


def start_service(sandbox: Any) -> int:
    """Start the Deno service once and return its in-sandbox PID."""
    command = (
        f"if test -s {shlex.quote(APP_PID_FILE)} "
        f'&& kill -0 "$(cat {shlex.quote(APP_PID_FILE)})" 2>/dev/null; then '
        f"cat {shlex.quote(APP_PID_FILE)}; "
        "else "
        f"nohup deno task start </dev/null >{shlex.quote(APP_LOG_FILE)} 2>&1 & "
        f"pid=$!; printf '%s\\n' \"$pid\" >{shlex.quote(APP_PID_FILE)}; "
        "printf '%s\\n' \"$pid\"; "
        "fi"
    )
    result = run_checked(
        sandbox,
        command,
        action="start Deno service",
        timeout=30,
    )
    pid_text = getattr(result, "stdout", "").strip().splitlines()
    if not pid_text or not pid_text[-1].isdigit():
        raise RuntimeError(f"Deno service returned an invalid PID: {pid_text!r}")
    return int(pid_text[-1])


def wait_for_app(sandbox: Any, timeout: float = 60) -> dict[str, Any]:
    url = public_url(sandbox)
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None

    while time.monotonic() < deadline:
        try:
            response = requests.get(
                url,
                headers=traffic_headers(sandbox),
                timeout=5,
                verify=tls_verify(),
            )
            response.raise_for_status()
            body = response.json()
            if body.get("status") != "ok" or body.get("runtime") != "deno":
                raise RuntimeError(f"Unexpected health response: {body!r}")
            return body
        except (requests.RequestException, ValueError, RuntimeError) as exc:
            last_error = exc
            time.sleep(1)

    log_result = sandbox.commands.run(
        f"test ! -f {shlex.quote(APP_LOG_FILE)} || tail -n 80 {shlex.quote(APP_LOG_FILE)}",
        timeout=10,
        user="user",
    )
    log_tail = getattr(log_result, "stdout", "").strip()
    raise RuntimeError(
        f"Deno service did not become ready at {url}: {last_error}\n"
        f"service log:\n{log_tail}"
    )


def assert_public_access_restricted(sandbox: Any) -> int:
    """Prove that CubeProxy rejects a request without the traffic token."""
    response = requests.get(public_url(sandbox), timeout=10, verify=tls_verify())
    if response.status_code not in {401, 403}:
        raise RuntimeError(
            "CubeProxy accepted an unauthenticated request to a restricted sandbox: "
            f"status={response.status_code}"
        )
    return response.status_code


def assert_public_egress_blocked(sandbox: Any) -> str:
    """Prove public TCP is blocked after validating bash /dev/tcp locally."""
    command = f"""\
set -eu
if ! command -v bash >/dev/null 2>&1; then
  echo 'bash is required to verify the public egress policy' >&2
  exit 2
fi
if ! command -v timeout >/dev/null 2>&1; then
  echo 'timeout is required to verify the public egress policy' >&2
  exit 2
fi
if ! timeout 5 bash -c '</dev/null >/dev/tcp/127.0.0.1/{ENVD_PORT}' 2>/dev/null; then
  echo 'bash /dev/tcp support check failed against local envd port {ENVD_PORT}' >&2
  exit 2
fi
if timeout 5 bash -c '</dev/null >/dev/tcp/1.1.1.1/80' 2>/dev/null; then
  echo 'unexpected public egress access' >&2
  exit 1
fi
printf '%s\n' 'public egress blocked'
"""
    result = run_checked(
        sandbox,
        command,
        action="verify public egress is blocked",
        timeout=15,
    )
    return getattr(result, "stdout", "").strip()


def counter_request(sandbox: Any, method: str = "GET") -> dict[str, int]:
    response = requests.request(
        method,
        public_url(sandbox, "/counter"),
        headers=traffic_headers(sandbox),
        timeout=10,
        verify=tls_verify(),
    )
    response.raise_for_status()
    body = response.json()
    counter = body.get("counter")
    if not isinstance(counter, int) or counter < 0:
        raise RuntimeError(f"Unexpected counter response: {body!r}")
    return {"counter": counter}


def cache_fingerprint(sandbox: Any) -> str:
    """Hash all cached dependency contents to prove cache persistence."""
    cache_dir = shlex.quote(DENO_CACHE_DIR)
    command = (
        f"test -d {cache_dir} "
        f"&& find {cache_dir} -type f -print -quit | grep -q . "
        f"&& find {cache_dir} -type f -print0 "
        "| sort -z | xargs -0 -r sha256sum | sha256sum | cut -d' ' -f1"
    )
    result = run_checked(
        sandbox,
        command,
        action="fingerprint Deno dependency cache",
        timeout=120,
    )
    fingerprint = getattr(result, "stdout", "").strip()
    if len(fingerprint) != 64:
        raise RuntimeError(f"Invalid Deno cache fingerprint: {fingerprint!r}")
    return fingerprint
