#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Build (and optionally push) the Cube Sandbox component container images from
# an extracted one-click release bundle.
#
# Usage
# -----
# After extracting cube-sandbox-one-click-*.tar.gz, also extract the inner
# package so this script and the Dockerfiles/artifacts sit side by side:
#
#   tar xzf cube-sandbox-one-click-*.tar.gz
#   cd cube-sandbox-one-click-*
#   tar xzf assets/package/sandbox-package.tar.gz -C assets/package/
#   assets/package/sandbox-package/terraform/tencentcloud/build_images.sh
#
# It builds five images straight from the package contents:
#   cube-api                <- CubeAPI/Dockerfile          (prebuilt CubeAPI/bin/cube-api)
#   cubemaster              <- CubeMaster/Dockerfile       (prebuilt CubeMaster/bin/cubemaster)
#   cubeproxy               <- cubeproxy/build-context/Dockerfile
#   cube-lifecycle-manager  <- cube-lifecycle-manager/build-context/Dockerfile
#   cube-webui              <- webui/Dockerfile.package    (prebuilt webui/dist)
#
# Selecting images:
#   build_images.sh                       # all five (default)
#   build_images.sh cube-api webui        # only the listed ones
#
# Options / environment:
#   --push | PUSH=1            also `docker push` each image after building
#   TAG=...                    shared image tag for ALL four images (default latest)
#   REGISTRY=...               registry host (default cube-sandbox-image.tencentcloudcr.com)
#   NAMESPACE=...              registry namespace (default cluster for standalone use).
#                              When pushing to the TCR created by this deployment,
#                              create.sh passes the Terraform namespace
#                              (`cubesandbox-cluster`, from `terraform output
#                              tcr_namespace`); set NAMESPACE to match for manual runs.
#   CUBE_API_IMAGE=...         fully-qualified ref overrides (per component);
#   CUBE_MASTER_IMAGE=...      default to ${REGISTRY}/${NAMESPACE}/<name>:${TAG},
#   CUBE_PROXY_IMAGE=...       matching terraform/tencentcloud (var.image_tag) so
#   CUBE_LCM_IMAGE=...
#   CUBE_WEBUI_IMAGE=...       freshly built images work with the TKE deployment.
#   WEB_UI_UPSTREAM=...        CubeAPI upstream baked into the webui image
#                              (default http://host.docker.internal:3000)
#
# Requirements: a working `docker` whose BuildKit builds run through the
# `docker buildx` CLI plugin (the component Dockerfiles use the BuildKit-only
# `COPY --chmod`). buildx must therefore be installed; this script installs it
# automatically when it can (see ensure_buildx) and otherwise prints how to.

set -euo pipefail

# CubeAPI/Dockerfile (and CubeMaster/Dockerfile) use `COPY --chmod`, which
# requires the BuildKit builder. BuildKit is the default on Docker Engine 23.0+,
# but force it on so the build still works on older daemons or where it was
# disabled via DOCKER_BUILDKIT=0. Modern Docker runs BuildKit builds through the
# buildx CLI plugin; ensure_buildx() below makes sure that plugin is present so
# the build does not abort with "BuildKit is enabled but the buildx component is
# missing or broken".
export DOCKER_BUILDKIT=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# build_images.sh lives at <pkg>/terraform/tencentcloud/, so the package root
# (the extracted sandbox-package/) is two levels up.
PKG_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

REGISTRY="${REGISTRY:-cube-sandbox-cn.tencentcloudcr.com}"
NAMESPACE="${NAMESPACE:-cube-sandbox}"
# One shared, externally overridable tag for all four component images. Keep in
# sync with terraform/tencentcloud (var.image_tag) so the default TKE deployment
# consumes exactly what this script builds.
TAG="${TAG:-v0.5.1}"

CUBE_API_IMAGE="${CUBE_API_IMAGE:-${REGISTRY}/${NAMESPACE}/cube-api:${TAG}}"
CUBE_MASTER_IMAGE="${CUBE_MASTER_IMAGE:-${REGISTRY}/${NAMESPACE}/cube-master:${TAG}}"
CUBE_PROXY_IMAGE="${CUBE_PROXY_IMAGE:-${REGISTRY}/${NAMESPACE}/cube-proxy:${TAG}}"
CUBE_LCM_IMAGE="${CUBE_LCM_IMAGE:-${REGISTRY}/${NAMESPACE}/cube-lifecycle-manager:${TAG}}"
CUBE_WEBUI_IMAGE="${CUBE_WEBUI_IMAGE:-${REGISTRY}/${NAMESPACE}/webui:${TAG}}"

