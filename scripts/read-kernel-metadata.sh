#!/usr/bin/env bash
# Parse kernel-metadata.json from a kernel-release-* GitHub Release.
#
#   eval "$(scripts/read-kernel-metadata.sh path/to/kernel-metadata.json)"
#   scripts/read-kernel-metadata.sh path/to/kernel-metadata.json --export
#
# Emits:
#   KERNEL_BM_SOURCE_TAG   (bm.source_tag)
#   KERNEL_PVM_SOURCE_TAG  (pvm.source_tag)
set -euo pipefail

usage() {
  sed -n '2,10p' "$0"
}

META_FILE=""
MODE=""
for arg in "$@"; do
  case "${arg}" in
    -h|--help)
      usage
      exit 0
      ;;
    --export)
      MODE=--export
      ;;
    *)
      if [[ -n "${META_FILE}" ]]; then
        echo "usage: $0 <kernel-metadata.json> [--export]" >&2
        exit 2
      fi
      META_FILE="${arg}"
      ;;
  esac
done

if [[ -z "${META_FILE}" ]]; then
  echo "usage: $0 <kernel-metadata.json> [--export]" >&2
  exit 2
fi
if [[ ! -f "${META_FILE}" ]]; then
  echo "error: kernel metadata not found: ${META_FILE}" >&2
  exit 1
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "error: python3 is required to parse ${META_FILE}" >&2
  exit 1
fi

PARSED="$(
  python3 - "${META_FILE}" <<'PY'
import json
import shlex
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    data = json.load(f)

schema = data.get("schema_version")
if schema != 1:
    raise SystemExit(f"{path}: unsupported schema_version {schema!r} (want 1)")

bm = data.get("bm") or {}
pvm = data.get("pvm") or {}
bm_tag = (bm.get("source_tag") or "").strip()
pvm_tag = (pvm.get("source_tag") or "").strip()
if not bm_tag:
    raise SystemExit(f"{path}: missing bm.source_tag")
if not pvm_tag:
    raise SystemExit(f"{path}: missing pvm.source_tag")

print(f"KERNEL_BM_SOURCE_TAG={shlex.quote(bm_tag)}")
print(f"KERNEL_PVM_SOURCE_TAG={shlex.quote(pvm_tag)}")
PY
)"
eval "${PARSED}"

case "${MODE}" in
  "" )
    printf 'KERNEL_BM_SOURCE_TAG=%q\n' "${KERNEL_BM_SOURCE_TAG}"
    printf 'KERNEL_PVM_SOURCE_TAG=%q\n' "${KERNEL_PVM_SOURCE_TAG}"
    ;;
  --export)
    if [[ -n "${GITHUB_ENV:-}" ]]; then
      {
        echo "KERNEL_BM_SOURCE_TAG=${KERNEL_BM_SOURCE_TAG}"
        echo "KERNEL_PVM_SOURCE_TAG=${KERNEL_PVM_SOURCE_TAG}"
      } >> "${GITHUB_ENV}"
    fi
    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
      {
        echo "kernel_bm_source_tag=${KERNEL_BM_SOURCE_TAG}"
        echo "kernel_pvm_source_tag=${KERNEL_PVM_SOURCE_TAG}"
      } >> "${GITHUB_OUTPUT}"
    fi
    echo "bm_source=${KERNEL_BM_SOURCE_TAG} pvm_source=${KERNEL_PVM_SOURCE_TAG}"
    ;;
  *)
    echo "usage: $0 <kernel-metadata.json> [--export]" >&2
    exit 2
    ;;
esac
