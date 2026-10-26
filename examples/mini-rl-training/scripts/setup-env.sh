#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

echo "=== cube-sandbox RL Example: Environment Setup ==="
echo ""

if ! command -v python3 &>/dev/null; then
    echo "Error: python3 not found. Please install Python 3.10+."
    exit 1
fi

PYTHON_VERSION=$(python3 -c "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
echo "Python version: $PYTHON_VERSION"

echo ""
echo "--- Installing Python dependencies ---"
pip install -r "$PROJECT_DIR/requirements.txt"

echo ""
echo "--- Verifying installations ---"
python3 -c "import litellm; print(f'litellm OK')"
python3 -c "from e2b_code_interpreter import Sandbox; print('e2b-code-interpreter OK')"
python3 -c "import minisweagent; print(f'mini-swe-agent OK (v{minisweagent.__version__})')" 2>/dev/null \
    || echo "Warning: mini-swe-agent not installed. Install with: pip install mini-swe-agent[extra]"

echo ""
echo "--- Checking .env file ---"
if [[ -f "$PROJECT_DIR/.env" ]]; then
    echo ".env file found."
else
    echo ".env file not found. Creating from template..."
    cp "$PROJECT_DIR/.env.example" "$PROJECT_DIR/.env"
    echo "Please edit $PROJECT_DIR/.env with your actual values."
fi

echo ""
echo "--- SSL Certificate Check ---"
if [[ -n "${SSL_CERT_FILE:-}" ]]; then
    if [[ -f "$SSL_CERT_FILE" ]]; then
        echo "SSL_CERT_FILE=$SSL_CERT_FILE (exists)"
    else
        echo "Warning: SSL_CERT_FILE=$SSL_CERT_FILE does not exist."
    fi
else
    echo "SSL_CERT_FILE not set. If using self-hosted cube-sandbox with mkcert,"
    echo "you may need to install the root CA and set SSL_CERT_FILE."
    echo "See README.md for instructions."
fi

echo ""
echo "=== Setup complete ==="
