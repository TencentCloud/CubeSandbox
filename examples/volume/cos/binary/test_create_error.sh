#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Verifies create returns non-zero + error JSON when coscmd upload soft-fails
# (exit 0 with an error body) — the failure mode seen with bad SECRET_ID.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN="${SCRIPT_DIR}/cube-volume-cos.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

mkdir -p "${TMP}/bin"
cat >"${TMP}/bin/coscmd" <<'EOF'
#!/usr/bin/env bash
# Soft-fail: exit 0 while printing the error body coscmd emits on bad keys.
if [[ "${1:-}" == "config" ]]; then
  exit 0
fi
cat <<'ERR'
Upload /tmp/x => cos://bucket/volumes/v/.keep
<?xml version='1.0' encoding='utf-8' ?>
<Error>
	<Code>InvalidAccessKeyId</Code>
	<Message>The access key Id format you provided is invalid.</Message>
</Error>
Upload file failed
ERR
exit 0
EOF
chmod +x "${TMP}/bin/coscmd"

cat >"${TMP}/volume-cos.conf" <<'EOF'
SECRET_ID=not-a-real-key
SECRET_KEY=not-a-real-secret
BUCKET=examplebucket-1250000000
REGION=ap-guangzhou
EOF

export PATH="${TMP}/bin:${PATH}"
export CUBE_COS_CONFIG="${TMP}/volume-cos.conf"

set +e
out="$("${PLUGIN}" --op create --volume-id test-vol --name test-vol 2>/dev/null)"
rc=$?
set -e

if [[ "$rc" -eq 0 ]]; then
  echo "FAIL: create exited 0 on coscmd soft-failure; stdout=${out}" >&2
  exit 1
fi
if ! printf '%s' "$out" | grep -q '"error"'; then
  echo "FAIL: expected error JSON; stdout=${out}" >&2
  exit 1
fi
if printf '%s' "$out" | grep -q '"error":""'; then
  echo "FAIL: error field empty; stdout=${out}" >&2
  exit 1
fi

echo "ok: create fails when coscmd soft-fails (rc=${rc})"
