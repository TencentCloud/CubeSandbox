#!/usr/bin/env bash
# Coverage for scripts/sync-release-images.sh: argument validation, the
# vX.Y.Z -> latest rule, the index/platform assertions, and the cross-registry
# digest contract. The registry checks need docker and crane; without them only
# the static checks run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
SYNC_SCRIPT="${REPO_ROOT}/scripts/sync-release-images.sh"

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}

pass() {
	echo "ok   $*"
}

# Static checks always run; they need no docker.
bash -n "${SYNC_SCRIPT}" || fail "bash -n ${SYNC_SCRIPT}"
if command -v shellcheck >/dev/null 2>&1; then
	shellcheck -x -e SC1091 --severity=warning "${SYNC_SCRIPT}" ||
		fail "shellcheck ${SYNC_SCRIPT}"
fi

# Argument validation must fail without reaching a registry.
expect_invalid() {
	local label="$1"
	shift
	local rc=0
	env "$@" "${SYNC_SCRIPT}" >/dev/null 2>&1 || rc=$?
	if [[ "${rc}" != "1" ]]; then
		fail "expected rc=1 got rc=${rc} for ${label}"
	fi
}

expect_invalid "empty SRC_REGISTRY" \
	SRC_REGISTRY= DST_REGISTRY=dst IMAGE_NAME=x IMAGE_TAG=v1.2.3 ARCHES=amd64
expect_invalid "empty DST_REGISTRY" \
	SRC_REGISTRY=src DST_REGISTRY= IMAGE_NAME=x IMAGE_TAG=v1.2.3 ARCHES=amd64
expect_invalid "empty IMAGE_NAME" \
	SRC_REGISTRY=src DST_REGISTRY=dst IMAGE_NAME= IMAGE_TAG=v1.2.3 ARCHES=amd64
expect_invalid "empty IMAGE_TAG" \
	SRC_REGISTRY=src DST_REGISTRY=dst IMAGE_NAME=x IMAGE_TAG= ARCHES=amd64
expect_invalid "empty ARCHES" \
	SRC_REGISTRY=src DST_REGISTRY=dst IMAGE_NAME=x IMAGE_TAG=v1.2.3 ARCHES=
expect_invalid "blank ARCHES" \
	SRC_REGISTRY=src DST_REGISTRY=dst IMAGE_NAME=x IMAGE_TAG=v1.2.3 ARCHES="   "
pass "argument validation"

# --help exits 0 and documents every knob.
if ! "${SYNC_SCRIPT}" --help | grep -q 'SRC_REGISTRY'; then
	fail "--help does not document SRC_REGISTRY"
fi
if ! "${SYNC_SCRIPT}" --help | grep -q 'CRANE_INDEX_TIMEOUT'; then
	fail "--help does not document CRANE_INDEX_TIMEOUT"
fi
pass "--help"

if [[ "${CUBE_SYNC_TEST_SKIP:-0}" == "1" ]]; then
	echo "SKIP: end-to-end registry checks (CUBE_SYNC_TEST_SKIP=1)"
	[[ "${failures}" -eq 0 ]] || exit 1
	exit 0
fi
for tool in docker crane; do
	if ! command -v "${tool}" >/dev/null 2>&1; then
		echo "SKIP: end-to-end registry checks (${tool} not installed)"
		[[ "${failures}" -eq 0 ]] || exit 1
		exit 0
	fi
done
if ! docker info >/dev/null 2>&1; then
	echo "SKIP: end-to-end registry checks (docker daemon unavailable)"
	[[ "${failures}" -eq 0 ]] || exit 1
	exit 0
fi

work="$(mktemp -d)"
src_name="cubesync-src-$$"
dst_name="cubesync-dst-$$"
cleanup() {
	docker rm -f "${src_name}" "${dst_name}" >/dev/null 2>&1 || true
	rm -rf "${work}"
}
trap cleanup EXIT

src_port="${CUBE_SYNC_TEST_SRC_PORT:-5501}"
dst_port="${CUBE_SYNC_TEST_DST_PORT:-5502}"
docker run -d --name "${src_name}" -p "${src_port}:5000" registry:2 >/dev/null
docker run -d --name "${dst_name}" -p "${dst_port}:5000" registry:2 >/dev/null
src="127.0.0.1:${src_port}"
dst="127.0.0.1:${dst_port}"
for _ in $(seq 1 30); do
	curl -fsS "${src}/v2/" >/dev/null 2>&1 && curl -fsS "${dst}/v2/" >/dev/null 2>&1 && break
	sleep 1
done

