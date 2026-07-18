#!/usr/bin/env bash
# Install or remove the CubeSandbox bash-routing plugin for OpenCode.
#
# OpenCode loads JavaScript plugins automatically from:
#   - $XDG_CONFIG_HOME/opencode/plugins/        (global, default ~/.config)
#   - .opencode/plugins/                        (project-local)
#
# This installer copies cubesandbox-sandbox.js into the global plugin
# directory and writes a sanitized subset of CUBE_* settings into the
# OpenCode config so the spawned sandbox_exec.py can authenticate against
# CubeMaster. Provider API keys (ANTHROPIC_API_KEY, OPENAI_API_KEY, ...) are
# never copied into the OpenCode config.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXAMPLE_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# XDG_CONFIG_HOME defaults to ~/.config per the spec.
OPENCODE_CONFIG_HOME="${OPENCODE_CONFIG_HOME:-$HOME/.config/opencode}"
PLUGIN_DIR="$OPENCODE_CONFIG_HOME/plugins"
PLUGIN_FILE="$PLUGIN_DIR/cubesandbox-sandbox.js"
PACKAGE_FILE="$PLUGIN_DIR/package.json"
CONFIG_FILE="$OPENCODE_CONFIG_HOME/opencode.json"
SOURCE_ENV="$EXAMPLE_DIR/.env.example"

ALLOWED_KEYS=(
    "CUBE_API_URL"
    "CUBE_TEMPLATE_ID"
    "CUBE_PROXY_NODE_IP"
    "CUBE_PROXY_PORT_HTTP"
    "CUBE_SANDBOX_DOMAIN"
    "CUBE_SANDBOX_USER"
    "CUBE_SANDBOX_TIMEOUT"
    "CUBE_EXEC_TIMEOUT"
)

write_opencode_config() {
    python3 - "$SOURCE_ENV" "$CONFIG_FILE" <<'PYEOF'
import json
import os
import stat
import sys
import tempfile
from pathlib import Path

try:
    from dotenv import dotenv_values
except ImportError as exc:
    raise SystemExit(
        "python-dotenv is required; install examples/opencode-integration/requirements.txt"
    ) from exc

ALLOWED_KEYS = {
    "CUBE_API_URL",
    "CUBE_TEMPLATE_ID",
    "CUBE_PROXY_NODE_IP",
    "CUBE_PROXY_PORT_HTTP",
    "CUBE_SANDBOX_DOMAIN",
    "CUBE_SANDBOX_USER",
    "CUBE_SANDBOX_TIMEOUT",
    "CUBE_EXEC_TIMEOUT",
}

source = Path(sys.argv[1])
destination = Path(sys.argv[2])
values = dotenv_values(source) if source.is_file() else {}

cube_settings = {
    key: values[key]
    for key in ALLOWED_KEYS
    if isinstance(values.get(key), str) and values[key]
}

destination.parent.mkdir(parents=True, exist_ok=True)

existing = {}
if destination.exists() and destination.stat().st_size:
    try:
        existing = json.loads(destination.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        existing = {}
if not isinstance(existing, dict):
    raise SystemExit(f"{destination} must contain a JSON object")

# Merge: union of CUBE_* keys wins; everything else is preserved so the
# user keeps their MCP / provider / model settings untouched.
existing_cube = existing.get("cubesandbox", {})
if not isinstance(existing_cube, dict):
    existing_cube = {}
existing_cube.update(cube_settings)
# Drop entries whose value resolved empty so we never serialize "" into the
# installed config — OpenCode would forward that as a literal empty env.
existing_cube = {k: v for k, v in existing_cube.items() if v}
existing["cubesandbox"] = existing_cube

fd, tmp_name = tempfile.mkstemp(
    dir=destination.parent,
    prefix=f".{destination.name}.",
    suffix=".tmp",
)
temporary = Path(tmp_name)
try:
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fp:
        json.dump(existing, fp, indent=2)
        fp.write("\n")
        fp.flush()
        os.fsync(fp.fileno())
    os.replace(temporary, destination)
    os.chmod(destination, 0o600)
finally:
    with __import__("contextlib").suppress(FileNotFoundError):
        temporary.unlink()
PYEOF
}

if [[ "${1:-}" == "--uninstall" ]]; then
    rm -f "$PLUGIN_FILE" "$PACKAGE_FILE"
    echo "CubeSandbox OpenCode plugin uninstalled from $PLUGIN_DIR."
    echo "(Your opencode.json was left intact — remove the \"cubesandbox\" key manually if desired.)"
    exit 0
fi

if [[ $# -gt 0 ]]; then
    echo "usage: $0 [--uninstall]" >&2
    exit 2
fi

command -v python3 >/dev/null 2>&1 || {
    echo "python3 is required" >&2
    exit 1
}

mkdir -p "$PLUGIN_DIR"
install -m 0644 "$SCRIPT_DIR/cubesandbox-sandbox.js" "$PLUGIN_FILE"
# OpenCode loads the plugin as an ES module; the host plugin dir must
# declare "type": "module" so the .js file's `import` statements resolve.
cat > "$PACKAGE_FILE" <<'JSONEOF'
{
  "name": "opencode-cubesandbox-plugins",
  "version": "1.0.0",
  "private": true,
  "type": "module",
  "description": "Local plugin directory for OpenCode's CubeSandbox bash router. Auto-loaded by OpenCode."
}
JSONEOF
write_opencode_config

echo "CubeSandbox OpenCode plugin installed:"
echo "  plugin file: $PLUGIN_FILE"
echo "  config:      $CONFIG_FILE  (only CUBE_* keys added)"
echo ""
echo "Restart OpenCode for the plugin to take effect."
echo "Uninstall with: $0 --uninstall"