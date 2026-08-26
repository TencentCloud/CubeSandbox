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
CUBE_PROXY_COMPOSE="${ONE_CLICK_DIR}/cubeproxy/docker-compose.yaml.template"
CUBE_PROXY_UP="${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh"
CUBE_DIAG_COLLECTOR="${ONE_CLICK_DIR}/scripts/cube-diag/collect-logs.sh"

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
  require_file "${ONE_CLICK_DIR}/CubeOps/Dockerfile" "cube-ops Dockerfile"
  require_file "${ONE_CLICK_DIR}/CubeMaster/Dockerfile" "cubemaster Dockerfile"
  require_file "${ONE_CLICK_DIR}/webui/Dockerfile.package" "cube-webui Dockerfile"
  # cube-proxy's build-context (and its Dockerfile) come from the CubeProxy source.
  require_file "${ROOT_DIR}/CubeProxy/Dockerfile" "cube-proxy Dockerfile (CubeProxy source)"
  require_file "${ROOT_DIR}/cube-lifecycle-manager/Dockerfile" "cube-lifecycle-manager Dockerfile"
  # webui nginx.conf is the canonical source for both the package and the
  # terraform webui-nginx.conf the addons render.
  require_file "${ONE_CLICK_DIR}/webui/nginx.conf" "webui nginx.conf source"
  # Shared volume-deps installer: must exist and be wired into the CubeMaster
  # package context (Dockerfile COPY expects it beside bin/cubemaster).
  require_file "${ROOT_DIR}/deploy/scripts/docker-install-volume-deps.sh" \
    "volume deps installer (single source)"
  if ! grep -q -F 'docker-install-volume-deps.sh' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must copy deploy/scripts/docker-install-volume-deps.sh into CubeMaster/"
  fi
  # Do not keep a checked-in duplicate under one-click CubeMaster/.
  if [[ -f "${ONE_CLICK_DIR}/CubeMaster/docker-install-volume-deps.sh" ]]; then
    fail "remove duplicate ${ONE_CLICK_DIR}/CubeMaster/docker-install-volume-deps.sh; use deploy/scripts/ only"
  fi
  # Bare-metal COS deps script ships next to the volume plugin binaries.
  require_file "${ROOT_DIR}/examples/volume/cos/install-deps.sh" \
    "COS volume install-deps.sh (examples source)"
  if ! grep -q -F 'examples/volume/cos/install-deps.sh' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must copy examples/volume/cos/install-deps.sh into CubeMaster/plugin and Cubelet/plugin"
  fi
  require_file "${ROOT_DIR}/examples/volume/s3/install-deps.sh" \
    "S3 volume install-deps.sh (examples source)"
  if ! grep -q -F 'install-s3-deps.sh' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must copy the S3 install-deps.sh into CubeMaster/plugin and Cubelet/plugin as install-s3-deps.sh"
  fi
  # The S3 plugin is a Go binary compiled at pack time, not a copied script.
  require_file "${ROOT_DIR}/examples/volume/s3/go.mod" "S3 volume plugin Go module"
  require_file "${ROOT_DIR}/examples/volume/s3/cmd/cube-volume-s3/main.go" \
    "S3 volume plugin entry point"
  if [[ -e "${ROOT_DIR}/examples/volume/s3/binary" ]]; then
    fail "examples/volume/s3/binary must be gone; the plugin is built from cmd/cube-volume-s3"
  fi
  if ! grep -q -F 'build_or_copy_go_binary' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must build Go binaries via build_or_copy_go_binary"
  fi
  if ! grep -q -F './cmd/cube-volume-s3' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must compile ./cmd/cube-volume-s3"
  fi
  if ! grep -q -F '${PACKAGE_ROOT}/CubeMaster/plugin/cube-volume-s3' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must install cube-volume-s3 into CubeMaster/plugin"
  fi
  if ! grep -q -F '${PACKAGE_ROOT}/Cubelet/plugin/cube-volume-s3' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must install cube-volume-s3 into Cubelet/plugin"
  fi

  # cube-volume-s3 must be statically linked (one-click hosts are Ubuntu 20.04 /
  # glibc 2.31). A global CGO_ENABLED=0 on build_go_binary would compile cubelet
  # without cubecow / nsenter.
  local helper
  helper="$(awk '/^build_go_binary\(\)/ {flag=1} flag {print} /^}$/ && flag {exit}' "${BUNDLE_SH}")"
  if ! printf '%s\n' "${helper}" | grep -E 'go build -trimpath' | grep -v 'CGO_ENABLED=0' >/dev/null; then
    fail "build_go_binary default path must not set CGO_ENABLED=0 (cubelet needs cgo)"
  fi
  if ! printf '%s\n' "${helper}" | grep -q 'CGO_ENABLED=0'; then
    fail "build_go_binary static_linux path must set CGO_ENABLED=0"
  fi
  if ! grep -A12 './cmd/cube-volume-s3' "${BUNDLE_SH}" | grep -q 'static_linux'; then
    fail "cube-volume-s3 must be built with static_linux (CGO_ENABLED=0)"
  fi
  if ! grep -q 'CGO_ENABLED=0.*go build' "${BUNDLE_SH}"; then
    fail "cube-volume-s3 compile must use CGO_ENABLED=0 (build_go_binary static_linux branch)"
  fi
  if ! grep -B2 './cmd/cube-volume-s3' \
        "${ROOT_DIR}/CubeMaster/docker/Dockerfile" | grep -q 'CGO_ENABLED=0'; then
    fail "CubeMaster Dockerfile must compile cube-volume-s3 with CGO_ENABLED=0"
  fi
  if ! grep -B2 './cmd/cube-volume-s3' \
        "${ROOT_DIR}/Cubelet/Dockerfile" | grep -q 'CGO_ENABLED=0'; then
    fail "Cubelet Dockerfile must compile cube-volume-s3 with CGO_ENABLED=0"
  fi

  # The plugin has a built-in S3 client, so nothing may reintroduce the AWS CLI:
  # it was ~100MB installed and ~60MB of zip inside the release bundle.
  if [[ -e "${ONE_CLICK_DIR}/lib/awscli-bundle.sh" ]]; then
    fail "lib/awscli-bundle.sh must be gone; the S3 plugin no longer needs the AWS CLI"
  fi
  if [[ -e "${ONE_CLICK_DIR}/assets/vendor/awscli" ]]; then
    fail "assets/vendor/awscli must be gone; the S3 plugin no longer needs the AWS CLI"
  fi
  if grep -qi 'awscli' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must not reference the AWS CLI"
  fi
  if grep -qi 'awscli' "${ROOT_DIR}/deploy/scripts/docker-install-volume-deps.sh"; then
    fail "docker-install-volume-deps.sh must not install the AWS CLI"
  fi
  if grep -q -- '--aws' "${ROOT_DIR}/examples/volume/s3/install-deps.sh"; then
    fail "S3 install-deps.sh must not offer --aws"
  fi
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
  if [[ "${built_n}" -ne 6 ]]; then
    fail "expected 6 component images in build_images.sh, found ${built_n}: $(echo "${built}" | tr '\n' ' ')"
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
  for t in __WEB_UI_UPSTREAM__ __SANDBOX_PROXY_UPSTREAM__ __CUBE_OPS_UPSTREAM__; do
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
    -e 's|^\(\s*listen \)9090\( http2 reuseport;\)|\1__CUBE_PROXY_GRPC_PORT__\2|' \
    -e 's|^\(\s*set \$host_proxy_port \)8081;|\1__CUBE_PROXY_HTTP_PORT__;|' \
    -e 's|^\(\s*set \$host_proxy_port \)8080;|\1__CUBE_PROXY_HTTPS_PORT__;|' \
    -e 's|^\(\s*listen \)127\.0\.0\.1:8082;|\1__CUBE_PROXY_ADMIN_LISTEN__:__CUBE_PROXY_ADMIN_PORT__;|' \
    -e 's|/usr/local/openresty/nginx/certs/cube\.app+3\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_CERT__|' \
    -e 's|/usr/local/openresty/nginx/certs/cube\.app+3-key\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_KEY__|' \
    "${src}" >"${tmp}"

  for token in __CUBE_PROXY_HTTP_PORT__ __CUBE_PROXY_HTTPS_PORT__ __CUBE_PROXY_GRPC_PORT__ __CUBE_PROXY_ADMIN_LISTEN__ __CUBE_PROXY_ADMIN_PORT__ __CUBE_PROXY_SSL_CERT__ __CUBE_PROXY_SSL_KEY__; do
    grep -q -F "${token}" "${tmp}" || fail "cube-proxy nginx template generation is missing ${token}; CubeProxy/nginx.conf may have changed"
  done
  rm -f "${tmp}"
}

