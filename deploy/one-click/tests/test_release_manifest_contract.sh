#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

test_accepts_declared_valid_manifest() {
  local bundle="${TMP_DIR}/valid"
  mkdir -p "${bundle}"
  cat > "${bundle}/VERSION.txt" <<'EOF'
release_version=v0.5.0
manifest=release-manifest.json
EOF
  cat > "${bundle}/release-manifest.json" <<'EOF'
{
  "components": {},
  "guest_image": {},
  "kernel": {}
}
EOF

  validate_declared_release_manifest "${bundle}"
}

test_rejects_missing_declared_manifest() {
  local bundle="${TMP_DIR}/missing"
  mkdir -p "${bundle}"
  cat > "${bundle}/VERSION.txt" <<'EOF'
release_version=v0.5.0
manifest=release-manifest.json
EOF

  if (validate_declared_release_manifest "${bundle}") >/dev/null 2>&1; then
    fail "expected missing declared manifest to be rejected"
  fi
}

test_rejects_invalid_declared_manifest_json() {
  local bundle="${TMP_DIR}/invalid"
  mkdir -p "${bundle}"
  cat > "${bundle}/VERSION.txt" <<'EOF'
release_version=v0.5.0
manifest=release-manifest.json
EOF
  cat > "${bundle}/release-manifest.json" <<'EOF'
{"components":{}}
EOF

  if (validate_declared_release_manifest "${bundle}") >/dev/null 2>&1; then
    fail "expected invalid declared manifest json to be rejected"
  fi
}

test_accepts_bundle_without_declared_manifest() {
  local bundle="${TMP_DIR}/legacy"
  mkdir -p "${bundle}"
  cat > "${bundle}/VERSION.txt" <<'EOF'
release_version=v0.2.2
EOF

  validate_declared_release_manifest "${bundle}"
}

test_accepts_declared_valid_manifest
test_rejects_missing_declared_manifest
test_rejects_invalid_declared_manifest_json
test_accepts_bundle_without_declared_manifest

echo "release manifest contract tests OK"
