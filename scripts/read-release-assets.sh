#!/usr/bin/env bash
# Parse deploy/release-assets.yaml with PyYAML.
#
#   eval "$(scripts/read-release-assets.sh)"   # shell vars for local use
#   scripts/read-release-assets.sh --export    # GITHUB_ENV + GITHUB_OUTPUT
#
# Requires: python3 + PyYAML (pip install pyyaml).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
PIN_FILE="${RELEASE_ASSETS_FILE:-${ROOT_DIR}/deploy/release-assets.yaml}"

if [[ ! -f "${PIN_FILE}" ]]; then
  echo "error: pin file not found: ${PIN_FILE}" >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "error: python3 is required to parse ${PIN_FILE}" >&2
  exit 1
fi

PINS="$(
  python3 - "${PIN_FILE}" <<'PY'
import shlex
import sys

try:
    import yaml
except ImportError:
    raise SystemExit(
        "error: PyYAML is required to parse release-assets.yaml; "
        "install with: pip install pyyaml"
    )

path = sys.argv[1]
required = (
    "kernel_bm_amd64",
    "kernel_bm_arm64",
    "kernel_pvm",
    "guest_image",
)

with open(path, encoding="utf-8") as f:
    data = yaml.safe_load(f)

if not isinstance(data, dict):
    raise SystemExit(f"expected a mapping in {path}, got {type(data).__name__}")

pins = {}
for key in required:
    if key not in data:
        raise SystemExit(f"missing required key '{key}' in {path}")
    value = data[key]
    if not isinstance(value, str) or not value.strip():
        raise SystemExit(f"key '{key}' in {path} must be a non-empty string")
    pins[key] = value.strip()

unknown = sorted(set(data) - set(required))
if unknown:
    raise SystemExit(f"unknown keys in {path}: {', '.join(unknown)}")

for key in ("kernel_bm_amd64", "kernel_bm_arm64", "kernel_pvm"):
    if not pins[key].startswith("kernel-release-"):
        raise SystemExit(f"{key} pin must start with 'kernel-release-': {pins[key]}")
if not pins["guest_image"].startswith("guest-image-"):
    raise SystemExit(
        f"guest_image pin must start with 'guest-image-': {pins['guest_image']}"
    )

print(f"KERNEL_BM_AMD64_RELEASE_TAG={shlex.quote(pins['kernel_bm_amd64'])}")
print(f"KERNEL_BM_ARM64_RELEASE_TAG={shlex.quote(pins['kernel_bm_arm64'])}")
print(f"KERNEL_PVM_RELEASE_TAG={shlex.quote(pins['kernel_pvm'])}")
print(f"GUEST_IMAGE_RELEASE_TAG={shlex.quote(pins['guest_image'])}")
PY
)"
eval "${PINS}"

MODE="${1:-}"
case "${MODE}" in
  "" )
    printf 'KERNEL_BM_AMD64_RELEASE_TAG=%q\n' "${KERNEL_BM_AMD64_RELEASE_TAG}"
    printf 'KERNEL_BM_ARM64_RELEASE_TAG=%q\n' "${KERNEL_BM_ARM64_RELEASE_TAG}"
    printf 'KERNEL_PVM_RELEASE_TAG=%q\n' "${KERNEL_PVM_RELEASE_TAG}"
    printf 'GUEST_IMAGE_RELEASE_TAG=%q\n' "${GUEST_IMAGE_RELEASE_TAG}"
    ;;
  --export)
    if [[ -n "${GITHUB_ENV:-}" ]]; then
      {
        echo "KERNEL_BM_AMD64_RELEASE_TAG=${KERNEL_BM_AMD64_RELEASE_TAG}"
        echo "KERNEL_BM_ARM64_RELEASE_TAG=${KERNEL_BM_ARM64_RELEASE_TAG}"
        echo "KERNEL_PVM_RELEASE_TAG=${KERNEL_PVM_RELEASE_TAG}"
        echo "GUEST_IMAGE_RELEASE_TAG=${GUEST_IMAGE_RELEASE_TAG}"
      } >> "${GITHUB_ENV}"
    fi
    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
      {
        echo "kernel_bm_amd64_release_tag=${KERNEL_BM_AMD64_RELEASE_TAG}"
        echo "kernel_bm_arm64_release_tag=${KERNEL_BM_ARM64_RELEASE_TAG}"
        echo "kernel_pvm_release_tag=${KERNEL_PVM_RELEASE_TAG}"
        echo "guest_image_release_tag=${GUEST_IMAGE_RELEASE_TAG}"
      } >> "${GITHUB_OUTPUT}"
    fi
    echo "bm_amd64=${KERNEL_BM_AMD64_RELEASE_TAG} bm_arm64=${KERNEL_BM_ARM64_RELEASE_TAG} pvm=${KERNEL_PVM_RELEASE_TAG} guest_image=${GUEST_IMAGE_RELEASE_TAG}"
    ;;
  -h|--help)
    sed -n '2,7p' "$0"
    ;;
  *)
    echo "usage: $0 [--export]" >&2
    exit 2
    ;;
esac
