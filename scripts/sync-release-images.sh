#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Mirror a released component image from TCR int to TCR cn, keeping every tag
# identical to the source (same digest and media type).
#
# Blobs move per arch through a local OCI layout; manifests and the release
# index are copied at manifest level. Release tags never take a tarball
# round-trip, which would collapse them to one platform and change their digest.
#
# Usage:
#   SRC_REGISTRY=cube-sandbox-int.tencentcloudcr.com \
#   DST_REGISTRY=cube-sandbox-cn.tencentcloudcr.com \
#   IMAGE_NAME=cube-master IMAGE_TAG=vX.Y.Z ARCHES="amd64 arm64" \
#     scripts/sync-release-images.sh
#
# Environment:
#   SRC_REGISTRY          source registry host (required)
#   DST_REGISTRY          destination registry host (required)
#   REPO_PREFIX           repository prefix inside both registries
#                         (default: cube-sandbox)
#   IMAGE_NAME            component image name (required)
#   IMAGE_TAG             release tag, e.g. vX.Y.Z (required)
#   ARCHES                space-separated arch list, e.g. "amd64 arm64" (required)
#   CRANE_SYNC_ATTEMPTS   per-arch blob transfer attempts (default: 3)
#   CRANE_PULL_TIMEOUT    per-attempt pull timeout (default: 20m)
#   CRANE_PUSH_TIMEOUT    per-attempt push timeout (default: 20m)
#   CRANE_INDEX_TIMEOUT   timeout for index/tag manifest copies (default: 5m)
#   WORK_DIR              scratch dir for OCI layouts
#                         (default: ${RUNNER_TEMP:-/tmp}/crane-sync)
#
# A `latest` tag is mirrored only when IMAGE_TAG is a strict vX.Y.Z release.
# ARCHES decides which per-arch tags are transferred and which platforms the
# destination index must contain; an amd64-only component passes ARCHES=amd64.
set -euo pipefail

usage() {
  sed -n '5,/^set -euo pipefail$/p' "${BASH_SOURCE[0]}" |
    sed -e 's/^# \{0,1\}//' -e '/^set -euo pipefail$/d'
  exit "${1:-0}"
}

for arg in "$@"; do
  case "${arg}" in
    -h | --help) usage 0 ;;
    *)
      echo "error: unknown argument: ${arg}" >&2
      usage 2
      ;;
  esac
done

die() {
  echo "error: $*" >&2
  exit 1
}

SRC_REGISTRY="${SRC_REGISTRY:-}"
DST_REGISTRY="${DST_REGISTRY:-}"
REPO_PREFIX="${REPO_PREFIX:-cube-sandbox}"
IMAGE_NAME="${IMAGE_NAME:-}"
IMAGE_TAG="${IMAGE_TAG:-}"
ARCHES="${ARCHES:-}"
CRANE_SYNC_ATTEMPTS="${CRANE_SYNC_ATTEMPTS:-3}"
CRANE_PULL_TIMEOUT="${CRANE_PULL_TIMEOUT:-20m}"
CRANE_PUSH_TIMEOUT="${CRANE_PUSH_TIMEOUT:-20m}"
CRANE_INDEX_TIMEOUT="${CRANE_INDEX_TIMEOUT:-5m}"
WORK_DIR="${WORK_DIR:-${RUNNER_TEMP:-/tmp}/crane-sync}"

[[ -n "${SRC_REGISTRY}" ]] || die "SRC_REGISTRY is empty"
[[ -n "${DST_REGISTRY}" ]] || die "DST_REGISTRY is empty"
[[ -n "${IMAGE_NAME}" ]] || die "IMAGE_NAME is empty"
[[ -n "${IMAGE_TAG}" ]] || die "IMAGE_TAG is empty"
[[ -n "${ARCHES// }" ]] || die "ARCHES is empty"
command -v crane >/dev/null 2>&1 || die "crane is required but not on PATH"
command -v jq >/dev/null 2>&1 || die "jq is required but not on PATH"

