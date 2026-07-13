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
  "kernel": {
    "version": "6.6.119-49.6",
    "pvm_version": "6.6.69-1.2.cubesandbox",
    "vmlinux_digest_sha256": "sha256:ordinary",
    "vmlinux_pvm_digest_sha256": "sha256:pvm"
  }
}
EOF

  validate_declared_release_manifest "${bundle}"

  python3 - "${bundle}/release-manifest.json" <<'PY'
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    kernel = json.load(f)["kernel"]
for key in ("version", "pvm_version", "vmlinux_digest_sha256", "vmlinux_pvm_digest_sha256"):
    if key not in kernel:
        raise SystemExit(f"missing kernel key: {key}")

def kernel_identity(tag, digest):
    tag = (tag or "").strip()
    digest = (digest or "").strip()
    if tag == "unknown":
        tag = ""
    if digest:
        return f"{tag}@{digest}" if tag else digest
    return tag

ordinary_identity = kernel_identity(kernel["version"], kernel["vmlinux_digest_sha256"])
pvm_identity = kernel_identity(kernel["pvm_version"], kernel["vmlinux_pvm_digest_sha256"])
if ordinary_identity != "6.6.119-49.6@sha256:ordinary":
    raise SystemExit(f"unexpected ordinary kernel identity: {ordinary_identity}")
if pvm_identity != "6.6.69-1.2.cubesandbox@sha256:pvm":
    raise SystemExit(f"unexpected PVM kernel identity: {pvm_identity}")
if kernel_identity("unknown", "sha256:fallback") != "sha256:fallback":
    raise SystemExit("kernel identity must use digest when tag is unknown")
PY
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
