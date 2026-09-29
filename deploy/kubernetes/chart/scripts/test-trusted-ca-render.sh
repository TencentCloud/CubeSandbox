#!/bin/sh
# Guard: trustedCACerts renders a chart-managed ConfigMap (or references an
# existing one), injects a merge-ca init container into CubeTemplateCenter,
# and exposes the merged bundle via SSL_CERT_FILE. Disabled by default: the
# rendered manifests must not contain any trusted-ca artifacts.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

# 1. Default (disabled): no trusted-ca artifacts anywhere.
helm template guard-off "$CHART_DIR" $COMMON_SETS >"$TMP_DIR/off.yaml"
if grep -qE 'merge-ca|SSL_CERT_FILE|trusted-ca' "$TMP_DIR/off.yaml"; then
  echo "FAIL: default render must not contain trusted-ca artifacts" >&2
  exit 1
fi

# 2. Enabled with inline certs: ConfigMap + init container + env + checksum
#    annotation rendered.
helm template guard-on "$CHART_DIR" $COMMON_SETS \
  --set trustedCACerts.enabled=true \
  --set-string trustedCACerts.certs[0]="-----BEGIN CERTIFICATE----- guard" >"$TMP_DIR/on.yaml"
for needle in 'name: guard-on-cube-trusted-ca' 'ca-0.crt' 'name: merge-ca' 'SSL_CERT_FILE' 'checksum/trusted-ca'; do
  grep -q "$needle" "$TMP_DIR/on.yaml" || {
    echo "FAIL: enabled render missing /$needle/" >&2
    exit 1
  }
done

# 2b. Enabled must follow global.imageRegistry for the init image (merge-ca
#     uses cube.cubeImage, same as every other cube-owned image). Scope the
#     assertion to the merge-ca block: the main container renders the same
#     image and would satisfy a bare grep.
helm template guard-mirror "$CHART_DIR" $COMMON_SETS \
  --set global.imageRegistry=mirror.example.com \
  --set trustedCACerts.enabled=true \
  --set-string trustedCACerts.certs[0]="-----BEGIN CERTIFICATE----- guard" >"$TMP_DIR/mirror.yaml"
awk '/- name: merge-ca/,/- name: cube-templatecenter/' "$TMP_DIR/mirror.yaml" \
  | grep -q 'image: "mirror.example.com/cube-sandbox/cube-templatecenter' || {
  echo "FAIL: merge-ca init image does not follow global.imageRegistry" >&2
  exit 1
}

# 2c. Enabled without certs and without existingConfigMap must fail at render
#     time (otherwise merge-ca would crashloop with stderr swallowed).
if helm template guard-empty "$CHART_DIR" $COMMON_SETS \
     --set trustedCACerts.enabled=true >/dev/null 2>"$TMP_DIR/empty.err"; then
  echo "FAIL: enabled=true without certs/existingConfigMap must fail validation" >&2
  exit 1
fi
grep -qi 'trustedCACerts' "$TMP_DIR/empty.err" || {
  echo "FAIL: validation error does not mention trustedCACerts:" >&2
  cat "$TMP_DIR/empty.err" >&2
  exit 1
}

# 2d. certs and existingConfigMap are mutually exclusive: the inline certs
#     would be silently ignored while checksum/trusted-ca still hashed them.
if helm template guard-both "$CHART_DIR" $COMMON_SETS \
     --set trustedCACerts.enabled=true \
     --set-string trustedCACerts.existingConfigMap=my-ca-certs \
     --set-string trustedCACerts.certs[0]="-----BEGIN CERTIFICATE----- guard" >/dev/null 2>"$TMP_DIR/both.err"; then
  echo "FAIL: certs + existingConfigMap must fail validation" >&2
  exit 1
fi
grep -qi 'mutually exclusive' "$TMP_DIR/both.err" || {
  echo "FAIL: validation error does not mention mutual exclusion:" >&2
  cat "$TMP_DIR/both.err" >&2
  exit 1
}

# 3. existingConfigMap: reference it, and do not render a chart-managed one.
helm template guard-existing "$CHART_DIR" $COMMON_SETS \
  --set trustedCACerts.enabled=true \
  --set-string trustedCACerts.existingConfigMap=my-ca-certs >"$TMP_DIR/existing.yaml"
grep -q 'configMap: {name: my-ca-certs}' "$TMP_DIR/existing.yaml" \
  || grep -q 'name: my-ca-certs' "$TMP_DIR/existing.yaml" || {
    echo "FAIL: existingConfigMap reference missing" >&2
    exit 1
  }
if grep -q 'name: guard-existing-cube-trusted-ca' "$TMP_DIR/existing.yaml"; then
  echo "FAIL: existingConfigMap must not render a chart-managed ConfigMap" >&2
  exit 1
fi

# 4. Lint passes with the feature enabled (must include a cert, otherwise the
#    validate.yaml guard from 2c correctly rejects the render).
helm lint "$CHART_DIR" $COMMON_SETS --set trustedCACerts.enabled=true \
  --set-string trustedCACerts.certs[0]="-----BEGIN CERTIFICATE----- guard" >/dev/null

echo "trusted-ca guard OK"
