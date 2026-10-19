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
# The block-type enumeration itself must scan everywhere, case-insensitively
# (same pattern as validate.yaml): a line-anchored or uppercase-label-only
# scan lets an indented, hyphen-labeled or lowercase private key slip past
# while the CERTIFICATE shape checks are satisfied by the surrounding cert.
grep -qF 'regexall("(?si)-----BEGIN .*?-----"' "${TF_VARS}" \
  || fail "variables.tf block-type enumeration must be unanchored and case-insensitive (parity with validate.yaml)"

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

# The BEGIN scan is case-insensitive and not line-anchored: an INDENTED or
# LOWERCASE-labeled private key must fail the runtime pass too (a
# line-anchored scan would not even see the indented one, and Go's
# pem.Decode would silently skip the lowercase block -- CA absent from the
# bundle, key text still shipped).
printf -- '-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n   -----BEGIN PRIVATE KEY-----\nCCCC\n   -----END PRIVATE KEY-----\n' >"${REPLAY}/trusted-ca/ca-0.crt"
rm -f "${REPLAY}/merged/ca-bundle.crt"
if run_merge_ca; then
  fail "merge-ca must reject an indented private key"
fi
grep -q 'non-exact-line PEM BEGIN marker' "${REPLAY}/err" \
  || fail "rejection message does not mention the marker shape for the indented key"

printf -- '-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----\n-----begin private key-----\nCCCC\n-----end private key-----\n' >"${REPLAY}/trusted-ca/ca-0.crt"
rm -f "${REPLAY}/merged/ca-bundle.crt"
if run_merge_ca; then
  fail "merge-ca must reject a lowercase-labeled private key"
fi
grep -q 'non-exact-line PEM BEGIN marker' "${REPLAY}/err" \
  || fail "rejection message does not mention the marker shape for the lowercase key"

# An END marker with trailing junk must fail the exact-line symmetry count:
# previously the prefix-count kept it "balanced" and, without openssl in the
# image, the junk line landed in the bundle only for Go's pem.Decode to
# silently skip the whole block.
printf -- '-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE----- junk\n' >"${REPLAY}/trusted-ca/ca-0.crt"
rm -f "${REPLAY}/merged/ca-bundle.crt"
if run_merge_ca; then
  fail "merge-ca must reject an END marker with trailing junk (exact-line symmetry)"
fi
grep -q 'unbalanced BEGIN/END' "${REPLAY}/err" \
  || fail "rejection message does not mention the unbalanced markers for END junk"

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

  # A private key hidden behind a marker shape the old line-anchored,
  # uppercase-label-only enumeration missed (indented / lowercase) must be
  # rejected by the plan-time validation too -- the cert satisfies the
  # shape and symmetry checks, so the block-type enumeration is what fires.
  for keycase in 'INDENTED' 'LOWERCASE'; do
    if [ "${keycase}" = 'INDENTED' ]; then
      KEYBLOCK='   -----BEGIN PRIVATE KEY-----
   CCCC
   -----END PRIVATE KEY-----'
    else
      KEYBLOCK='-----begin private key-----
CCCC
-----end private key-----'
    fi
    echo 'var.templatecenter_trusted_ca_certs' \
      | terraform -chdir="${TF_DIR}" console -var="templatecenter_trusted_ca_certs=[<<PEM
$(cat <<PEMBODY
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
${KEYBLOCK}
PEMBODY
)
PEM
]" >"${TMP_DIR}/${keycase,,}-key-var.out" 2>&1
    grep -q 'Invalid value for variable' "${TMP_DIR}/${keycase,,}-key-var.out" \
      && grep -qi 'only CERTIFICATE blocks' "${TMP_DIR}/${keycase,,}-key-var.out" \
      || fail "variable validation must reject a ${keycase,,} private key: $(grep -A3 'Invalid value' "${TMP_DIR}/${keycase,,}-key-var.out" | head -5)"
  done
else
  echo "terraform not found; skipping variable-validation checks" >&2
fi

