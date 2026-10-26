#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Cloud-free dry run of the Tencent Cloud one-click deployer's phase/rerun state
# machine (terraform/tencentcloud/lib-phases.sh). It exercises the first-run,
# rerun and partial-failure transitions that decide TF_VAR_create_tke /
# TF_VAR_deploy_tke_addons, compute-node scaling and stale-state pruning — the
# highest-risk part of create.sh's recoverability — without provisioning
# anything.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PHASES_LIB="$(cd "${SCRIPT_DIR}/../terraform/tencentcloud" && pwd)/lib-phases.sh"

# shellcheck source=../terraform/tencentcloud/lib-phases.sh
source "${PHASES_LIB}"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

assert_eq() {
  local expected="$1" actual="$2" msg="${3:-}"
  [[ "${expected}" == "${actual}" ]] || fail "expected '${expected}', got '${actual}' ${msg}"
}

# Returns 0/1 from a predicate without tripping `set -e`.
predicate() {
  if "$@"; then echo 0; else echo 1; fi
}

# ---- phase flag tuples -------------------------------------------------------
test_flag_tuples() {
  # Base applies must never let the kubernetes provider connect.
  assert_eq "create_tke=false deploy_tke_addons=false" "$(phase_base_flags)" "(base)"
  # Cluster purchase: cluster on, addons still off.
  assert_eq "create_tke=true deploy_tke_addons=false" "$(phase_cluster_step_flags)" "(cluster step)"
  # Final addons apply: both on.
  assert_eq "create_tke=true deploy_tke_addons=true" "$(phase_addons_flags)" "(addons)"
}

# ---- producer -> parser contract --------------------------------------------
# The deployer's recoverability hinges on the phase_*_flags tuples being parsed
# back into the exact TF_VAR_* keys create.sh drives terraform with. Round-trip
# each producer through the real consumer (_set_phase_flags, also in lib-phases.sh)
# so a key rename on either side fails here instead of silently mis-phasing the
# kubernetes provider.
# shellcheck disable=SC2154  # TF_VAR_* are exported by _set_phase_flags (sourced lib)
test_set_phase_flags_contract() {
  _set_phase_flags "$(phase_base_flags)"
  assert_eq "false" "${TF_VAR_create_tke}" "(base -> create_tke)"
  assert_eq "false" "${TF_VAR_deploy_tke_addons}" "(base -> deploy_tke_addons)"

  _set_phase_flags "$(phase_cluster_step_flags)"
  assert_eq "true" "${TF_VAR_create_tke}" "(cluster -> create_tke)"
  assert_eq "false" "${TF_VAR_deploy_tke_addons}" "(cluster -> deploy_tke_addons)"

  _set_phase_flags "$(phase_addons_flags)"
  assert_eq "true" "${TF_VAR_create_tke}" "(addons -> create_tke)"
  assert_eq "true" "${TF_VAR_deploy_tke_addons}" "(addons -> deploy_tke_addons)"
}

# ---- compute count: never scale down on rerun --------------------------------
test_effective_compute_count() {
  # First run: nothing in state yet, honor desired.
  assert_eq "2" "$(phase_effective_compute_count 2 0)" "(first run)"
  # Rerun after partial failure: more nodes already exist than desired → keep them.
  assert_eq "3" "$(phase_effective_compute_count 2 3)" "(no scale down)"
  # Rerun with matching counts.
  assert_eq "2" "$(phase_effective_compute_count 2 2)" "(steady state)"
  # Desired grows on a rerun → scale up is allowed.
  assert_eq "5" "$(phase_effective_compute_count 5 3)" "(scale up)"
  # Zero desired, none in state.
  assert_eq "0" "$(phase_effective_compute_count 0 0)" "(zero)"
  # Defensive: non-numeric inputs collapse to 0 rather than erroring.
  assert_eq "0" "$(phase_effective_compute_count '' '')" "(empty)"
  assert_eq "4" "$(phase_effective_compute_count abc 4)" "(garbage desired)"
  assert_eq "3" "$(phase_effective_compute_count 3 xyz)" "(garbage existing)"
  assert_eq "0" "$(phase_effective_compute_count abc def)" "(both garbage)"
}

# ---- cluster reuse on rerun --------------------------------------------------
test_should_reuse_cluster() {
  # First run: empty state → create the cluster (predicate false → 1).
  assert_eq "1" "$(predicate phase_should_reuse_cluster "")" "(empty state)"

  # Rerun: cluster already in state → reuse it (predicate true → 0), so the
  # cluster apply is skipped and the addon Services / CLB IPs are preserved.
  local state_with_cluster
  state_with_cluster=$'tencentcloud_vpc.demo\ntencentcloud_kubernetes_cluster.tke[0]\nkubernetes_namespace.cubesandbox[0]'
  assert_eq "0" "$(predicate phase_should_reuse_cluster "${state_with_cluster}")" "(cluster in state)"

  # Base resources only (cluster not yet created) → do not reuse.
  local state_base_only
  state_base_only=$'tencentcloud_vpc.demo\ntencentcloud_instance.jumpserver'
  assert_eq "1" "$(predicate phase_should_reuse_cluster "${state_base_only}")" "(no cluster)"

  # Node-pool present but the cluster itself is gone (half-torn-down) → must NOT
  # reuse; the cluster apply has to recreate it.
  local state_nodepool_only
  state_nodepool_only=$'tencentcloud_vpc.demo\ntencentcloud_kubernetes_node_pool.tke[0]'
  assert_eq "1" "$(predicate phase_should_reuse_cluster "${state_nodepool_only}")" "(node pool only)"

  # A data-source address must not be mistaken for the managed cluster (^ anchor).
  local state_data_only
  state_data_only=$'data.tencentcloud_kubernetes_cluster.tke'
  assert_eq "1" "$(predicate phase_should_reuse_cluster "${state_data_only}")" "(data source only)"
}

# ---- prune stale kubernetes_* state when cluster is gone ---------------------
test_should_prune_stale_k8s() {
  # Cluster gone but kubernetes_* leftovers remain → must prune (true → 0).
  local stale
  stale=$'tencentcloud_vpc.demo\nkubernetes_namespace.cubesandbox[0]\nkubernetes_deployment.cubemaster[0]'
  assert_eq "0" "$(predicate phase_should_prune_stale_k8s "${stale}")" "(stale k8s, no cluster)"

  # Cluster present alongside kubernetes_* → do NOT prune (false → 1).
  local healthy
  healthy=$'tencentcloud_kubernetes_cluster.tke[0]\nkubernetes_namespace.cubesandbox[0]'
  assert_eq "1" "$(predicate phase_should_prune_stale_k8s "${healthy}")" "(cluster + k8s)"

  # No kubernetes_* resources at all → nothing to prune.
  local base
  base=$'tencentcloud_vpc.demo\ntencentcloud_instance.jumpserver'
  assert_eq "1" "$(predicate phase_should_prune_stale_k8s "${base}")" "(no k8s)"

  # Empty state → nothing to prune.
  assert_eq "1" "$(predicate phase_should_prune_stale_k8s "")" "(empty)"
}

test_flag_tuples
test_set_phase_flags_contract
test_effective_compute_count
test_should_reuse_cluster
test_should_prune_stale_k8s

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} phase-flag test(s) failed" >&2
  exit 1
fi

echo "phase flag tests OK"
