#!/usr/bin/env bash
# Install the CubeSandbox PreToolUse hook for Claude Code.
#
# What this does:
#   1. Copies cubesandbox_exec.py / cubesandbox_rewrite.py into ~/.claude/hooks/
#   2. Installs a `cubesandbox-exec` shim on your PATH (~/.local/bin by default)
#   3. Registers the PreToolUse hook in ~/.claude/settings.json (idempotent --
#      safe to re-run, never clobbers your other settings/hooks)
#
# Usage:
#   ./install.sh                 # install for the current user (~/.claude)
#   CLAUDE_DIR=/path ./install.sh  # install into a different Claude config dir
#   ./install.sh --uninstall     # remove the hook registration + shim

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAUDE_DIR="${CLAUDE_DIR:-$HOME/.claude}"
HOOKS_DIR="$CLAUDE_DIR/hooks"
SETTINGS_FILE="$CLAUDE_DIR/settings.json"
BIN_DIR="${CUBE_BIN_DIR:-$HOME/.local/bin}"

REWRITE_HOOK_PATH="$HOOKS_DIR/cubesandbox_rewrite.py"
EXEC_SCRIPT_PATH="$HOOKS_DIR/cubesandbox_exec.py"
SHIM_PATH="$BIN_DIR/cubesandbox-exec"

# ── settings.json merge helper (pure python, no jq dependency) ─────────
merge_hook() {
python3 - "$SETTINGS_FILE" "$REWRITE_HOOK_PATH" <<'PYEOF'
import json
import sys
from pathlib import Path

settings_path, hook_path = sys.argv[1], sys.argv[2]
path = Path(settings_path)
data = {}
if path.exists() and path.stat().st_size > 0:
    data = json.loads(path.read_text())

hooks = data.setdefault("hooks", {})
pretool = hooks.setdefault("PreToolUse", [])

entry = {"type": "command", "command": hook_path}
for group in pretool:
    if group.get("matcher") == "Bash":
        for h in group.get("hooks", []):
            if h.get("command") == hook_path:
                print("Hook already registered, skipping.")
                sys.exit(0)
        group.setdefault("hooks", []).append(entry)
        break
else:
    pretool.append({"matcher": "Bash", "hooks": [entry]})

path.parent.mkdir(parents=True, exist_ok=True)
path.write_text(json.dumps(data, indent=2) + "\n")
print(f"Registered PreToolUse hook in {settings_path}")
PYEOF
}

unmerge_hook() {
python3 - "$SETTINGS_FILE" "$REWRITE_HOOK_PATH" <<'PYEOF'
import json
import sys
from pathlib import Path

settings_path, hook_path = sys.argv[1], sys.argv[2]
path = Path(settings_path)
if not path.exists():
    sys.exit(0)

data = json.loads(path.read_text())
pretool = data.get("hooks", {}).get("PreToolUse", [])
for group in pretool:
    if group.get("matcher") == "Bash":
        group["hooks"] = [h for h in group.get("hooks", []) if h.get("command") != hook_path]
pretool[:] = [g for g in pretool if g.get("hooks")]

path.write_text(json.dumps(data, indent=2) + "\n")
print(f"Removed PreToolUse hook from {settings_path}")
PYEOF
}

if [[ "${1:-}" == "--uninstall" ]]; then
    if command -v python3 >/dev/null 2>&1; then
        unmerge_hook
    else
        echo "python3 not found; skipping settings.json cleanup." >&2
    fi
    rm -f "$REWRITE_HOOK_PATH" "$EXEC_SCRIPT_PATH" "$HOOKS_DIR/.env" "$SHIM_PATH"
    echo "Uninstalled. (Sandboxes created by previous sessions are not auto-killed --"
    echo "run 'cubesandbox-exec --reset --session <id>' or clean up via CubeMaster.)"
    exit 0
fi

command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }

echo "Installing CubeSandbox hook backend for Claude Code..."

mkdir -p "$HOOKS_DIR" "$BIN_DIR"

install -m 0755 "$SCRIPT_DIR/cubesandbox_rewrite.py" "$REWRITE_HOOK_PATH"
install -m 0755 "$SCRIPT_DIR/cubesandbox_exec.py" "$EXEC_SCRIPT_PATH"
# Copy .env so that cubesandbox_exec.py can load CUBE_TEMPLATE_ID at runtime
if [[ -f "$SCRIPT_DIR/.env" ]]; then
    cp "$SCRIPT_DIR/.env" "$HOOKS_DIR/.env"
    echo "Copied .env to $HOOKS_DIR/.env"
fi

cat > "$SHIM_PATH" <<SHIMEOF
#!/usr/bin/env bash
exec python3 "$EXEC_SCRIPT_PATH" "\$@"
SHIMEOF
chmod +x "$SHIM_PATH"

echo "Installing Python dependencies (cubesandbox, python-dotenv)..."
if ! python3 -m pip install --user -q -r "$SCRIPT_DIR/requirements.txt"; then
    echo "pip install failed; install 'cubesandbox' and 'python-dotenv' manually." >&2
    exit 1
fi

# Back up settings.json before mutating it so a botched merge is recoverable.
if [[ -f "$SETTINGS_FILE" ]]; then
    cp "$SETTINGS_FILE" "$SETTINGS_FILE.bak.$(date +%s)"
fi

merge_hook

echo
echo "Done."
echo
if [[ ":$PATH:" != *":$BIN_DIR:"* ]]; then
    echo "WARNING: $BIN_DIR is not on your PATH. Add this to your shell profile:"
    echo "  export PATH=\"$BIN_DIR:\$PATH\""
    echo
fi
if [[ ! -f "$SCRIPT_DIR/.env" ]]; then
    echo "Next step: cp .env.example .env and fill in CUBE_TEMPLATE_ID / CUBE_API_URL,"
    echo "then restart Claude Code. Every Bash command it runs will now execute inside"
    echo "an isolated CubeSandbox MicroVM."
fi
