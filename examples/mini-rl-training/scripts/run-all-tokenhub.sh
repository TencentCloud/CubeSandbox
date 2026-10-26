#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

INSTANCE=""
SUBSET="lite"
SPLIT="test"
STEP_LIMIT=""

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Run all TokenHub models in parallel on a SWE-bench instance."
    echo ""
    echo "Options:"
    echo "  --instance    SWE-bench instance ID (default: django__django-13447)"
    echo "  --subset      Dataset subset: lite|verified|full (default: lite)"
    echo "  --split       Dataset split: test|dev (default: test)"
    echo "  --step-limit  Max agent steps (default: from config YAML)"
    echo ""
    echo "Examples:"
    echo "  $0"
    echo "  $0 --instance django__django-13447"
    echo "  $0 --instance django__django-13447 --step-limit 50"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --instance)   INSTANCE="$2"; shift 2 ;;
        --subset)     SUBSET="$2"; shift 2 ;;
        --split)      SPLIT="$2"; shift 2 ;;
        --step-limit) STEP_LIMIT="$2"; shift 2 ;;
        -h|--help)    usage ;;
        *)            echo "Unknown option: $1"; usage ;;
    esac
done

INSTANCE="${INSTANCE:-django__django-13447}"

if [[ -f "$PROJECT_DIR/.env" ]]; then
    set -a
    source "$PROJECT_DIR/.env"
    set +a
fi

export OPENAI_API_KEY="${TOKENHUB_API_KEY:-$OPENAI_API_KEY}"

if [[ -z "${OPENAI_API_KEY:-}" ]]; then
    echo "Error: TOKENHUB_API_KEY or OPENAI_API_KEY must be set in .env or environment."
    exit 1
fi

TOKENHUB_CONFIG="$PROJECT_DIR/configs/e2b-tokenhub.yaml"
THINKING_CONFIG="$PROJECT_DIR/configs/e2b-kimi.yaml"

declare -A MODELS=(
    ["openai/glm-5"]="$TOKENHUB_CONFIG"
    ["openai/glm-5-turbo"]="$TOKENHUB_CONFIG"
    ["openai/minimax-m2.7"]="$TOKENHUB_CONFIG"
    ["openai/deepseek-v3.2"]="$TOKENHUB_CONFIG"
    ["openai/kimi-k2.5"]="$THINKING_CONFIG"
    ["openai/deepseek-r1-0528"]="$THINKING_CONFIG"
    ["openai/hunyuan-2.0-thinking-20251109"]="$THINKING_CONFIG"
)

LOG_DIR="$PROJECT_DIR/results/_parallel_logs"
mkdir -p "$LOG_DIR"

echo "=== Parallel TokenHub Evaluation ==="
echo ""
echo "Instance:   $INSTANCE"
echo "Subset:     $SUBSET"
echo "Step Limit: ${STEP_LIMIT:-from config}"
echo "Models:     ${#MODELS[@]}"
echo ""

PIDS=()
MODEL_NAMES=()

for model in "${!MODELS[@]}"; do
    config="${MODELS[$model]}"
    model_short="${model##*/}"
    log_file="$LOG_DIR/${model_short}.log"

    echo "Starting: $model → $log_file"

    STEP_ARGS=()
    if [[ -n "$STEP_LIMIT" ]]; then
        STEP_ARGS+=(--step-limit "$STEP_LIMIT")
    fi

    bash "$SCRIPT_DIR/run-swebench.sh" \
        --model "$model" \
        --config "$config" \
        --instance "$INSTANCE" \
        --subset "$SUBSET" \
        --split "$SPLIT" \
        "${STEP_ARGS[@]}" \
        > "$log_file" 2>&1 &

    PIDS+=($!)
    MODEL_NAMES+=("$model")
done

echo ""
echo "All ${#PIDS[@]} models launched. Waiting for completion..."
echo ""

extract_stats() {
    local log_file="$1"
    local duration="-" steps="-" cost="-" setup="-" api="-" sdk="-" status="-"

    if [[ -f "$log_file" ]]; then
        local dur
        dur=$(grep -oP 'Duration:\s+\K\d+' "$log_file" 2>/dev/null | tail -1 || true)
        [[ -n "$dur" ]] && duration="${dur}s"

        local stats_line
        stats_line=$(grep 'STATS:' "$log_file" 2>/dev/null | tail -1 || true)
        if [[ -n "$stats_line" ]]; then
            local s c su ap sd st
            s=$(echo "$stats_line" | grep -oP 'steps=\K\S+' || true)
            c=$(echo "$stats_line" | grep -oP 'cost=\K\S+' || true)
            su=$(echo "$stats_line" | grep -oP 'setup_ms=\K\S+' || true)
            ap=$(echo "$stats_line" | grep -oP 'api_ms=\K\S+' || true)
            sd=$(echo "$stats_line" | grep -oP 'sdk_ms=\K\S+' || true)
            st=$(echo "$stats_line" | grep -oP 'status=\K.*' || true)
            [[ -n "$s" ]] && steps="$s"
            [[ -n "$c" ]] && cost="\$${c}"
            [[ -n "$su" ]] && setup="${su}ms"
            [[ -n "$ap" ]] && api="${ap}ms"
            [[ -n "$sd" ]] && sdk="${sd}ms"
            [[ -n "$st" ]] && status="$st"
        fi
    fi

    echo "$duration|$steps|$cost|$setup|$api|$sdk|$status"
}

FAILED=0
declare -A EXIT_CODES=()

for i in "${!PIDS[@]}"; do
    pid="${PIDS[$i]}"
    model="${MODEL_NAMES[$i]}"

    if wait "$pid"; then
        EXIT_CODES["$model"]="OK"
        echo "  [OK]   $model"
    else
        EXIT_CODES["$model"]="FAIL"
        echo "  [FAIL] $model"
        FAILED=$((FAILED + 1))
    fi
done

echo ""
echo "=== Results ==="
echo ""
printf "%-35s %8s %8s %8s %10s %10s %10s %12s %6s\n" "Model" "Duration" "Steps" "Cost" "Setup" "API" "SDK" "Status" "Exit"
printf "%-35s %8s %8s %8s %10s %10s %10s %12s %6s\n" "---" "---" "---" "---" "---" "---" "---" "---" "---"

for model in "${MODEL_NAMES[@]}"; do
    model_short="${model##*/}"
    log_file="$LOG_DIR/${model_short}.log"
    IFS='|' read -r duration steps cost setup api sdk status <<< "$(extract_stats "$log_file")"
    exit_code="${EXIT_CODES[$model]}"
    printf "%-35s %8s %8s %8s %10s %10s %10s %12s %6s\n" "$model_short" "$duration" "$steps" "$cost" "$setup" "$api" "$sdk" "$status" "$exit_code"
done

echo ""
echo "=== Summary ==="
echo "Instance: $INSTANCE"
echo "Total:    ${#PIDS[@]}"
echo "Success:  $((${#PIDS[@]} - FAILED))"
echo "Failed:   $FAILED"
echo "Results:  $PROJECT_DIR/results/"
echo "Logs:     $LOG_DIR/"
