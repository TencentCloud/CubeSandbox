#!/usr/bin/env bash
# Guard: the cubevs/sandbox CIDR a provider preset pins must not overlap any
# range this repo itself provisions, documents, or that a stock cluster ships
# with.
#
# Regression for #1555: values-tke.yaml pinned 192.168.0.0/18, which fully
# contains the Service CIDR this repo's own one-click Terraform provisions by
# default (192.168.0.0/20). The preset moved the collision instead of removing
# it, and the "packaged Cubelet fallback" justification pointed at the same
# colliding range (Cubelet/config/config.toml).
#
# #1555's framing implied the one-click Terraform path runs the Helm chart,
# but it does not — that path deploys the control plane through kubernetes_*
# resources and runs Cubelet on the compute CVMs via systemd
# (deploy/one-click/terraform/tencentcloud/create.sh). The chart is used on an
# operator-provided cluster (docs/guide/kubernetes/install.md), which may or
# may not be the cluster this repo's Terraform created. So this is a policy
# guard rather than a reproduced regression: a chart preset should not pick a
# cubevs range colliding with the network layout this repo provisions, because
# that layout is what operators following the docs end up with.
#
# The kubeadm/Flannel default Pod CIDR is checked too, and it is the important
# one: it is not in this repo, and the cubevs-cidr-preflight Hook cannot see a
# Pod/CNI collision (it inspects only the Service CIDR and existing
# ClusterIPs), so a preset embedded in it fails silently instead of loudly.
#
# bash, not sh: this sources cubevs-cidr-preflight.sh, which uses [[ ]].
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(CDPATH= cd -- "$CHART_DIR/../../.." && pwd)"
PREFLIGHT="$CHART_DIR/files/cubevs-cidr-preflight.sh"

# Source the chart's own CIDR helpers (cidr_overlaps / cidr_range). Two
# caveats, both learned the hard way: the preflight defines its own fail() and
# validate_cidr(), so ours is named guard_fail to survive the source, and
# validate_cidr() cannot be used for validation here because it calls fail()
# and exits the shell instead of returning non-zero — use is_valid_cidr.
guard_fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

CUBEVS_CIDR_PREFLIGHT_SOURCE_ONLY=1
# shellcheck disable=SC1090
. "$PREFLIGHT"

# A CIDR is valid when cidr_range can parse it into a start/end pair.
is_valid_cidr() {
  cidr_range "$1" >/dev/null 2>&1
}

# Pull the cubevs CIDR a values file pins (top-level cubeNode.network.cidr),
# without depending on a YAML parser — the chart values stay flat here.
# Reset both flags on a new top-level key: leaving in_net set past the
# cubeNode: block would let a later section's "  cidr:" match.
pinned_cidr() {
  awk '
    /^cubeNode:/ { in_node = 1; in_net = 0; next }
    in_node && /^[^ ]/ { in_node = 0; in_net = 0 }
    in_node && /^  network:/ { in_net = 1; next }
    in_net && /^  [^ ]/ { in_net = 0 }
    in_net && /^    cidr:/ { gsub(/"/, "", $2); print $2; exit }
  ' "$1"
}

# Same walk for cubeEgress.network.tproxyOnIP.
pinned_tproxy() {
  awk '
    /^cubeEgress:/ { in_egress = 1; in_net = 0; next }
    in_egress && /^[^ ]/ { in_egress = 0; in_net = 0 }
    in_egress && /^  network:/ { in_net = 1; next }
    in_net && /^  [^ ]/ { in_net = 0 }
    in_net && /^    tproxyOnIP:/ { gsub(/"/, "", $2); print $2; exit }
  ' "$1"
}

# Derive tproxyOnIP from a CIDR: mask down to the network address, then add 1
# (mirrors sandbox_gateway_ip_from_cidr in CubeEgress/scripts/
# cube-proxy-iptables-init.sh). The mask matters — adding 1 to the literal
# address would accept an unaligned CIDR whose gateway differs from what the
# dataplane computes.
gateway_from_cidr() {
  awk -F'[./]' '{
    ip = $1 * 16777216 + $2 * 65536 + $3 * 256 + $4
    host_bits = 32 - $5
    block = (host_bits == 32) ? 4294967296 : 2 ^ host_bits
    gw = int(ip / block) * block + 1
    printf "%d.%d.%d.%d\n", int(gw / 16777216) % 256, int(gw / 65536) % 256, int(gw / 256) % 256, gw % 256
  }' <<CIDR_EOF
$1
CIDR_EOF
}

TF_VARS="$REPO_ROOT/deploy/one-click/terraform/tencentcloud/variables.tf"
TF_MAIN="$REPO_ROOT/deploy/one-click/terraform/tencentcloud/main.tf"
[ -f "$TF_VARS" ] || guard_fail "missing $TF_VARS — the ranges this repo provisions for TKE"
[ -f "$TF_MAIN" ] || guard_fail "missing $TF_MAIN — the VPC cidr_block"

# Ranges this repo's Terraform provisions, read from the same defaults the
# deployer uses so a bumped default keeps this guard honest. Anchor on the
# assignment (`default =`) rather than any line containing "default" — a
# description that happens to use the word would otherwise be read as the
# value.
tke_service_cidr="$(sed -n '/variable "tke_service_cidr"/,/^}/p' "$TF_VARS" \
  | awk '/^[[:space:]]*default[[:space:]]*=/ { gsub(/"/, "", $3); print $3; exit }')"
tke_pod_cidr="$(sed -n '/variable "tke_cluster_cidr"/,/^}/p' "$TF_VARS" \
  | awk '/^[[:space:]]*default[[:space:]]*=/ { gsub(/"/, "", $3); print $3; exit }')"
