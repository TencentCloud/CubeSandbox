#!/usr/bin/env sh
set -eu

ENVD_PORT="${ENVD_PORT:-49983}"

if command -v envd >/dev/null 2>&1; then
  envd -port "${ENVD_PORT}" &
  envd_pid="$!"
else
  echo "envd binary not found in image" >&2
  exit 127
fi

term() {
  if [ -n "${app_pid:-}" ]; then
    kill "${app_pid}" 2>/dev/null || true
  fi
  kill "${envd_pid}" 2>/dev/null || true
}
trap term INT TERM

"$@" &
app_pid="$!"

wait "${app_pid}"
status="$?"
kill "${envd_pid}" 2>/dev/null || true
wait "${envd_pid}" 2>/dev/null || true
exit "${status}"
