#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Lightweight, cloud-free dry run of the one-click release-bundle / image-build
# layout. The quickstart and TKE deployment depend on build-release-bundle.sh
# laying the package out exactly where build_images.sh and the Terraform addons
# expect it; a rename or moved Dockerfile there silently breaks bundle/image
# builds. This test catches that drift statically (no docker, no terraform, no
# cloud) so it can run on every PR that touches the bundle/build inputs.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ROOT_DIR="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"
TF_DIR="${ONE_CLICK_DIR}/terraform/tencentcloud"
BUNDLE_SH="${ONE_CLICK_DIR}/build-release-bundle.sh"
BUILD_IMAGES_SH="${TF_DIR}/build_images.sh"
TKE_ADDONS_TF="${TF_DIR}/tke-addons.tf"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

require_file() {
  [[ -f "$1" ]] || fail "${2:-required file} missing: $1"
}

# 1) The component Dockerfiles/contexts that build_images.sh builds from must
#    exist in the source tree build-release-bundle.sh assembles the package from.
test_component_build_inputs_exist() {
  require_file "${ONE_CLICK_DIR}/CubeAPI/Dockerfile" "cube-api Dockerfile"
  require_file "${ONE_CLICK_DIR}/CubeMaster/Dockerfile" "cubemaster Dockerfile"
  require_file "${ONE_CLICK_DIR}/webui/Dockerfile.package" "cube-webui Dockerfile"
  # cube-proxy's build-context (and its Dockerfile) come from the CubeProxy source.
  require_file "${ROOT_DIR}/CubeProxy/Dockerfile" "cube-proxy Dockerfile (CubeProxy source)"
  require_file "${ROOT_DIR}/cube-lifecycle-manager/Dockerfile" "cube-lifecycle-manager Dockerfile"
  # webui nginx.conf is the canonical source for both the package and the
  # terraform webui-nginx.conf the addons render.
  require_file "${ONE_CLICK_DIR}/webui/nginx.conf" "webui nginx.conf source"
}

# 2) The component image base names must match between what build_images.sh
#    builds/pushes and what the addon Terraform composes/deploys. If they drift,
#    create.sh pushes one ref and TKE pulls another → ImagePullBackOff.
extract_image_names() {
  # Print the <name> in .../<name>:<tag> for each component image line, sorted.
  # `|| true` so a no-match (which trips `pipefail`) doesn't abort the caller
  # before its own "could not extract ..." diagnostic runs.
  local file="$1" pattern="$2"
  { grep -E "${pattern}" "${file}" | sed -E 's#.*/([a-z0-9-]+):.*#\1#' | sort -u; } || true
}

test_image_names_match() {
  local built composed
  # Match ANY component (not a fixed API|MASTER|PROXY|WEBUI list) so adding a 5th
  # component image on one side but not the other is caught as drift. The `^`
  # anchor on build_images.sh skips its `#   CUBE_*_IMAGE=...` comment header.
  built="$(extract_image_names "${BUILD_IMAGES_SH}" '^CUBE_[A-Z0-9]+_IMAGE=')"
  composed="$(extract_image_names "${TKE_ADDONS_TF}" 'cube_[a-z0-9]+_image[[:space:]]*=')"

  if [[ -z "${built}" ]]; then
    fail "could not extract image names from build_images.sh"
  fi
  if [[ -z "${composed}" ]]; then
    fail "could not extract image names from tke-addons.tf"
  fi
  # Guard against a regex that silently matches too few/many lines.
  local built_n
  built_n="$(printf '%s\n' "${built}" | grep -c .)"
  if [[ "${built_n}" -ne 5 ]]; then
    fail "expected 5 component images in build_images.sh, found ${built_n}: $(echo "${built}" | tr '\n' ' ')"
  fi
  if [[ "${built}" != "${composed}" ]]; then
    fail "image name drift between build_images.sh and tke-addons.tf:
build_images.sh: $(echo "${built}" | tr '\n' ' ')
tke-addons.tf:   $(echo "${composed}" | tr '\n' ' ')"
  fi
}

