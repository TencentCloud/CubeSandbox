#!/usr/bin/env bash
# Coverage for scripts/sync-release-images.sh: argument validation, the
# vX.Y.Z -> latest rule, the index/platform assertions, and the cross-registry
# digest contract.
#
# The registry checks need docker and crane. When either is missing they are
# skipped locally, unless CUBE_SYNC_TEST_REQUIRE=1, which turns a missing
# dependency into a failure so CI cannot pass without running them.
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

require_e2e="${CUBE_SYNC_TEST_REQUIRE:-0}"

# Skip locally, fail in required mode.
skip_or_fail() {
	local reason="$1"
	if [[ "${require_e2e}" == "1" ]]; then
		fail "end-to-end registry checks are required but ${reason}"
		exit 1
	fi
	echo "SKIP: end-to-end registry checks (${reason})"
	[[ "${failures}" -eq 0 ]] || exit 1
	exit 0
}

if [[ "${CUBE_SYNC_TEST_SKIP:-0}" == "1" ]]; then
	echo "SKIP: end-to-end registry checks (CUBE_SYNC_TEST_SKIP=1)"
	[[ "${failures}" -eq 0 ]] || exit 1
	exit 0
fi
for tool in docker crane jq; do
	command -v "${tool}" >/dev/null 2>&1 || skip_or_fail "${tool} not installed"
done
docker info >/dev/null 2>&1 || skip_or_fail "docker daemon unavailable"

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
# Seed the source index the way release publishes it, using buildx when
# available (Docker manifest list) and crane otherwise (OCI index). Both media
# types are accepted by the mirror, so either seeding path is valid.
seed_index() {
	local ref="$1"
	shift
	if command -v docker >/dev/null 2>&1 &&
		docker buildx imagetools create -t "${ref}" "$@" >/dev/null 2>&1; then
		return 0
	fi
	local args=()
	local m
	for m in "$@"; do args+=(-m "${m}"); done
	crane index append -t "${ref}" "${args[@]}" >/dev/null 2>&1
}

seed_index "${src}/cube-sandbox/testapp:v1.2.3" \
	"${src}/cube-sandbox/testapp:v1.2.3-amd64" \
	"${src}/cube-sandbox/testapp:v1.2.3-arm64" ||
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

# A source tag that does not exist must fail rather than publish a partial set.
if run_sync env IMAGE_TAG=v1.2.3 ARCHES="amd64 s390x" >"${work}/badarch.log" 2>&1; then
	fail "sync succeeded for a non-existent arch tag"
else
	pass "non-existent arch tag fails the sync"
fi

# The platform assertion itself: both per-arch tags exist under one repo, but the
# index there was built from amd64 only (the collapsed shape the old mirror
# produced), so the platform check is what must reject it.
crane copy "${src}/cube-sandbox/testapp:v1.2.3-amd64" \
	"${src}/cube-sandbox/partial:v2.0.0-amd64" >/dev/null 2>&1 ||
	fail "could not seed partial amd64 tag"
crane copy "${src}/cube-sandbox/testapp:v1.2.3-arm64" \
	"${src}/cube-sandbox/partial:v2.0.0-arm64" >/dev/null 2>&1 ||
	fail "could not seed partial arm64 tag"
seed_index "${src}/cube-sandbox/partial:v2.0.0" \
	"${src}/cube-sandbox/partial:v2.0.0-amd64"
mkdir -p "${work}/partial"
if ! crane manifest "${src}/cube-sandbox/partial:v2.0.0" | jq -e \
	'[.manifests[].platform.architecture] == ["amd64"]' >/dev/null 2>&1; then
	fail "could not seed a single-arch source index"
else
	if SRC_REGISTRY="${src}" DST_REGISTRY="${dst}" IMAGE_NAME=partial \
		WORK_DIR="${work}/partial" IMAGE_TAG=v2.0.0 ARCHES="amd64 arm64" \
		"${SYNC_SCRIPT}" >"${work}/partial.log" 2>&1; then
		fail "sync succeeded although the source index lacks arm64"
	elif grep -q 'is missing linux/arm64' "${work}/partial.log"; then
		pass "source index missing a platform fails on the platform assertion"
	else
		fail "unexpected failure for a single-arch source index"
		tail -5 "${work}/partial.log" >&2
	fi
fi

# A destination tag that is still a collapsed single-arch image is replaced by a
# correct multi-arch index on the next sync.
crane copy "${src}/cube-sandbox/testapp:v1.2.3-amd64" \
	"${dst}/cube-sandbox/testapp:v1.2.3" >/dev/null 2>&1
if run_sync env IMAGE_TAG=v1.2.3 ARCHES="amd64 arm64" >"${work}/repair.log" 2>&1; then
	repaired="$(crane manifest "${dst}/cube-sandbox/testapp:v1.2.3" | jq -r .mediaType 2>/dev/null || true)"
	want="$(crane digest "${src}/cube-sandbox/testapp:v1.2.3")"
	got="$(crane digest "${dst}/cube-sandbox/testapp:v1.2.3" 2>/dev/null || true)"
	if [[ "${repaired}" != "application/vnd.oci.image.index.v1+json" &&
		"${repaired}" != "application/vnd.docker.distribution.manifest.list.v2+json" ]]; then
		fail "collapsed destination tag was not restored to an index"
	elif [[ "${want}" != "${got}" ]]; then
		fail "repaired destination digest differs: src=${want} dst=${got}"
	else
		pass "collapsed destination tag is restored to the source index"
	fi