# --- 4. Reconciler gate + persistence wiring (no terraform needed) ----------
# The reconciler must never treat the operator-owned ConfigMap as drift on
# the existing_config_map path: the gate decides delete-vs-keep on every
# re-run, and the persistence wiring decides whether the gate's inputs
# survive the run that rewrites .env and resolved.auto.tfvars.json.
# shellcheck source=../terraform/tencentcloud/lib-state-sync.sh
source "${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/lib-state-sync.sh"

# 4a. Gate polarity: true only when the inline path is really active; every
#     ambiguous input (missing file, empty mirror, malformed JSON, no jq)
#     fails open toward "not managed" -- a false negative merely skips a
#     delete, a false positive would destroy operator data.
unset TF_VAR_templatecenter_trusted_ca_certs
if _tc_trusted_ca_inline_enabled "${TMP_DIR}/nope.json"; then
  fail "gate must be false without an env var and without a mirror"
fi

printf '{"templatecenter_trusted_ca_certs":[]}' >"${TMP_DIR}/empty-certs.json"
if _tc_trusted_ca_inline_enabled "${TMP_DIR}/empty-certs.json"; then
  fail "gate must be false for an empty mirror"
fi

printf 'not json' >"${TMP_DIR}/broken.json"
if _tc_trusted_ca_inline_enabled "${TMP_DIR}/broken.json"; then
  fail "gate must be false for a malformed mirror (fail open)"
fi

mkdir -p "${TMP_DIR}/empty-bin"
BASH_ABS="$(command -v bash)"
# shellcheck disable=SC2016
if PATH="${TMP_DIR}/empty-bin" "${BASH_ABS}" -c 'source "${1}"; _tc_trusted_ca_inline_enabled "${2}"' _ \
     "${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/lib-state-sync.sh" \
     "${TMP_DIR}/broken.json" >/dev/null 2>&1; then
  fail "gate must be false when jq is unavailable (fail open)"
fi

printf '{"templatecenter_trusted_ca_certs":["-----BEGIN CERTIFICATE-----\\nAA==\\n-----END CERTIFICATE-----"]}' >"${TMP_DIR}/with-certs.json"
if ! _tc_trusted_ca_inline_enabled "${TMP_DIR}/with-certs.json"; then
  fail "gate must be true when the mirror carries certs"
fi

TF_VAR_templatecenter_trusted_ca_certs='["-----BEGIN CERTIFICATE-----"]'
if ! _tc_trusted_ca_inline_enabled ""; then
  fail "gate must be true when the env var is set"
fi
# '[]' is valid HCL/JSON and the natural way to keep the .env line while
# turning the feature off: terraform's definition is length > 0, and the
# gate must match it or an existing_config_map operator risks the
# reconciler deleting their ConfigMap. Tab/spaces forms included, plus
# unparseable values (terraform hard-fails on those at plan time; the gate
# fails open toward "no delete").
for empty_form in '[]' '[ ]' '[  ]' "$(printf '[\t]')" "$(printf '[\t ]\t')"; do
  TF_VAR_templatecenter_trusted_ca_certs="${empty_form}"
  if _tc_trusted_ca_inline_enabled "${TMP_DIR}/with-certs.json"; then
    fail "gate must be false for an empty-list env var ($(printf '%q' "${empty_form}")): terraform says length 0"
  fi
done
for bad_form in 'not json' '["a"' 'null' '"abc"'; do
  TF_VAR_templatecenter_trusted_ca_certs="${bad_form}"
  if _tc_trusted_ca_inline_enabled "${TMP_DIR}/with-certs.json"; then
    fail "gate must fail open for an unparseable env var ($(printf '%q' "${bad_form}"))"
  fi
done
# The multi-document class: jq's non-slurped -e reports the LAST document's
# status, so '[] ["PEM"]' would read as inline-managed even though terraform
# cannot parse a two-document value at all. Same class the mirror helper's
# -es precheck guards -- the gate must reject it in one expression.
TF_VAR_templatecenter_trusted_ca_certs='[] ["-----BEGIN CERTIFICATE-----"]'
if _tc_trusted_ca_inline_enabled "${TMP_DIR}/with-certs.json"; then
  fail "gate must fail open for a multi-document env var (jq -e only reports the last document)"
