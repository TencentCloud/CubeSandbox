#!/bin/bash
# Helm pre-install/pre-upgrade: refuse to change cube-node's network mode on an
# existing release without an explicit acknowledgement. The mode is a Pod-template
# field, so the change recreates every Big Pod and strands the sandbox network
# devices in the old netns, in either direction.
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
    log "no DaemonSet ${ns}/${ds} yet (fresh install); hostNetwork=${desired}"
    return 0
  fi
  rm -f "${err}"

  decision="$(hostnet_change_decision "${live}" "${desired}" "${ack}")"
  case "${decision}" in
    keep)
      log "hostNetwork unchanged ($(normalize_bool "${live}")); nothing to do"
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
