#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
OBS_INSTALLER="${ONE_CLICK_DIR}/observability/install.sh"
OTEL_INSTALLER="${ONE_CLICK_DIR}/observability/otelcol/install.sh"
TF_DIR="${ONE_CLICK_DIR}/terraform/tencentcloud"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

help_output="$(bash "${OBS_INSTALLER}" --help 2>&1)"
if grep -Eq '^[[:space:]]+terraform([[:space:]]|$)' <<<"${help_output}"; then
  fail "Terraform must not be listed as an observability installer command"
fi
if bash "${OBS_INSTALLER}" terraform >"${TMP_DIR}/terraform.out" 2>&1; then
  fail "Terraform observability command must be rejected"
fi
grep -Fq 'unknown command: terraform' "${TMP_DIR}/terraform.out" || \
  fail "Terraform observability command did not report unsupported command"

for file in "${TF_DIR}/create.sh" "${TF_DIR}/env.example"; do
  if grep -Eq 'TENCENTCLOUD_ENABLE_OBSERVABILITY|CUBE_TERRAFORM_INSTALL_BACKEND|CUBE_TERRAFORM_DIR' "${file}"; then
    fail "Terraform deployment still configures observability in ${file}"
  fi
done
echo "OK observability install scope excludes Terraform"

# Observability ships exactly two install paths: one-click/systemd and Helm.
REPO_ROOT="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"
if [[ -e "${REPO_ROOT}/deploy/kubernetes/observability" ]]; then
  fail "deploy/kubernetes/observability must stay removed; Helm is the Kubernetes install path"
fi
scan_roots=(
  "${REPO_ROOT}/docs"
  "${REPO_ROOT}/deploy"
  "${REPO_ROOT}/CubeObservability"
)
# CI workflows live in a nested checkout; include them so stale references to the
# removed installer cannot survive here while breaking the pipeline.
for workflow_dir in "${REPO_ROOT}/.github" "${REPO_ROOT}/CubeSandbox/.github"; do
  [[ -d "${workflow_dir}" ]] && scan_roots+=("${workflow_dir}")
done
if grep -rln --exclude="$(basename "${BASH_SOURCE[0]}")" 'deploy/kubernetes/observability' \
  "${scan_roots[@]}" 2>/dev/null; then
  fail "the standalone Kubernetes observability installer is still referenced"
fi
echo "OK observability install paths are one-click/systemd and Helm only"

grafana_output="${TMP_DIR}/grafana-config"
if env -i PATH="${PATH}" HOME="${HOME:-/tmp}" \
  CUBE_GRAFANA_CONFIG_OUTPUT_DIR="${grafana_output}" \
  bash "${ONE_CLICK_DIR}/observability/grafana/install.sh" render \
  >"${TMP_DIR}/grafana-default.out" 2>&1; then
  fail "Grafana render must reject an unset admin password"
fi
grep -Fq 'set CUBE_GRAFANA_ADMIN_PASSWORD' "${TMP_DIR}/grafana-default.out" || \
  fail "Grafana missing-password error was not clear"

if env -i PATH="${PATH}" HOME="${HOME:-/tmp}" \
  CUBE_GRAFANA_ADMIN_PASSWORD=short \
  CUBE_GRAFANA_CONFIG_OUTPUT_DIR="${grafana_output}" \
  bash "${ONE_CLICK_DIR}/observability/grafana/install.sh" render \
  >"${TMP_DIR}/grafana-short.out" 2>&1; then
  fail "Grafana render must reject short admin passwords"
fi

if env -i PATH="${PATH}" HOME="${HOME:-/tmp}" \
  CUBE_GRAFANA_ADMIN_PASSWORD='this-is-a-test-password-12345' \
  CUBE_GRAFANA_CONFIG_OUTPUT_DIR="${grafana_output}" \
  bash "${ONE_CLICK_DIR}/observability/grafana/install.sh" render \
  >"${TMP_DIR}/grafana-strong.out" 2>&1; then
  :
else
  fail "Grafana render rejected a 16+ character password"
fi
if mode="$(stat -c '%a' "${grafana_output}/grafana.ini" 2>/dev/null)"; then
  :
else
  mode="$(stat -f '%Lp' "${grafana_output}/grafana.ini")"
fi
[[ "${mode}" == "600" ]] || fail "rendered Grafana config mode is ${mode}, expected 600"
echo "OK Grafana requires a long password and renders secret config privately"
for dashboard in cubesandbox-cluster cubesandbox-node cubesandbox-sandbox; do
  [[ -s "${grafana_output}/dashboards/${dashboard}.json" ]] || fail "missing provisioned ${dashboard}"
done
[[ ! -e "${grafana_output}/dashboards/cubesandbox-host-sandbox.json" ]] || fail "legacy metrics dashboard was retained"
echo "OK cluster, node, and sandbox metrics dashboards are provisioned"

