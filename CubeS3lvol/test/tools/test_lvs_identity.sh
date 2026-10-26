#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Offline tests for rcow lvstore identity: IPv4/IPv6 and numeric short names
# hash the full hostname; DNS FQDNs still use hostname -s.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMMON="${ROOT}/scripts/rcow_common.sh"

fail() {
	echo "FAIL: $*" >&2
	echo "result: 0 passed, 1 failed"
	exit 1
}

[[ -f "${COMMON}" ]] || fail "missing ${COMMON}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# Pin so sourcing does not depend on this machine's hostname, and so mkdir
# does not touch /data/cubelet.
# shellcheck source=../../scripts/rcow_common.sh
RCOW_LVS_NAME=lvs-identity-test
RCOW_ACTIVE_FILE="${tmp}/active_lvols"
RCOW_BSTORE_FILE="${tmp}/bstore.json"
# shellcheck disable=SC1090
source "${COMMON}"

want_ip() {
	rcow_is_ip_hostname "$1" || fail "expected IP hostname: $1"
}

refuse_ip() {
	if rcow_is_ip_hostname "$1"; then
		fail "did not expect IP hostname: $1"
	fi
}

want_id() {
	local full="$1" short="$2" expect="$3" got
	got="$(rcow_identity_host_from "${full}" "${short}")" \
		|| fail "identity_host_from ${full} / ${short} failed"
	[[ "${got}" == "${expect}" ]] \
		|| fail "identity_host_from ${full} / ${short}: got ${got}, want ${expect}"
}

want_ip "192.0.2.48"
want_ip "192.0.2.44"
want_ip "2001:db8::1"
want_ip "fe80::1"
want_ip "::ffff:192.0.2.1"
refuse_ip "worker-1"
refuse_ip "n1.zone-a"
refuse_ip "192.0.2.48.internal"
refuse_ip ""

want_id "worker-1" "worker-1" "worker-1"
want_id "n1.zone-a" "n1" "n1"
want_id "worker1.example.com" "worker1" "worker1"
want_id "192.0.2.48" "192" "192.0.2.48"
want_id "192.0.2.44" "192" "192.0.2.44"
want_id "2001:db8::1" "2001:db8::1" "2001:db8::1"
want_id "192.0.2.48.internal" "192" "192.0.2.48.internal"
want_id "123" "123" "123"

h48="$(rcow_node_hash 192.0.2.48)" || fail "rcow_node_hash 192.0.2.48"
h44="$(rcow_node_hash 192.0.2.44)" || fail "rcow_node_hash 192.0.2.44"
h192="$(rcow_node_hash 192)" || fail "rcow_node_hash 192"
[[ "${h48}" != "${h44}" ]] || fail "IPv4 nodes must not share a hash"
[[ "${h48}" != "${h192}" ]] || fail "full IPv4 must not hash like hostname -s"
[[ "${h48}" == "$(rcow_node_hash 192.0.2.48)" ]] \
	|| fail "rcow_node_hash must be stable for the same input"

h_short="$(rcow_node_hash worker-1)" || fail "rcow_node_hash worker-1"
expect_short="$(printf '%s' worker-1 | sha256sum | cut -c1-8)"
[[ "${h_short}" == "${expect_short}" ]] \
	|| fail "hash(worker-1) must match sha256 8hex (got ${h_short})"

echo "lvs identity tests OK"
echo "result: 21 passed, 0 failed"
