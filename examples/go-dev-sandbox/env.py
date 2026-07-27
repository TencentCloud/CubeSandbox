# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Shared environment helper for the go-dev-sandbox demos.
#
# Required env vars:
#   CUBE_API_URL       e.g. http://127.0.0.1:3000
#   CUBE_TEMPLATE_ID   the template built from ./Dockerfile
#                      Look one up with `cubemastercli tpl list`.
#   CUBE_PROXY_NODE_IP the CubeProxy node to reach sandboxes through, e.g.
#                      127.0.0.1 on a single-node install. Only omit it when
#                      *.cube.app resolves through your DNS.
# Optional:
#   CUBE_API_KEY       any non-empty value satisfies the SDK check
#   SSL_CERT_FILE      path to your cluster's root CA when CubeAPI is HTTPS
#   FANOUT_TARGETS     fanout_build.py only — GOOS/GOARCH list, one sandbox each
#   FANOUT_WORK_DIR    fanout_build.py only — workspace under an allowed
#                      host-mount prefix (/data/shared/ by default)

import os
import sys
from pathlib import Path

from dotenv import load_dotenv


def _load_local_dotenv() -> None:
    """Best-effort load of a nearby .env file without overriding real env vars."""
    for path in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if path.is_file():
            load_dotenv(dotenv_path=path, override=False)
            return


_load_local_dotenv()

TEMPLATE_ID = os.environ.get("CUBE_TEMPLATE_ID")

if not TEMPLATE_ID:
    sys.stderr.write(
        "ERROR: CUBE_TEMPLATE_ID is not set.\n"
        "  Copy .env.example to .env and fill it in, or look one up with:\n"
        "    cubemastercli tpl list | awk 'NR>1 && $1 ~ /^tpl-/{print $1; exit}'\n"
    )
    sys.exit(2)


def check(result, what: str):
    """Fail loudly when a command inside the sandbox did not exit cleanly."""
    if result.exit_code != 0:
        sys.stderr.write(
            f"ERROR: {what} failed (exit_code={result.exit_code})\n"
            f"--- stdout ---\n{result.stdout}\n"
            f"--- stderr ---\n{result.stderr}\n"
        )
        sys.exit(1)
    return result