fi
TF_VAR_templatecenter_trusted_ca_certs='["-----BEGIN CERTIFICATE-----"] []'
if _tc_trusted_ca_inline_enabled "${TMP_DIR}/with-certs.json"; then
  fail "gate must fail open for a trailing multi-document env var"
fi
# An empty-but-set env var must NOT count: save_env_file never writes one
# (an empty TF_VAR_ for a list variable is not parseable HCL), and counting
# it would let a hand-cleared line re-enable deletes.
TF_VAR_templatecenter_trusted_ca_certs=''
if _tc_trusted_ca_inline_enabled "${TMP_DIR}/empty-certs.json"; then
  fail "gate must be false for an empty-but-set env var"
fi
unset TF_VAR_templatecenter_trusted_ca_certs

# 4c. Persistence round-trip: the saved line must reload byte-for-byte
#     through the same _env_value every other .env entry uses.
CERTS_LINE="TF_VAR_templatecenter_trusted_ca_certs='[\"-----BEGIN CERTIFICATE-----\\nAA==\\n-----END CERTIFICATE-----\"]'"
assert_saved_line() {
  local want="$1" got
  got="$(_env_value "${CERTS_LINE}")"
  [ "$got" = "$want" ] || fail ".env round-trip drifted: expected '${want}', got '${got}'"
}
assert_saved_line '["-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----"]'

# 4d. Static wiring pins: the gate must actually gate the trusted-ca entry,
#     .env is the persistence for the pair (save_env_file carries both
#     conditionally) and the resolved mirror must record the pair too -- it
#     is the drift pin that keeps a raw terraform plan/apply in the
#     terraform directory reading the DEPLOYED value instead of the
#     variable defaults (which plan the feature away: destroy the
#     ConfigMap, drop the merge-ca init container). The loader must skip
#     the pair on force-export so .env stays authoritative across create.sh
#     re-runs. The STEP2_TARGETS must NOT target the trusted-ca ConfigMap
#     directly (the Deployment's volume config_map name reference in
#     tke-addons.tf is the dependency edge that pulls it into the addons
#     -target apply -- an explicit target on an instance index that is
#     count=0 whenever the inline path is off would be dead there and
#     version-sensitive on top), and that edge must exist.
CREATE_SH="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/create.sh"
grep -q '_tc_trusted_ca_inline_enabled "$RESOLVED_TFVARS_FILE"' "${CREATE_SH}" \
  || fail "_reconcile_addons must gate the trusted-ca ConfigMap entry on the inline path"
grep -qF 'kubernetes_config_map.templatecenter_trusted_ca[0]' "${CREATE_SH}" \
  && fail "STEP2_TARGETS must not target the trusted-ca ConfigMap directly (count=0 whenever the inline path is off; the volume's resource-name reference in tke-addons.tf is the dependency edge)"
grep -qF 'kubernetes_config_map.templatecenter_trusted_ca[0].metadata[0].name' \
  "${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/tke-addons.tf" \
  || fail "the Deployment volume must reference the managed ConfigMap resource -- that edge is what pulls the ConfigMap into the addons -target apply"
grep -qF '${TF_VAR_templatecenter_trusted_ca_certs:+TF_VAR_templatecenter_trusted_ca_certs=' "${CREATE_SH}" \
  || fail "save_env_file must persist the certs TF_VAR_ conditionally (an empty TF_VAR_ for a list variable is not parseable HCL)"
grep -qF '${TF_VAR_templatecenter_trusted_ca_existing_config_map:+TF_VAR_templatecenter_trusted_ca_existing_config_map=' "${CREATE_SH}" \
  || fail "save_env_file must persist the existing_configMap TF_VAR_"
grep -qF '_jq_add_trusted_ca_pair "$tmp"' "${CREATE_SH}" \
  || fail "write_resolved_tfvars_file must mirror the trusted-ca pair into the resolved snapshot (without it a raw terraform plan reads the variable defaults and plans the feature away)"