# Split once so every loop sees the same arch list. An empty value was rejected
# above rather than yielding a loop that publishes nothing and exits 0.
read -r -a arch_list <<<"${ARCHES}"
[[ ${#arch_list[@]} -gt 0 ]] || die "ARCHES is empty"

src="${SRC_REGISTRY}/${REPO_PREFIX}/${IMAGE_NAME}"
dst="${DST_REGISTRY}/${REPO_PREFIX}/${IMAGE_NAME}"

# Only a strict vX.Y.Z release repoints `latest`; prereleases must not.
tags=("${IMAGE_TAG}")
if [[ "${IMAGE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  tags+=("latest")
fi

mkdir -p "${WORK_DIR}"

log() {
  echo "[$(date -u +%H:%M:%S)] $*"
}

# Transfer one per-arch image's blobs via a local OCI layout. Pull and push stay
# separate phases: a concurrent stream stalls against TCR cn.
sync_blobs() {
  local from="$1"
  local to="$2"
  local attempt
  local layout
  local slug

  slug="$(printf '%s' "${from}" | tr '/:' '__')"
  layout="${WORK_DIR}/${slug}"

  for attempt in $(seq 1 "${CRANE_SYNC_ATTEMPTS}"); do
    log "Transferring blobs ${from} -> ${to} (attempt ${attempt}/${CRANE_SYNC_ATTEMPTS})"
    rm -rf "${layout}"
    if timeout "${CRANE_PULL_TIMEOUT}" crane pull --format=oci "${from}" "${layout}" &&
      timeout "${CRANE_PUSH_TIMEOUT}" crane push "${layout}" "${to}"; then
      rm -rf "${layout}"
      return 0
    fi
    rm -rf "${layout}"
    if [[ "${attempt}" == "${CRANE_SYNC_ATTEMPTS}" ]]; then
      echo "error: failed to transfer ${from} -> ${to} after ${CRANE_SYNC_ATTEMPTS} attempts" >&2
      return 1
    fi
    sleep "$((attempt * 10))"
  done
}

# Manifest-level copy: blobs are already at the destination, so this only PUTs
# the manifest document and preserves the source digest and media type.
copy_manifest() {
  local from="$1"
  local to="$2"

  log "Copying manifest ${from} -> ${to}"
  if ! timeout "${CRANE_INDEX_TIMEOUT}" crane copy "${from}" "${to}"; then
    echo "error: failed to copy manifest ${from} -> ${to}" >&2
    return 1
  fi
}

# Consumers may pin either registry, so a destination digest that differs from
# the source fails the release.
assert_same_digest() {
  local tag="$1"
  local src_digest dst_digest

  src_digest="$(crane digest "${src}:${tag}")" || die "cannot resolve ${src}:${tag} digest"
  dst_digest="$(crane digest "${dst}:${tag}")" || die "cannot resolve ${dst}:${tag} digest"
  if [[ "${src_digest}" != "${dst_digest}" ]]; then
    die "digest mismatch for ${tag}: source ${src_digest} != destination ${dst_digest}"
  fi
  log "Digest OK ${tag} (${src_digest})"
}

# A release tag must be an index carrying every transferred arch; a collapsed
# single-arch image would otherwise ship and only fail on a non-amd64 node.
verify_index() {
  local ref="$1"
  local manifest
  local arch

  manifest="$(crane manifest "${ref}")" || die "cannot read ${ref} manifest"
  if ! jq -e '.mediaType == "application/vnd.oci.image.index.v1+json"
    or .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"' \
    <<<"${manifest}" >/dev/null; then
    echo "error: ${ref} is not an image index:" >&2
    echo "${manifest}" >&2
    exit 1
  fi
  for arch in "${arch_list[@]}"; do
    if ! jq -e --arg arch "${arch}" \
      '[.manifests[].platform.architecture] | index($arch) != null' \
      <<<"${manifest}" >/dev/null; then
      echo "error: ${ref} is missing platform ${arch}:" >&2
      echo "${manifest}" >&2
      exit 1
    fi
  done
}

for arch in "${arch_list[@]}"; do
  sync_blobs "${src}:${IMAGE_TAG}-${arch}" "${dst}:${IMAGE_TAG}-${arch}"
  copy_manifest "${src}:${IMAGE_TAG}-${arch}" "${dst}:${IMAGE_TAG}-${arch}"
  # The index references child digests, so it can match while a per-arch tag has
  # drifted. Assert the tag itself.
  assert_same_digest "${IMAGE_TAG}-${arch}"
done

copy_manifest "${src}:${IMAGE_TAG}" "${dst}:${IMAGE_TAG}"
verify_index "${dst}:${IMAGE_TAG}"
assert_same_digest "${IMAGE_TAG}"

# Mirror extra tags from the source instead of retagging the release locally.
# A re-sync of an older release must not move `latest` back to that release, and
# the source tag is what consumers on the other registries resolve.
for extra in "${tags[@]:1}"; do
  copy_manifest "${src}:${extra}" "${dst}:${extra}"
  verify_index "${dst}:${extra}"
  assert_same_digest "${extra}"
done

log "Synced ${src}:${tags[*]} -> ${dst}"
