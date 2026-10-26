#!/bin/sh
# Guard: the self-signed CubeProxy certificate must cover the sandbox host
# shape <port>-<sandboxId>.<cubeProxy.domain>. Also covers the opt-in
# rotateOnSanChange contract and the empty-domain edge case.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="--set-string mysql.password=test --set-string mysql.rootPassword=test --set-string redis.password=test"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

render() {
  name="$1"
  shift
  helm template "$name" "$CHART_DIR" $COMMON_SETS "$@" > "$TMP_DIR/$name.yaml"
}

extract_secret() {
  input="$1"
  output="$2"
  awk 'BEGIN { RS="\n---\n"; ORS="\n---\n" }
       /kind: Secret/ && /proxy-certs/ { print; found=1 }
       END { exit found ? 0 : 1 }' "$input" > "$output"
}

extract_deployment() {
  input="$1"
  output="$2"
  awk 'BEGIN { RS="\n---\n"; ORS="\n---\n" }
       /kind: Deployment/ && /component: cube-proxy/ { print; found=1 }
       END { exit found ? 0 : 1 }' "$input" > "$output"
}

extract_san() {
  secret="$1"
  crt="$2"
  awk '
    f && /^  "tls.key": \|-/ { exit }
    f { print }
    /^  "tls.crt": \|-/ { f=1 }
  ' "$secret" | sed 's/^    //' > "$crt"
  openssl x509 -in "$crt" -noout -ext subjectAltName \
    | tr '\n' ' ' | sed 's/  */ /g'
}

extract_annotation() {
  input="$1"
  key="$2"
  grep -o "$key: \"[0-9a-f]\{64\}\"" "$input" | head -n 1 | sed 's/.*: "//; s/"$//'
}

expect_contains() {
  haystack="$1"
  needle="$2"
  label="$3"
  case "$haystack" in
    *"$needle"*) ;;
    *) fail "$label: missing '$needle' in '$haystack'" ;;
  esac
}

expect_not_contains() {
  haystack="$1"
  needle="$2"
  label="$3"
  case "$haystack" in
    *"$needle"*) fail "$label: unexpected '$needle' in '$haystack'" ;;
  esac
}

expect_fail() {
  label="$1"
  pattern="$2"
  shift 2
  if helm template "$label" "$CHART_DIR" $COMMON_SETS "$@" > "$TMP_DIR/$label.out" 2>&1; then
    fail "$label: helm template should have failed"
  fi
  grep -q "$pattern" "$TMP_DIR/$label.out" \
    || fail "$label: expected error containing '$pattern', got: $(cat "$TMP_DIR/$label.out")"
}

# 1) Default domain: SAN set unchanged; no new annotations.
render default --set cubeProxy.tls.mode=selfSigned
extract_secret "$TMP_DIR/default.yaml" "$TMP_DIR/default-secret.yaml"
extract_deployment "$TMP_DIR/default.yaml" "$TMP_DIR/default-deployment.yaml"
default_san="$(extract_san "$TMP_DIR/default-secret.yaml" "$TMP_DIR/default.crt")"
echo "default SANs: $default_san"
expect_contains "$default_san" "DNS:cube.app" "default"
expect_contains "$default_san" "DNS:*.cube.app" "default"
expect_contains "$default_san" "DNS:localhost" "default"
expect_contains "$default_san" "IP Address:127.0.0.1" "default"
expect_not_contains "$default_san" "DNS:*.cube.app, DNS:*.cube.app" "default duplicate"
if grep -q "cube.tencent.com/self-signed-cert-spec" "$TMP_DIR/default-secret.yaml"; then
  fail "default render should not add the self-signed cert spec annotation"
fi
if grep -q "checksum/proxy-cert" "$TMP_DIR/default-deployment.yaml"; then
  fail "default render should not add the proxy cert checksum"
fi

# 2) Custom domain with the default reuse behavior: wildcard SAN, no rotation.
render custom --set cubeProxy.tls.mode=selfSigned --set cubeProxy.domain=sandbox.example.com
extract_secret "$TMP_DIR/custom.yaml" "$TMP_DIR/custom-secret.yaml"
custom_san="$(extract_san "$TMP_DIR/custom-secret.yaml" "$TMP_DIR/custom.crt")"
echo "custom SANs: $custom_san"
expect_contains "$custom_san" "DNS:sandbox.example.com" "custom"
expect_contains "$custom_san" "DNS:*.sandbox.example.com" "custom"
if grep -q "cube.tencent.com/self-signed-cert-spec" "$TMP_DIR/custom-secret.yaml"; then
  fail "custom render without rotateOnSanChange should not add the spec annotation"
fi

# 3) Opt-in rotation: spec annotation and pod checksum are present.
render custom-rotate --set cubeProxy.tls.mode=selfSigned \
  --set cubeProxy.domain=sandbox.example.com \
  --set cubeProxy.tls.selfSigned.rotateOnSanChange=true
