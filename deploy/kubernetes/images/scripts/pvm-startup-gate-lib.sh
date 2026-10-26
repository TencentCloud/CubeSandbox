#!/bin/sh
# Node-level PVM startup gate helpers. Callers must source node-prep-lib.sh first.

STARTUP_GATE_ENABLED="${STARTUP_GATE_ENABLED:-false}"
STARTUP_GATE_TAINT_KEY="${STARTUP_GATE_TAINT_KEY:-cube.tencent.com/pvm-not-ready}"
STARTUP_GATE_TAINT_EFFECT="${STARTUP_GATE_TAINT_EFFECT:-NoSchedule}"
STARTUP_GATE_DRAIN_TIMEOUT_SECONDS="${STARTUP_GATE_DRAIN_TIMEOUT_SECONDS:-300}"
STARTUP_GATE_CLEAR_MAINTENANCE="${STARTUP_GATE_CLEAR_MAINTENANCE:-false}"

startup_gate_active() {
  [ "$STARTUP_GATE_ENABLED" = "true" ]
}

startup_gate_require_kubectl() {
  command -v kubectl >/dev/null 2>&1 \
    || fail "kubectl is required while the PVM startup gate is enabled"
}

# Prints "effect=value" tokens for the configured taint key (space-separated).
startup_gate_taint_tokens() {
  kubectl get node "$NODE_NAME" \
    -o "jsonpath={range .spec.taints[?(@.key==\"${STARTUP_GATE_TAINT_KEY}\")]}{.effect}{\"=\"}{.value}{\" \"}{end}" 2>/dev/null
}

startup_gate_has_taint() {
  case " $(startup_gate_taint_tokens) " in
    *" ${STARTUP_GATE_TAINT_EFFECT}="*) return 0 ;;
    *) return 1 ;;
  esac
}

startup_gate_has_maintenance_taint() {
  case " $(startup_gate_taint_tokens) " in
    *" ${STARTUP_GATE_TAINT_EFFECT}=maintenance "*) return 0 ;;
    *) return 1 ;;
  esac
}

ensure_startup_gate_taint() {
  startup_gate_active || return 0
  startup_gate_require_kubectl
  if ! startup_gate_has_taint; then
    log "ensuring node taint ${STARTUP_GATE_TAINT_KEY}:${STARTUP_GATE_TAINT_EFFECT}"
    kubectl taint node "$NODE_NAME" \
      "${STARTUP_GATE_TAINT_KEY}=true:${STARTUP_GATE_TAINT_EFFECT}" --overwrite >/dev/null \
      || fail "failed to ensure PVM startup-gate taint"
  fi
  startup_gate_has_taint || fail "PVM startup-gate taint verification failed"
}

clear_startup_gate_taint() {
  startup_gate_active || return 0
  startup_gate_require_kubectl
  pvm_host_fingerprint_matches_file \
    || fail "refusing to clear startup-gate taint: live PVM fingerprint mismatch"
  if startup_gate_has_taint; then
    if startup_gate_has_maintenance_taint \
      && [ "$STARTUP_GATE_CLEAR_MAINTENANCE" != "true" ]; then
      log "preserving operator maintenance startup-gate taint"
      return 0
    fi
    log "clearing node taint ${STARTUP_GATE_TAINT_KEY}"
    kubectl taint node "$NODE_NAME" "${STARTUP_GATE_TAINT_KEY}-" >/dev/null \
      || fail "failed to clear PVM startup-gate taint"
  fi
  startup_gate_has_taint && fail "PVM startup-gate taint is still present after clear"
  return 0
}

# One API list: "name<TAB>component" lines for Cube pods on this node.
startup_gate_list_dependents() {
  kubectl -n "$NAMESPACE" get pods \
    --field-selector "spec.nodeName=${NODE_NAME}" \
    -l "app.kubernetes.io/instance=${CUBE_RELEASE}" \
    -o "jsonpath={range .items[*]}{.metadata.name}{\"\t\"}{.metadata.labels.app\.kubernetes\.io/component}{\"\n\"}{end}"
}

startup_gate_count_drain_targets() {
  remaining=0
  while IFS="$(printf '\t')" read -r pod_name component || [ -n "${pod_name:-}" ]; do
    [ -n "${pod_name:-}" ] || continue
    [ "$component" = "cube-node-pvm" ] && continue
    remaining=$((remaining + 1))
  done <<EOF
$(startup_gate_list_dependents)
EOF
  printf '%s' "$remaining"
}

drain_startup_gate_dependents() {
  startup_gate_active || return 0
  startup_gate_require_kubectl
  [ -n "${CUBE_RELEASE:-}" ] || fail "CUBE_RELEASE is required to drain Cube dependents"

  while IFS="$(printf '\t')" read -r pod_name component || [ -n "${pod_name:-}" ]; do
    [ -n "${pod_name:-}" ] || continue
    [ "$component" = "cube-node-pvm" ] && continue
    log "evicting startup-gate dependent pod/${pod_name} (component=${component:-unknown})"
    cat <<EOF | kubectl create --raw \
      "/api/v1/namespaces/${NAMESPACE}/pods/${pod_name}/eviction" -f - >/dev/null \
      || fail "failed to evict dependent pod/${pod_name}; check PodDisruptionBudget"
{
  "apiVersion": "policy/v1",
  "kind": "Eviction",
  "metadata": {"name": "${pod_name}", "namespace": "${NAMESPACE}"}
}
EOF
  done <<EOF
$(startup_gate_list_dependents)
EOF

  deadline=$(( $(date +%s) + STARTUP_GATE_DRAIN_TIMEOUT_SECONDS ))
  while true; do
    remaining="$(startup_gate_count_drain_targets)"
    [ "$remaining" -eq 0 ] && return 0
    [ "$(date +%s)" -lt "$deadline" ] \
      || fail "timed out waiting for ${remaining} Cube dependent pod(s) to terminate"
    sleep 2
  done
}
