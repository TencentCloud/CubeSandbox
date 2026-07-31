#!/usr/bin/env bash
# install.sh — install or remove the CubeSandbox bash-redirect plugin for OpenCode.
#
# OpenCode loads every .js / .ts file it finds in a plugin directory at startup
# (https://opencode.ai/docs/plugins/), so installing means placing one file:
#
#   project scope  .opencode/plugin/          (default; affects one project)
#   global scope   ~/.config/opencode/plugin/ (affects every project)
#
# A symlink is used when the filesystem allows it, so editing the file in this
# example directory takes effect without reinstalling. Copying is the fallback.
#
# Usage:
#   ./install.sh                  install into ./.opencode/plugin
#   ./install.sh --global         install into ~/.config/opencode/plugin
#   ./install.sh --dir <path>     install into an explicit directory
#   ./install.sh --uninstall      remove it again (honours --global / --dir)
#   ./install.sh --status         show where it is currently installed
#
# Idempotent: running it twice leaves the same result.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_SRC="${SCRIPT_DIR}/cubesandbox-bash.js"
PLUGIN_NAME="cubesandbox-bash.js"
BACKEND="${SCRIPT_DIR}/../exec_backend.py"

MODE="install"
SCOPE="project"
EXPLICIT_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --global)    SCOPE="global" ;;
    --uninstall) MODE="uninstall" ;;
    --status)    MODE="status" ;;
    --dir)
      shift
      [[ $# -gt 0 ]] || { echo "--dir requires a path" >&2; exit 2; }
      EXPLICIT_DIR="$1"
      ;;
    -h|--help)
      sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      exit 2
      ;;
  esac
  shift
done

if [[ -n "$EXPLICIT_DIR" ]]; then
  TARGET_DIR="$EXPLICIT_DIR"
elif [[ "$SCOPE" == "global" ]]; then
  TARGET_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/opencode/plugin"
else
  TARGET_DIR="$(pwd)/.opencode/plugin"
fi

TARGET="${TARGET_DIR}/${PLUGIN_NAME}"

ok()   { echo "  [OK]   $*"; }
info() { echo "  [..]   $*"; }
bad()  { echo "  [FAIL] $*"; }

# --------------------------------------------------------------------------- #
case "$MODE" in

status)
  echo "CubeSandbox OpenCode plugin — status"
  echo
  echo "  source : ${PLUGIN_SRC}"
  echo "  backend: ${BACKEND}"
  echo
  for d in \
    "$(pwd)/.opencode/plugin" \
    "${XDG_CONFIG_HOME:-$HOME/.config}/opencode/plugin"
  do
    p="${d}/${PLUGIN_NAME}"
    if [[ -L "$p" ]]; then
      ok "symlink  ${p} -> $(readlink "$p")"
    elif [[ -f "$p" ]]; then
      ok "copy     ${p}"
    else
      info "absent   ${p}"
    fi
  done
  echo
  echo "  CUBE_TEMPLATE_ID = ${CUBE_TEMPLATE_ID:-<unset>}"
  echo "  E2B_API_URL      = ${E2B_API_URL:-<unset, defaults to http://127.0.0.1:3000>}"
  echo "  CUBE_OPENCODE_PYTHON = ${CUBE_OPENCODE_PYTHON:-<unset, defaults to python3>}"
  exit 0
  ;;

uninstall)
  echo "Removing CubeSandbox OpenCode plugin"
  echo
  if [[ -L "$TARGET" || -f "$TARGET" ]]; then
    rm -f "$TARGET" && ok "removed ${TARGET}"
  else
    info "not installed at ${TARGET}"
  fi
  # Clean up the directory only when this example created it and left it empty.
  if [[ -d "$TARGET_DIR" ]] && [[ -z "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]]; then
    rmdir "$TARGET_DIR" 2>/dev/null && ok "removed empty ${TARGET_DIR}"
  fi
  echo
  echo "Restart OpenCode for the change to take effect."
  echo "bash commands will run on the host again."
  exit 0
  ;;

install)
  echo "Installing CubeSandbox OpenCode plugin"
  echo
  echo "  scope : ${SCOPE}"
  echo "  target: ${TARGET}"
  echo

  [[ -f "$PLUGIN_SRC" ]] || { bad "plugin source missing: ${PLUGIN_SRC}"; exit 1; }
  ok "plugin source found"

  # The plugin fails closed when the backend is absent, which would block every
  # bash call. Refuse to install rather than hand the user a broken editor.
  if [[ ! -f "$BACKEND" ]]; then
    bad "backend missing: ${BACKEND}"
    echo
    echo "  The plugin fails closed, so without the backend every bash call"
    echo "  would be blocked. Aborting instead of installing a broken setup."
    exit 1
  fi
  ok "backend found"

  mkdir -p "$TARGET_DIR" || { bad "cannot create ${TARGET_DIR}"; exit 1; }

  # Replace whatever is there so reinstalling always converges.
  rm -f "$TARGET"

  if ln -s "$PLUGIN_SRC" "$TARGET" 2>/dev/null; then
    ok "symlinked (edits to the example take effect immediately)"
  else
    cp "$PLUGIN_SRC" "$TARGET" || { bad "copy failed"; exit 1; }
    ok "copied (symlink unavailable on this filesystem)"
  fi

  echo
  # Warn about configuration, but do not fail: the user may export these later.
  if [[ -z "${CUBE_TEMPLATE_ID:-}" ]]; then
    info "CUBE_TEMPLATE_ID is not set yet"
    echo "         export CUBE_TEMPLATE_ID=tpl-xxxxxxxx   # see .env.example"
  else
    ok "CUBE_TEMPLATE_ID = ${CUBE_TEMPLATE_ID}"
  fi

  PY="${CUBE_OPENCODE_PYTHON:-python3}"
  if command -v "$PY" >/dev/null 2>&1; then
    if "$PY" -c "import e2b_code_interpreter" 2>/dev/null; then
      ok "${PY} has e2b-code-interpreter"
    else
      info "${PY} cannot import e2b_code_interpreter"
      echo "         pip install e2b-code-interpreter"
      echo "         On Ubuntu 24.04 use a venv (PEP 668), then set:"
      echo "           export CUBE_OPENCODE_PYTHON=/path/to/venv/bin/python"
    fi
  else
    info "interpreter not found: ${PY}"
  fi

  cat <<EOF

Done. Restart OpenCode so it picks up the plugin.

Verify with a prompt such as:

    Run "uname -r" and tell me the kernel version.

The reported kernel should be the sandbox guest kernel, which differs from
your host kernel (compare with "uname -r" in your own terminal).

Uninstall: ${SCRIPT_DIR}/install.sh --uninstall$([[ "$SCOPE" == "global" ]] && echo " --global")
EOF
  exit 0
  ;;

esac