# INI treats unquoted '#'/';' as inline comments; an emptied admin_password
# silently falls back to Grafana's default "admin". The password must reach
# Grafana byte-for-byte through $__file{} and never appear in grafana.ini.
for password in '#leading-hash-password-123' ';leading-semicolon-pass-123' \
  'middle#hash;semicolon-pass' 'quote"single'"'"'back\slash$HOME${X}'; do
  out="${TMP_DIR}/grafana-special"
  rm -rf "${out}"
  env -i PATH="${PATH}" HOME="${HOME:-/tmp}" \
    CUBE_GRAFANA_ADMIN_PASSWORD="${password}" \
    CUBE_GRAFANA_CONFIG_OUTPUT_DIR="${out}" \
    bash "${ONE_CLICK_DIR}/observability/grafana/install.sh" render >/dev/null 2>&1 || \
    fail "Grafana render rejected a valid password containing special characters"
  if grep -Fq -- "${password}" "${out}/grafana.ini"; then
    fail "Grafana admin password must not be written into grafana.ini"
  fi
  grep -Fxq "admin_password = \$__file{${out}/admin_password}" "${out}/grafana.ini" || \
    fail "grafana.ini must read the admin password through \$__file{}"
  [[ "$(cat "${out}/admin_password"; printf x)" == "${password}x" ]] || \
    fail "Grafana admin password file does not match the configured password byte-for-byte"
  if mode="$(stat -c '%a' "${out}/admin_password" 2>/dev/null)"; then :; else
    mode="$(stat -f '%Lp' "${out}/admin_password")"
  fi
  [[ "${mode}" == "600" ]] || fail "Grafana admin password file mode is ${mode}, expected 600"
done
if env -i PATH="${PATH}" HOME="${HOME:-/tmp}" \
  CUBE_GRAFANA_ADMIN_PASSWORD=' padded-password-12345 ' \
  CUBE_GRAFANA_CONFIG_OUTPUT_DIR="${TMP_DIR}/grafana-padded" \
  bash "${ONE_CLICK_DIR}/observability/grafana/install.sh" render >/dev/null 2>&1; then
  fail "Grafana render must reject passwords with surrounding whitespace (\$__file{} trims them)"
fi
echo "OK Grafana admin passwords with INI-special characters are preserved"

# /etc/cube-sandbox is shared by services running as different users. No
# installer may use it as its own (restricted) config dir, and any installer
# that touches it must keep it 0755.
for installer in "${ONE_CLICK_DIR}"/observability/*/install.sh; do
  if grep -Eq '^CONFIG_DIR="/etc/cube-sandbox/?"$' "${installer}"; then
    fail "${installer#"${ONE_CLICK_DIR}/"} must keep its config under /etc/cube-sandbox/<component>"
  fi
  if grep -E 'install -d|chmod|chown' "${installer}" |
     grep -E 'SHARED_CONFIG_ROOT|dirname "\$\{CONFIG_DIR\}"' |
     grep -Ev 'install -d -m 0755 -o root -g root'; then
    fail "${installer#"${ONE_CLICK_DIR}/"} must keep /etc/cube-sandbox at 0755 root:root"
  fi
done
echo "OK shared /etc/cube-sandbox stays traversable for every service user"

rw_dir="${TMP_DIR}/remote-write-only"
mkdir -p "${rw_dir}"
(
  cd "${rw_dir}"
  env -i PATH="${PATH}" HOME="${HOME:-/tmp}" \
    CUBE_SANDBOX_NODE_IP=127.0.0.1 \
    CUBE_GRAFANA_ADMIN_PASSWORD='this-is-a-test-password-12345' \
    CUBE_METRICS_REMOTE_WRITE_URL=http://127.0.0.1:9092/api/v1/write \
    bash "${OBS_INSTALLER}" render all >"${TMP_DIR}/remote-write.out" 2>&1
) || fail "render all with only CUBE_METRICS_REMOTE_WRITE_URL must not report mutually exclusive endpoints"
grep -Fq 'prometheus_remote_write:' "${rw_dir}/cubesandbox-otelcol.yaml" || \
  fail "remote-write-only render did not configure the Remote Write exporter"
if grep -Fq 'otlp_http/metrics' "${rw_dir}/cubesandbox-otelcol.yaml"; then
  fail "remote-write-only render must not also add the default OTLP metrics exporter"
fi
echo "OK remote-write-only metrics do not inherit the default OTLP endpoint"

output="${TMP_DIR}/otel config.yaml"
printf 'previous config\n' > "${output}"
chmod 0644 "${output}"
env -i \
  PATH="${PATH}" \
  HOME="${HOME:-/tmp}" \
  CUBE_SANDBOX_NODE_IP=127.0.0.1 \
  CUBE_OTEL_CONFIG_OUTPUT="${output}" \
  CUBE_METRICS_OTLP_HTTP_ENDPOINT=http://127.0.0.1:9092/api/v1/otlp \
  CUBE_METRICS_AUTHORIZATION='Bearer test-secret' \
  CUBE_LOGS_OTLP_HTTP_ENDPOINT=http://127.0.0.1:3100/otlp \
  bash "${OTEL_INSTALLER}" render >/dev/null 2>&1

if mode="$(stat -c '%a' "${output}" 2>/dev/null)"; then
  :
else
  mode="$(stat -f '%Lp' "${output}")"
fi
[[ "${mode}" == "600" ]] || fail "rendered Collector config mode is ${mode}, expected 600"
grep -Fq 'Bearer test-secret' "${output}" || fail "rendered config omitted test auth header"
echo "OK rendered Collector credentials are written with mode 0600"
