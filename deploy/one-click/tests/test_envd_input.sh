#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Exercise the optional package input without executing an uploaded binary.
set -euo pipefail
source "$(dirname "$0")/../lib/common.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

validate_envd_binary /bin/sh "$(uname -m)"
cp /bin/sh "$work/foreign"
python3 - "$work/foreign" <<'PY'
import pathlib
import struct
import sys
p = pathlib.Path(sys.argv[1])
data = bytearray(p.read_bytes())
arch = struct.unpack_from('<H', data, 18)[0]
struct.pack_into('<H', data, 18, 183 if arch == 62 else 62)
p.write_bytes(data)
PY
touch "$work/empty"
printf '#!/bin/sh\nexit 0\n' > "$work/script"
truncate -s 16777217 "$work/oversized"
for invalid in missing empty script foreign oversized; do
  if validate_envd_binary "$work/$invalid" "$(uname -m)" >"$work/error" 2>&1; then
    echo "FAIL: accepted $invalid ENVD_LOCAL_PATH" >&2
    exit 1
  fi
  [[ $(cat "$work/error") == *ENVD_LOCAL_PATH* ]]
done
if validate_envd_binary /bin/sh riscv64 >"$work/error" 2>&1; then
  echo 'FAIL: unsupported architecture accepted' >&2
  exit 1
fi
echo 'PASS: native ELF accepted; six invalid package inputs rejected'
