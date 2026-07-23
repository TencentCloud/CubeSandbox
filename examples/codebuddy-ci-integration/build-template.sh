#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: $0 <registry/image:tag>" >&2
  exit 2
fi

image="$1"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

docker build --platform linux/amd64 -t "$image" "$script_dir"
docker push "$image"
