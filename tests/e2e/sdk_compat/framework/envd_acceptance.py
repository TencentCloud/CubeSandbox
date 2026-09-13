# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Selected-daemon assertions over the existing CubeSandbox SDK and proxy."""
from __future__ import annotations

import base64
import os
import shlex
import sys
import time
import uuid
from contextlib import contextmanager
from dataclasses import replace

import pytest
import requests

from adapters import create_adapter_with_capacity_retry
from framework.auth import auth_headers
from framework.build_throttle import template_build_slot
from framework.cleanup import safe_kill


def required_env(name):
    value = os.environ.get(name, "").strip()
    if not value:
        raise ValueError(f"{name} is required for selected-daemon acceptance")
    return value


def selection():
    provider = os.environ.get("SDK_ENVD_PROVIDER", "go")
    if provider not in {"go", "rust"}:
        raise ValueError("SDK_ENVD_PROVIDER must be go or rust")
    return provider, required_env("SDK_ENVD_COMMIT")


def require_cube_sdk(config):
    if config.backends != ("cubesandbox",):
        raise ValueError("selected-daemon acceptance requires --sdk-e2e-backends=cubesandbox")


@contextmanager
def sandbox(config, reporter, template_id, **identity):
    require_cube_sdk(config)
    cfg = replace(config, cube_template_id=template_id)
    adapter = None
    started = time.monotonic()
    try:
        adapter = create_adapter_with_capacity_retry(
            "cubesandbox", cfg, metadata={"test_suite": "envd_acceptance", "test_run_id": uuid.uuid4().hex},
        )
        reporter.record("envd_sandbox_created", template_id=template_id, sandbox_id=adapter.sandbox_id,
                        duration=time.monotonic() - started, **identity)
        yield adapter
    except BaseException as exc:
        reporter.record("envd_sandbox_failed", template_id=template_id,
                        sandbox_id=getattr(adapter, "sandbox_id", None), error=str(exc), **identity)
        raise
    finally:
        if adapter is not None:
            failed = sys.exc_info()[0] is not None
            errors = safe_kill(adapter, cfg)
            reporter.record("envd_sandbox_cleanup", sandbox_id=adapter.sandbox_id, errors=errors, **identity)
            if errors and not failed:
                raise AssertionError(f"sandbox cleanup failed: {errors}")


@contextmanager
def template(config, reporter, image):
    from cubesandbox import Template
    from adapters.cubesandbox_adapter import CubeSandboxAdapter
    from framework.templates import wait_for_ready, delete_with_retry

    cfg = CubeSandboxAdapter._sdk_config(config)
    created_id = None
    started = time.monotonic()
    try:
        with template_build_slot(label="envd_acceptance"):
            job = Template.build(image=image, writable_layer_size="1G", cpu_count=1000,
                                 memory_mb=512, exposed_ports=[49983, 80], probe_port=49983,
                                 probe_path="/health", envs={"ENVD_LOG_FILE": "-"}, config=cfg)
            created_id = job.template_id
            reporter.record("envd_template_created", template_id=created_id, image=image, job_id=job.job_id)
            detail = wait_for_ready(created_id, cfg, timeout=config.create_timeout, build_id=job.job_id)
            reporter.record("envd_template_ready", template_id=created_id, image=image,
                            duration=time.monotonic() - started, status=detail.status,
                            cpu_count=detail.cpu_count, memory_mb=detail.memory_mb)
        yield created_id
    except BaseException as exc:
        reporter.record("envd_template_failed", template_id=created_id, image=image, error=str(exc))
        raise
    finally:
        if created_id:
            failed = sys.exc_info()[0] is not None
            try:
                delete_with_retry(created_id, cfg)
                reporter.record("envd_template_cleanup", template_id=created_id, errors=[])
            except Exception as exc:
                reporter.record("envd_template_cleanup", template_id=created_id, errors=[str(exc)])
                if not failed:
                    raise


def command_ok(adapter, command, **kwargs):
    # raw_sandbox is the existing adapter escape hatch: omitting user here really
    # exercises the SDK default, which the shared root-explicit adapter cannot.
    result = adapter.raw_sandbox.commands.run(command, **kwargs)
    assert result.exit_code == 0, result
    return result.stdout


