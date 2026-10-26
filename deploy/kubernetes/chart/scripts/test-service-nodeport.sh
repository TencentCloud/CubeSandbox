#!/bin/sh
# Guard: optional fixed Service nodePorts render only when type allows them,
# and validate.yaml rejects ClusterIP + explicit nodePort / out-of-range values.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS="
  --set-string mysql.password=test
  --set-string mysql.rootPassword=test
  --set-string redis.password=test
"

fail_msg() {
  echo "FAIL: $1" >&2
  exit 1
}

# Default ClusterIP render must not emit nodePort fields.
helm template nodeport-default "$CHART_DIR" $COMMON_SETS > "$TMP_DIR/default.yaml"
if grep -E '^\s+nodePort:' "$TMP_DIR/default.yaml" >/dev/null; then
  fail_msg "default ClusterIP render unexpectedly includes nodePort"
fi

# Explicit NodePort + fixed ports must appear on api and proxy Services.
helm template nodeport-fixed "$CHART_DIR" $COMMON_SETS \
  --set controlPlane.api.service.type=NodePort \
  --set controlPlane.api.service.nodePort=30030 \
  --set cubeProxy.service.type=NodePort \
  --set cubeProxy.service.nodePorts.http=30080 \
  --set cubeProxy.service.nodePorts.https=30443 \
  --set cubeProxy.service.nodePorts.grpc=30090 \
  --set cubeProxy.service.nodePorts.admin=30082 \
  --set cubeProxy.ingress.enabled=false \
  > "$TMP_DIR/fixed.yaml"

awk '
  /^kind: Service$/ { in_svc=1; name=""; component=""; next }
  in_svc && /^  name: / { name=$2 }
  in_svc && /app.kubernetes.io\/component: / { component=$2 }
  in_svc && /^---$/ { in_svc=0 }
  in_svc && name != "" && component == "api" && /nodePort: 30030/ { api_np=1 }
  in_svc && name != "" && component == "cube-proxy" && /nodePort: 30080/ { proxy_http=1 }
  in_svc && name != "" && component == "cube-proxy" && /nodePort: 30443/ { proxy_https=1 }
  in_svc && name != "" && component == "cube-proxy" && /nodePort: 30090/ { proxy_grpc=1 }
  in_svc && name != "" && component == "cube-proxy" && /nodePort: 30082/ { proxy_admin=1 }
  END {
    if (!api_np) { print "api nodePort 30030 missing"; exit 1 }
    if (!proxy_http) { print "proxy http nodePort 30080 missing"; exit 1 }
    if (!proxy_https) { print "proxy https nodePort 30443 missing"; exit 1 }
    if (!proxy_grpc) { print "proxy grpc nodePort 30090 missing"; exit 1 }
    if (!proxy_admin) { print "proxy admin nodePort 30082 missing"; exit 1 }
  }
' "$TMP_DIR/fixed.yaml" || fail_msg "fixed nodePorts not rendered as expected"

# ClusterIP + explicit nodePort must fail render.
if helm template nodeport-bad-type "$CHART_DIR" $COMMON_SETS \
  --set controlPlane.api.service.nodePort=30030 \
  >"$TMP_DIR/bad-type.out" 2>"$TMP_DIR/bad-type.err"; then
  fail_msg "expected fail when ClusterIP api sets nodePort"
fi
grep -q 'controlPlane.api.service.nodePort requires service.type' "$TMP_DIR/bad-type.err" \
  || fail_msg "missing api ClusterIP+nodePort guard message"

if helm template nodeport-bad-proxy "$CHART_DIR" $COMMON_SETS \
  --set cubeProxy.service.nodePorts.http=30080 \
  >"$TMP_DIR/bad-proxy.out" 2>"$TMP_DIR/bad-proxy.err"; then
  fail_msg "expected fail when ClusterIP proxy sets nodePorts.http"
fi
grep -q 'cubeProxy.service.nodePorts.http requires service.type' "$TMP_DIR/bad-proxy.err" \
  || fail_msg "missing proxy ClusterIP+nodePort guard message"

# Out-of-range nodePort must fail.
if helm template nodeport-range "$CHART_DIR" $COMMON_SETS \
  --set controlPlane.api.service.type=NodePort \
  --set controlPlane.api.service.nodePort=80 \
  >"$TMP_DIR/range.out" 2>"$TMP_DIR/range.err"; then
  fail_msg "expected fail for api nodePort outside 30000-32767"
fi
grep -q 'must be in 30000-32767' "$TMP_DIR/range.err" \
  || fail_msg "missing api nodePort range guard message"

# Unknown nodePorts key must fail (typos must not silently become auto-allocation).
if helm template nodeport-unknown-key "$CHART_DIR" $COMMON_SETS \
  --set cubeProxy.service.type=NodePort \
  --set cubeProxy.service.nodePorts.htttp=30080 \
  --set cubeProxy.ingress.enabled=false \
  >"$TMP_DIR/unknown.out" 2>"$TMP_DIR/unknown.err"; then
  fail_msg "expected fail for unknown cubeProxy.service.nodePorts key"
fi
grep -q 'cubeProxy.service.nodePorts.htttp is not supported' "$TMP_DIR/unknown.err" \
  || fail_msg "missing unknown nodePorts key guard message"

echo "Service nodePort guard passed"