grep -qF 'templatecenter_trusted_ca_certs | templatecenter_trusted_ca_existing_config_map) continue' "${CREATE_SH}" \
  || fail "load_resolved_tfvars_file must skip the trusted-ca pair on force-export (the mirror records it for raw terraform runs; .env must stay authoritative across create.sh re-runs)"

# 4e. write_resolved_tfvars_file must EXECUTE correctly, not merely contain
#     the right text: a syntax-legal-but-semantically-broken edit inside its
#     jq continuation chain (a comment line in the middle, a backslash-TAB
#     instead of backslash-newline) passes `bash -n` and every grep pin and
#     still kills every real run at the final write with exit 127. Extract
#     the function and run it with mocked inputs.
CREATE_SH="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/create.sh"
awk '/^write_resolved_tfvars_file\(\) \{/,/^\}/' "${CREATE_SH}" >"${TMP_DIR}/resolved-fn.sh"
[ -s "${TMP_DIR}/resolved-fn.sh" ] || fail "could not extract write_resolved_tfvars_file from create.sh"
grep -q 'RESOLVED_TFVARS_FILE' "${TMP_DIR}/resolved-fn.sh" \
  || fail "extracted write_resolved_tfvars_file looks truncated"
# The numeric/JSON coercion helpers, the declared-variable check, and the
# trusted-ca mirror helper live in create.sh itself (not the lib); the
# extracted function needs them alongside. _tf_var_declared greps
# ${SCRIPT_DIR}/variables.tf -- point SCRIPT_DIR at the real directory,
# exactly what create.sh's SCRIPT_DIR is at runtime.
for _helper in _tf_var_declared _json_or_default _number_or_default _bool_json _jq_add_trusted_ca_pair; do
	awk "/^${_helper}\(\) \{/,/^\}/" "${CREATE_SH}" >>"${TMP_DIR}/resolved-fn.sh"
done

RED='' GREEN='' YELLOW='' CYAN='' NC=''
# _tf_var_declared (extracted with the function below) greps
# ${SCRIPT_DIR}/variables.tf -- point it at the real directory, exactly
# what create.sh's SCRIPT_DIR is at runtime.
SCRIPT_DIR="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud"
RESOLVED_TFVARS_FILE="${TMP_DIR}/resolved.json"
RESOLVED_TFVARS_SUSPENDED=''
SSH_PUB_KEY="${TMP_DIR}/id.pub"
SSH_PRI_KEY="${TMP_DIR}/id.key"
touch "${SSH_PUB_KEY}" "${SSH_PRI_KEY}"
terraform() { return 1; } # no state: the function must fall back to defaults
# shellcheck source=../terraform/tencentcloud/lib-state-sync.sh
source "${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/lib-state-sync.sh"
source "${TMP_DIR}/resolved-fn.sh"

run_resolved() {
	(
		set -euo pipefail
		unset TF_VAR_templatecenter_trusted_ca_certs TF_VAR_templatecenter_trusted_ca_existing_config_map
		if [ "${1:-}" = "set" ]; then
			export TF_VAR_templatecenter_trusted_ca_certs='["-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----"]'
			export TF_VAR_templatecenter_trusted_ca_existing_config_map='my-ca-certs'
		fi
		# The caller tests this function's result in an || list, which
		# suppresses errexit for the ENTIRE subshell body -- bash ignores a
		# re-issued `set -e` inside a tested context until the caller's list
		# completes. `|| exit 1` here therefore only catches failures that
		# return non-zero wholesale; a mid-function failure that limps
		# through to a coincidentally-successful last command still returns
		# 0 and is caught by the content assertions on the written mirror
		# below, which are the real backstop.
		write_resolved_tfvars_file || {
			echo "write_resolved_tfvars_file failed" >&2
			exit 1
		}
	)
}

rm -f "${RESOLVED_TFVARS_FILE}"
run_resolved || fail "write_resolved_tfvars_file must succeed on defaults (untrusted-ca vars unset)"
jq -e '.region == "ap-guangzhou"' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  || fail "resolved mirror must carry the default region: $(cat "${RESOLVED_TFVARS_FILE}")"
jq -e 'has("templatecenter_trusted_ca_certs") or has("templatecenter_trusted_ca_existing_config_map")' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  && fail "resolved mirror must omit the trusted-ca keys when their env vars are unset"

