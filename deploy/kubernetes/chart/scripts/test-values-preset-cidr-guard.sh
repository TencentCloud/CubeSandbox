#!/usr/bin/env bash
# Guard: the cubevs/sandbox CIDR a provider preset pins must not overlap any
# range this repo itself provisions or documents for that provider.
#
# bash, not sh: this sources cubevs-cidr-preflight.sh, which uses [[ ]].
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
# The TKE ranges are applied to every preset, which is stricter than necessary
# for a non-TKE preset. Replace the flat list with a per-provider range table
# when a second provider preset appears.
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

# Derive tproxyOnIP from a CIDR (first usable IP = network + 1).
gateway_from_cidr() {
  awk -F'[./]' '{
    base = $1 * 16777216 + $2 * 65536 + $3 * 256 + $4 + 1
    printf "%d.%d.%d.%d\n", int(base / 16777216) % 256, int(base / 65536) % 256, int(base / 256) % 256, base % 256
  }' <<EOF
$1
EOF
}

TF_VARS="$REPO_ROOT/deploy/one-click/terraform/tencentcloud/variables.tf"
[ -f "$TF_VARS" ] || guard_fail "missing $TF_VARS — the ranges this repo provisions for TKE"

# CIDRs the TKE Terraform provisions, read from the same defaults the deployer
# uses, so a bumped default keeps this guard honest. Anchor on the assignment
# (`default =`) rather than any line containing "default" — a description that
# happens to use the word would otherwise be read as the value.
tke_service_cidr="$(sed -n '/variable "tke_service_cidr"/,/^}/p' "$TF_VARS" \
  | awk '/^[[:space:]]*default[[:space:]]*=/ { gsub(/"/, "", $3); print $3; exit }')"
tke_pod_cidr="$(sed -n '/variable "tke_cluster_cidr"/,/^}/p' "$TF_VARS" \
  | awk '/^[[:space:]]*default[[:space:]]*=/ { gsub(/"/, "", $3); print $3; exit }')"
vpc_cidr="$(sed -n '/resource "tencentcloud_vpc" "cluster"/,/^}/p' \
  "$REPO_ROOT/deploy/one-click/terraform/tencentcloud/main.tf" \
  | awk '/^[[:space:]]*cidr_block[[:space:]]*=/ { gsub(/"/, "", $3); print $3; exit }')"

[ -n "$tke_service_cidr" ] || guard_fail "could not read tke_service_cidr default from $TF_VARS"
[ -n "$tke_pod_cidr" ] || guard_fail "could not read tke_cluster_cidr default from $TF_VARS"
[ -n "$vpc_cidr" ] || guard_fail "could not read the VPC cidr_block from one-click terraform"

# The ranges every TKE preset CIDR must stay clear of.
known_tke_ranges="
$tke_service_cidr
$tke_pod_cidr
$vpc_cidr
"

presets="$(ls "$CHART_DIR"/values-*.yaml 2>/dev/null || true)"
[ -n "$presets" ] || guard_fail "no values-*.yaml presets found in $CHART_DIR"

checked=0
for preset in $presets; do
  cidr="$(pinned_cidr "$preset")"
  [ -n "$cidr" ] || continue # preset does not pin a cubevs CIDR
  checked=$((checked + 1))
  printf '%s pins cubeNode.network.cidr=%s\n' "${preset##*/}" "$cidr"

  # The pinned CIDR must be a parseable IPv4 CIDR. Note the preflight's own
  # validate_cidr() exits the shell on bad input, so it cannot be used here.
  if ! is_valid_cidr "$cidr"; then
    guard_fail "${preset##*/}: cubeNode.network.cidr ${cidr} is not a valid CIDR"
  fi

  # ...and must not overlap any range this repo provisions for TKE.
  while IFS= read -r range; do
    [ -n "$range" ] || continue
    # cidr_overlaps returns non-zero both for "no overlap" and for input it
    # cannot parse, so an unparseable range would silently pass. Validate the
    # range first, otherwise the guard degrades into a no-op.
    is_valid_cidr "$range" || guard_fail "terraform range ${range} is not a valid CIDR (deploy/one-click/terraform)"
    if cidr_overlaps "$cidr" "$range"; then
      guard_fail "${preset##*/}: cubevs CIDR ${cidr} overlaps ${range} provisioned by deploy/one-click/terraform; the cubevs-cidr-preflight Hook would fail an install on that layout (#1555)"
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
    [ "$pinned_tproxy" = "$want_tproxy" ] || guard_fail "${preset##*/}: tproxyOnIP ${pinned_tproxy} is not the first usable IP of ${cidr} (expected ${want_tproxy})"
    printf '  tproxyOnIP=%s matches the cubevs gateway\n' "$pinned_tproxy"
  fi
done

[ "$checked" -gt 0 ] || guard_fail "no preset pins cubeNode.network.cidr — did the values files change shape?"

echo "ok: $checked preset cubevs CIDR(s) clear of the TKE VPC/Pod/Service ranges this repo provisions"