else
	fail "sync failed while repairing a collapsed destination tag"
	tail -5 "${work}/repair.log" >&2
fi

# A prerelease is mirrored but must leave `latest` on the previous release.
crane tag "${src}/cube-sandbox/testapp:v1.2.3-amd64" v1.2.4-rc1-amd64 >/dev/null 2>&1
crane tag "${src}/cube-sandbox/testapp:v1.2.3-arm64" v1.2.4-rc1-arm64 >/dev/null 2>&1
seed_index "${src}/cube-sandbox/testapp:v1.2.4-rc1" \
	"${src}/cube-sandbox/testapp:v1.2.4-rc1-amd64" \
	"${src}/cube-sandbox/testapp:v1.2.4-rc1-arm64"
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

# A rejected source index must leave the destination tag untouched: the
# destination is what consumers pull by name, so validating after publishing
# would leave a pullable but broken release tag behind. The source has both
# per-arch tags so the blob phase succeeds and the index check is what rejects.
crane copy "${src}/cube-sandbox/testapp:v1.2.3-amd64" \
	"${src}/cube-sandbox/broken:v3.0.0-amd64" >/dev/null 2>&1 ||
	fail "could not seed broken amd64 tag"
crane copy "${src}/cube-sandbox/testapp:v1.2.3-arm64" \
	"${src}/cube-sandbox/broken:v3.0.0-arm64" >/dev/null 2>&1 ||
	fail "could not seed broken arm64 tag"
seed_index "${src}/cube-sandbox/broken:v3.0.0" \
	"${src}/cube-sandbox/broken:v3.0.0-amd64"
# Pre-seed the destination with a healthy two-arch index of different content.
crane copy "${src}/cube-sandbox/testapp:v1.2.3" \
	"${dst}/cube-sandbox/broken:v3.0.0" >/dev/null 2>&1 ||
	fail "could not pre-seed the destination tag"
broken_before="$(crane digest "${dst}/cube-sandbox/broken:v3.0.0" 2>/dev/null || true)"
if SRC_REGISTRY="${src}" DST_REGISTRY="${dst}" IMAGE_NAME=broken \
	WORK_DIR="${work}/broken" IMAGE_TAG=v3.0.0 ARCHES="amd64 arm64" \
	"${SYNC_SCRIPT}" >"${work}/broken.log" 2>&1; then
	fail "sync accepted a source index that is missing a platform"
elif ! grep -q 'is missing linux/arm64' "${work}/broken.log"; then
	# Without this the case would pass for any failure, including one unrelated
	# to the platform assertion.
	fail "rejected sync failed for an unexpected reason"
	tail -5 "${work}/broken.log" >&2
else
	broken_after="$(crane digest "${dst}/cube-sandbox/broken:v3.0.0" 2>/dev/null || true)"
	if [[ -z "${broken_before}" || "${broken_after}" != "${broken_before}" ]]; then
		fail "rejected sync modified the destination tag: ${broken_before:-<missing>} -> ${broken_after:-<missing>}"
	else
		pass "rejected sync leaves the destination tag unchanged"
	fi
fi

# Re-syncing an older release must not drag `latest` back to it: `latest` is
# mirrored from the source, which may already point at a newer release. The
# newer release needs genuinely different content, otherwise its index digest
# equals the older one and the case cannot distinguish the two behaviours.
crane pull --platform linux/amd64 alpine:3.20 "${work}/n-amd.tar" >/dev/null 2>&1
crane pull --platform linux/arm64 alpine:3.20 "${work}/n-arm.tar" >/dev/null 2>&1
crane push "${work}/n-amd.tar" "${src}/cube-sandbox/testapp:v1.2.5-amd64" >/dev/null 2>&1
crane push "${work}/n-arm.tar" "${src}/cube-sandbox/testapp:v1.2.5-arm64" >/dev/null 2>&1
seed_index "${src}/cube-sandbox/testapp:v1.2.5" \
	"${src}/cube-sandbox/testapp:v1.2.5-amd64" \
	"${src}/cube-sandbox/testapp:v1.2.5-arm64"
if crane tag "${src}/cube-sandbox/testapp:v1.2.5" latest >/dev/null 2>&1 &&
	[[ "$(crane digest "${src}/cube-sandbox/testapp:latest")" != \
		"$(crane digest "${src}/cube-sandbox/testapp:v1.2.3")" ]]; then
	newer="$(crane digest "${src}/cube-sandbox/testapp:latest")"
	if run_sync env IMAGE_TAG=v1.2.3 ARCHES="amd64 arm64" >"${work}/resync.log" 2>&1; then
		dst_latest="$(crane digest "${dst}/cube-sandbox/testapp:latest" 2>/dev/null || true)"
		if [[ "${dst_latest}" != "${newer}" ]]; then
			fail "re-sync of an older release moved latest: want ${newer} got ${dst_latest:-<missing>}"
		else
			pass "re-sync of an older release keeps the source latest"
		fi
	else
		fail "re-sync of an older release failed"
		tail -5 "${work}/resync.log" >&2
	fi
else
	fail "could not repoint the source latest to a distinct newer release"
fi

[[ "${failures}" -eq 0 ]] || {
	echo "${failures} failure(s)" >&2
	exit 1
}
echo "all sync-release-images checks passed"