# 3) Every Terraform deployer file the bundle promises must exist in source, and
#    the bundle must ship the sourced helper modules (create.sh sources
#    lib-state-sync.sh and lib-phases.sh at runtime, so a missing copy would make
#    the extracted deployer fail to start).
test_terraform_deployer_files_present() {
  local f
  for f in create.sh destroy.sh build_images.sh lib-state-sync.sh lib-phases.sh \
    main.tf variables.tf tke-addons.tf query_outputs.tf; do
    require_file "${TF_DIR}/${f}" "terraform deployer file"
  done

  # build-release-bundle.sh must explicitly verify (ensure_file) the helper
  # modules so a renamed/emptied module fails the build loudly instead of
  # producing a deployer that can't source its dependencies. It ships two copies
  # (sandbox-package + top-level), so each module name must appear at least twice.
  local module count
  for module in lib-state-sync.sh lib-phases.sh; do
    count="$(grep -c -F "${module}" "${BUNDLE_SH}" || true)"
    if [[ "${count}" -lt 2 ]]; then
      fail "build-release-bundle.sh must verify ${module} for both bundle copies (found ${count} reference(s))"
    fi
  done
}

# 3b) The cube-webui nginx config the addon renders from webui/nginx.conf MUST
#     contain both upstream placeholders. tke-addons.tf string-replaces them; if a
#     token is renamed upstream the replace silently leaves the literal and nginx
#     rejects it at runtime ("invalid URL prefix"). Catch that drift statically.
test_webui_nginx_placeholders() {
  local f="${ONE_CLICK_DIR}/webui/nginx.conf" t
  [[ -f "${f}" ]] || { fail "webui nginx.conf missing: ${f}"; return; }
  for t in __WEB_UI_UPSTREAM__ __SANDBOX_PROXY_UPSTREAM__; do
    grep -q -F "${t}" "${f}" || fail "webui/nginx.conf is missing placeholder ${t} (tke-addons.tf expects it)"
  done
}

# 3c) The Terraform cube-proxy ConfigMap consumes a placeholder-rendered copy of
#     CubeProxy/nginx.conf. Guard the sed-based template generation so an upstream
#     listen/cert path format change fails in CI instead of leaving the admin
#     listener on 127.0.0.1 or a literal placeholder in the rendered ConfigMap.
test_cubeproxy_nginx_template_generation() {
  local src="${ROOT_DIR}/CubeProxy/nginx.conf"
  local tmp token
  [[ -f "${src}" ]] || { fail "CubeProxy nginx.conf missing: ${src}"; return; }

  tmp="$(mktemp)"
  sed \
    -e 's|^worker_processes [0-9]\+;|worker_processes auto;|' \
    -e 's|^\(\s*listen \)8081\( reuseport;\)|\1__CUBE_PROXY_HTTP_PORT__\2|' \
    -e 's|^\(\s*listen \)8080\( ssl reuseport;\)|\1__CUBE_PROXY_HTTPS_PORT__\2|' \
    -e 's|^\(\s*set \$host_proxy_port \)8081;|\1__CUBE_PROXY_HTTP_PORT__;|' \
    -e 's|^\(\s*set \$host_proxy_port \)8080;|\1__CUBE_PROXY_HTTPS_PORT__;|' \
    -e 's|^\(\s*listen \)127\.0\.0\.1:8082;|\1__CUBE_PROXY_ADMIN_LISTEN__:8082;|' \
    -e 's|/usr/local/openresty/nginx/certs/cube\.app+3\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_CERT__|' \
    -e 's|/usr/local/openresty/nginx/certs/cube\.app+3-key\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_KEY__|' \
    "${src}" >"${tmp}"

  for token in __CUBE_PROXY_HTTP_PORT__ __CUBE_PROXY_HTTPS_PORT__ __CUBE_PROXY_ADMIN_LISTEN__ __CUBE_PROXY_SSL_CERT__ __CUBE_PROXY_SSL_KEY__; do
    grep -q -F "${token}" "${tmp}" || fail "cube-proxy nginx template generation is missing ${token}; CubeProxy/nginx.conf may have changed"
  done
  rm -f "${tmp}"
}

