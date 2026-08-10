#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# CubeSandbox Rollback — idempotent Claude Code hook installer
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOKS_DIR="${HOME}/.claude/hooks/rollback"

# Project root detection
_find_project_root() {
    local dir
    dir="${1:-$PWD}"
    if git -C "$dir" rev-parse --show-toplevel 2>/dev/null; then
        return 0
    fi
    echo "$PWD"
    return 1
}

PROJECT_ROOT="$(_find_project_root || true)"
PROJECT_SETTINGS="${PROJECT_ROOT}/.claude/settings.json"
SKILL_DIR="${PROJECT_ROOT}/.claude/skills/cubesandbox-rollback"

# ---------------------------------------------------------------------------
# Helper: read/write JSON settings
# ---------------------------------------------------------------------------
_HAS_JQ=false
if command -v jq &>/dev/null; then _HAS_JQ=true; fi

_json_merge_hooks() {
    # Merge rollback hooks into existing settings.json, preserving other hooks.
    local file="$1"
    python3 - "$file" "$HOOKS_DIR" "${_HAS_JQ}" <<'PYEOF'
import json, sys, os

path = sys.argv[1]
hooks_dir = sys.argv[2]

# Read existing
try:
    with open(path, "r") as fh:
        cfg = json.load(fh)
except (FileNotFoundError, json.JSONDecodeError):
    cfg = {}

hooks = cfg.setdefault("hooks", {})

# --- PreToolUse / Bash ---
ptu = hooks.setdefault("PreToolUse", [])
bash_block = None
for b in ptu:
    if isinstance(b, dict) and b.get("matcher") == "Bash":
        bash_block = b
        break
if bash_block is None:
    bash_block = {"matcher": "Bash", "hooks": []}
    ptu.append(bash_block)

bash_hooks = bash_block.setdefault("hooks", [])
snapshot_cmd = {"type": "command", "command": f"python3 {hooks_dir}/cubesandbox_snapshot.py"}
rollback_cmd = {"type": "command", "command": f"python3 {hooks_dir}/cubesandbox_rollback.py"}
# Idempotent: only add if not present
existing_cmds = {json.dumps(h, sort_keys=True) for h in bash_hooks}
if json.dumps(snapshot_cmd, sort_keys=True) not in existing_cmds:
    bash_hooks.append(snapshot_cmd)
if json.dumps(rollback_cmd, sort_keys=True) not in existing_cmds:
    bash_hooks.append(rollback_cmd)

# --- SessionStart ---
ss = hooks.setdefault("SessionStart", [])
session_block = None
for b in ss:
    if isinstance(b, dict):
        session_block = b
        break
if session_block is None:
    session_block = {"hooks": []}
    ss.append(session_block)
ss_hooks = session_block.setdefault("hooks", [])
session_cmd = {"type": "command", "command": f"python3 {hooks_dir}/cubesandbox_session.py"}
existing_ss = {json.dumps(h, sort_keys=True) for h in ss_hooks}
if json.dumps(session_cmd, sort_keys=True) not in existing_ss:
    ss_hooks.append(session_cmd)

# --- PostToolUse / Bash ---
post = hooks.setdefault("PostToolUse", [])
post_block = None
for b in post:
    if isinstance(b, dict) and b.get("matcher") == "Bash":
        post_block = b
        break
if post_block is None:
    post_block = {"matcher": "Bash", "hooks": []}
    post.append(post_block)
post_hooks = post_block.setdefault("hooks", [])
post_cmd = {"type": "command", "command": f"python3 {hooks_dir}/cubesandbox_poststart.py"}
existing_post = {json.dumps(h, sort_keys=True) for h in post_hooks}
if json.dumps(post_cmd, sort_keys=True) not in existing_post:
    post_hooks.append(post_cmd)

os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path, "w") as fh:
    json.dump(cfg, fh, indent=2)
    fh.write("\n")
PYEOF
}