# Seed a two-arch source the way release does: per-arch images plus an index.
crane pull --platform linux/amd64 alpine:3.19 "${work}/amd.tar" >/dev/null 2>&1
crane pull --platform linux/arm64 alpine:3.19 "${work}/arm.tar" >/dev/null 2>&1
crane push "${work}/amd.tar" "${src}/cube-sandbox/testapp:v1.2.3-amd64" >/dev/null 2>&1
crane push "${work}/arm.tar" "${src}/cube-sandbox/testapp:v1.2.3-arm64" >/dev/null 2>&1
if ! command -v docker >/dev/null 2>&1; then
	echo "SKIP: end-to-end registry checks (docker CLI needed to seed the index)"
	[[ "${failures}" -eq 0 ]] || exit 1
	exit 0
fi
docker buildx imagetools create -t "${src}/cube-sandbox/testapp:v1.2.3" \
	"${src}/cube-sandbox/testapp:v1.2.3-amd64" \
	"${src}/cube-sandbox/testapp:v1.2.3-arm64" >/dev/null 2>&1 ||
	{
		echo "FAIL: could not seed the multi-arch source index" >&2
		exit 1
	}
crane tag "${src}/cube-sandbox/testapp:v1.2.3" latest >/dev/null 2>&1

run_sync() {
	SRC_REGISTRY="${src}" DST_REGISTRY="${dst}" IMAGE_NAME=testapp \
		WORK_DIR="${work}/sync" "$@" "${SYNC_SCRIPT}"
}

if run_sync env IMAGE_TAG=v1.2.3 ARCHES="amd64 arm64" >"${work}/sync.log" 2>&1; then
	pass "sync of a two-arch component"
else
	fail "sync of a two-arch component (see log)"
	cat "${work}/sync.log" >&2
fi

# The contract: every tag, including latest, has the exact source digest.
for tag in v1.2.3-amd64 v1.2.3-arm64 v1.2.3 latest; do
	s="$(crane digest "${src}/cube-sandbox/testapp:${tag}" 2>/dev/null || true)"
	d="$(crane digest "${dst}/cube-sandbox/testapp:${tag}" 2>/dev/null || true)"
	if [[ -z "${s}" || "${s}" != "${d}" ]]; then
		fail "digest mismatch for ${tag}: src=${s:-<missing>} dst=${d:-<missing>}"
	else
		pass "digest matches for ${tag}"
	fi
done

# The mirrored index must stay a real multi-arch index.
if crane manifest "${dst}/cube-sandbox/testapp:v1.2.3" | jq -e \
	'.mediaType == "application/vnd.oci.image.index.v1+json"
	 or .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"' >/dev/null 2>&1; then
	pass "destination index media type"
else
	fail "destination index is not an image index"
fi

# A component missing one of the requested arches must fail, not silently ship.
if run_sync env IMAGE_TAG=v1.2.3 ARCHES="amd64 s390x" >"${work}/badarch.log" 2>&1; then
	fail "sync succeeded for a missing arch"
else
	pass "missing arch fails the sync"
fi

# A prerelease is mirrored but must leave `latest` on the previous release.
crane tag "${src}/cube-sandbox/testapp:v1.2.3-amd64" v1.2.4-rc1-amd64 >/dev/null 2>&1
crane tag "${src}/cube-sandbox/testapp:v1.2.3-arm64" v1.2.4-rc1-arm64 >/dev/null 2>&1
docker buildx imagetools create -t "${src}/cube-sandbox/testapp:v1.2.4-rc1" \
	"${src}/cube-sandbox/testapp:v1.2.4-rc1-amd64" \
	"${src}/cube-sandbox/testapp:v1.2.4-rc1-arm64" >/dev/null 2>&1
latest_before="$(crane digest "${dst}/cube-sandbox/testapp:latest" 2>/dev/null || true)"
if run_sync env IMAGE_TAG=v1.2.4-rc1 ARCHES="amd64 arm64" >"${work}/rc.log" 2>&1; then
	rc_digest="$(crane digest "${src}/cube-sandbox/testapp:v1.2.4-rc1")"
	dst_rc_digest="$(crane digest "${dst}/cube-sandbox/testapp:v1.2.4-rc1" 2>/dev/null || true)"
	latest_after="$(crane digest "${dst}/cube-sandbox/testapp:latest" 2>/dev/null || true)"
	if [[ "${rc_digest}" != "${dst_rc_digest}" ]]; then
		fail "prerelease digest mismatch: src=${rc_digest} dst=${dst_rc_digest:-<missing>}"
	elif [[ "${latest_after}" != "${latest_before}" ]]; then
		fail "prerelease moved latest: ${latest_before} -> ${latest_after}"
	else
		pass "prerelease is mirrored without moving latest"
	fi
else
	fail "sync of an existing prerelease failed"
	cat "${work}/rc.log" >&2
fi

[[ "${failures}" -eq 0 ]] || {
	echo "${failures} failure(s)" >&2
	exit 1
}
echo "all sync-release-images checks passed"