vpc_cidr="$(sed -n '/resource "tencentcloud_vpc" "cluster"/,/^}/p' "$TF_MAIN" \
  | awk '/^[[:space:]]*cidr_block[[:space:]]*=/ { gsub(/"/, "", $3); print $3; exit }')"

[ -n "$tke_service_cidr" ] || guard_fail "could not read tke_service_cidr default from $TF_VARS"
[ -n "$tke_pod_cidr" ] || guard_fail "could not read tke_cluster_cidr default from $TF_VARS"
[ -n "$vpc_cidr" ] || guard_fail "could not read the VPC cidr_block from $TF_MAIN"

# Container-network CIDR of a stock kubeadm/Flannel cluster. Not in this repo,
# and invisible to the preflight — see the header.
KUBEADM_POD_CIDR="10.244.0.0/16"

known_tke_ranges="$tke_service_cidr
$tke_pod_cidr
$vpc_cidr
$KUBEADM_POD_CIDR"

presets="$(ls "$CHART_DIR"/values-*.yaml 2>/dev/null || true)"
[ -n "$presets" ] || guard_fail "no values-*.yaml presets found in $CHART_DIR"

checked=0
for preset in $presets; do
  # A preset carrying a cubeNode.network: block must pin a cidr under it;
  # silently skipping it is how an override gets dropped unnoticed.
  if grep -qE '^cubeNode:[[:space:]]*$' "$preset" \
    && grep -qE '^  network:[[:space:]]*$' "$preset" \
    && [ -z "$(pinned_cidr "$preset")" ]; then
    guard_fail "${preset##*/}: has a cubeNode.network: block but no cidr key — did the override get dropped?"
  fi

  cidr="$(pinned_cidr "$preset")"
  [ -n "$cidr" ] || continue # preset does not pin a cubevs CIDR
  checked=$((checked + 1))
  printf '%s pins cubeNode.network.cidr=%s\n' "${preset##*/}" "$cidr"

  # The pinned CIDR must be a parseable IPv4 CIDR. The preflight's own
  # validate_cidr() exits the shell on bad input, so it cannot be used here.
  if ! is_valid_cidr "$cidr"; then
    guard_fail "${preset##*/}: cubeNode.network.cidr ${cidr} is not a valid CIDR"
  fi

  # ...and must be network-aligned. An unaligned range is a real bug: Cubelet
  # and the egress dataplane both mask the CIDR down before deriving the
  # gateway, so 10.187.0.1/18 would put the bridge on 10.187.0.0/18 while an
  # operator reading the value expects .1 — and is_valid_cidr accepts it.
  cidr_ip="${cidr%/*}"
  cidr_mask="${cidr#*/}"
  aligned_network="$(awk -v ip="$cidr_ip" -v mask="$cidr_mask" 'BEGIN {
      split(ip, o, ".")
      v = o[1] * 16777216 + o[2] * 65536 + o[3] * 256 + o[4]
      hb = 32 - mask
      block = (hb == 32) ? 4294967296 : 2 ^ hb
      net = int(v / block) * block
      printf "%d.%d.%d.%d\n", int(net / 16777216) % 256, int(net / 65536) % 256, int(net / 256) % 256, net % 256
    }')"
  if [ "$cidr_ip" != "$aligned_network" ]; then
    guard_fail "${preset##*/}: cubeNode.network.cidr ${cidr} is not aligned to its network address (expected ${aligned_network}/${cidr_mask})"
  fi

  # ...and must not overlap any known range. cidr_overlaps returns non-zero
  # both for "no overlap" and for input it cannot parse, so an unparseable
  # range would silently pass — validate each one first.
  while IFS= read -r range; do
    [ -n "$range" ] || continue
    is_valid_cidr "$range" || guard_fail "known range ${range} is not a valid CIDR"
    if cidr_overlaps "$cidr" "$range"; then
      guard_fail "${preset##*/}: cubevs CIDR ${cidr} overlaps ${range}; the cubevs-cidr-preflight Hook fails an install on that layout (#1555)"
    fi
  done <<RANGES_EOF
$known_tke_ranges
RANGES_EOF

  # tproxyOnIP must stay the first usable IP of the pinned CIDR.
  tproxy="$(pinned_tproxy "$preset")"
  if [ -n "$tproxy" ]; then
    want_tproxy="$(gateway_from_cidr "$cidr")"
    [ "$tproxy" = "$want_tproxy" ] || guard_fail "${preset##*/}: tproxyOnIP ${tproxy} is not the first usable IP of ${cidr} (expected ${want_tproxy})"
    printf '  tproxyOnIP=%s matches the cubevs gateway\n' "$tproxy"
  fi
done

[ "$checked" -gt 0 ] || guard_fail "no preset pins cubeNode.network.cidr — did the values files change shape?"

echo "ok: $checked preset cubevs CIDR(s) clear of the VPC/Pod/Service and kubeadm-default ranges"