WEB_UI_UPSTREAM="${WEB_UI_UPSTREAM:-http://host.docker.internal:3000}"
PUSH="${PUSH:-0}"

log() { echo "[build-images] $*" >&2; }
die() {
	echo "[build-images] ERROR: $*" >&2
	exit 1
}

usage() {
	# Print the leading comment header (line 2 up to the first blank line).
	sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

require_path() {
	[[ -e "$1" ]] || die "required package artifact not found: $1
  (did you extract assets/package/sandbox-package.tar.gz into assets/package/?)"
}

# Make sure the buildx CLI plugin is available. We force DOCKER_BUILDKIT=1 (the
# component Dockerfiles use the BuildKit-only `COPY --chmod`), and modern Docker
# routes `docker build` through the buildx plugin. The bare `docker` package
# (on the jumpserver, but also on many user machines) does NOT pull in buildx,
# so the build would otherwise fail with "BuildKit is enabled but the buildx
# component is missing or broken". Try to install it on the fly so a build host
# self-heals: directly when running as root (the jumpserver path), or via a
# non-interactive `sudo -n` for standalone non-root users (never prompting, so
# this cannot hang). If it still cannot be installed, fail with a copy-pasteable
# hint instead of the cryptic docker error.
ensure_buildx() {
	if docker buildx version >/dev/null 2>&1; then
		return 0
	fi

	# Pick a package manager (RPM-based first, then Debian/Ubuntu).
	local -a pm=()
	if command -v dnf >/dev/null 2>&1; then
		pm=(dnf install -y)
	elif command -v yum >/dev/null 2>&1; then
		pm=(yum install -y)
	elif command -v apt-get >/dev/null 2>&1; then
		pm=(apt-get install -y)
	fi

	# Installing packages needs root. Use it directly, or escalate with a
	# non-interactive `sudo -n` (never prompts, so it cannot hang). Probe sudo
	# up front: when it would require a password we report "cannot escalate"
	# rather than hammering the package manager with attempts that each fail
	# with "a password is required".
	local -a priv=()
	local privileged=1
	if [[ "${EUID}" -ne 0 ]]; then
		if command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
			priv=(sudo -n)
		else
			privileged=0
		fi
	fi

	# Cap each attempt so a slow/unreachable package mirror cannot hang the
	# (SSH-synchronous) deployment; if an attempt hits the cap we stop early
	# instead of retrying the other names against the same dead mirror. Only
	# used when `timeout` is available (e.g. absent on stock macOS).
	local timeout_secs=180
	local -a limit=()
	command -v timeout >/dev/null 2>&1 && limit=(timeout "${timeout_secs}")

	local out="" rc=0
	if [[ "${#pm[@]}" -gt 0 && "${privileged}" -eq 1 ]]; then
		log "docker buildx plugin not found; attempting to install it"

		# On Debian/Ubuntu refresh the index first so the package is found;
		# best-effort and time-limited like the installs below. Keep the output
		# only when it FAILS, so a broken/expired repo is reported instead of a
		# later misleading "Unable to locate package".
		if [[ "${pm[0]}" == "apt-get" ]]; then
			local -a upd=()
			[[ "${#limit[@]}" -gt 0 ]] && upd+=("${limit[@]}")
			[[ "${#priv[@]}" -gt 0 ]] && upd+=("${priv[@]}")
			upd+=(apt-get update)
			if out="$("${upd[@]}" 2>&1)"; then
				out=""
			fi
		fi

		# Candidate package names: docker-buildx-plugin is the Docker CE repo
		# name (RPM and Debian/Ubuntu); moby-buildx / docker-buildx are names
		# used by some stock distro repos. Try each, re-checking after every
		# attempt.
		local pkg attempt
		for pkg in docker-buildx-plugin moby-buildx docker-buildx; do
			local -a cmd=()
			[[ "${#limit[@]}" -gt 0 ]] && cmd+=("${limit[@]}")
			[[ "${#priv[@]}" -gt 0 ]] && cmd+=("${priv[@]}")
			cmd+=("${pm[@]}" "${pkg}")
			attempt="$("${cmd[@]}" 2>&1)" && rc=0 || rc=$?
			if docker buildx version >/dev/null 2>&1; then
				log "installed buildx via ${pkg}"
				return 0
			fi
			# Keep the first real error (the primary package); the later
			# "no such package" misses for the alternative names are less useful.
			[[ -z "${out}" && -n "${attempt}" ]] && out="${attempt}"
			# timeout(1) exits 124 when it kills the command: the mirror is
			# unreachable, so retrying the other names would only stall again.
			if [[ "${#limit[@]}" -gt 0 && "${rc}" -eq 124 ]]; then
				out="package install timed out after ${timeout_secs}s; the package mirror may be unreachable"
				break
			fi
		done

		# Surface the captured failure so a real problem (locked package db,
		# signature error, dead mirror) is visible instead of swallowed.
		if [[ -n "${out}" ]]; then
			log "buildx install did not succeed; package manager output:
${out}"
		fi
	elif [[ "${#pm[@]}" -eq 0 ]]; then
		log "no supported package manager (dnf/yum/apt-get) found; cannot auto-install buildx"
	else
		log "not running as root and cannot escalate via 'sudo -n'; cannot auto-install buildx"
	fi

	die "BuildKit is enabled but the buildx plugin is missing and could not be installed automatically.
  Install it (as root or with sudo) and re-run, for example:
    dnf install -y docker-buildx-plugin       # RHEL / OpenCloudOS (or: moby-buildx)
    apt-get install -y docker-buildx-plugin   # Debian / Ubuntu via Docker apt repo (stock repos: docker-buildx)
  macOS / Windows: install Docker Desktop, which bundles buildx.
  Buildx is required because the component Dockerfiles use 'COPY --chmod', a BuildKit-only feature.
  See https://docs.docker.com/go/buildx/"
}

docker_build() {
	local image="$1" dockerfile="$2" context="$3"
	shift 3
	require_path "${dockerfile}"
	require_path "${context}"
	log "building ${image}"
	docker build -t "${image}" -f "${dockerfile}" "$@" "${context}"
	if [[ "${PUSH}" == "1" ]]; then
		log "pushing ${image}"
		docker push "${image}"
	fi
}

build_cube_api() {
	docker_build "${CUBE_API_IMAGE}" \
		"${PKG_ROOT}/CubeAPI/Dockerfile" "${PKG_ROOT}/CubeAPI"
}

build_cube_master() {
	docker_build "${CUBE_MASTER_IMAGE}" \
		"${PKG_ROOT}/CubeMaster/Dockerfile" "${PKG_ROOT}/CubeMaster"
}

build_cube_proxy() {
	docker_build "${CUBE_PROXY_IMAGE}" \
		"${PKG_ROOT}/cubeproxy/build-context/Dockerfile" \
		"${PKG_ROOT}/cubeproxy/build-context"
}

build_cube_lifecycle_manager() {
	docker_build "${CUBE_LCM_IMAGE}" \
		"${PKG_ROOT}/cube-lifecycle-manager/build-context/Dockerfile" \
		"${PKG_ROOT}/cube-lifecycle-manager/build-context"
}

build_cube_webui() {
	docker_build "${CUBE_WEBUI_IMAGE}" \
		"${PKG_ROOT}/webui/Dockerfile.package" "${PKG_ROOT}/webui" \
		--build-arg "WEB_UI_UPSTREAM=${WEB_UI_UPSTREAM}"
}

main() {
	local -a targets=()
	local arg
	for arg in "$@"; do
		case "${arg}" in
		-h | --help) usage 0 ;;
		--push) PUSH=1 ;;
		all) targets+=(cube-api cube-master cube-proxy cube-lifecycle-manager webui) ;;
		cube-api | cubeapi) targets+=(cube-api) ;;
		cube-master | cubemaster | master) targets+=(cube-master) ;;
		cube-proxy | cubeproxy | proxy) targets+=(cube-proxy) ;;
		cube-lifecycle-manager | lifecycle-manager | lcm) targets+=(cube-lifecycle-manager) ;;
		webui | cube-webui) targets+=(webui) ;;
		*) die "unknown argument: ${arg} (run with --help)" ;;
		esac
	done

	if [[ "${#targets[@]}" -eq 0 ]]; then
		targets=(cube-api cube-master cube-proxy cube-lifecycle-manager webui)
	fi

	command -v docker >/dev/null 2>&1 || die "docker is required but was not found in PATH"
	ensure_buildx

	local t
	for t in "${targets[@]}"; do
		case "${t}" in
		cube-api) build_cube_api ;;
		cube-master) build_cube_master ;;
		cube-proxy) build_cube_proxy ;;
		cube-lifecycle-manager) build_cube_lifecycle_manager ;;
		webui) build_cube_webui ;;
		esac
	done

	log "done. built: ${targets[*]}$([[ "${PUSH}" == "1" ]] && echo " (pushed)")"
}

main "$@"
