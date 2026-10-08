#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Guard for the terraform twin of the helm chart's trustedCACerts feature
# (tke-addons.tf: merge-ca init container + ConfigMap + SSL_CERT_FILE).
#
# Two failure modes this test pins down:
#
# 1. Script drift: the terraform merge-ca args are a byte-for-byte copy of the
#    chart's (comments excepted). The two must stay 1:1 -- a fix landing in
#    one copy silently leaves the other deployment path vulnerable. The
#    comment in tke-addons.tf says "keep 1:1 when changing either"; this test
#    enforces it by comparing the executable (non-comment, non-blank) lines.
#
# 2. Runtime behavior: the extracted terraform script is replayed under
#    `sh -e` with rewritten paths (same harness as the chart guard), asserting
#    the happy path and the fail-closed rejections. No helm/terraform needed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
TF_FILE="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/tke-addons.tf"
CHART_TEMPLATE="${REPO_ROOT}/deploy/kubernetes/chart/templates/templatecenter.yaml"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# --- 1. Chart <-> terraform script sync ------------------------------------
# Executable lines only: the two copies deliberately differ in comments (each
# names its own validation layer), everything that runs must be identical.

# terraform side: heredoc body between the MERGE_CA_SCRIPT markers, common
# indent stripped the way terraform's <<- would, comments/blanks dropped.
awk '/<<-MERGE_CA_SCRIPT$/{f=1; next} /^ *MERGE_CA_SCRIPT$/{f=0} f' "${TF_FILE}" \
  | sed -e 's/^              //' \
  | grep -vE '^\s*(#|$)' >"${TMP_DIR}/tf-script.sh"
[ -s "${TMP_DIR}/tf-script.sh" ] || fail "could not extract the merge-ca script from ${TF_FILE}"

# chart side: args block of the merge-ca init container in the rendered-shape
# template source (between `args:` and `volumeMounts:`), minus the `|-` line,
# comments/blanks dropped.
awk '
  /^        - name: merge-ca$/ { inmc = 1 }
  /^      containers:/         { inmc = 0; inargs = 0 }
  inmc && /^          args:$/           { inargs = 1; next }
  inmc && inargs && /^          volumeMounts:$/ { inargs = 0 }
  inmc && inargs { print }
' "${CHART_TEMPLATE}" \
  | sed -e 's/^            - |-$//' -e 's/^              //' \
  | grep -vE '^\s*(#|$)' >"${TMP_DIR}/chart-script.sh"
[ -s "${TMP_DIR}/chart-script.sh" ] || fail "could not extract the merge-ca args from ${CHART_TEMPLATE}"

if ! diff -u "${TMP_DIR}/chart-script.sh" "${TMP_DIR}/tf-script.sh" >"${TMP_DIR}/sync.diff" 2>&1; then
  fail "merge-ca scripts have drifted between the chart and the terraform twin (the comment in tke-addons.tf says keep 1:1):"
  sed 's/^/    /' "${TMP_DIR}/sync.diff" >&2
fi

# 1b. The plan-time validation in variables.tf must use the same anchored
#     regexes as the chart's validate.yaml and the runtime awk -- unanchored
#     regexall calls admit marker junk and indented markers that the pod-start
#     check then rejects with a different (misleading) diagnosis.
TF_VARS="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/variables.tf"
grep -q 'regexall("(?m)\^-----BEGIN CERTIFICATE-----\[\[:space:\]\]\*\$"' "${TF_VARS}" \
  || fail "variables.tf validation must anchor the BEGIN marker regex like validate.yaml"
grep -q 'regexall("(?m)\^-----END CERTIFICATE-----\[\[:space:\]\]\*\$"' "${TF_VARS}" \
  || fail "variables.tf validation must anchor the END marker regex like validate.yaml"
grep -q 'regexall("-----BEGIN CERTIFICATE-----"' "${TF_VARS}" \
  && fail "variables.tf still contains an unanchored BEGIN CERTIFICATE regexall (parity gap with the runtime check)"
grep -q 'regexall("-----END CERTIFICATE-----"' "${TF_VARS}" \
  && fail "variables.tf still contains an unanchored END CERTIFICATE regexall (parity gap with the runtime check)"

# --- 2. Runtime replay of the terraform copy --------------------------------

REPLAY="${TMP_DIR}/replay"
mkdir -p "${REPLAY}/trusted-ca" "${REPLAY}/sys" "${REPLAY}/merged"
sed -e "s|/trusted-ca|${REPLAY}/trusted-ca|g" \
    -e "s|/merged|${REPLAY}/merged|g" \
    -e "s|/etc/ssl/certs/ca-certificates.crt|${REPLAY}/sys/debian.crt|g" \
    -e "s|/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem|${REPLAY}/sys/rhel.pem|g" \
    -e "s|/etc/pki/tls/certs/ca-bundle.crt|${REPLAY}/sys/rhel2.crt|g" \
    "${TMP_DIR}/tf-script.sh" >"${TMP_DIR}/merge-ca-replay.sh"
run_merge_ca() { sh -e "${TMP_DIR}/merge-ca-replay.sh" >/dev/null 2>"${REPLAY}/err"; }