# 3d) one-click cube-proxy logs must use the same host-visible /data/log
#     contract as the Kubernetes deployment and the other runtime components.
test_cubeproxy_host_log_wiring() {
  grep -q -F -- '- /data/log/cube-proxy:/data/log/cube-proxy' "${CUBE_PROXY_COMPOSE}" \
    || fail "cube-proxy compose template does not bind-mount the host log directory"
  grep -q -F 'CUBE_PROXY_LOG_DIR="/data/log/cube-proxy"' "${CUBE_PROXY_UP}" \
    || fail "up-cube-proxy.sh does not define the host log directory"
  grep -q -F 'mkdir -p "${CUBE_PROXY_LOG_DIR}"' "${CUBE_PROXY_UP}" \
    || fail "up-cube-proxy.sh does not create the host log directory"
  grep -q -F '_collect_data_log_dir "${DATA_LOG_DIR}/cube-proxy"' "${CUBE_DIAG_COLLECTOR}" \
    || fail "diagnostic collector does not read cube-proxy logs from the host"
}

# 3e) The TKE addon ConfigMap embeds a cube_box_req_template whose default egress
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

# 3f) Reinstall first removes packaged component directories, then lays the new
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

# 3g) Build-machine knobs live in build.env.example; the shipped env.example is
#     deploy-only. The release bundle must copy env.example and must not ship
#     build.env.example.
test_env_templates_are_split() {
  local env_example="${ONE_CLICK_DIR}/env.example"
  local build_example="${ONE_CLICK_DIR}/build.env.example"
  require_file "${env_example}" "deploy env.example"
  require_file "${build_example}" "build.env.example"

  if grep -E '^[[:space:]]*(#[[:space:]]*)?ONE_CLICK_[A-Z0-9_]+_BUILD_MODE=' "${env_example}" >/dev/null; then
    fail "env.example must not contain ONE_CLICK_*_BUILD_MODE keys"
  fi
  if grep -E '^[[:space:]]*(#[[:space:]]*)?ONE_CLICK_[A-Z0-9_]+_BIN=' "${env_example}" >/dev/null; then
    fail "env.example must not contain ONE_CLICK_*_BIN keys"
  fi

  grep -q '^ONE_CLICK_CUBEMASTER_BUILD_MODE=' "${build_example}" \
    || fail "build.env.example missing ONE_CLICK_CUBEMASTER_BUILD_MODE"
  grep -q 'ONE_CLICK_CUBEMASTER_BIN=' "${build_example}" \
    || fail "build.env.example missing ONE_CLICK_CUBEMASTER_BIN"
  grep -q 'ONE_CLICK_MKCERT_BIN=' "${build_example}" \
    || fail "build.env.example missing ONE_CLICK_MKCERT_BIN"
  grep -q 'ONE_CLICK_VOLUME_S3_BIN=' "${build_example}" \
    || fail "build.env.example missing ONE_CLICK_VOLUME_S3_BIN"
  grep -q 'ONE_CLICK_WEB_DIST_DIR=' "${build_example}" \
    || fail "build.env.example missing ONE_CLICK_WEB_DIST_DIR"
  if grep -qi 'awscli' "${build_example}"; then
    fail "build.env.example must not reference the AWS CLI"
  fi

  grep -q 'copy_file "${SCRIPT_DIR}/env.example"' "${BUNDLE_SH}" \
    || fail "build-release-bundle.sh must copy env.example into DIST_ROOT"
  if grep -q 'build.env.example' "${BUNDLE_SH}"; then
    fail "build-release-bundle.sh must not ship build.env.example"
  fi
}

# 4) The build entrypoints AND every shipped Terraform deployer script must at
#    least be syntactically valid — a cheap, cloud-free guard so a broken script
#    fails here instead of only when a user runs it from the bundle.
test_build_scripts_parse() {
  local f
  for f in "${BUNDLE_SH}" "${BUILD_IMAGES_SH}" \
    "${ROOT_DIR}/examples/volume/s3/install-deps.sh" \
    "${TF_DIR}/create.sh" "${TF_DIR}/destroy.sh" \
    "${TF_DIR}/lib-phases.sh" "${TF_DIR}/lib-state-sync.sh" "${TF_DIR}/validate.sh"; do
    bash -n "${f}" || fail "syntax error in ${f}"
  done
}

test_component_build_inputs_exist
test_image_names_match
test_webui_nginx_placeholders
test_cubeproxy_nginx_template_generation
test_cubeproxy_host_log_wiring
test_tke_addons_network_config_key
test_reinstall_cleanup_tracks_packaged_components
test_terraform_deployer_files_present
test_env_templates_are_split
test_build_scripts_parse

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} package-layout test(s) failed" >&2
  exit 1
fi

echo "package layout tests OK"
