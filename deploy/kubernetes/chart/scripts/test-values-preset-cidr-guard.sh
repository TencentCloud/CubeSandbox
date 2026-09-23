#!/bin/sh
# Guard: the cubevs/sandbox CIDR a provider preset pins must not overlap any
# range this repo itself provisions or documents for that provider.
#
# Regression for #1555: values-tke.yaml pinned 192.168.0.0/18, which fully
# contains the Service CIDR this repo's own one-click Terraform provisions by
# default (192.168.0.0/20), so the cubevs-cidr-preflight Hook failed the
# documented TKE install path. The preset moved the collision instead of
# removing it, and the "packaged Cubelet fallback" justification pointed at
# the same colliding range (Cubelet/config/config.toml).
#
# The guard is provider-agnostic: any values-<provider>.yaml that pins
# cubeNode.network.cidr is checked against every CIDR that provider's
# Terraform provisions (tke_* variables for TKE), so a future preset cannot
# reintroduce the same class of bug.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(CDPATH= cd -- "$CHART_DIR/../../.." && pwd)"
PREFLIGHT="$CHART_DIR/files/cubevs-cidr-preflight.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# Pull the cubevs CIDR a values file pins (top-level cubeNode.network.cidr),
# without depending on a YAML parser — the chart values stay flat here.
pinned_cidr() {
  awk '
    /^cubeNode:/ { in_node = 1; next }
    in_node && /^[^ ]/ { in_node = 0 }
    in_node && /^  network:/ { in_net = 1; next }
    in_net && /^  [^ ]/ { in_net = 0 }
    in_net && /^    cidr:/ { gsub(/"/, "", $2); print $2; exit }
  ' "$1"
}

# Derive tproxyOnIP from a CIDR (first usable IP = network + 1).
gateway_from_cidr() {
  awk -F'[./]' '{
    base = $1 * 16777216 + $2 * 65536 + $3 * 256 + $4 + 1
    printf "%d.%d.%d.%d\n", int(base / 16777216) % 256, int(base / 65536) % 256, int(base / 256) % 256, base % 256
  }' <<EOF
$1
EOF
}

CUBEVS_CIDR_PREFLIGHT_SOURCE_ONLY=1
# shellcheck disable=SC1090
. "$PREFLIGHT"

TF_VARS="$REPO_ROOT/deploy/one-click/terraform/tencentcloud/variables.tf"
[ -f "$TF_VARS" ] || fail "missing $TF_VARS — the ranges this repo provisions for TKE"

# CIDRs the TKE Terraform provisions, read from the same defaults the deployer
# uses, so a bumped default keeps this guard honest.
tke_service_cidr="$(sed -n '/variable "tke_service_cidr"/,/^}/p' "$TF_VARS" \
  | awk '/default/ { gsub(/"/, "", $3); print $3; exit }')"
tke_pod_cidr="$(sed -n '/variable "tke_cluster_cidr"/,/^}/p' "$TF_VARS" \
  | awk '/default/ { gsub(/"/, "", $3); print $3; exit }')"
vpc_cidr="$(sed -n '/resource "tencentcloud_vpc" "cluster"/,/^}/p' \
  "$REPO_ROOT/deploy/one-click/terraform/tencentcloud/main.tf" \
  | awk '/cidr_block/ { gsub(/"/, "", $3); print $3; exit }')"

[ -n "$tke_service_cidr" ] || fail "could not read tke_service_cidr default from $TF_VARS"
[ -n "$tke_pod_cidr" ] || fail "could not read tke_cluster_cidr default from $TF_VARS"
[ -n "$vpc_cidr" ] || fail "could not read the VPC cidr_block from one-click terraform"

# The ranges every TKE preset CIDR must stay clear of.
known_tke_ranges="
$tke_service_cidr
$tke_pod_cidr
$vpc_cidr
"

presets="$(ls "$CHART_DIR"/values-*.yaml 2>/dev/null || true)"
[ -n "$presets" ] || fail "no values-*.yaml presets found in $CHART_DIR"

checked=0
for preset in $presets; do
  cidr="$(pinned_cidr "$preset")"
  [ -n "$cidr" ] || continue # preset does not pin a cubevs CIDR
  checked=$((checked + 1))
  printf '%s pins cubeNode.network.cidr=%s\n' "${preset##*/}" "$cidr"

  # The pinned CIDR must be a valid, network-aligned IPv4 CIDR.
  if ! validate_cidr "$cidr" >/dev/null 2>&1; then
    fail "${preset##*/}: cubeNode.network.cidr ${cidr} is not a valid aligned CIDR"
  fi

  # ...and must not overlap any range this repo provisions for TKE.
  while IFS= read -r range; do
    [ -n "$range" ] || continue
    if cidr_overlaps "$cidr" "$range"; then
      fail "${preset##*/}: cubevs CIDR ${cidr} overlaps ${range} provisioned by deploy/one-click/terraform; the cubevs-cidr-preflight Hook would fail this install path (#1555)"
    fi
  done <<EOF
$known_tke_ranges
EOF

  # tproxyOnIP must stay the first usable IP of the pinned CIDR.
  pinned_tproxy="$(awk '
    /^cubeEgress:/ { in_egress = 1; next }
    in_egress && /^[^ ]/ { in_egress = 0 }
    in_egress && /^  network:/ { in_net = 1; next }
    in_net && /^  [^ ]/ { in_net = 0 }
    in_net && /^    tproxyOnIP:/ { gsub(/"/, "", $2); print $2; exit }
  ' "$preset")"
  if [ -n "$pinned_tproxy" ]; then
    want_tproxy="$(gateway_from_cidr "$cidr")"
    [ "$pinned_tproxy" = "$want_tproxy" ] || fail "${preset##*/}: tproxyOnIP ${pinned_tproxy} is not the first usable IP of ${cidr} (expected ${want_tproxy})"
    printf '  tproxyOnIP=%s matches the cubevs gateway\n' "$pinned_tproxy"
  fi
done

[ "$checked" -gt 0 ] || fail "no preset pins cubeNode.network.cidr — did the values files change shape?"

echo "ok: $checked preset cubevs CIDR(s) clear of the TKE VPC/Pod/Service ranges this repo provisions"
