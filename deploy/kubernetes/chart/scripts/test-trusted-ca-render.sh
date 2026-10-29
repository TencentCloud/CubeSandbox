#!/bin/sh
# Guard: trustedCACerts adds private-CA trust to the TemplateCenter pod via
# SSL_CERT_DIR. Disabled by default: the rendered manifests must be
# byte-identical to a render without the feature at all.
#
# Behavior assertions only (no source-grep pins, no script replay):
#   off            -> zero artifacts, render identical to a feature-free chart
#   on + certs     -> chart-managed ConfigMap (one key per cert), SSL_CERT_DIR,
#                     readOnly mount, checksum annotation
#   certs change   -> checksum annotation changes (rolls the Deployment)
#   existingConfig -> no chart ConfigMap, volume points at the operator's,
#                     no checksum annotation (manual restart, see NOTES)
#   invalid config -> enabled without a source fails; certs + existingConfigMap
#                     fails; a non-CERTIFICATE PEM block (private key) fails

set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); }
bad() { FAIL=$((FAIL + 1)); echo "FAIL: $*" >&2; }

render() { # render <name> [extra sets...]
  local name="$1"; shift
  helm template "$name" "$CHART_DIR" $COMMON_SETS "$@" >"$TMP_DIR/$name.yaml" 2>"$TMP_DIR/$name.err"
}
expect_fail() { # expect_fail <name> <needle> [extra sets...]
  local name="$1" needle="$2"; shift 2
  if helm template "$name" "$CHART_DIR" $COMMON_SETS "$@" >/dev/null 2>"$TMP_DIR/$name.err"; then
    bad "$name: expected render to fail"
  elif ! grep -qi "$needle" "$TMP_DIR/$name.err"; then
    bad "$name: failure does not mention /$needle/: $(head -1 "$TMP_DIR/$name.err")"
  else
    ok
  fi
}

# --- 1. Disabled: true no-op ---------------------------------------------
# Same release name for both renders so the only allowed difference is the
# feature itself. The diff is scoped to the TemplateCenter Deployment: other
# documents embed randomly generated material (egress CA Secret), which
# differs between any two renders regardless of this feature.
helm template guard-noop "$CHART_DIR" $COMMON_SETS --set trustedCACerts.enabled=false >"$TMP_DIR/off.yaml"
helm template guard-noop "$CHART_DIR" $COMMON_SETS >"$TMP_DIR/baseline.yaml"
for f in off baseline; do
  awk '/^# Source: / { keep = ($0 ~ /templatecenter.yaml/) } keep' "$TMP_DIR/$f.yaml" >"$TMP_DIR/$f.tc.yaml"
done
if ! diff -u "$TMP_DIR/baseline.tc.yaml" "$TMP_DIR/off.tc.yaml" >"$TMP_DIR/off.diff" 2>&1; then
  bad "disabled render differs from the baseline render (TemplateCenter Deployment)"
  head -10 "$TMP_DIR/off.diff" >&2
else
  ok
fi
if grep -qE 'trusted-ca|SSL_CERT_DIR|trustedCACerts' "$TMP_DIR/off.yaml"; then
  bad "disabled render leaks trusted-ca artifacts"
else
  ok
fi

# --- 2. Enabled with inline certs ----------------------------------------
cat >"$TMP_DIR/certs-values.yaml" <<'EOF'
trustedCACerts:
  enabled: true
  certs:
    - |
      -----BEGIN CERTIFICATE-----
      guard-ca-one
      -----END CERTIFICATE-----
    - |
      -----BEGIN CERTIFICATE-----
      guard-ca-two
      -----END CERTIFICATE-----
      -----BEGIN CERTIFICATE-----
      guard-ca-two-chain
      -----END CERTIFICATE-----
EOF
render on --set trustedCACerts.enabled=true -f "$TMP_DIR/certs-values.yaml"
grep -q 'name: on-cube-trusted-ca' "$TMP_DIR/on.yaml" \
  && ok || bad "enabled render missing the chart-managed ConfigMap"
grep -q 'ca-0.crt' "$TMP_DIR/on.yaml" && grep -q 'guard-ca-two-chain' "$TMP_DIR/on.yaml" \
  && ok || bad "cert entries not rendered as ConfigMap keys"
grep -q 'name: SSL_CERT_DIR' "$TMP_DIR/on.yaml" \
  && grep -q '/etc/cube/trusted-ca:/etc/ssl/certs:/etc/pki/tls/certs' "$TMP_DIR/on.yaml" \
  && ok || bad "SSL_CERT_DIR env missing or wrong value"
