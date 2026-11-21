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

RCOW_NODE_ID=inventory-node_7
[[ "$(rcow_node_id)" == "inventory-node_7" ]] \
	|| fail "explicit RCOW_NODE_ID must be returned unchanged"

RCOW_NODE_ID='bad/node'
rcow_node_id >/dev/null 2>&1 &&
	fail "RCOW_NODE_ID with unsafe characters must be rejected"

RCOW_NODE_ID="$(printf 'x%.0s' {1..64})"
rcow_node_id >/dev/null 2>&1 &&
	fail "RCOW_NODE_ID longer than the marker field must be rejected"

unset RCOW_NODE_ID
hostname() { printf '%s\n' 'legacy/host'; }
[[ "$(rcow_node_id)" == "legacy_host" ]] \
	|| fail "hostname fallback must match the C marker canonicalisation"

hostname() { printf '%064d\n' 0; }
legacy_long="$(rcow_node_id)"
[[ "${#legacy_long}" -eq 63 ]] \
	|| fail "hostname fallback must fit the 63-byte owner marker field"
unset -f hostname

# --- identity from NODE_NAME (cube-s3lvol image, kubectl exec) --------------
# Resolved at source time, so each case sources rcow_common.sh afresh in a
# clean child shell. Prints "<RCOW_NODE_ID> <RCOW_LVS_NAME>" or fails.
# The Pod hostname is stubbed to something else: the point is that it is not
# what the identity comes from.
source_identity() {
	env -i PATH="${PATH}" HOME="${tmp}" \
		RCOW_ACTIVE_FILE="${tmp}/active_lvols" \
		RCOW_BSTORE_FILE="${tmp}/bstore.json" \
		"$@" bash -c '
			hostname() { printf "%s\n" cube-node-pod-x7k2q; }
			export -f hostname
			source "$0" >/dev/null || exit 1
			printf "%s %s\n" "${RCOW_NODE_ID}" "${RCOW_LVS_NAME}"
		' "${COMMON}"
}

entry_hash() { printf 'rcow-%s' "$(printf '%s' "$1" | sha256sum | cut -c1-8)"; }

got="$(source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 NODE_NAME=worker-1)" \
	|| fail "sourcing with NODE_NAME identity failed"
[[ "${got}" == "worker-1 $(entry_hash worker-1)" ]] \
	|| fail "NODE_NAME must give the entrypoint's RCOW_NODE_ID and lvstore name (got ${got})"

got="$(source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 NODE_NAME=192.0.2.48)" \
	|| fail "sourcing with an address NODE_NAME failed"
[[ "${got}" == "192.0.2.48 $(entry_hash 192.0.2.48)" ]] \
	|| fail "an address NODE_NAME must hash in full (got ${got})"

# spec.nodeName may be 64 bytes, one more than the marker field holds. The
# derived identity keeps the first 63 (as the hostname fallback does) so the
# target still starts; the lvstore name hashes the full name.
node64="node-$(printf 'a%.0s' {1..54})-x9.b"
[[ "${#node64}" -eq 64 ]] || fail "test setup: node64 must be 64 bytes"
got="$(source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 NODE_NAME="${node64}")" \
	|| fail "sourcing with a 64-byte NODE_NAME failed"
[[ "${got% *}" == "${node64:0:63}" ]] \
	|| fail "a 64-byte NODE_NAME must give its first 63 bytes as RCOW_NODE_ID (got ${got% *})"
[[ "${got#* }" == "$(entry_hash "${node64}")" ]] \
	|| fail "the lvstore name must still hash the full 64-byte NODE_NAME (got ${got#* })"
[[ "$(RCOW_NODE_ID="${got% *}" rcow_node_id 2>/dev/null)" == "${node64:0:63}" ]] \
	|| fail "the identity derived from a 64-byte NODE_NAME must pass rcow_node_id"

got="$(source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 NODE_NAME=worker-1 \
	RCOW_NODE_ID="${node64}" RCOW_LVS_NAME=pinned-lvs)" \
	|| fail "sourcing with a 64-byte explicit RCOW_NODE_ID failed"
[[ "${got}" == "${node64} pinned-lvs" ]] \
	|| fail "an explicit RCOW_NODE_ID must not be rewritten, only validated (got ${got})"

got="$(source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 NODE_NAME=worker-1 \
	RCOW_NODE_ID=inventory-7 RCOW_LVS_NAME=pinned-lvs)" \
	|| fail "sourcing with explicit values failed"
[[ "${got}" == "inventory-7 pinned-lvs" ]] \
	|| fail "explicit RCOW_NODE_ID / RCOW_LVS_NAME must win over NODE_NAME (got ${got})"

if source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 >/dev/null 2>&1; then
	fail "the image flag without NODE_NAME must refuse, not fall back to the Pod hostname"
fi

# NODE_NAME is needed only for a value that has to be derived.
got="$(source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 \
	RCOW_NODE_ID=inventory-7 RCOW_LVS_NAME=pinned-lvs)" \
	|| fail "explicit RCOW_NODE_ID and RCOW_LVS_NAME must be enough without NODE_NAME"
[[ "${got}" == "inventory-7 pinned-lvs" ]] \
	|| fail "explicit values without NODE_NAME must be kept (got ${got})"
if source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 RCOW_NODE_ID=inventory-7 \
	>/dev/null 2>&1; then
	fail "without NODE_NAME, a lvstore name that must be derived must refuse"
fi
if source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 RCOW_LVS_NAME=pinned-lvs \
	>/dev/null 2>&1; then
	fail "without NODE_NAME, an owner identity that must be derived must refuse"
fi

# Without the image flag a stray NODE_NAME is ignored: hostname, as before.
got="$(source_identity NODE_NAME=worker-1)" \
	|| fail "sourcing without the image flag failed"
[[ "${got}" == " $(entry_hash cube-node-pod-x7k2q)" ]] \
	|| fail "NODE_NAME must not change identity outside the cube-s3lvol image (got ${got})"

# The entrypoint and a manual run must agree.
entry="${ROOT}/../deploy/kubernetes/images/scripts/cube-s3lvol-entrypoint.sh"
if [[ -f "${entry}" ]]; then
	ep="$(grep -A5 '^s3lvol_derived_lvs_name()' "${entry}")"
	eval "${ep}"
	[[ "$(s3lvol_derived_lvs_name worker-1)" == "$(entry_hash worker-1)" ]] \
		|| fail "entrypoint and rcow_common.sh must derive the same lvstore name"
	eval "$(sed -n '/^s3lvol_derived_node_id()/,/^}/p' "${entry}")"
	[[ "$(s3lvol_derived_node_id worker-1)" == "worker-1" ]] \
		|| fail "entrypoint must keep a short NODE_NAME as the owner identity"
	got="$(source_identity RCOW_IDENTITY_FROM_NODE_NAME=1 NODE_NAME="${node64}")"
	[[ "$(s3lvol_derived_node_id "${node64}")" == "${got% *}" ]] \
		|| fail "entrypoint and rcow_common.sh must derive the same owner identity"
fi

echo "lvs identity tests OK"
echo "result: 41 passed, 0 failed"
