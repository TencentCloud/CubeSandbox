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
printf 'ID="rhel"\nID_LIKE="rhel centos fedora"\n' >"${tmp}/rhel-release"
# ID does not name OpenCloudOS, only ID_LIKE does.
printf 'ID="tencentos"\nID_LIKE="centos rhel opencloudos"\n' >"${tmp}/id-like-release"

[[ "$(host_os_release_field ID "${tmp}/opencloudos-release")" == "opencloudos" ]] \
  || die "quoted ID not stripped"
host_is_opencloudos "${tmp}/opencloudos-release" || die "opencloudos not detected"
host_is_opencloudos "${tmp}/id-like-release" || die "ID_LIKE opencloudos not detected"
host_is_opencloudos "${tmp}/rhel-release" && die "rhel wrongly detected"
host_is_opencloudos "${tmp}/missing" && die "missing os-release wrongly detected"

# --- OpenCloudOS hint --------------------------------------------------------
out="$(die_yum_docker_packages_missing "docker docker-compose" "${tmp}/opencloudos-release" 2>&1)" \
  && die "missing packages should abort"
grep -Fq "failed to install docker docker-compose with yum" <<<"${out}" \
  || die "attempted packages missing from the error"
grep -Fq "epol-release" <<<"${out}" || die "OpenCloudOS epol hint missing"
grep -Fq "sudo dnf install -y docker docker-compose" <<<"${out}" \
  || die "OpenCloudOS remediation missing the attempted packages"

# A compose-only failure must not claim docker failed as well (review on #2010).
out="$(die_yum_docker_packages_missing "docker-compose" "${tmp}/opencloudos-release" 2>&1)" \
  && die "compose-only failure should abort"
grep -Fq "failed to install docker-compose with yum" <<<"${out}" \
  || die "compose-only error should name only docker-compose"
grep -Fq "failed to install docker and docker-compose" <<<"${out}" \
  && die "compose-only error wrongly claims docker failed"
grep -Fq "sudo dnf install -y docker-compose" <<<"${out}" \
  || die "compose-only remediation should name only docker-compose"

# --- Generic hint for non-OpenCloudOS ---------------------------------------
out="$(die_yum_docker_packages_missing "docker docker-compose" "${tmp}/rhel-release" 2>&1)" \
  && die "missing packages should abort on a non-OpenCloudOS host"
grep -Fq "failed to install docker docker-compose with yum" <<<"${out}" \
  || die "generic error missing the attempted packages"
grep -Fq "epol-release" <<<"${out}" && die "EPOL hint leaked to a non-OpenCloudOS host"

# --- yum failure behavior -----------------------------------------------------
# Only the stub yum + tools the hint needs are on PATH, so host docker cannot
# short-circuit and systemctl running would prove the failure leaked through.
bin="${tmp}/bin"; mkdir "${bin}"
for t in cat grep head; do ln -s "$(command -v "${t}")" "${bin}/${t}"; done
printf '#!/bin/sh\necho "No match for argument: docker-compose" >&2\nexit 1\n' >"${bin}/yum"
printf '#!/bin/sh\necho "systemctl should not run" >&2\nexit 0\n' >"${bin}/systemctl"
chmod +x "${bin}/yum" "${bin}/systemctl"

out="$(PATH="${bin}" install_docker 2>&1)" && die "install_docker should fail on yum failure"
grep -Fq "failed to install docker docker-compose with yum" <<<"${out}" \
  || die "generic failure hint missing"
grep -Fq "systemctl should not run" <<<"${out}" && die "systemctl ran after yum failure"

# docker is deliberately absent from PATH (so `docker compose version` fails)
# and only docker-compose is requested; the hint must name just that package.
out="$(PATH="${bin}" install_docker_compose 2>&1)" && die "install_docker_compose should fail on yum failure"
grep -Fq "failed to install docker-compose with yum" <<<"${out}" \
  || die "compose install failure should name only docker-compose"
grep -Fq "failed to install docker and docker-compose" <<<"${out}" \
  && die "compose install failure wrongly claims docker failed"

echo "yum docker hint tests OK"
