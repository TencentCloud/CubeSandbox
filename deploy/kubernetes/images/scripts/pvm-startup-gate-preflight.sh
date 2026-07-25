#!/bin/sh
# Helm pre-install/pre-upgrade PVM startup-gate checks.
# Parameters come from the Job env (see pvm-startup-gate-preflight.yaml).
#
# For placement.pvm nodes that are not fingerprint-ready and lack the gate
# taint, this Hook ensures cube.tencent.com/pvm-not-ready=true:NoSchedule
# before probing CNI — operators need not pre-taint manually.
set -eu

: "${POD_NAMESPACE:?POD_NAMESPACE is required}"
: "${NODE_SELECTOR:?NODE_SELECTOR is required}"
: "${TAINT_KEY:?TAINT_KEY is required}"
: "${TAINT_EFFECT:?TAINT_EFFECT is required}"
: "${PREFLIGHT_TIMEOUT_SECONDS:?PREFLIGHT_TIMEOUT_SECONDS is required}"
: "${RELEASE_NAME:?RELEASE_NAME is required}"
: "${IS_UPGRADE:?IS_UPGRADE is required}"
: "${CHECK_IMAGE:?CHECK_IMAGE is required}"
: "${CHECK_IMAGE_PULL_POLICY:?CHECK_IMAGE_PULL_POLICY is required}"
: "${PREFLIGHT_SERVICE_ACCOUNT:?PREFLIGHT_SERVICE_ACCOUNT is required}"
: "${STATE_DIR:?STATE_DIR is required}"
: "${DESIRED_KERNEL_PATTERN:?DESIRED_KERNEL_PATTERN is required}"
# KERNEL_BOOT_ARGS and IMAGE_PULL_SECRET_NAMES may be empty.
KERNEL_BOOT_ARGS="${KERNEL_BOOT_ARGS:-}"
IMAGE_PULL_SECRET_NAMES="${IMAGE_PULL_SECRET_NAMES:-}"
PREFLIGHT_NODE_CONCURRENCY="${PREFLIGHT_NODE_CONCURRENCY:-8}"

fail() {
  printf 'PVM preflight: %s\n' "$*" >&2
  exit 1
}

# Per-pod wait budget: avoid one slow pod consuming the full Job timeout.
per_pod_wait_seconds() {
  if [ "$PREFLIGHT_TIMEOUT_SECONDS" -gt 120 ]; then
    echo 120
  else
    echo "$PREFLIGHT_TIMEOUT_SECONDS"
  fi
}

cleanup_stale_check_pods() {
  kubectl -n "$POD_NAMESPACE" delete pod \
    -l "cube.tencent.com/pvm-preflight=${RELEASE_NAME}" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

cleanup_stale_check_pods
trap cleanup_stale_check_pods EXIT

cleanup_deadline=$(( $(date +%s) + 60 ))
while [ -n "$(kubectl -n "$POD_NAMESPACE" get pods \
  -l "cube.tencent.com/pvm-preflight=${RELEASE_NAME}" -o name)" ]; do
  [ "$(date +%s)" -lt "$cleanup_deadline" ] \
    || fail "timed out cleaning stale check pods"
  sleep 2
done

tolerates_gate() {
  awk -F'|' -v key="$TAINT_KEY" '
    ($2 == "Exists") && ($1 == "" || $1 == key) && ($3 == "" || $3 == "NoSchedule") {found=1}
    END {exit(found ? 0 : 1)}
  '
}

nodes="$(kubectl get nodes -l "$NODE_SELECTOR" -o name)"
[ -n "$nodes" ] || fail "no nodes match placement.pvm (${NODE_SELECTOR})"

render_image_pull_secrets() {
  [ -n "$IMAGE_PULL_SECRET_NAMES" ] || return 0
  printf '  imagePullSecrets:\n'
  old_ifs=$IFS
  IFS=,
  # shellcheck disable=SC2086
  set -- $IMAGE_PULL_SECRET_NAMES
  IFS=$old_ifs
  for secret in "$@"; do
    [ -n "$secret" ] || continue
    printf '    - name: %s\n' "$secret"
  done
}

# Stable, collision-safe check pod name for parallel batches (no shared counter).
check_pod_name_for() {
  node_name=$1
  suffix=$2
  safe="$(printf '%s' "$node_name" | tr '.:/_' '-' | cut -c1-32)"
  printf 'pvm-check-%s-%s-%s' "$safe" "$suffix" "$RELEASE_NAME" | cut -c1-63
}

create_check_pod() {
  pod_name=$1
  node_name=$2
  require_fingerprint=$3

  {
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  labels:
    cube.tencent.com/pvm-preflight: "${RELEASE_NAME}"
spec:
  restartPolicy: Never
  nodeName: ${node_name}
  serviceAccountName: ${PREFLIGHT_SERVICE_ACCOUNT}
  automountServiceAccountToken: true
EOF
    render_image_pull_secrets
    cat <<EOF
  containers:
    - name: check
      image: ${CHECK_IMAGE}
      imagePullPolicy: ${CHECK_IMAGE_PULL_POLICY}
      command: ["/bin/sh", "-ec"]
      args:
        - |
          kubectl get --raw=/readyz >/dev/null
          [ "\${REQUIRE_FINGERPRINT}" = "true" ] || exit 0
          . /scripts/node-prep-lib.sh
          pvm_host_fingerprint_matches_file
      env:
        - name: REQUIRE_FINGERPRINT
          value: "${require_fingerprint}"
        - name: HOST_ROOT
          value: /host
        - name: STATE_DIR
          value: "${STATE_DIR}"
        - name: PVM_ENABLED
          value: "1"
        - name: DESIRED_KERNEL_PATTERN
          value: "${DESIRED_KERNEL_PATTERN}"
        - name: KERNEL_BOOT_ARGS
          value: "${KERNEL_BOOT_ARGS}"
      volumeMounts:
        - name: host-root
          mountPath: /host
          readOnly: true
  volumes:
    - name: host-root
      hostPath:
        path: /
        type: Directory
EOF
  } | kubectl -n "$POD_NAMESPACE" create -f -
}

# Returns 0 on Succeeded, 1 on Failed/timeout (does not exit the script).
wait_check_pod() {
  pod_name=$1
  wait_secs="$(per_pod_wait_seconds)"

  if kubectl -n "$POD_NAMESPACE" wait \
    --for=jsonpath='{.status.phase}'=Succeeded \
    "pod/${pod_name}" \
    --timeout="${wait_secs}s"; then
    return 0
  fi
  kubectl -n "$POD_NAMESPACE" logs "$pod_name" >&2 || true
  phase="$(kubectl -n "$POD_NAMESPACE" get pod "$pod_name" \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  printf 'PVM preflight: check pod %s ended phase=%s\n' "$pod_name" "${phase:-unknown}" >&2
  return 1
}

node_taint_effects() {
  kubectl get "$1" \
    -o "jsonpath={range .spec.taints[?(@.key==\"${TAINT_KEY}\")]}{.effect}{\" \"}{end}"
}

node_taint_values() {
  kubectl get "$1" \
    -o "jsonpath={range .spec.taints[?(@.key==\"${TAINT_KEY}\")]}{.value}{\" \"}{end}"
}

ensure_gate_taint_on_node() {
  node_name=$1
  printf 'PVM preflight: ensuring %s=%s:%s on %s\n' \
    "$TAINT_KEY" "true" "$TAINT_EFFECT" "$node_name"
  kubectl taint node "$node_name" \
    "${TAINT_KEY}=true:${TAINT_EFFECT}" --overwrite >/dev/null \
    || fail "failed to ensure startup-gate taint on ${node_name}"
  effects="$(node_taint_effects "node/${node_name}")"
  case " ${effects} " in
    *" ${TAINT_EFFECT} "*) ;;
    *) fail "startup-gate taint verification failed on ${node_name}" ;;
  esac
}

