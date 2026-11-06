# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Explicit resource ownership for opt-in destructive webhook experiments."""
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.parse import urlsplit


def setting(name):
    key = "WEBHOOK_P6_" + name
    value = os.environ.get(key, "")
    if not value:
        raise ValueError(f"Set {key} for this experiment (see tests/e2e/webhook/README.md)")
    return value


def run_root():
    path = Path(setting("RUN_ROOT"))
    if not path.is_absolute() or len(path.resolve().parts) < 3 or not path.is_dir():
        raise ValueError("RUN_ROOT must be an existing dedicated absolute run directory, not a filesystem root")
    return path.resolve()


def owned_path(value):
    path = Path(value)
    root = run_root()
    resolved = path.resolve()
    if not path.is_absolute() or resolved == root or not resolved.is_relative_to(root):
        raise ValueError(f"Test path must resolve strictly inside {root}: {path}")
    return resolved


def resource_label():
    label = setting("RESOURCE_LABEL")
    if not label.startswith("webhook-p6-") or label == "webhook-p6-":
        raise ValueError("RESOURCE_LABEL must start with webhook-p6- and identify this run")
    return label


def container_info(name):
    if not re.fullmatch(r"webhook-1903-[a-zA-Z0-9_.-]+", name):
        raise ValueError("Dedicated container names must start with webhook-1903-")
    info = json.loads(subprocess.check_output(["docker", "inspect", name], text=True))[0]
    if (info["Config"].get("Labels") or {}).get("purpose") != resource_label():
        raise ValueError(f"Container {name} does not belong to this run")
    return info


def checked_unit(name):
    if not re.fullmatch(r"webhook-1903-[a-zA-Z0-9_.-]+\.service", name):
        raise ValueError("Dedicated units must start with webhook-1903- and end with .service")
    definition = subprocess.check_output(["systemctl", "show", name, "-p", "ExecStart", "--value"], text=True)
    paths = re.findall(r"(?:^|[ ;{])path=(.*?)\s*;", definition)
    if not paths:
        raise ValueError(f"Cannot identify ExecStart binary for {name}")
    for path in paths:
        owned_path(re.sub(r"\\x([0-9a-fA-F]{2})", lambda m: chr(int(m[1], 16)), path))
    return name


def process_binary(pid, variable):
    if pid <= 1:
        raise ValueError("Expected a running dedicated service PID")
    expected = owned_path(setting(variable))
    actual = Path(subprocess.check_output(["sudo", "readlink", f"/proc/{pid}/exe"], text=True).strip()).resolve()
    if actual != expected:
        raise ValueError(f"PID {pid} does not use the inventoried {variable}")
    return actual


def endpoint(name):
    value = setting(name)
    parsed = urlsplit(value)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password:
        raise ValueError(f"{name} must be an HTTP(S) endpoint without embedded credentials")
    return value.rstrip("/")
