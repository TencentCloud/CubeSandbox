#!/bin/bash
# Helm pre-install/pre-upgrade/pre-rollback: refuse to change cube-node's
# network mode on an existing release without an explicit acknowledgement. The
# mode is a Pod-template field, so the change recreates every Big Pod and
# strands the sandbox network devices in the old netns, in either direction.
# pre-rollback runs from the *target* revision's stored manifest: it gates
# helm rollback / --atomic only when that revision already shipped this Hook.
if [[ "${CUBE_NODE_HOSTNET_PREFLIGHT_SOURCE_ONLY:-0}" != "1" ]]; then
  set -euo pipefail
fi

# alpine/k8s kubectl does not auto-load in-cluster config.
if [[ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]]; then
  kubectl() {
    command kubectl \
      --server="https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT}" \
      --token="$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
      --certificate-authority=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt \
      "$@"
  }
fi

kubectl_opts=(--request-timeout=30s)

log() { printf 'cube-node-hostnet preflight: %s\n' "$*"; }
fail() {
  printf 'cube-node-hostnet preflight: ERROR: %s\n' "$*" >&2
  exit 1
}

# Kubernetes omits hostNetwork when it is false, so anything that is not "true"
# means the Pod network.
normalize_bool() {
  case "${1:-}" in
    true|True|TRUE) printf 'true\n' ;;
    *) printf 'false\n' ;;
  esac
}

# hostnet_change_decision <live|absent> <desired> <ack>
#   keep    - fresh install, or the live value already matches the desired one
#   ack-ok  - the mode changes and the operator acknowledged the disruption
#   block   - the mode changes without an acknowledgement
hostnet_change_decision() {
  local live="$1" desired ack

  desired="$(normalize_bool "${2:-}")"
  ack="$(normalize_bool "${3:-}")"

  if [[ "${live}" == "absent" ]]; then
    printf 'keep\n'
    return 0
  fi
  if [[ "$(normalize_bool "${live}")" == "${desired}" ]]; then
    printf 'keep\n'
    return 0
  fi
  if [[ "${ack}" == "true" ]]; then
    printf 'ack-ok\n'
    return 0
  fi
  printf 'block\n'
}

block_message() {
  local live="$1" desired="$2" ns="$3" ds="$4"
  cat <<EOF
cube-node hostNetwork is changing on an existing release: ${live} -> ${desired} (DaemonSet ${ns}/${ds}).
This recreates every compute node's Big Pod and moves the sandbox dataplane
between network namespaces, so every sandbox on those nodes loses its
networking -- inbound and outbound -- until it is recreated.
Either:
  1. keep the current mode: set cubeNode.hostNetwork: ${live} in your values; or
  2. adopt the new mode: isolate each compute node, wait >= 60s and destroy its
     sandboxes (docs/guide/node-operations.md), then set
     cubeNode.hostNetworkChangeAck: true and re-run this upgrade. Remove the ack
     once the upgrade has gone through.
EOF
}

# juicefs_change_decision <live|absent> <live_hash> <desired> <desired_hash> <ack> <fresh_install>
#   keep    - a fresh installation (nothing to disrupt), no lifecycle change on
#             an existing release, or an absent DaemonSet staying absent
#   ack-ok  - the lifecycle changes and the operator acknowledged the disruption
#   block   - the lifecycle changes without an acknowledgement
juicefs_change_decision() {
  local live_state="$1" live_hash="$2" desired ack desired_hash fresh_install
  desired="$(normalize_bool "${3:-}")"
  desired_hash="$4"
  ack="$(normalize_bool "${5:-}")"
  fresh_install="$(normalize_bool "${6:-false}")"

  if [[ "${live_state}" == "absent" ]]; then
    # Creating the DaemonSet. A fresh installation has no running sandbox to
    # disrupt, but enabling on an existing release recreates every cube-node
    # Pod, so it keeps requiring the acknowledgement there.
    if [[ "${desired}" == "false" || "${fresh_install}" == "true" ]]; then
      printf 'keep\n'
      return 0
    fi
  fi
  if [[ "${live_state}" == "present" && "${desired}" == "true" && -n "${live_hash}" && "${live_hash}" == "${desired_hash}" ]]; then
    printf 'keep\n'
    return 0
  fi
  if [[ "${ack}" == "true" ]]; then
    printf 'ack-ok\n'
    return 0
  fi
  printf 'block\n'
}

