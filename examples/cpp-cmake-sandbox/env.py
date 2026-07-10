# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Shared environment helper for the native `cubesandbox` SDK demos
# (03_ccache_snapshot.py, 04_ccache_rollback.py).
#
# Required env vars:
#   CUBE_API_URL       e.g. http://127.0.0.1:3000   (read by the SDK Config)
#   CUBE_TEMPLATE_ID   e.g. tpl-aa14fc963b9c443aaff65b17
#                      Look one up with `cubemastercli tpl list`.
# Optional:
#   SSL_CERT_FILE      path to your cluster's root CA when CubeAPI is HTTPS

import os
import sys

from env_utils import load_local_dotenv

load_local_dotenv()

TEMPLATE_ID = os.environ.get("CUBE_TEMPLATE_ID")

if not TEMPLATE_ID:
    sys.stderr.write(
        "ERROR: CUBE_TEMPLATE_ID is not set.\n"
        "  Look one up with: cubemastercli tpl list | awk 'NR>1 && $1 ~ /^tpl-/{print $1; exit}'\n"
    )
    sys.exit(2)