# The mirror MUST record the pair with the vars SET (a JSON-clean certs
# value round-trips losslessly): it is the drift pin that keeps raw
# terraform plan/apply runs reading the deployed value instead of the
# variable defaults, which plan the feature away. .env stays authoritative
# on the create.sh path because the loader skips the pair (asserted in 4h).
run_resolved set || fail "write_resolved_tfvars_file must succeed with the trusted-ca vars set"
jq -e '.templatecenter_trusted_ca_certs == ["-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----"]' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  || fail "resolved mirror must record the deployed certs value verbatim: $(cat "${RESOLVED_TFVARS_FILE}")"
jq -e '.templatecenter_trusted_ca_existing_config_map == "my-ca-certs"' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  || fail "resolved mirror must record the existing_config_map value"

# The same write with an HCL-valid/JSON-invalid certs value: the write must
# succeed and the mirror must NOT record certs -- a JSON tfvars file cannot
# represent the value losslessly, and a lossy substitute would un-pin the
# deployed config. The operator is warned that raw terraform invocations
# must pass the pair themselves (documented in env.example). The helper's
# existing_config_map half is independent: with it ALSO set, it must still
# be recorded even though certs is unrecordable.
(
	set -euo pipefail
	export TF_VAR_templatecenter_trusted_ca_certs='["a",]'
	export TF_VAR_templatecenter_trusted_ca_existing_config_map='my-ca-certs'
	write_resolved_tfvars_file || {
		echo "write_resolved_tfvars_file failed" >&2
		exit 1
	}
) 2>"${TMP_DIR}/hcl-warn.txt" || fail "write_resolved_tfvars_file must survive an HCL-only certs value (the mirror does not record it)"
grep -q 'cannot record it losslessly' "${TMP_DIR}/hcl-warn.txt" \
  || fail "an HCL-only certs value must produce the raw-terraform-drift warning: $(cat "${TMP_DIR}/hcl-warn.txt")"
jq -e 'has("templatecenter_trusted_ca_certs")' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  && fail "an HCL-only certs value must not reach the mirror"
jq -e '.templatecenter_trusted_ca_existing_config_map == "my-ca-certs"' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  || fail "the existing_config_map half must still be recorded when only the certs value is unrecordable"

# 4e2. Edge-of-the-domain inputs: a SET-but-EMPTY certs value is the same
#      unrecordable class (it would hard-fail terraform's list(string)
#      conversion anyway), so it must be omitted with the same warning; a
#      SET-but-EMPTY existing_config_map equals its default and must be
#      silently skipped without dragging the clean certs value out with it.
(
	set -euo pipefail
	export TF_VAR_templatecenter_trusted_ca_certs=''
	unset TF_VAR_templatecenter_trusted_ca_existing_config_map
	write_resolved_tfvars_file || {
		echo "write_resolved_tfvars_file failed" >&2
		exit 1
	}
) 2>"${TMP_DIR}/empty-warn.txt" || fail "write_resolved_tfvars_file must survive a set-but-empty certs value"
grep -q 'cannot record it losslessly' "${TMP_DIR}/empty-warn.txt" \
  || fail "a set-but-empty certs value must produce the same warning: $(cat "${TMP_DIR}/empty-warn.txt")"
jq -e 'has("templatecenter_trusted_ca_certs") or has("templatecenter_trusted_ca_existing_config_map")' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  && fail "a set-but-empty certs value must not reach the mirror"
(
	set -euo pipefail
	export TF_VAR_templatecenter_trusted_ca_certs='["-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----"]'
	export TF_VAR_templatecenter_trusted_ca_existing_config_map=''
	write_resolved_tfvars_file || {
		echo "write_resolved_tfvars_file failed" >&2
		exit 1
	}
) || fail "write_resolved_tfvars_file must survive a set-but-empty existing_config_map"
jq -e '.templatecenter_trusted_ca_certs == ["-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----"]' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  || fail "a clean certs value must still be recorded when existing_config_map is set-but-empty"
jq -e 'has("templatecenter_trusted_ca_existing_config_map")' "${RESOLVED_TFVARS_FILE}" >/dev/null \
  && fail "a set-but-empty existing_config_map (equal to its default) must not reach the mirror"