grep -q 'mountPath: /etc/cube/trusted-ca' "$TMP_DIR/on.yaml" \
  && ok || bad "trusted-ca mount missing"
grep -q 'checksum/trusted-ca' "$TMP_DIR/on.yaml" \
  && ok || bad "checksum annotation missing on the inline-certs path"

# --- 3. Cert change -> checksum change (rolls the Deployment) -------------
sed 's/guard-ca-one/guard-ca-one-rotated/' "$TMP_DIR/certs-values.yaml" >"$TMP_DIR/certs-rotated.yaml"
render rotated --set trustedCACerts.enabled=true -f "$TMP_DIR/certs-rotated.yaml"
c1=$(grep 'checksum/trusted-ca' "$TMP_DIR/on.yaml" | awk '{print $2}')
c2=$(grep 'checksum/trusted-ca' "$TMP_DIR/rotated.yaml" | awk '{print $2}')
[ -n "$c1" ] && [ -n "$c2" ] && [ "$c1" != "$c2" ] \
  && ok || bad "cert change did not change the checksum annotation"

# --- 4. existingConfigMap path -------------------------------------------
render existing --set trustedCACerts.enabled=true \
  --set trustedCACerts.existingConfigMap=my-own-ca-certs
# Assert by template source, not by name: the chart-managed ConfigMap would
# render as "<release>-cube-trusted-ca", and a hardcoded needle is exactly
# the kind of negative assertion that silently stops matching.
if awk '/^# Source: / { keep = ($0 ~ /trusted-ca-configmap.yaml/) } keep' "$TMP_DIR/existing.yaml" | grep -q .; then
  bad "existingConfigMap path must not render a chart-managed ConfigMap"
else
  ok
fi
grep -q 'name: my-own-ca-certs' "$TMP_DIR/existing.yaml" \
  && ok || bad "volume does not reference the operator's ConfigMap"
if grep -q 'checksum/trusted-ca' "$TMP_DIR/existing.yaml"; then
  bad "existingConfigMap path must not carry the checksum annotation (operator-owned content)"
else
  ok
fi

# --- 5. Invalid configs fail the render ----------------------------------
expect_fail nosource 'trustedCACerts' --set trustedCACerts.enabled=true
expect_fail mutex 'mutually exclusive' --set trustedCACerts.enabled=true \
  --set trustedCACerts.existingConfigMap=my-own-ca-certs \
  --set trustedCACerts.certs[0]='-----BEGIN CERTIFICATE-----'
cat >"$TMP_DIR/privatekey-values.yaml" <<'EOF'
trustedCACerts:
  enabled: true
  certs:
    - |
      -----BEGIN CERTIFICATE-----
      guard
      -----END CERTIFICATE-----
      -----BEGIN PRIVATE KEY-----
      notakey
      -----END PRIVATE KEY-----
EOF
if helm template privkey "$CHART_DIR" $COMMON_SETS --set trustedCACerts.enabled=true -f "$TMP_DIR/privatekey-values.yaml" \
     >/dev/null 2>"$TMP_DIR/privkey.err"; then
  bad "pasted private key must fail the render"
elif ! grep -qi 'CERTIFICATE' "$TMP_DIR/privkey.err"; then
  bad "private-key rejection does not mention the block type: $(head -1 "$TMP_DIR/privkey.err")"
else
  ok
fi

# --- 6. External control plane: no TC, so no trusted-ca artifacts ---------
# With controlPlane.enabled=false nothing consumes the certs; rendering a
# chart ConfigMap holding them (and skipping the validation with it) would
# be an unused copy of the trust material.
if helm template extcp "$CHART_DIR" $COMMON_SETS \
     --set trustedCACerts.enabled=true -f "$TMP_DIR/certs-values.yaml" \
     --set controlPlane.enabled=false --set externalControlPlane.enabled=true \
     --set externalControlPlane.masterEndpoint=http://external:8089 \
     --set externalControlPlane.opsEndpoint=http://external:3010 \
     --set-string opsAgent.tokenSecret.name=compute-only-token \
     >"$TMP_DIR/extcp.yaml" 2>"$TMP_DIR/extcp.err"; then
  if grep -qE 'trusted-ca|SSL_CERT_DIR' "$TMP_DIR/extcp.yaml"; then
    bad "external control plane render leaks trusted-ca artifacts"
  else
    ok
  fi
else
  bad "external control plane render failed outright: $(head -1 "$TMP_DIR/extcp.err")"
fi

echo "trusted-ca render guard: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