def health_identity(adapter, config, reporter, provider, commit):
    host = adapter.get_host(49983)
    headers = auth_headers()
    token = adapter.traffic_access_token()
    if token:
        headers["e2b-traffic-access-token"] = token
    # This is the configured CubeProxy node, never a guest IP or direct envd port.
    if config.cube_proxy_node_ip:
        headers["Host"] = host
        url = f"http://{config.cube_proxy_node_ip}:{config.cube_proxy_port_http}/health"
    else:
        url = f"http://{host}/health"
    deadline = time.monotonic() + config.create_timeout
    last = None
    while time.monotonic() < deadline:
        try:
            response = requests.get(url, headers=headers, timeout=config.api_timeout, allow_redirects=False)
            last = f"HTTP {response.status_code}: {response.text[:200]}"
            if response.status_code == 204:
                break
        except requests.RequestException as exc:
            last = str(exc)
        time.sleep(1)
    else:
        raise AssertionError(f"envd public health never returned 204: {last}")
    # Discover a running executable, then execute that /proc handle, not an
    # unrelated binary merely present in the image. Require a single daemon.
    output = command_ok(adapter, """set -eu
for proc in /proc/[0-9]*; do
  exe=$(readlink "$proc/exe" 2>/dev/null) || continue
  case "$exe" in */envd|*/cube-envd) printf '%s %s\\n' "${proc##*/}" "$exe";; esac
done""", timeout=config.command_timeout)
    lines = output.strip().splitlines()
    assert len(lines) == 1, f"expected one running envd: {output!r}"
    pid, executable = lines[0].split(" ", 1)
    assert pid.isdecimal()
    running = f"/proc/{pid}/exe"
    actual_commit = command_ok(adapter, f"{running} -commit", timeout=config.command_timeout).strip()
    version = command_ok(adapter, f"{running} -version", timeout=config.command_timeout).strip()
    assert actual_commit == commit, (provider, actual_commit, commit)
    assert version
    if provider == "rust":
        from pathlib import Path
        import re
        manifest = Path(__file__).resolve().parents[4] / "cube-envd" / "Cargo.toml"
        product = re.search(r'^version = "([^"]+)"', manifest.read_text(), re.MULTILINE).group(1)
        assert command_ok(adapter, f"{running} cube-version", timeout=config.command_timeout).strip() == product
    else:
        command_ok(adapter, f"LC_ALL=C grep -aq 'Go buildinf:' {running}", timeout=config.command_timeout)
    reporter.record("envd_identity", provider=provider, sandbox_id=adapter.sandbox_id, pid=pid,
                    executable=executable, commit=actual_commit, version=version, health=204,
                    route=url, info=adapter.info().raw,
                    runtime=command_ok(adapter, "uname -m; uname -r; cat /etc/os-release; id; cat /proc/self/cgroup; cat /proc/cmdline; dpkg-query -W",
                                       timeout=config.command_timeout))
    return pid


def commands(adapter, timeout):
    raw = adapter.raw_sandbox
    assert command_ok(adapter, "printf 'ok'", timeout=timeout) == "ok"
    result = raw.commands.run("printf 'out'; printf 'err' >&2; exit 7", timeout=timeout)
    assert (result.stdout, result.stderr, result.exit_code) == ("out", "err", 7)
    assert command_ok(adapter, 'printf "%s" "$SDK_ENVD_VALUE"', envs={"SDK_ENVD_VALUE": "测试-value"}, timeout=timeout) == "测试-value"
    assert command_ok(adapter, "id -u", timeout=timeout).strip() == "0"
    assert command_ok(adapter, "id -u", user="user", timeout=timeout).strip() == "1000"
    start = time.monotonic()
    with pytest.raises(Exception, match=r"(?i)timeout|timed out|deadline"):
        raw.commands.run("sleep 5", timeout=1)
    assert time.monotonic() - start < 5, "SDK timeout did not bound the command"


def files(adapter, timeout):
    raw = adapter.raw_sandbox
    directory = f"/tmp/envd-files-{uuid.uuid4().hex}"
    command_ok(adapter, f"mkdir {directory} && chown user:user {directory}", timeout=timeout)
    path = f"{directory}/roundtrip"
    for user in (None, "user"):
        kwargs = {} if user is None else {"user": user}
        for content in ("", "first\n第二行 café\n", "long-content-with-a-suffix", "short"):
            raw.files.write(path, content, **kwargs)
            assert raw.files.read(path, **kwargs) == content
            assert command_ok(adapter, f"cat {path}", timeout=timeout, **kwargs) == content
        binary = bytes(range(256))
        raw.files.write(path, binary, **kwargs)
        encoded = command_ok(adapter, f"base64 -w0 {path}", timeout=timeout, **kwargs)
        assert base64.b64decode(encoded, validate=True) == binary
        command_ok(adapter, f"printf '%s' {shlex.quote('command-written')} > {path}", timeout=timeout, **kwargs)
        assert raw.files.read(path, **kwargs) == "command-written"
        # Remove root's file before testing the independent UID1000 write path.
        command_ok(adapter, f"rm {path}", timeout=timeout)
    with pytest.raises(Exception, match=r"(?i)not found|does not exist|no such file|404"):
        raw.files.read(f"{directory}/missing")