extract_secret "$TMP_DIR/custom-rotate.yaml" "$TMP_DIR/custom-rotate-secret.yaml"
extract_deployment "$TMP_DIR/custom-rotate.yaml" "$TMP_DIR/custom-rotate-deployment.yaml"
rotate_san="$(extract_san "$TMP_DIR/custom-rotate-secret.yaml" "$TMP_DIR/custom-rotate.crt")"
echo "custom-rotate SANs: $rotate_san"
expect_contains "$rotate_san" "DNS:*.sandbox.example.com" "custom-rotate"
spec_hash="$(extract_annotation "$TMP_DIR/custom-rotate-secret.yaml" "cube.tencent.com/self-signed-cert-spec")"
[ -n "$spec_hash" ] || fail "custom-rotate: missing self-signed cert spec annotation"
cert_hash="$(extract_annotation "$TMP_DIR/custom-rotate-deployment.yaml" "checksum/proxy-cert")"
[ -n "$cert_hash" ] || fail "custom-rotate: missing checksum/proxy-cert annotation"
echo "spec hash: $spec_hash"
echo "cert checksum: $cert_hash"
expected_cert_hash="$(python3 - "$TMP_DIR/custom-rotate.crt" <<'PY'
import hashlib
import sys
print(hashlib.sha256(open(sys.argv[1], "rb").read().strip()).hexdigest())
PY
)"
[ "$cert_hash" = "$expected_cert_hash" ] \
  || fail "checksum/proxy-cert must hash the rendered certificate ($cert_hash != $expected_cert_hash)"

# 4) Empty domain: no literal '*.', no zero-length DNS SAN.
render empty --set cubeProxy.tls.mode=selfSigned \
  --set cubeProxy.domain= \
  --set cubeProxy.configureClusterDNS=false \
  --set cubeProxy.ingress.enabled=false \
  --set cubeProxy.tls.selfSigned.rotateOnSanChange=true
extract_secret "$TMP_DIR/empty.yaml" "$TMP_DIR/empty-secret.yaml"
empty_san="$(extract_san "$TMP_DIR/empty-secret.yaml" "$TMP_DIR/empty.crt")"
echo "empty SANs: $empty_san"
expect_not_contains "$empty_san" "DNS:*.," "empty"
expect_not_contains "$empty_san" "DNS:*. " "empty"
expect_not_contains "$empty_san" "DNS:, " "empty"

# 5) dnsNames order must not change the recorded spec hash.
cat > "$TMP_DIR/dns-a.yaml" <<'YAML'
cubeProxy:
  domain: sandbox.example.com
  tls:
    mode: selfSigned
    selfSigned:
      rotateOnSanChange: true
      dnsNames:
        - z.example.com
        - a.example.com
YAML
cat > "$TMP_DIR/dns-b.yaml" <<'YAML'
cubeProxy:
  domain: sandbox.example.com
  tls:
    mode: selfSigned
    selfSigned:
      rotateOnSanChange: true
      dnsNames:
        - a.example.com
        - z.example.com
YAML
helm template order-a "$CHART_DIR" $COMMON_SETS -f "$TMP_DIR/dns-a.yaml" > "$TMP_DIR/order-a.yaml"
helm template order-b "$CHART_DIR" $COMMON_SETS -f "$TMP_DIR/dns-b.yaml" > "$TMP_DIR/order-b.yaml"
extract_secret "$TMP_DIR/order-a.yaml" "$TMP_DIR/order-a-secret.yaml"
extract_secret "$TMP_DIR/order-b.yaml" "$TMP_DIR/order-b-secret.yaml"
hash_a="$(extract_annotation "$TMP_DIR/order-a-secret.yaml" "cube.tencent.com/self-signed-cert-spec")"
hash_b="$(extract_annotation "$TMP_DIR/order-b-secret.yaml" "cube.tencent.com/self-signed-cert-spec")"
[ -n "$hash_a" ] || fail "order-a: missing self-signed cert spec annotation"
[ -n "$hash_b" ] || fail "order-b: missing self-signed cert spec annotation"
[ "$hash_a" = "$hash_b" ] || fail "dnsNames order changed the spec hash: $hash_a != $hash_b"
order_san="$(extract_san "$TMP_DIR/order-a-secret.yaml" "$TMP_DIR/order-a.crt")"
expect_contains "$order_san" "DNS:a.example.com" "order-a additive"
expect_contains "$order_san" "DNS:z.example.com" "order-a additive"

# 6) certManager mode is untouched by the self-signed rotation switch.
render certmanager --set cubeProxy.tls.mode=certManager \
  --set cubeProxy.tls.certManager.issuerRef.name=test \
  --set cubeProxy.tls.selfSigned.rotateOnSanChange=true
grep -q "kind: Certificate" "$TMP_DIR/certmanager.yaml" \
  || fail "certmanager: Certificate document missing"
if grep -q "cube.tencent.com/self-signed-cert-spec\|checksum/proxy-cert" "$TMP_DIR/certmanager.yaml"; then
  fail "certmanager: self-signed annotations leaked into certManager mode"
fi

# 7) Validation: wildcard base domain and non-boolean rotation flag.
expect_fail invalid-domain "must be the base domain" \
  --set-string 'cubeProxy.domain=*.example.com'
expect_fail invalid-flag "must be a boolean" \
  --set-string cubeProxy.tls.selfSigned.rotateOnSanChange=yes

echo "Proxy self-signed SAN and rotation guard passed"
