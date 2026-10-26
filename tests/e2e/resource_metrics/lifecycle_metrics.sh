#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="${CUBE_REPO_ROOT:-$script_dir}"
if [[ -z "${CUBE_REPO_ROOT:-}" ]]; then
  while [[ "$repo_root" != "/" && ! -f "$repo_root/sdk/python/pyproject.toml" ]]; do
    repo_root="$(dirname -- "$repo_root")"
  done
fi
if [[ ! -f "$repo_root/sdk/python/pyproject.toml" ]]; then
  echo "cannot locate repository sdk/python/pyproject.toml" >&2
  exit 2
fi
export CUBE_REPO_ROOT="$repo_root"

if python3 -c 'import httpx, requests' >/dev/null 2>&1; then
  export PYTHONPATH="$repo_root/sdk/python${PYTHONPATH:+:$PYTHONPATH}"
  exec python3 "$script_dir/lifecycle_metrics.py" "$@"
fi

if command -v uv >/dev/null 2>&1; then
  exec uv run --isolated --no-project --with "$repo_root/sdk/python" \
    python "$script_dir/lifecycle_metrics.py" "$@"
fi

echo "resource metrics E2E requires uv or Python with httpx and requests installed" >&2
exit 2
