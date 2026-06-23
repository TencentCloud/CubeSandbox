#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# lib-phases.sh — pure decision helpers for create.sh's phased terraform apply.
#
# create.sh has grown into the one-click deployment state machine: it phases the
# apply with `terraform apply -target`, toggling TF_VAR_create_tke /
# TF_VAR_deploy_tke_addons so the Kubernetes provider only connects once the TKE
# API server exists, and it has to stay recoverable across reruns and partial
# failures (keep already-purchased compute nodes, reuse an existing cluster,
# prune stale kubernetes_* state).
#
# Those decisions are the highest-risk part of the deployer, but they are also
# pure mappings from inputs (desired counts, `terraform state list` output) to
# outputs. Isolating them here, with NO cloud calls and NO terraform invocation,
# lets tests/test_phase_flags.sh exercise the first-run / rerun / partial-failure
# transitions without provisioning anything — a lightweight dry-run that guards
# the one-click recovery path against regressions.
#
# This file is sourced by create.sh (and by the test). It must not run anything
# at source time.

# phase_base_flags — flag tuple for every BASE apply (subnet, TCR, CVMs, MySQL,
# Redis). The Kubernetes provider must not connect before the API server exists,
# so both flags are OFF here.
phase_base_flags() {
	echo "create_tke=false deploy_tke_addons=false"
}

# phase_cluster_step_flags — flag tuple while purchasing the TKE cluster itself.
# create_tke is ON (we want the cluster), but deploy_tke_addons stays OFF so the
# Kubernetes provider is not used before the cluster answers.
phase_cluster_step_flags() {
	echo "create_tke=true deploy_tke_addons=false"
}

# phase_addons_flags — flag tuple for the final addons apply (cube-master / api /
# proxy / webui). Both ON.
phase_addons_flags() {
	echo "create_tke=true deploy_tke_addons=true"
}

# _set_phase_flags "create_tke=.. deploy_tke_addons=.." — consume a phase-flag
# tuple from the producers above and apply it to the TF_VAR_* environment that
# drives the phased `terraform apply -target` flow. Kept here (next to the
# producers) so test_phase_flags.sh can round-trip producer→parser and catch a
# key rename that would otherwise leave create.sh silently using stale flags.
# The word-split over $1 is intentional (the tuple is space-separated).
# shellcheck disable=SC2086
_set_phase_flags() {
	local kv
	for kv in $1; do
		case "$kv" in
		create_tke=*) export TF_VAR_create_tke="${kv#create_tke=}" ;;
		deploy_tke_addons=*) export TF_VAR_deploy_tke_addons="${kv#deploy_tke_addons=}" ;;
		esac
	done
}

# phase_effective_compute_count <desired> <existing> — never scale compute nodes
# DOWN on a rerun. Returns whichever of desired/existing is larger, so an
# interrupted or re-run deployment keeps compute nodes that were already
# purchased instead of destroying them. Non-numeric inputs are treated as 0.
phase_effective_compute_count() {
	local desired="${1:-0}" existing="${2:-0}"
	[[ "${desired}" =~ ^[0-9]+$ ]] || desired=0
	[[ "${existing}" =~ ^[0-9]+$ ]] || existing=0
	if [ "${existing}" -gt "${desired}" ]; then
		echo "${existing}"
	else
		echo "${desired}"
	fi
}

# phase_should_reuse_cluster <terraform-state-list> — on a rerun an existing TKE
# cluster in state must be REUSED (skip the cluster apply); re-applying it with
# deploy_tke_addons=false would drop the addon resources to count 0 and tear down
# the cube-* Services (changing their CLB IPs). Returns 0 (reuse) when the state
# already has the cluster, 1 (create it) otherwise.
phase_should_reuse_cluster() {
	printf '%s\n' "${1:-}" | grep -q '^tencentcloud_kubernetes_cluster\.tke'
}

# phase_should_prune_stale_k8s <terraform-state-list> — when the TKE cluster is
# NOT in state (destroyed / never created / removed) but leftover kubernetes_*
# resources remain, the Kubernetes provider has no cluster to reach and every
# plan/apply fails with "connection refused". Those resources must be pruned
# from state first. Returns 0 when pruning is required.
phase_should_prune_stale_k8s() {
	local state="${1:-}"
	if printf '%s\n' "${state}" | grep -q '^tencentcloud_kubernetes_cluster\.tke'; then
		return 1
	fi
	printf '%s\n' "${state}" | grep -q '^kubernetes_'
}
