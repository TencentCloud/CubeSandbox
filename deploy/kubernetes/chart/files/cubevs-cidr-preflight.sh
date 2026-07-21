#!/bin/bash
# Helm pre-install/pre-upgrade: fail if cubevs/sandbox CIDR overlaps cluster Service CIDR.
# Overlap blackholes ClusterDNS from cube-node (cube-dev route), so cubelet cannot
# resolve cube-master and node registration fails.
if [[ "${CUBEVS_CIDR_PREFLIGHT_SOURCE_ONLY:-0}" != "1" ]]; then
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

log() { printf 'cubevs-cidr preflight: %s\n' "$*"; }
fail() {
  printf 'cubevs-cidr preflight: ERROR: %s\n' "$*" >&2
  exit 1
}

PACKAGED_CUBELET_CIDR="${PACKAGED_CUBELET_CIDR:-192.168.0.0/18}"
SKIP_CONFLICT_CHECK="${CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK:-0}"

ip_to_int() {
  local ip="$1"
  local a b c d
  IFS=. read -r a b c d <<< "${ip}"
  # Reject truncated/extra-field inputs (e.g. "1.2.3") that would otherwise
  # treat empty octets as 0 in arithmetic.
  [ -n "$a" ] && [ -n "$b" ] && [ -n "$c" ] && [ -n "$d" ] || return 1
  case "${a}${b}${c}${d}" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$a" -le 255 ] && [ "$b" -le 255 ] && [ "$c" -le 255 ] && [ "$d" -le 255 ] || return 1
  echo $((a * 16777216 + b * 65536 + c * 256 + d))
}

int_to_ip() {
  local n="$1"
  echo "$((n >> 24 & 255)).$((n >> 16 & 255)).$((n >> 8 & 255)).$((n & 255))"
}

cidr_range() {
  local cidr="$1"
  local ip="${cidr%/*}"
  local mask="${cidr#*/}"
  [ "$ip" != "$cidr" ] || return 1
  case "$mask" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ "$mask" -ge 0 ] && [ "$mask" -le 32 ] || return 1
  local ip_int host_bits block_size start end
  ip_int="$(ip_to_int "$ip")" || return 1
  host_bits=$((32 - mask))
  if [ "$host_bits" -eq 32 ]; then
    block_size=4294967296
  else
    block_size=$((1 << host_bits))
  fi
  start=$((ip_int / block_size * block_size))
  end=$((start + block_size - 1))
  printf '%s %s\n' "$start" "$end"
}

cidr_overlaps() {
  local a="$1" b="$2"
  local a_range b_range a_start a_end b_start b_end
  a_range="$(cidr_range "$a")" || return 1
  b_range="$(cidr_range "$b")" || return 1
  # shellcheck disable=SC2086
  set -- $a_range $b_range
  a_start="$1"
  a_end="$2"
  b_start="$3"
  b_end="$4"
  [ "$a_start" -le "$b_end" ] && [ "$b_start" -le "$a_end" ]
}

ip_in_cidr() {
  local ip="$1" cidr="$2"
  cidr_overlaps "${ip}/32" "$cidr"
}

validate_cidr() {
  local cidr="$1"
  local ip="${cidr%/*}"
  local mask="${cidr#*/}"
  [ "$ip" != "$cidr" ] || fail "cubeNode.network.cidr must be IPv4 CIDR, got ${cidr}"
  case "$mask" in
    ''|*[!0-9]*) fail "invalid CIDR mask: ${cidr}" ;;
  esac
  [ "$mask" -ge 8 ] && [ "$mask" -le 30 ] || fail "CIDR mask must be between 8 and 30: ${cidr}"
  local ip_int host_bits block_size aligned
  ip_int="$(ip_to_int "$ip")" || fail "invalid CIDR IP: ${cidr}"
  host_bits=$((32 - mask))
  block_size=$((1 << host_bits))
  aligned=$((ip_int / block_size * block_size))
  if [ "$ip_int" -ne "$aligned" ]; then
    fail "CIDR is not aligned to network address: ${cidr}; did you mean $(int_to_ip "$aligned")/${mask}"
  fi
}

effective_sandbox_cidr() {
  local cidr="${CUBE_SANDBOX_NETWORK_CIDR:-}"
  if [ -n "$cidr" ]; then
    printf '%s\n' "$cidr"
    return 0
  fi
  log "cubeNode.network.cidr is empty; using packaged Cubelet CIDR ${PACKAGED_CUBELET_CIDR}"
  printf '%s\n' "$PACKAGED_CUBELET_CIDR"
}