delete_check_pod() {
  kubectl -n "$POD_NAMESPACE" delete pod "$1" --wait=false >/dev/null 2>&1 || true
}

probe_cni_under_gate() {
  node_name=$1
  pod_name=$2
  printf 'PVM preflight: %s is gated; probing CNI/apiserver path\n' "$node_name"
  create_check_pod "$pod_name" "$node_name" "false"
  if ! wait_check_pod "$pod_name"; then
    fail "${node_name} cannot reach the apiserver through CNI while gated"
  fi
  final_effects="$(node_taint_effects "node/${node_name}")"
  case " ${final_effects} " in
    *" ${TAINT_EFFECT} "*) ;;
    *) fail "${node_name} lost the startup gate during preflight" ;;
  esac
  delete_check_pod "$pod_name"
  printf 'PVM preflight: %s CNI/apiserver path is ready under the gate taint\n' "$node_name"
}

# Per-node preflight (safe to run in parallel across nodes).
check_one_node() {
  node_ref=$1
  node="${node_ref#node/}"
  effects="$(node_taint_effects "$node_ref")"

  case " ${effects} " in
    *" ${TAINT_EFFECT} "*)
      if [ "$IS_UPGRADE" = "true" ]; then
        values="$(node_taint_values "$node_ref")"
        case " ${values} " in
          *" maintenance "*) ;;
          *) fail "upgrade gate on ${node} must use value=maintenance" ;;
        esac
      fi
      probe_cni_under_gate "$node" "$(check_pod_name_for "$node" cni)"
      return 0
      ;;
  esac

  # No gate: try fingerprint-ready path first (daily upgrade / already prepared).
  fp_pod="$(check_pod_name_for "$node" fp)"
  create_check_pod "$fp_pod" "$node" "true"
  if wait_check_pod "$fp_pod"; then
    delete_check_pod "$fp_pod"
    printf 'PVM preflight: %s is already fingerprint-ready\n' "$node"
    return 0
  fi
  delete_check_pod "$fp_pod"

  # Not fingerprint-ready: auto-ensure gate, then probe CNI under the taint.
  ensure_gate_taint_on_node "$node"
  probe_cni_under_gate "$node" "$(check_pod_name_for "$node" cni)"
}

# Batch nodes with concurrency cap (no wait -n; portable /bin/sh).
run_node_batches() {
  concurrency="$PREFLIGHT_NODE_CONCURRENCY"
  [ "$concurrency" -ge 1 ] || concurrency=1

  batch_pids=""
  batch_count=0
  failed=0

  for node_ref in $nodes; do
    check_one_node "$node_ref" &
    batch_pids="$batch_pids $!"
    batch_count=$((batch_count + 1))
    if [ "$batch_count" -ge "$concurrency" ]; then
      for pid in $batch_pids; do
        wait "$pid" || failed=1
      done
      batch_pids=""
      batch_count=0
      [ "$failed" -eq 0 ] || fail "one or more PVM nodes failed preflight"
    fi
  done

  for pid in $batch_pids; do
    wait "$pid" || failed=1
  done
  [ "$failed" -eq 0 ] || fail "one or more PVM nodes failed preflight"
}

run_node_batches
