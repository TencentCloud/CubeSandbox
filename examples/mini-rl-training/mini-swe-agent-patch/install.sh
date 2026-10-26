#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if ! python3 -c "import minisweagent" &>/dev/null; then
    echo "Error: mini-swe-agent is not installed."
    echo "Install it first: pip install 'mini-swe-agent[extra]'"
    exit 1
fi

AGENT_DIR=$(python3 -c "import os, minisweagent; print(os.path.dirname(minisweagent.__file__))" 2>/dev/null | tail -1)

if [[ -z "$AGENT_DIR" || ! -d "$AGENT_DIR" ]]; then
    echo "Error: could not locate minisweagent package directory."
    exit 1
fi

echo "mini-swe-agent found at: $AGENT_DIR"

patch_file() {
    local src="$1" dst="$2" label="$3"
    if [[ ! -f "$src" ]]; then
        echo "  [SKIP] source not found: $src"
        return 1
    fi
    mkdir -p "$(dirname "$dst")"
    cp "$src" "$dst"
    echo "  [OK]   $label"
}

echo ""
echo "Applying patches..."
patch_file "$SCRIPT_DIR/environments/__init__.py"   "$AGENT_DIR/environments/__init__.py"   "environments/__init__.py  (register 'e2b' environment)"
patch_file "$SCRIPT_DIR/environments/extra/e2b.py"  "$AGENT_DIR/environments/extra/e2b.py"  "environments/extra/e2b.py (E2BEnvironment class)"
patch_file "$SCRIPT_DIR/run/benchmarks/swebench.py" "$AGENT_DIR/run/benchmarks/swebench.py" "run/benchmarks/swebench.py (add 'e2b' support)"

echo ""
echo "Done. You can now use 'environment_class: e2b' in your config YAML."