# Print discovered Service CIDR candidates (one per line). May be empty.
discover_service_cidrs() {
  local found="" subnet line cmd

  # kubeadm ClusterConfiguration
  if subnet="$(kubectl "${kubectl_opts[@]}" -n kube-system get configmap kubeadm-config \
    -o jsonpath='{.data.ClusterConfiguration}' 2>/dev/null || true)" \
    && [ -n "$subnet" ]; then
    subnet="$(printf '%s\n' "$subnet" | awk '
      $1 == "serviceSubnet:" { print $2; exit }
      $1 == "serviceSubnet:" || $1 ~ /^serviceSubnet:/ {
        sub(/^serviceSubnet:[[:space:]]*/, "", $0); print $0; exit
      }
    ')"
    if [ -n "$subnet" ]; then
      printf '%s\n' "$subnet"
      found=1
    fi
  fi

  # kube-apiserver --service-cluster-ip-range (static pods / control-plane pods)
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    case "$line" in
      --service-cluster-ip-range=*)
        subnet="${line#--service-cluster-ip-range=}"
        # May be comma-separated dual-stack; keep IPv4-looking entries.
        IFS=',' read -r -a parts <<< "$subnet"
        for subnet in "${parts[@]}"; do
          case "$subnet" in
            *:* ) continue ;;
            */*)
              printf '%s\n' "$subnet"
              found=1
              ;;
          esac
        done
        ;;
    esac
  done < <(
    kubectl "${kubectl_opts[@]}" -n kube-system get pods \
      -l component=kube-apiserver \
      -o jsonpath='{range .items[*].spec.containers[*].command[*]}{.}{"\n"}{end}{range .items[*].spec.containers[*].args[*]}{.}{"\n"}{end}' \
      2>/dev/null || true
  )

  # Some managed clusters expose the range on kube-proxy ConfigMap as a comment
  # or field; prefer explicit keys when present.
  if cmd="$(kubectl "${kubectl_opts[@]}" -n kube-system get configmap kube-proxy \
    -o jsonpath='{.data.config\.conf}' 2>/dev/null || true)" \
    && [ -n "$cmd" ]; then
    subnet="$(printf '%s\n' "$cmd" | awk '
      $1 == "serviceClusterIPRange:" || $1 ~ /^serviceClusterIPRange:/ {
        sub(/^serviceClusterIPRange:[[:space:]]*/, "", $0); print $0; exit
      }
    ')"
    if [ -n "$subnet" ]; then
      case "$subnet" in
        *:* ) ;;
        */*)
          printf '%s\n' "$subnet"
          found=1
          ;;
      esac
    fi
  fi

  [ -n "$found" ] || true
}

list_cluster_ips() {
  kubectl "${kubectl_opts[@]}" get svc -A \
    -o jsonpath='{range .items[?(@.spec.clusterIP!="None")]}{.metadata.namespace}{"/"}{.metadata.name}{" "}{.spec.clusterIP}{"\n"}{end}' \
    2>/dev/null || true
}

main() {
  if [ "$SKIP_CONFLICT_CHECK" = "1" ] || [ "$SKIP_CONFLICT_CHECK" = "true" ]; then
    log "skipped (cubeNode.network.cidrSkipConflictCheck=true)"
    exit 0
  fi

  local sandbox svc_cidr hit ns_name ip discovered=0
  sandbox="$(effective_sandbox_cidr)"
  validate_cidr "$sandbox"
  log "checking cubevs/sandbox CIDR ${sandbox} against cluster Service CIDR"

  while IFS= read -r svc_cidr; do
    [ -n "$svc_cidr" ] || continue
    discovered=1
    log "discovered Service CIDR candidate: ${svc_cidr}"
    if cidr_overlaps "$sandbox" "$svc_cidr"; then
      fail "cubevs/sandbox CIDR ${sandbox} overlaps cluster Service CIDR ${svc_cidr}.
This routes ClusterDNS (and other ClusterIPs) into cube-dev and blackholes in-cluster DNS,
so cubelet cannot resolve cube-master and node registration fails.
Set cubeNode.network.cidr to a non-overlapping private range (chart default: 172.16.0.0/18),
or set cubeNode.network.cidrSkipConflictCheck=true only if you accept the risk.

cubevs/sandbox CIDR ${sandbox} 与集群 Service CIDR ${svc_cidr} 重叠。
重叠会把 ClusterDNS 等 ClusterIP 黑洞进 cube-dev，导致 cubelet 无法解析 cube-master、节点注册失败。
请将 cubeNode.network.cidr 改为不冲突的私网段（Chart 默认 172.16.0.0/18）；
仅在明确接受风险时设置 cubeNode.network.cidrSkipConflictCheck=true。"
    fi
  done < <(discover_service_cidrs | awk 'NF && !seen[$0]++')

  hit=""
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    ns_name="${line%% *}"
    ip="${line##* }"
    case "$ip" in
      ''|None|null) continue ;;
      *:* ) continue ;; # skip IPv6
    esac
    if ip_in_cidr "$ip" "$sandbox"; then
      hit="${hit}
- Service ${ns_name} ClusterIP ${ip}"
    fi
  done < <(list_cluster_ips)

  if [ -n "$hit" ]; then
    fail "cubevs/sandbox CIDR ${sandbox} contains existing Service ClusterIP(s):${hit}
This usually means the sandbox CIDR overlaps the cluster Service CIDR (often 192.168.0.0/16 on TKE/single-node).
Set cubeNode.network.cidr to a non-overlapping private range (chart default: 172.16.0.0/18).

cubevs/sandbox CIDR ${sandbox} 覆盖了已有 Service ClusterIP（见上）。
通常表示沙箱网段与集群 Service CIDR 重叠（单节点/TKE 常见 192.168.0.0/16）。
请将 cubeNode.network.cidr 改为不冲突的私网段（Chart 默认 172.16.0.0/18）。"
  fi

  if [ "$discovered" -eq 0 ]; then
    log "could not read Service CIDR from kubeadm/apiserver; ClusterIP sampling found no overlap with ${sandbox}"
  else
    log "no Service CIDR overlap with ${sandbox}"
  fi
  log "ok"
}

# Allow sourcing for unit tests.
if [[ "${CUBEVS_CIDR_PREFLIGHT_SOURCE_ONLY:-0}" != "1" ]]; then
  main "$@"
fi
