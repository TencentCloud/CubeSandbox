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

fail() {
  printf 'PVM preflight: %s\n' "$*" >&2
  exit 1
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

kubectl get --raw=/apis/apps.kruise.io/v1beta1 >/dev/null \
  || fail "OpenKruise Advanced DaemonSet API is unavailable"
kubectl get --raw=/apis/apps.kruise.io/v1alpha1 >/dev/null \
  || fail "OpenKruise CloneSet API is unavailable"

tolerates_gate() {
  awk -F'|' -v key="$TAINT_KEY" '
    ($2 == "Exists") && ($1 == "" || $1 == key) && ($3 == "" || $3 == "NoSchedule") {found=1}
    END {exit(found ? 0 : 1)}
  '
}

manager_ready="$(kubectl -n kruise-system get deployment kruise-controller-manager \
  -o jsonpath='{.status.readyReplicas}')"
[ "${manager_ready:-0}" -gt 0 ] \
  || fail "kruise-controller-manager has no ready replica"
# Manager Exists toleration is recommended for rebuild resilience (see QUICKSTART),
# but is not a preflight hard gate: manager typically runs on control-plane nodes
# that are not gated, and Ready already covers a hung control plane.

daemon_desired="$(kubectl -n kruise-system get daemonset kruise-daemon \
  -o jsonpath='{.status.desiredNumberScheduled}')"
daemon_ready="$(kubectl -n kruise-system get daemonset kruise-daemon \
  -o jsonpath='{.status.numberReady}')"
[ "${daemon_desired:-0}" -gt 0 ] && [ "$daemon_ready" -eq "$daemon_desired" ] \
  || fail "kruise-daemon is not fully ready"
kubectl -n kruise-system get daemonset kruise-daemon \
  -o jsonpath='{range .spec.template.spec.tolerations[*]}{.key}{"|"}{.operator}{"|"}{.effect}{"\n"}{end}' \
  | tolerates_gate \
  || fail "kruise-daemon does not tolerate the startup gate"

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
  hostPID: true
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

  deadline=$(( $(date +%s) + PREFLIGHT_TIMEOUT_SECONDS ))
  while true; do
    phase="$(kubectl -n "$POD_NAMESPACE" get pod "$pod_name" -o jsonpath='{.status.phase}')"
    [ "$phase" = "Succeeded" ] && return 0
    if [ "$phase" = "Failed" ] || [ "$(date +%s)" -ge "$deadline" ]; then
      kubectl -n "$POD_NAMESPACE" logs "$pod_name" >&2 || true
      return 1
    fi
    sleep 2
  done
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

alloc_check_pod_name() {
  index=$((index + 1))
  CHECK_POD_NAME="$(printf 'pvm-check-%s-%s' "$index" "$RELEASE_NAME" | cut -c1-63)"
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

index=0
for node_ref in $nodes; do
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
      alloc_check_pod_name
      probe_cni_under_gate "$node" "$CHECK_POD_NAME"
      continue
      ;;
  esac

  # No gate: try fingerprint-ready path first (daily upgrade / already prepared).
  alloc_check_pod_name
  create_check_pod "$CHECK_POD_NAME" "$node" "true"
  if wait_check_pod "$CHECK_POD_NAME"; then
    delete_check_pod "$CHECK_POD_NAME"
    printf 'PVM preflight: %s is already fingerprint-ready\n' "$node"
    continue
  fi
  delete_check_pod "$CHECK_POD_NAME"

  # Not fingerprint-ready: auto-ensure gate, then probe CNI under the taint.
  ensure_gate_taint_on_node "$node"
  alloc_check_pod_name
  probe_cni_under_gate "$node" "$CHECK_POD_NAME"
done