_json_remove_rollback() {
    local file="$1"
    python3 - "$file" "$HOOKS_DIR" <<'PYEOF'
import json, sys, os

path = sys.argv[1]
hooks_dir = sys.argv[2]

try:
    with open(path, "r") as fh:
        cfg = json.load(fh)
except (FileNotFoundError, json.JSONDecodeError):
    cfg = {}

hooks = cfg.get("hooks", {})


def _hook_cmd(h, default=""):
    """CC hooks may be dicts ({type, command}) or plain strings."""
    return h.get("command", default) if isinstance(h, dict) else h


# Remove our commands from PreToolUse/Bash
for b in hooks.get("PreToolUse", []):
    if isinstance(b, dict) and b.get("matcher") == "Bash":
        b["hooks"] = [h for h in b.get("hooks", [])
                       if _hook_cmd(h).find("cubesandbox_rollback") == -1
                       and _hook_cmd(h).find("cubesandbox_snapshot") == -1]

# Remove SessionStart hooks
for b in hooks.get("SessionStart", []):
    if isinstance(b, dict):
        b["hooks"] = [h for h in b.get("hooks", [])
                       if _hook_cmd(h).find("cubesandbox_session") == -1]

# Remove PostToolUse/Bash hooks
for b in hooks.get("PostToolUse", []):
    if isinstance(b, dict) and b.get("matcher") == "Bash":
        b["hooks"] = [h for h in b.get("hooks", [])
                       if _hook_cmd(h).find("cubesandbox_poststart") == -1]

# Remove empty blocks
for key in ("PreToolUse", "SessionStart", "PostToolUse"):
    if key in hooks:
        hooks[key] = [b for b in hooks[key]
                       if isinstance(b, dict) and b.get("hooks")]
        if not hooks[key]:
            del hooks[key]

if not hooks:
    cfg.pop("hooks", None)

with open(path, "w") as fh:
    json.dump(cfg, fh, indent=2)
    fh.write("\n")
PYEOF
}

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: $0 [--uninstall] [--help]

Install or uninstall the CubeSandbox rollback hooks for Claude Code.

Options:
  --uninstall   Remove rollback hooks from project settings
  --help        Show this help message

The installer:
  - Copies hook scripts to ~/.claude/hooks/rollback/
  - Registers hooks in <project>/.claude/settings.json
  - Creates a skill at <project>/.claude/skills/cubesandbox-rollback/
  - NEVER touches ~/.claude/settings.json (cubesandbox-hook's domain)
EOF
}

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------
do_install() {
    echo "=== CubeSandbox Rollback Installer ==="
    echo "Project root: ${PROJECT_ROOT}"

    # 1. Copy hooks
    echo "Installing hooks to ${HOOKS_DIR} ..."
    mkdir -p "${HOOKS_DIR}"
    for f in cubesandbox_lib.py cubesandbox_risky.py \
             cubesandbox_snapshot.py cubesandbox_rollback.py \
             cubesandbox_session.py cubesandbox_poststart.py \
             .env.example; do
        if [[ -f "${SCRIPT_DIR}/${f}" ]]; then
            cp "${SCRIPT_DIR}/${f}" "${HOOKS_DIR}/"
            chmod +x "${HOOKS_DIR}/${f}" 2>/dev/null || true
            echo "  ${f}"
        fi
    done

    # 2. Register hooks in project settings
    echo "Registering hooks in ${PROJECT_SETTINGS} ..."
    _json_merge_hooks "${PROJECT_SETTINGS}"
    echo "  done"

    # 3. Create skill
    echo "Creating skill at ${SKILL_DIR} ..."
    mkdir -p "${SKILL_DIR}"
    cp "${SCRIPT_DIR}/SKILL.md" "${SKILL_DIR}/"
    echo "  done"

    # 4. Check cubesandbox-hook
    if [[ -f "${HOME}/.claude/settings.json" ]]; then
        echo "  cubesandbox-hook user-level settings.json: found"
    else
        echo "  ⚠  cubesandbox-hook user-level settings.json NOT found — install cubesandbox-hook first"
    fi

    # 5. Deps
    if command -v pip &>/dev/null; then
        for dep in cubesandbox bashlex; do
            if python3 -c "import ${dep}" 2>/dev/null; then
                echo "  ${dep}: installed"
            else
                echo "  Installing ${dep} via pip ..."
                pip install "${dep}"
            fi
        done
    fi

    echo "=== Install complete ==="
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
do_uninstall() {
    echo "=== CubeSandbox Rollback Uninstaller ==="
    echo "Project root: ${PROJECT_ROOT}"

    echo "Removing hooks from ${PROJECT_SETTINGS} ..."
    _json_remove_rollback "${PROJECT_SETTINGS}"
    echo "  done"

    echo "Removing ${HOOKS_DIR} ..."
    rm -rf "${HOOKS_DIR}"
    echo "  done"

    echo "Removing ${SKILL_DIR} ..."
    rm -rf "${SKILL_DIR}"
    echo "  done"

    echo "User-level ~/.claude/settings.json: left untouched"
    echo "=== Uninstall complete ==="
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
case "${1:-}" in
    --help|-h) usage; exit 0 ;;
    --uninstall) do_uninstall ;;
    "") do_install ;;
    *) usage; exit 1 ;;
esac
