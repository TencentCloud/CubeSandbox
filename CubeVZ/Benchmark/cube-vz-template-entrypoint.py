#!/usr/bin/env python3
"""Start a template image command without blocking the CubeVZ control agent."""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path


CONFIG_PATH = Path("/etc/cube-vz/template-runtime.json")
PID_PATH = Path("/run/cube-vz-template.pid")
LOG_PATH = Path("/var/log/cube-vz-template.log")


def main() -> None:
    if not CONFIG_PATH.exists():
        return
    config = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    command = config.get("command") or []
    args = config.get("args") or []
    if not isinstance(command, list) or not isinstance(args, list) or not command:
        return

    if PID_PATH.exists():
        try:
            os.kill(int(PID_PATH.read_text().strip()), 0)
            return
        except (OSError, ValueError):
            PID_PATH.unlink(missing_ok=True)

    environment = os.environ.copy()
    for entry in config.get("env") or []:
        if isinstance(entry, str) and "=" in entry:
            key, value = entry.split("=", 1)
            if key:
                environment[key] = value
    cwd = config.get("cwd") or "/"
    if not isinstance(cwd, str) or not os.path.isdir(cwd):
        cwd = "/"

    LOG_PATH.parent.mkdir(parents=True, exist_ok=True)
    with LOG_PATH.open("ab", buffering=0) as output:
        process = subprocess.Popen(
            [str(value) for value in command + args],
            cwd=cwd,
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=output,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
    PID_PATH.write_text(str(process.pid), encoding="ascii")


if __name__ == "__main__":
    main()
