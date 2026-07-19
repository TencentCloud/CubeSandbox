#!/usr/bin/env bash
set -euo pipefail

mkdir -p /workspace/notebooks /workspace/artifacts /workspace/data

exec jupyter lab \
  --ip=0.0.0.0 \
  --port=8888 \
  --no-browser \
  --ServerApp.allow_root=True \
  --ServerApp.root_dir=/workspace \
  --ServerApp.default_url=/lab \
  --ServerApp.token='' \
  --ServerApp.password='' \
  --ServerApp.disable_check_xsrf=True

