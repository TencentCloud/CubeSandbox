#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# CPU-list helpers for s3lvol_tgt's SPDK -m mask. Sourced by rcow_common.sh
# (and one-click install.sh) so the default can be computed without pulling in
# the rest of the data-plane layout. Safe to source more than once.

# SPDK -m is a 64-bit core mask; ids 64 and above are not expressible.
RCOW_SPDK_CPUMASK_MAX_ID=63

# Expand a Linux CPU list ("0-3,8") into numeric IDs on stdout, one per line,
# in the order they appear. Empty or unparseable tokens are skipped.
rcow_expand_cpu_list() {
	local list="${1-}"
	local token start end id oldifs
	list="${list// /}"
	[ -n "${list}" ] || return 0
	oldifs="${IFS}"
	IFS=','
	# shellcheck disable=SC2086
	set -- ${list}
	IFS="${oldifs}"
	for token in "$@"; do
		[ -n "${token}" ] || continue
		case "${token}" in
		*-*)
			start="${token%-*}"
			end="${token#*-}"
			case "${start}" in
			''|*[!0-9]*) continue ;;
			esac
			case "${end}" in
			''|*[!0-9]*) continue ;;
			esac
			id="${start}"
			while [ "${id}" -le "${end}" ]; do
				printf '%s\n' "${id}"
				id=$((id + 1))
			done
			;;
		*)
			case "${token}" in
			''|*[!0-9]*) continue ;;
			esac
			printf '%s\n' "${token}"
			;;
		esac
	done
}

# SPDK -m hex mask for the last two CPUs this process may run on.
#
# Busy-poll reactors consume those cores at 100%. Taking the high end of
# Cpus_allowed_list keeps CPU0 (IRQs, systemd, kubelet) free when the allowed
# set is 0..N-1. Pass a Linux CPU list to override /proc/self/status (tests).
# CPU ids >= 64 are skipped (SPDK -m is 64-bit). One allowed CPU uses that
# CPU. An unreadable/empty list falls back to 0x1 with a warning; a list
# whose every id is >= 64 fails.
rcow_default_tgt_cpumask() {
	local list="" from_proc=0
	local a="" b="" id mask
	local saw_cpu=0
	if [ "$#" -ge 1 ]; then
		list="$1"
	else
		from_proc=1
		list="$(awk -F: '/^Cpus_allowed_list:/{gsub(/^[ \t]+/, "", $2); print $2; exit}' /proc/self/status 2>/dev/null || true)"
	fi
	while IFS= read -r id; do
		[ -n "${id}" ] || continue
		saw_cpu=1
		if [ "${id}" -gt "${RCOW_SPDK_CPUMASK_MAX_ID}" ]; then
			continue
		fi
		a="${b}"
		b="${id}"
	done < <(rcow_expand_cpu_list "${list}")
	if [ -z "${b}" ]; then
		if [ "${saw_cpu}" -eq 1 ]; then
			printf 'no CPU id <= %s in allowed list %s; SPDK -m is a 64-bit mask\n' \
				"${RCOW_SPDK_CPUMASK_MAX_ID}" "${list}" >&2
			return 1
		fi
		if [ "${from_proc}" -eq 1 ]; then
			printf 'could not read Cpus_allowed_list from /proc/self/status; falling back to 0x1\n' >&2
		elif [ -z "${list}" ]; then
			printf 'empty CPU list; falling back to 0x1\n' >&2
		else
			printf 'could not parse CPU list %s; falling back to 0x1\n' "${list}" >&2
		fi
		printf '0x1\n'
		return 0
	fi
	if [ -z "${a}" ]; then
		mask=$((1 << b))
	else
		mask=$(( (1 << a) | (1 << b) ))
	fi
	printf '0x%x\n' "${mask}"
}
