#!/usr/bin/env bash
set -euo pipefail

TEMPLATE_ID=""
ACTION="list"

usage() {
    echo "Usage: $0 --template <template-id> [--kill]"
    echo ""
    echo "Check or clean up sandbox instances using a specific template."
    echo ""
    echo "Options:"
    echo "  --template  Template ID to filter (e.g. tpl-c301a4f1b99d4a1f87deb7d4)"
    echo "  --kill      Kill all matching instances (default: list only)"
    echo ""
    echo "Examples:"
    echo "  $0 --template tpl-c301a4f1b99d4a1f87deb7d4"
    echo "  $0 --template tpl-c301a4f1b99d4a1f87deb7d4 --kill"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --template) TEMPLATE_ID="$2"; shift 2 ;;
        --kill)     ACTION="kill"; shift ;;
        -h|--help)  usage ;;
        *)          echo "Unknown option: $1"; usage ;;
    esac
done

if [[ -z "$TEMPLATE_ID" ]]; then
    echo "Error: --template is required."
    echo ""
    usage
fi

if ! command -v cubecli &>/dev/null; then
    echo "Error: cubecli not found in PATH."
    exit 1
fi

CUBEBOX_IDS=$(cubecli ls 2>/dev/null | awk 'NR>1 && $1!="" {print $3}')

if [[ -z "$CUBEBOX_IDS" ]]; then
    echo "No running instances found."
    exit 0
fi

MATCHED=()

for cid in $CUBEBOX_IDS; do
    if cubecli ctr info "$cid" 2>/dev/null | grep -q "$TEMPLATE_ID"; then
        MATCHED+=("$cid")
    fi
done

if [[ ${#MATCHED[@]} -eq 0 ]]; then
    echo "No instances found using template $TEMPLATE_ID"
    exit 0
fi

echo "Found ${#MATCHED[@]} instance(s) using template $TEMPLATE_ID:"
echo ""
printf "  %s\n" "${MATCHED[@]}"
echo ""

if [[ "$ACTION" == "kill" ]]; then
    echo "Killing ${#MATCHED[@]} instance(s)..."
    for cid in "${MATCHED[@]}"; do
        if cubecli unsafe destroy "$cid" &>/dev/null; then
            echo "  [OK]   $cid"
        else
            echo "  [FAIL] $cid"
        fi
    done
    echo ""
    echo "Done."
else
    echo "To kill these instances, run:"
    echo "  $0 --template $TEMPLATE_ID --kill"
fi
