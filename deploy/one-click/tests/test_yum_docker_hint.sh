#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

ONE_CLICK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

die() { echo "FAIL: $*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# --- OS detection -----------------------------------------------------------
printf 'ID="opencloudos"\n' >"${tmp}/opencloudos-release"
printf 'ID="rhel"\n' >"${tmp}/rhel-release"

[[ "$(host_os_release_field ID "${tmp}/opencloudos-release")" == "opencloudos" ]] \
  || die "quoted ID not stripped"
host_is_opencloudos "${tmp}/opencloudos-release" || die "opencloudos not detected"
host_is_opencloudos "${tmp}/rhel-release" && die "rhel wrongly detected"
host_is_opencloudos "${tmp}/missing" && die "missing os-release wrongly detected"

# --- OpenCloudOS hint --------------------------------------------------------
out="$(die_yum_docker_packages_missing "${tmp}/opencloudos-release" 2>&1)" \
  && die "missing packages should abort"
grep -Fq "epol-release" <<<"${out}" || die "OpenCloudOS epol hint missing"

# --- yum failure behavior -----------------------------------------------------
# Only the stub yum + tools the hint needs are on PATH, so host docker cannot
# short-circuit and systemctl running would prove the failure leaked through.
bin="${tmp}/bin"; mkdir "${bin}"
for t in cat grep head; do ln -s "$(command -v "${t}")" "${bin}/${t}"; done
printf '#!/bin/sh\necho "No match for argument: docker-compose" >&2\nexit 1\n' >"${bin}/yum"
printf '#!/bin/sh\necho "systemctl should not run" >&2\nexit 0\n' >"${bin}/systemctl"
chmod +x "${bin}/yum" "${bin}/systemctl"

out="$(PATH="${bin}" install_docker 2>&1)" && die "install_docker should fail on yum failure"
grep -Fq "failed to install docker and docker-compose with yum" <<<"${out}" \
  || die "generic failure hint missing"
grep -Fq "systemctl should not run" <<<"${out}" && die "systemctl ran after yum failure"

out="$(PATH="${bin}" install_docker_compose 2>&1)" && die "install_docker_compose should fail on yum failure"

echo "yum docker hint tests OK"
