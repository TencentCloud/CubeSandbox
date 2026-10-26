#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

render() {
  output="$1"
  shift
  helm template registry-host "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" > "$output"
}

extract_registry_host() {
  input="$1"
  python3 - "$input" <<'PY'
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
m = re.search(r"name: CUBE_PROXY_REGISTRY_REDIS_HOST\s*\n\s*value:\s*\"([^\"]*)\"", text)
if not m:
    raise SystemExit("CUBE_PROXY_REGISTRY_REDIS_HOST not found in rendered output")
sys.stdout.write(m.group(1) + "\n")
PY
}

extract_checksum() {
  input="$1"
  python3 - "$input" <<'PY'
import pathlib
import re
import sys

def main():
    text = pathlib.Path(sys.argv[1]).read_text()
    for doc in text.split("\n---\n"):
        if re.search(r"^kind: Deployment$", doc, re.M) and "app.kubernetes.io/component: cube-proxy" in doc:
            m = re.search(r"checksum/entrypoint:\s*\"([0-9a-f]{64})\"", doc)
            if not m:
                raise SystemExit("checksum/entrypoint annotation not found on proxy Deployment")
            sys.stdout.write(m.group(1) + "\n")
            return
    raise SystemExit("cube-proxy Deployment not found")

main()
PY
}

render "$TMP_DIR/builtin.yaml"
host_builtin="$(extract_registry_host "$TMP_DIR/builtin.yaml")"

expected="registry-host-cube-redis.default.svc.cluster.local"
if [ "$host_builtin" != "$expected" ]; then
  echo "expected builtin registry host '$expected', got '$host_builtin'" >&2
  exit 1
fi
echo "builtin registry host OK: $host_builtin"

render "$TMP_DIR/external.yaml" --set-string redis.host=redis.ext.example.com
host_external="$(extract_registry_host "$TMP_DIR/external.yaml")"
if [ "$host_external" != "redis.ext.example.com" ]; then
  echo "expected external registry host passthrough, got '$host_external'" >&2
  exit 1
fi
echo "external registry host passthrough OK: $host_external"

render "$TMP_DIR/external-ip.yaml" --set-string redis.host=10.20.30.40
host_ip="$(extract_registry_host "$TMP_DIR/external-ip.yaml")"
if [ "$host_ip" != "10.20.30.40" ]; then
  echo "expected IP literal passthrough, got '$host_ip'" >&2
  exit 1
fi
echo "external registry IP passthrough OK: $host_ip"

render "$TMP_DIR/sentinel.yaml" \
  --set-string redis.masterName=mymaster \
  --set-string redis.sentinelNodes=10.0.0.11:26379
host_sentinel="$(extract_registry_host "$TMP_DIR/sentinel.yaml")"
if [ -n "$host_sentinel" ]; then
  echo "expected empty registry host in sentinel mode, got '$host_sentinel'" >&2
  exit 1
fi
echo "sentinel registry host empty OK"

checksum_base="$(extract_checksum "$TMP_DIR/builtin.yaml")"
[ -n "$checksum_base" ] || { echo "empty checksum/entrypoint" >&2; exit 1; }

CHART_COPY="$TMP_DIR/chart-copy"
cp -r "$CHART_DIR" "$CHART_COPY"
printf '\n# guard: change to force a different checksum\n' >> "$CHART_COPY/files/cube-proxy/cube-proxy-entrypoint.sh"
helm template registry-host "$CHART_COPY" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  > "$TMP_DIR/tampered.yaml"
checksum_tampered="$(extract_checksum "$TMP_DIR/tampered.yaml")"
if [ "$checksum_base" = "$checksum_tampered" ]; then
  echo "checksum/entrypoint did not change when entrypoint content changed" >&2
  exit 1
fi
echo "checksum/entrypoint OK (tracks entrypoint content)"

echo "Proxy registry host guard passed"