# 4f. The terraform variable docs live in the DEPLOYER's env template (the
#     file create.sh copies/loads), not the target-machine install template
#     an operator following the top-level guidance would edit in vain.
TF_ENV_EXAMPLE="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud/env.example"
grep -q 'TF_VAR_templatecenter_trusted_ca_certs' "${TF_ENV_EXAMPLE}" \
  || fail "terraform env.example must document the trusted-ca variables (create.sh reads THIS .env)"
grep -q 'TF_VAR_templatecenter_trusted_ca' "${REPO_ROOT}/deploy/one-click/env.example" \
  && fail "top-level env.example is the target-machine install template; the terraform variable docs belong only in the deployer's env.example"

# 4g. save_env_file must EXECUTE correctly with the trusted-ca lines: inside
#     an unquoted heredoc, quote characters in a ${VAR:+...} word are literal
#     data, NOT quoting syntax (the opposite of unquoted command context,
#     where a reviewer's one-line repro shows them consumed). This pins the
#     behavior executably instead of by argument: the function's own
#     two-quote validation loop is the assertion -- if the quotes were ever
#     consumed, the data line would land with zero quotes and the run below
#     would exit 1.
awk '/^save_env_file\(\) \{/,/^\}/' "${CREATE_SH}" >"${TMP_DIR}/save-env-fn.sh"
[ -s "${TMP_DIR}/save-env-fn.sh" ] || fail "could not extract save_env_file from create.sh"
grep -q 'templatecenter_trusted_ca' "${TMP_DIR}/save-env-fn.sh" \
  || fail "extracted save_env_file looks truncated"
ENV_FILE="${TMP_DIR}/saved.env"
RED='' YELLOW='' GREEN='' CYAN='' NC=''
terraform() { return 1; } # no state: the function must fall back to defaults
# shellcheck source=/dev/null
source "${TMP_DIR}/save-env-fn.sh"

(
	set -eu
	unset TF_VAR_templatecenter_trusted_ca_certs TF_VAR_templatecenter_trusted_ca_existing_config_map
	save_env_file quiet
)
grep -q '^TF_VAR_templatecenter_trusted_ca' "${ENV_FILE}" \
  && fail "save_env_file must not write the trusted-ca lines when their env vars are unset"

TF_VAR_templatecenter_trusted_ca_certs='["-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----"]'
TF_VAR_templatecenter_trusted_ca_existing_config_map='my-ca-certs'
(
	set -eu
	save_env_file quiet
) || fail "save_env_file must accept the trusted-ca lines (the two-quote validation loop would exit 1 if the heredoc consumed the quotes)"
grep -qF "TF_VAR_templatecenter_trusted_ca_certs='[\"-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\"]'" "${ENV_FILE}" \
  || fail "the saved certs line must carry its single quotes verbatim: $(grep 'trusted_ca_certs' "${ENV_FILE}" | head -1)"
grep -qF "TF_VAR_templatecenter_trusted_ca_existing_config_map='my-ca-certs'" "${ENV_FILE}" \
  || fail "the saved existing_config_map line must carry its single quotes verbatim"
unset TF_VAR_templatecenter_trusted_ca_certs TF_VAR_templatecenter_trusted_ca_existing_config_map

# 4h. The loader's trusted-ca skip must EXECUTE, not merely exist: the
#     mirror records the pair for raw terraform runs, but a create.sh
#     re-run must not force-export the snapshot's pair over the operator's
#     newer .env edits -- that is the same stale-propagation channel the
#     create_tke skip guards. The loader must also keep exporting every
#     other mirror key (the region pin below) and keep suspending the file.
awk '/^_tf_var_declared\(\) \{/,/^\}/' "${CREATE_SH}" >"${TMP_DIR}/loader-fn.sh"
awk '/^restore_suspended_resolved_tfvars\(\) \{/,/^\}/' "${CREATE_SH}" >>"${TMP_DIR}/loader-fn.sh"
awk '/^load_resolved_tfvars_file\(\) \{/,/^\}/' "${CREATE_SH}" >>"${TMP_DIR}/loader-fn.sh"
[ -s "${TMP_DIR}/loader-fn.sh" ] || fail "could not extract load_resolved_tfvars_file from create.sh"
grep -q 'templatecenter_trusted_ca' "${TMP_DIR}/loader-fn.sh" \
  || fail "extracted loader looks truncated (trusted-ca skip missing)"