# 3d) The TKE addon ConfigMap embeds a cube_box_req_template whose default egress
#     policy MUST sit under the "cube_network_config" JSON key — the only key
#     CubeMaster deserializes (CreateCubeSandboxReq.CubeNetworkConfig). The legacy
#     "cubevs_context" key is silently dropped, so the denyOut policy would never
#     apply. A Go regression test guards the shipped YAML configs; guard the .tf
#     template here, statically and cloud-free.
test_tke_addons_network_config_key() {
  local f="${TKE_ADDONS_TF}"
  [[ -f "${f}" ]] || { fail "tke-addons.tf missing: ${f}"; return; }
  # Match the escaped-quote JSON-key forms (\"key\":) so a prose mention of the
  # key in a comment never trips either check.
  if ! grep -q -F '\"cube_network_config\":' "${f}"; then
    fail "tke-addons.tf cube_box_req_template is missing the cube_network_config key"
  fi
  if grep -q -F '\"cubevs_context\":' "${f}"; then
    fail "tke-addons.tf uses the stale cubevs_context key (CubeMaster only reads cube_network_config)"
  fi
}

# 3e) Reinstall first removes packaged component directories, then lays the new
#     package down. Guard that list against drifting when build-release-bundle.sh
#     adds a new top-level package component.
extract_package_root_dirs() {
  { grep -oE '\$\{PACKAGE_ROOT\}/[^"[:space:]]+' "${BUNDLE_SH}" |
    sed -E 's#.*\$\{PACKAGE_ROOT\}/([^/"]+).*#\1#' |
    sort -u; } || true
}

extract_reinstall_cleanup_dirs() {
  { sed -n '/^rm -rf \\/,/^$/p' "${ONE_CLICK_DIR}/install.sh" |
    grep -oE '\$\{INSTALL_PREFIX\}/[^"[:space:]]+' |
    sed -E 's#.*\$\{INSTALL_PREFIX\}/([^/"]+).*#\1#' |
    sort -u; } || true
}

is_reinstall_cleanup_exception() {
  case "$1" in
    # Runtime data/object directories are intentionally preserved across reinstall.
    cube-snapshot|cube-vs)
      return 0
      ;;
    # The bundled Tencent Cloud deployer may hold local terraform state if an
    # operator runs it from an installed tree instead of the extracted bundle.
    terraform)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

test_reinstall_cleanup_tracks_packaged_components() {
  local packaged cleaned dir
  local missing=()
  packaged="$(extract_package_root_dirs)"
  cleaned="$(extract_reinstall_cleanup_dirs)"

  if [[ -z "${packaged}" ]]; then
    fail "could not extract package-root directories from build-release-bundle.sh"
  fi
  if [[ -z "${cleaned}" ]]; then
    fail "could not extract reinstall cleanup directories from install.sh"
  fi

  while IFS= read -r dir; do
    [[ -n "${dir}" ]] || continue
    is_reinstall_cleanup_exception "${dir}" && continue
    if ! grep -qxF "${dir}" <<<"${cleaned}"; then
      missing+=("${dir}")
    fi
  done <<<"${packaged}"

  if [[ "${#missing[@]}" -gt 0 ]]; then
    fail "install.sh reinstall cleanup is missing packaged component dir(s): ${missing[*]}"
  fi
}

# 4) The build entrypoints AND every shipped Terraform deployer script must at
#    least be syntactically valid — a cheap, cloud-free guard so a broken script
#    fails here instead of only when a user runs it from the bundle.
test_build_scripts_parse() {
  local f
  for f in "${BUNDLE_SH}" "${BUILD_IMAGES_SH}" \
    "${TF_DIR}/create.sh" "${TF_DIR}/destroy.sh" \
    "${TF_DIR}/lib-phases.sh" "${TF_DIR}/lib-state-sync.sh" "${TF_DIR}/validate.sh"; do
    bash -n "${f}" || fail "syntax error in ${f}"
  done
}

test_component_build_inputs_exist
test_image_names_match
test_webui_nginx_placeholders
test_cubeproxy_nginx_template_generation
test_tke_addons_network_config_key
test_reinstall_cleanup_tracks_packaged_components
test_terraform_deployer_files_present
test_build_scripts_parse

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} package-layout test(s) failed" >&2
  exit 1
fi

echo "package layout tests OK"
