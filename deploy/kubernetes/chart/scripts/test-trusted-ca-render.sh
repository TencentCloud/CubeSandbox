#!/bin/sh
# Guard: trustedCACerts renders a chart-managed ConfigMap (or references an
# existing one), injects a merge-ca init container into CubeTemplateCenter,
# and exposes the merged bundle via SSL_CERT_FILE. Disabled by default: the
# rendered manifests must be identical to the plain-off render.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

# Values-file fixture with a REAL multi-line PEM block (helm --set-string
# cannot carry newlines, and marker-soup fixtures prove less than claimed).
cat >"$TMP_DIR/certs-values.yaml" <<'PEMEOF'
trustedCACerts:
  certs:
    - |
      -----BEGIN CERTIFICATE-----
      guard
      -----END CERTIFICATE-----
PEMEOF
ENABLED_SETS="--set trustedCACerts.enabled=true -f $TMP_DIR/certs-values.yaml"

# 1. Default (disabled): no trusted-ca artifacts anywhere; render must be
#    identical to a render without the values file at all.
helm template guard-off "$CHART_DIR" $COMMON_SETS >"$TMP_DIR/off.yaml"
if grep -qE 'merge-ca|SSL_CERT_FILE|trusted-ca' "$TMP_DIR/off.yaml"; then
  echo "FAIL: default render must not contain trusted-ca artifacts" >&2
  exit 1
fi

# 1b. Disabled feature is a no-op even when a shared values file carries a
#     certs entry: no trusted-ca artifacts and no checksum annotation.
#     (Byte-identical comparison is impossible here -- the selfSigned egress
#     CA is randomly generated per render.)
helm template guard-off "$CHART_DIR" $COMMON_SETS \
  --set trustedCACerts.enabled=false -f "$TMP_DIR/certs-values.yaml" >"$TMP_DIR/off-certs.yaml"
if grep -qE 'merge-ca|SSL_CERT_FILE|trusted-ca|checksum/trusted-ca' "$TMP_DIR/off-certs.yaml"; then
  echo "FAIL: enabled=false with a certs entry must not render trusted-ca artifacts" >&2
  exit 1
fi

# 1c. No-consumer case: enabled=true with controlPlane.enabled=false renders
#     neither the ConfigMap nor the merge-ca init container.
helm template guard-no-tc "$CHART_DIR" $COMMON_SETS \
  --set controlPlane.enabled=false \
  --set externalControlPlane.enabled=true \
  --set externalControlPlane.masterEndpoint=http://10.0.0.1:8089 \
  --set externalControlPlane.opsEndpoint=http://10.0.0.1:3010 \
  --set cubeNode.enabled=false \
  $ENABLED_SETS >"$TMP_DIR/no-tc.yaml"
if grep -qE 'merge-ca|SSL_CERT_FILE|trusted-ca|checksum/trusted-ca' "$TMP_DIR/no-tc.yaml"; then
  echo "FAIL: controlPlane.enabled=false must not render trusted-ca artifacts" >&2
  exit 1
fi

# 2. Enabled with inline certs: ConfigMap + init container + env + checksum
#    annotation rendered.
helm template guard-on "$CHART_DIR" $COMMON_SETS $ENABLED_SETS >"$TMP_DIR/on.yaml"
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
  --set global.imageRegistry=mirror.example.com $ENABLED_SETS >"$TMP_DIR/mirror.yaml"
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
if helm template guard-both "$CHART_DIR" $COMMON_SETS $ENABLED_SETS \
     --set-string trustedCACerts.existingConfigMap=my-ca-certs >/dev/null 2>"$TMP_DIR/both.err"; then
  echo "FAIL: certs + existingConfigMap must fail validation" >&2
  exit 1
fi
grep -qi 'mutually exclusive' "$TMP_DIR/both.err" || {
  echo "FAIL: validation error does not mention mutual exclusion:" >&2
  cat "$TMP_DIR/both.err" >&2
  exit 1
}

# 2e. Non-PEM entries must fail at render time (Go would silently ignore
#     unparseable blocks and the operator would see x509 again).
if helm template guard-garbled "$CHART_DIR" $COMMON_SETS \
     --set trustedCACerts.enabled=true \
     --set-string trustedCACerts.certs[0]="not a pem" >/dev/null 2>"$TMP_DIR/garbled.err"; then
  echo "FAIL: non-PEM certs entry must fail validation" >&2
  exit 1
fi
grep -qi 'PEM certificate' "$TMP_DIR/garbled.err" || {
  echo "FAIL: validation error does not mention PEM:" >&2
  cat "$TMP_DIR/garbled.err" >&2
  exit 1
}

# 2f. A PEM header without the END marker must also fail (line-anchored
#     shape check covers both markers).
if helm template guard-truncated "$CHART_DIR" $COMMON_SETS \
     --set trustedCACerts.enabled=true \
     --set-string trustedCACerts.certs[0]="-----BEGIN CERTIFICATE----- guard" >/dev/null 2>"$TMP_DIR/trunc.err"; then
  echo "FAIL: PEM entry without END CERTIFICATE must fail validation" >&2
  exit 1
fi

# 3. existingConfigMap: reference it, and do not render a chart-managed one.
helm template guard-existing "$CHART_DIR" $COMMON_SETS \
  --set trustedCACerts.enabled=true \
  --set-string trustedCACerts.existingConfigMap=my-ca-certs >"$TMP_DIR/existing.yaml"
grep -q 'name: my-ca-certs' "$TMP_DIR/existing.yaml" || {
  echo "FAIL: existingConfigMap reference missing" >&2
  exit 1
}
if grep -q 'name: guard-existing-cube-trusted-ca' "$TMP_DIR/existing.yaml"; then
  echo "FAIL: existingConfigMap must not render a chart-managed ConfigMap" >&2
  exit 1
fi

# 4. Lint passes with the feature enabled (must include a valid fixture,
#    otherwise the validate.yaml guard from 2c correctly rejects the render).
helm lint "$CHART_DIR" $COMMON_SETS $ENABLED_SETS >/dev/null

# 5. Changing certs must change the checksum (the Deployment-roll contract).
cat >"$TMP_DIR/certs-values-2.yaml" <<'PEMEOF'
trustedCACerts:
  certs:
    - |
      -----BEGIN CERTIFICATE-----
      different body
      -----END CERTIFICATE-----
PEMEOF
CK1=$(grep -o 'checksum/trusted-ca: "[a-f0-9]*"' "$TMP_DIR/on.yaml")
helm template guard-on-2 "$CHART_DIR" $COMMON_SETS \
  --set trustedCACerts.enabled=true -f "$TMP_DIR/certs-values-2.yaml" >"$TMP_DIR/on2.yaml"
CK2=$(grep -o 'checksum/trusted-ca: "[a-f0-9]*"' "$TMP_DIR/on2.yaml")
[ -n "$CK1" ] && [ -n "$CK2" ] && [ "$CK1" != "$CK2" ] || {
  echo "FAIL: checksum/trusted-ca does not change when certs change" >&2
  exit 1
}

echo "trusted-ca guard OK"
