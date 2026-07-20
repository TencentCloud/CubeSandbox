#!/usr/bin/env bash
# Guard: CubeProxy ships OpenResty/LuaJIT 2.1, which does not support Lua 5.2
# goto / ::label:: syntax. Catch that before CI/runtime require() fails.
set -euo pipefail

ROOT="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
LUA_DIR="${ROOT}/lua"
failed=0

check_no_goto() {
  local file="$1"
  local hits
  hits="$(grep -nE '^[[:space:]]*goto[[:space:]]|^[[:space:]]*::[A-Za-z_][A-Za-z0-9_]*::' "${file}" || true)"
  if [[ -n "${hits}" ]]; then
    echo "LuaJIT-incompatible goto/label in ${file}:" >&2
    echo "${hits}" >&2
    failed=1
  fi
}

for f in "${LUA_DIR}"/*.lua; do
  check_no_goto "${f}"
done

# Prefer a real LuaJIT bytecode compile when available (local or via docker).
compile_with_luajit() {
  local luajit_bin="$1"
  shift
  local f
  for f in "$@"; do
    if ! "${luajit_bin}" -bl "${f}" >/dev/null; then
      echo "luajit -bl failed: ${f}" >&2
      failed=1
    fi
  done
}

shopt -s nullglob
lua_files=("${LUA_DIR}"/*.lua)
shopt -u nullglob

if command -v luajit >/dev/null 2>&1; then
  compile_with_luajit luajit "${lua_files[@]}"
elif command -v docker >/dev/null 2>&1; then
  # Public OpenResty alpine image includes LuaJIT; used only when present locally.
  if docker image inspect openresty/openresty:alpine >/dev/null 2>&1; then
    if ! docker run --rm --network host \
      -v "${LUA_DIR}:/lua:ro" -w /lua \
      openresty/openresty:alpine \
      sh -c 'for f in *.lua; do luajit -bl "$f" >/dev/null || exit 1; done'; then
      echo "docker luajit -bl failed for CubeProxy/lua" >&2
      failed=1
    fi
  fi
fi

if [[ "${failed}" -ne 0 ]]; then
  exit 1
fi
echo "CubeProxy Lua syntax OK (no LuaJIT-incompatible goto/labels)"