check_juicefs_existing_release() {
  local ns="$1" fresh_install="$2"
  local ds desired desired_hash ack live_hash err decision state
  ds="${CUBE_JUICEFS_DS_NAME:-}"
  desired="${CUBE_JUICEFS_DESIRED_ENABLED:-false}"
  desired_hash="${CUBE_JUICEFS_DESIRED_HASH:-}"
  ack="${CUBE_JUICEFS_LIFECYCLE_CHANGE_ACK:-false}"
  [[ -n "${ds}" ]] || fail "CUBE_JUICEFS_DS_NAME is required"
  [[ -n "${desired_hash}" ]] || fail "CUBE_JUICEFS_DESIRED_HASH is required"

  err="$(mktemp)"
  if live_hash="$(kubectl "${kubectl_opts[@]}" -n "${ns}" get daemonset "${ds}" \
    -o jsonpath='{.metadata.annotations.cube\.tencent\.com/juicefs-config-hash}' 2>"${err}")"; then
    state=present
  elif grep -q "NotFound" "${err}"; then
    state=absent
    live_hash=""
  else
    cat "${err}" >&2
    rm -f "${err}"
    fail "could not read JuiceFS DaemonSet ${ns}/${ds}"
  fi
  rm -f "${err}"

  decision="$(juicefs_change_decision "${state}" "${live_hash}" "${desired}" "${desired_hash}" "${ack}" "${fresh_install}")"
  case "${decision}" in
    keep)
      if [[ "$(normalize_bool "${ack}")" == "true" ]]; then
        log "WARNING: cubeJuiceFS.lifecycleChangeAck is set while JuiceFS lifecycle is unchanged; remove it"
      fi
      ;;
    ack-ok)
      log "WARNING: applying acknowledged JuiceFS lifecycle change; every compute node must already be drained"
      ;;
    block)
      fail "JuiceFS lifecycle changes on an existing release require draining every compute node first. Keep the current cubeJuiceFS settings, or set cubeJuiceFS.lifecycleChangeAck: true after the drain and remove it after the upgrade."
      ;;
  esac
}

main() {
  local ns ds desired ack live err decision

  ns="${RELEASE_NAMESPACE:-}"
  ds="${CUBE_NODE_DS_NAME:-}"
  desired="${CUBE_NODE_HOSTNETWORK_DESIRED:-true}"
  ack="${CUBE_NODE_HOSTNETWORK_CHANGE_ACK:-0}"

  [[ -n "${ns}" ]] || fail "RELEASE_NAMESPACE is required"
  [[ -n "${ds}" ]] || fail "CUBE_NODE_DS_NAME is required"

  err="$(mktemp)"
  if ! live="$(kubectl "${kubectl_opts[@]}" -n "${ns}" get daemonset "${ds}" \
    -o jsonpath='{.spec.template.spec.hostNetwork}' 2>"${err}")"; then
    # NotFound is the install case; any other error must not read as "safe to
    # flip", so it fails the release instead.
    if ! grep -q "NotFound" "${err}"; then
      cat "${err}" >&2
      rm -f "${err}"
      fail "could not read DaemonSet ${ns}/${ds}"
    fi
    rm -f "${err}"
    # A missing cube-node DaemonSet usually means a fresh install, but it can
    # also be a manually deleted or half-installed release. Guard only what
    # exists: a leftover JuiceFS mount DaemonSet still requires the ack, while
    # creating one from nothing disrupts nothing.
    if [[ -n "${CUBE_JUICEFS_DS_NAME:-}" ]]; then
      check_juicefs_existing_release "${ns}" true
    fi
    log "no DaemonSet ${ns}/${ds} yet (fresh install); hostNetwork=${desired}"
    return 0
  fi
  rm -f "${err}"

  # On an existing release, creating the JuiceFS DaemonSet (first enable)
  # recreates every cube-node Pod, so it needs the acknowledgement too.
  if [[ -n "${CUBE_JUICEFS_DS_NAME:-}" ]]; then
    check_juicefs_existing_release "${ns}" false
  fi

  decision="$(hostnet_change_decision "${live}" "${desired}" "${ack}")"
  case "${decision}" in
    keep)
      log "hostNetwork unchanged ($(normalize_bool "${live}")); nothing to do"
      if [[ "$(normalize_bool "${ack}")" == "true" ]]; then
        log "WARNING: cubeNode.hostNetworkChangeAck is set while the mode is unchanged; remove it so a future mode change is gated again"
      fi
      ;;
    ack-ok)
      log "WARNING: hostNetwork changes to ${desired} with cubeNode.hostNetworkChangeAck=true; every Big Pod is recreated and the sandboxes still running on those nodes lose their networking"
      ;;
    block)
      fail "$(block_message "$(normalize_bool "${live}")" "$(normalize_bool "${desired}")" "${ns}" "${ds}")"
      ;;
    *)
      fail "internal error: unexpected decision '${decision}'"
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