jq -n '{region: "mirror-region", templatecenter_trusted_ca_certs: ["MIRROR-OLD"], templatecenter_trusted_ca_existing_config_map: "mirror-old-cm"}' \
  >"${TMP_DIR}/mirror.json"
CYAN='' NC=''
(
	set -euo pipefail
	unset TF_VAR_templatecenter_trusted_ca_certs TF_VAR_templatecenter_trusted_ca_existing_config_map
	# The operator has since set a NEWER value in .env; the mirror still
	# carries the pair from the previous run.
	TF_VAR_templatecenter_trusted_ca_certs='["NEW-ENV-VALUE"]'
	# _tf_var_declared greps ${SCRIPT_DIR}/variables.tf -- point it at the
	# real directory, exactly what create.sh's SCRIPT_DIR is at runtime.
	SCRIPT_DIR="${REPO_ROOT}/deploy/one-click/terraform/tencentcloud"
	RESOLVED_TFVARS_FILE="${TMP_DIR}/mirror.json"
	# shellcheck source=/dev/null
	source "${TMP_DIR}/loader-fn.sh"
	load_resolved_tfvars_file
	[ "${TF_VAR_region:-}" = "mirror-region" ] || {
		echo "loader lost the ordinary mirror key's force-export" >&2
		exit 1
	}
	[ "${TF_VAR_templatecenter_trusted_ca_certs}" = '["NEW-ENV-VALUE"]' ] || {
		echo "loader force-exported the mirror's certs over the .env value" >&2
		exit 1
	}
	[ -z "${TF_VAR_templatecenter_trusted_ca_existing_config_map:-}" ] || {
		echo "loader force-exported the mirror's existing_config_map" >&2
		exit 1
	}
) || fail "the loader must skip the trusted-ca pair on force-export while keeping other keys and .env values"
[ -f "${TMP_DIR}/mirror.json" ] \
  || fail "the loader must restore the suspended mirror (the EXIT trap's mv back)"

# 4h2. The removal direction: the operator deletes the .env line entirely
#      (TF_VAR_ unset) while the mirror still carries the pair from the
#      previous run. The skip must hold there too -- a loader that only
#      skips when the variable is SET would resurrect a removed value.
printf '{"region":"mirror-region2","templatecenter_trusted_ca_certs":["MIRROR-OLD"],"templatecenter_trusted_ca_existing_config_map":"mirror-old-cm"}' \
  >"${TMP_DIR}/mirror2.json"
(
	set -euo pipefail
	unset TF_VAR_templatecenter_trusted_ca_certs TF_VAR_templatecenter_trusted_ca_existing_config_map
	RESOLVED_TFVARS_FILE="${TMP_DIR}/mirror2.json"
	# shellcheck source=/dev/null
	source "${TMP_DIR}/loader-fn.sh"
	load_resolved_tfvars_file
	[ "${TF_VAR_region:-}" = "mirror-region2" ] || {
		echo "loader lost the ordinary mirror key's force-export" >&2
		exit 1
	}
	[ -z "${TF_VAR_templatecenter_trusted_ca_certs:-}" ] || {
		echo "loader resurrected the removed certs value from the mirror" >&2
		exit 1
	}
	[ -z "${TF_VAR_templatecenter_trusted_ca_existing_config_map:-}" ] || {
		echo "loader resurrected the removed existing_config_map value from the mirror" >&2
		exit 1
	}
) || fail "the loader must not resurrect the pair when the .env line is removed entirely"

if [ "${failures}" -gt 0 ]; then
  echo "trusted-ca terraform guard FAILED (${failures} failure(s))" >&2
  exit 1
fi
echo "trusted-ca terraform guard OK"
