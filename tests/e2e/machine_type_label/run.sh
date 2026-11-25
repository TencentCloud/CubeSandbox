#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Run the live machine-type label suite. Requires CUBE_TEMPLATE_ID and a
# reachable cluster; see README.md.
set -euo pipefail

cd "$(dirname "$0")"

: "${CUBE_TEMPLATE_ID:?CUBE_TEMPLATE_ID is required}"
: "${CUBE_API_URL:=http://127.0.0.1:3000}"
: "${CUBEOPS_URL:=http://127.0.0.1:3010}"

# Locate the repo SDK without assuming this directory stays inside the repo.
ROOT=""
dir="$(pwd)"
while [ "$dir" != "/" ]; do
  if [ -f "$dir/sdk/python/cubesandbox/__init__.py" ]; then ROOT="$dir"; break; fi
  dir="$(dirname "$dir")"
done
if [ -z "$ROOT" ] && [ -n "${CUBE_PYTHON_SDK_PATH:-}" ]; then
  ROOT="$(dirname "$(dirname "$CUBE_PYTHON_SDK_PATH")")"
fi
export CUBE_API_URL CUBEOPS_URL
if [ -n "$ROOT" ]; then
  export PYTHONPATH="${ROOT}/sdk/python${PYTHONPATH:+:${PYTHONPATH}}"
fi

exec python3 -m pytest --run-e2e "$@"
