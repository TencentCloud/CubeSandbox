#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

MODEL=""
INSTANCE=""
CONFIG="$PROJECT_DIR/configs/e2b-swebench.yaml"
SUBSET="lite"
SPLIT="test"
STEP_LIMIT=""
OUTPUT_DIR="$PROJECT_DIR/results"

usage() {
    echo "Usage: $0 --model <model> --instance <instance-id> [options]"
    echo ""
    echo "Run mini-swe-agent on a SWE-bench instance using cube-sandbox (E2B)."
    echo ""
    echo "Required:"
    echo "  --model     LLM model name (e.g. gemini/gemini-3-flash-preview)"
    echo "  --instance  SWE-bench instance ID (e.g. django__django-13447)"
    echo ""
    echo "Options:"
    echo "  --config      YAML config file (default: configs/e2b-swebench.yaml)"
    echo "  --subset      SWE-bench subset: lite|verified|full (default: lite)"
    echo "  --split       Dataset split: test|dev (default: test)"
    echo "  --step-limit  Max agent steps (default: from config YAML)"
    echo "  --output      Output directory (default: results/)"
    echo ""
    echo "Examples:"
    echo "  $0 --model gemini/gemini-3-flash-preview --instance django__django-13447"
    echo "  $0 --model openai/glm-5 --config configs/e2b-tokenhub.yaml --instance django__django-13447"
    echo "  $0 --model openai/kimi-k2.5 --config configs/e2b-kimi.yaml --instance django__django-13447"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --model)      MODEL="$2"; shift 2 ;;
        --instance)   INSTANCE="$2"; shift 2 ;;
        --config)     CONFIG="$2"; shift 2 ;;
        --subset)     SUBSET="$2"; shift 2 ;;
        --split)      SPLIT="$2"; shift 2 ;;
        --step-limit) STEP_LIMIT="$2"; shift 2 ;;
        --output)     OUTPUT_DIR="$2"; shift 2 ;;
        -h|--help)  usage ;;
        *)          echo "Unknown option: $1"; usage ;;
    esac
done

if [[ -z "$MODEL" || -z "$INSTANCE" ]]; then
    echo "Error: --model and --instance are required."
    echo ""
    usage
fi

if [[ -f "$PROJECT_DIR/.env" ]]; then
    set -a
    source "$PROJECT_DIR/.env"
    set +a
fi

export E2B_API_URL="${E2B_API_URL:-}"
export E2B_API_KEY="${E2B_API_KEY:-}"
export CUBE_TEMPLATE_ID="${CUBE_TEMPLATE_ID:-}"
export HF_ENDPOINT="${HF_ENDPOINT:-https://hf-mirror.com}"

# SSL_CERT_FILE overrides Python's default CA bundle globally, which breaks
# HTTPS to public sites (e.g. hf-mirror.com). Instead, pass it via a custom
# var and let e2b.py apply it only when connecting to cube-sandbox.
if [[ -n "${SSL_CERT_FILE:-}" ]]; then
    export CUBE_SSL_CERT_FILE="$SSL_CERT_FILE"
    unset SSL_CERT_FILE
fi

if [[ -z "$E2B_API_URL" || -z "$CUBE_TEMPLATE_ID" ]]; then
    echo "Error: E2B_API_URL and CUBE_TEMPLATE_ID must be set in .env or environment."
    exit 1
fi

MODEL_SHORT="${MODEL##*/}"
RUN_DIR="$OUTPUT_DIR/${MODEL_SHORT}/${INSTANCE}"
mkdir -p "$RUN_DIR"

echo "=== cube-sandbox SWE-bench Evaluation ==="
echo ""
echo "Model:      $MODEL"
echo "Instance:   $INSTANCE"
echo "Config:     $CONFIG"
echo "Subset:     $SUBSET"
echo "Step Limit: ${STEP_LIMIT:-from config}"
echo "Output:     $RUN_DIR"
echo ""

EXTRA_CONFIGS=()
if [[ -n "$STEP_LIMIT" ]]; then
    EXTRA_CONFIGS+=(-c "agent.step_limit=$STEP_LIMIT")
fi

START_TIME=$(date +%s)

MINI_EXIT=0
mini-extra swebench-single \
    --model "$MODEL" \
    --config "$CONFIG" \
    "${EXTRA_CONFIGS[@]}" \
    --subset "$SUBSET" \
    --split "$SPLIT" \
    -i "$INSTANCE" \
    -o "$RUN_DIR/trajectory.json" \
    --exit-immediately \
    2>&1 | tee "$RUN_DIR/run.log" || MINI_EXIT=$?

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

echo ""
echo "=== Evaluation Complete ==="
echo "Duration:   ${ELAPSED}s"
echo "Results:    $RUN_DIR/"
echo "Trajectory: $RUN_DIR/trajectory.json"
echo "Log:        $RUN_DIR/run.log"

exit $MINI_EXIT