# A REAL self-signed certificate: when openssl is available the merge-ca
# script validates every user block as X.509, so marker-soup fixtures would
# fail the happy path (same fixture approach as the chart guard).
REAL_CERT="$(cat <<'REALEOF'
-----BEGIN CERTIFICATE-----
MIIBlzCCAT2gAwIBAgIUdth/oWQ0sZk+7Epg3C8VbQQ9B9UwCgYIKoZIzj0EAwIw
IDEeMBwGA1UEAwwVdHJ1c3RlZC1jYS1ndWFyZC10ZXN0MCAXDTI2MTAwODA2NTQx
NloYDzIxMjYwOTE0MDY1NDE2WjAgMR4wHAYDVQQDDBV0cnVzdGVkLWNhLWd1YXJk
LXRlc3QwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAATO8S7IBj3Mx7Vjhuni7hcF
RlAN819UkutgbvitP5A5BbZv790teGHlQNgHUDsEjCFZ1sE1FAZlcNxE/5c2PoWr
o1MwUTAdBgNVHQ4EFgQUC5THJvp7KLuSM1CAjJ85UxpDJQwwHwYDVR0jBBgwFoAU
C5THJvp7KLuSM1CAjJ85UxpDJQwwDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQD
AgNIADBFAiB8btp6hv+MX0lvynlT6jCLRMBDq67jEjE/eZxuWxYvUAIhAJ4tLWA9
rYx/K513++8bo/fIhMKbpB2PUffIbzgC0ubr
-----END CERTIFICATE-----
REALEOF
)"

printf -- '-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n' >"${REPLAY}/sys/debian.crt"
printf '%s\n' "${REAL_CERT}" >"${REPLAY}/trusted-ca/ca-0.crt"
run_merge_ca || fail "merge-ca must succeed on well-shaped input: $(cat "${REPLAY}/err")"
[ "$(grep -c '^-----BEGIN CERTIFICATE-----' "${REPLAY}/merged/ca-bundle.crt")" = "2" ] \
  || fail "merged bundle must contain the system bundle + the user cert"

printf -- '-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n-----BEGIN PRIVATE KEY-----\nCCCC\n-----END PRIVATE KEY-----\n' >"${REPLAY}/trusted-ca/ca-0.crt"
rm -f "${REPLAY}/merged/ca-bundle.crt"
if run_merge_ca; then
  fail "merge-ca must reject a non-CERTIFICATE PEM block"
fi
grep -q 'non-CERTIFICATE' "${REPLAY}/err" || fail "rejection message does not mention the block type"

: >"${REPLAY}/sys/debian.crt"
rm -f "${REPLAY}/sys/rhel.pem" "${REPLAY}/sys/rhel2.crt" "${REPLAY}/merged/ca-bundle.crt"
if run_merge_ca; then
  fail "merge-ca must reject an empty system bundle"
fi
grep -q 'no non-empty system CA bundle' "${REPLAY}/err" || fail "rejection message does not mention the empty bundle"

# --- 3. Variable validation (needs terraform; skipped when absent) -----------
# terraform console always exits 0 (it prints eval errors instead of failing),
# so assertions run on its output text. Unrelated locals eval errors from
# main.tf may appear in the output; the greps below are scoped to the
# variable-validation messages.
if command -v terraform >/dev/null 2>&1; then
  TF_DIR="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud"
  terraform -chdir="${TF_DIR}" init -backend=false -input=false >/dev/null 2>&1

  echo 'var.templatecenter_trusted_ca_certs' \
    | terraform -chdir="${TF_DIR}" console -var='templatecenter_trusted_ca_certs=["not a pem"]' >"${TMP_DIR}/bad-var.out" 2>&1
  grep -q 'Invalid value for variable' "${TMP_DIR}/bad-var.out" \
    && grep -qi 'only CERTIFICATE blocks' "${TMP_DIR}/bad-var.out" \
    || fail "variable validation must reject a non-PEM entry naming the block-type rule: $(grep -A3 'Invalid value' "${TMP_DIR}/bad-var.out" | head -5)"

  echo 'var.templatecenter_trusted_ca_certs' \
    | terraform -chdir="${TF_DIR}" console -var="templatecenter_trusted_ca_certs=[<<PEM
$(cat <<'PEMBODY'
-----BEGIN CERTIFICATE-----
MIIBlzCCAT2gAwIBAgIUdth/oWQ0sZk+7Epg3C8VbQQ9B9UwCgYIKoZIzj0EAwIw
IDEeMBwGA1UEAwwVdHJ1c3RlZC1jYS1ndWFyZC10ZXN0MCAXDTI2MTAwODA2NTQx
NloYDzIxMjYwOTE0MDY1NDE2WjAgMR4wHAYDVQQDDBV0cnVzdGVkLWNhLWd1YXJk
LXRlc3QwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAATO8S7IBj3Mx7Vjhuni7hcF
RlAN819UkutgbvitP5A5BbZv790teGHlQNgHUDsEjCFZ1sE1FAZlcNxE/5c2PoWr
o1MwUTAdBgNVHQ4EFgQUC5THJvp7KLuSM1CAjJ85UxpDJQwwHwYDVR0jBBgwFoAU
C5THJvp7KLuSM1CAjJ85UxpDJQwwDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQD
AgNIADBFAiB8btp6hv+MX0lvynlT6jCLRMBDq67jEjE/eZxuWxYvUAIhAJ4tLWA9
rYx/K513++8bo/fIhMKbpB2PUffIbzgC0ubr
-----END CERTIFICATE-----
PEMBODY
)
PEM
]" >"${TMP_DIR}/good-var.out" 2>&1
  if grep -q 'Invalid value for variable' "${TMP_DIR}/good-var.out"; then
    fail "variable validation must accept a real certificate: $(grep -A3 'Invalid value' "${TMP_DIR}/good-var.out" | head -5)"
  fi
else
  echo "terraform not found; skipping variable-validation checks" >&2
fi

if [ "${failures}" -gt 0 ]; then
  echo "trusted-ca terraform guard FAILED (${failures} failure(s))" >&2
  exit 1
fi
echo "trusted-ca terraform guard OK"
