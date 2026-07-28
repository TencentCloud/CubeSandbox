#!/usr/bin/env bash
# Install or remove the CubeSandbox PreToolUse hook for Claude Code.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAUDE_DIR="${CLAUDE_DIR:-$HOME/.claude}"
HOOKS_DIR="$CLAUDE_DIR/hooks"
SETTINGS_FILE="$CLAUDE_DIR/settings.json"
REWRITE_HOOK_PATH="$HOOKS_DIR/cubesandbox_rewrite.py"
EXEC_SCRIPT_PATH="$HOOKS_DIR/cubesandbox_exec.py"
CONFIG_PATH="$HOOKS_DIR/cubesandbox.env"
SOURCE_ENV="$SCRIPT_DIR/../.env"

update_hook() {
python3 - "$1" "$SETTINGS_FILE" "$REWRITE_HOOK_PATH" <<'PYEOF'
import contextlib
import fcntl
import json
import os
import shlex
import stat
import sys
import tempfile
from pathlib import Path

action = sys.argv[1]
settings_path = Path(sys.argv[2])
# Resolve a symlinked settings.json (dotfile tools like stow/chezmoi) so the
# atomic replace below writes through to the target instead of silently
# detaching the user's link.
settings_path = Path(os.path.realpath(settings_path))
direct_command = shlex.quote(sys.argv[3])
hook_command = f"{direct_command} || exit 2"


def atomic_write(path, data):
    mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o600
    descriptor, temporary_name = tempfile.mkstemp(
        dir=path.parent,
        prefix=f".{path.name}.",
        suffix=".tmp",
    )
    temporary = Path(temporary_name)
    try:
        os.fchmod(descriptor, mode)
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(data, output, indent=2)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        with contextlib.suppress(FileNotFoundError):
            temporary.unlink()


if action == "uninstall" and not settings_path.exists():
    raise SystemExit(0)

settings_path.parent.mkdir(parents=True, exist_ok=True)
lock_path = settings_path.with_name(f".{settings_path.name}.cubesandbox.lock")
flags = os.O_CREAT | os.O_RDWR
if hasattr(os, "O_NOFOLLOW"):
    flags |= os.O_NOFOLLOW
lock_descriptor = os.open(lock_path, flags, 0o600)
try:
    os.fchmod(lock_descriptor, 0o600)
    fcntl.flock(lock_descriptor, fcntl.LOCK_EX)

    data = {}
    if settings_path.exists() and settings_path.stat().st_size:
        try:
            data = json.loads(settings_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as exc:
            raise SystemExit(f"settings.json is not valid JSON: {exc}")
    if not isinstance(data, dict):
        raise SystemExit("settings.json must contain a JSON object")

    hooks = data.get("hooks")
    if hooks is None and action == "install":
        hooks = data.setdefault("hooks", {})
    if hooks is not None and not isinstance(hooks, dict):
        raise SystemExit("settings.json hooks must be a JSON object")

    pretool = hooks.get("PreToolUse") if isinstance(hooks, dict) else None
    if pretool is None and action == "install":
        pretool = hooks.setdefault("PreToolUse", [])
    if pretool is not None and not isinstance(pretool, list):
        raise SystemExit("settings.json hooks.PreToolUse must be a JSON array")

    if action == "install":
        entry = {"type": "command", "command": hook_command}
        for group in pretool:
            if not isinstance(group, dict) or group.get("matcher") != "Bash":
                continue
            group_hooks = group.setdefault("hooks", [])
            if not isinstance(group_hooks, list):
                raise SystemExit("Bash hook group hooks must be a JSON array")
            found = False
            for item in group_hooks:
                if not isinstance(item, dict):
                    continue
                item_command = item.get("command")
                if not isinstance(item_command, str):
                    continue
                if item_command == hook_command:
                    found = True
                elif item_command == direct_command or (
                    "cubesandbox_rewrite.py" in item_command
                ):
                    # Legacy form without the fail-closed backstop, or the
                    # same hook installed from a stale path (repo moved).
                    item["command"] = hook_command
                    found = True
            if not found:
                group_hooks.append(entry)
            break
        else:
            pretool.append({"matcher": "Bash", "hooks": [entry]})
    elif isinstance(pretool, list):
        removable_commands = {direct_command, hook_command}
        for group in pretool:
            if not isinstance(group, dict) or group.get("matcher") != "Bash":
                continue
            group_hooks = group.get("hooks")
            if isinstance(group_hooks, list):
                group["hooks"] = [
                    item
                    for item in group_hooks
                    if not (
                        isinstance(item, dict)
                        and isinstance(item.get("command"), str)
                        and (
                            item["command"] in removable_commands
                            or "cubesandbox_rewrite.py" in item["command"]
                        )
                    )
                ]
        hooks["PreToolUse"] = [
            group
            for group in pretool
            if not (
                isinstance(group, dict)
                and group.get("matcher") == "Bash"
                and not group.get("hooks")
            )
        ]

    atomic_write(settings_path, data)
finally:
    fcntl.flock(lock_descriptor, fcntl.LOCK_UN)
    os.close(lock_descriptor)
PYEOF
}

write_config() {
python3 - "$SOURCE_ENV" "$CONFIG_PATH" <<'PYEOF'
import os
import sys
import tempfile
from pathlib import Path

from dotenv import dotenv_values, set_key

allowed = (
    "CUBE_API_URL",
    "CUBE_TEMPLATE_ID",
    "CUBE_PROXY_NODE_IP",
    "CUBE_PROXY_PORT_HTTP",
    "CUBE_SANDBOX_DOMAIN",
    "CUBE_SANDBOX_USER",
    "CUBE_SANDBOX_TIMEOUT",
    "CUBE_EXEC_TIMEOUT",
    "CUBE_HOOK_STATE_DIR",
)
source = Path(sys.argv[1])
destination = Path(sys.argv[2])
values = dotenv_values(source) if source.is_file() else {}
filtered = {key: values[key] for key in allowed if isinstance(values.get(key), str)}

if not filtered and destination.is_file():
    # Re-installing after the source .env was removed must not wipe a
    # working installed configuration.
    print(
        f"note: no CubeSandbox values in {source}; "
        f"keeping existing {destination}",
        file=sys.stderr,
    )
    raise SystemExit(0)

destination.parent.mkdir(parents=True, exist_ok=True)
descriptor, temporary_name = tempfile.mkstemp(
    dir=destination.parent,
    prefix=f".{destination.name}.",
    suffix=".tmp",
)
os.close(descriptor)
temporary = Path(temporary_name)
try:
    os.chmod(temporary, 0o600)
    for key, value in filtered.items():
        set_key(str(temporary), key, value, quote_mode="always")
    os.replace(temporary, destination)
    os.chmod(destination, 0o600)
finally:
    if temporary.exists():
        temporary.unlink()
PYEOF
}

if [[ "${1:-}" == "--uninstall" ]]; then
    command -v python3 >/dev/null 2>&1 || {
        echo "python3 is required to update Claude Code settings" >&2
        exit 1
    }
    update_hook uninstall
    rm -f "$REWRITE_HOOK_PATH" "$EXEC_SCRIPT_PATH" "$CONFIG_PATH" \
        "$CLAUDE_DIR/.$(basename "$SETTINGS_FILE").cubesandbox.lock"
    echo "CubeSandbox Claude Code hook uninstalled."
    DEFAULT_STATE_DIR="$HOME/.cache/cubesandbox-hook"
    if compgen -G "$DEFAULT_STATE_DIR/*.json" >/dev/null 2>&1; then
        echo "note: cached sandbox state remains in $DEFAULT_STATE_DIR; any running" >&2
        echo "      sandboxes expire with their TTL. Remove the directory to discard it." >&2
    fi
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
if ! python3 - <<'PYEOF'
import sys

if sys.version_info < (3, 9):
    raise SystemExit("Python 3.9 or newer is required")

try:
    import cubesandbox  # noqa: F401
    import dotenv  # noqa: F401
except ImportError as exc:
    raise SystemExit(
        "missing Python dependency: " + str(exc)
        + "; install examples/claude-code-integration/requirements.txt first"
    )
PYEOF
then
    exit 1
fi

mkdir -p "$HOOKS_DIR"
install -m 0755 "$SCRIPT_DIR/cubesandbox_rewrite.py" "$REWRITE_HOOK_PATH"
install -m 0755 "$SCRIPT_DIR/cubesandbox_exec.py" "$EXEC_SCRIPT_PATH"
write_config
update_hook install

echo "CubeSandbox Claude Code hook installed in $HOOKS_DIR."
