#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

render() {
  output="$1"
  shift
  helm template terminal-wiring "$CHART_DIR" \
    --set-string mysql.password=test \
    --set-string mysql.rootPassword=test \
    --set-string redis.password=test \
    "$@" >"$output"
}

render "$TMP_DIR/generated.yaml"
render "$TMP_DIR/existing.yaml" \
  --set-string terminal.existingSecret=operator-terminal \
  --set-string terminal.secretKey=relay-token
render "$TMP_DIR/external.yaml" \
  --set externalControlPlane.enabled=true \
  --set-string externalControlPlane.masterEndpoint=master.example.internal:8088 \
  --set controlPlane.master.enabled=false \
  --set-string terminal.existingSecret=external-terminal \
  --set-string terminal.secretKey=relay-token

if helm template terminal-wiring "$CHART_DIR" \
  --set-string mysql.password=test \
  --set-string mysql.rootPassword=test \
  --set-string redis.password=test \
  --set externalControlPlane.enabled=true \
  --set-string externalControlPlane.masterEndpoint=master.example.internal:8088 \
  --set controlPlane.master.enabled=false >/dev/null 2>&1; then
  echo "external control plane unexpectedly accepted an auto-generated terminal Secret" >&2
  exit 1
fi

python3 - "$TMP_DIR/generated.yaml" "$TMP_DIR/existing.yaml" "$TMP_DIR/external.yaml" <<'PY'
import pathlib
import re
import sys


def documents(path):
    return [doc for doc in pathlib.Path(path).read_text().split("\n---\n") if doc.strip()]


def terminal_secret(docs):
    matches = [doc for doc in docs if "\nkind: Secret\n" in f"\n{doc}\n" and re.search(r"(?m)^  internal-token:", doc)]
    if len(matches) != 1:
        raise SystemExit(f"expected one generated terminal Secret, found {len(matches)}")
    value = re.search(r'(?m)^  internal-token:\s*"([^"]+)"\s*$', matches[0])
    if not value or len(value.group(1)) < 16:
        raise SystemExit("generated terminal token is missing or too short")
    name = re.search(r"(?m)^  name:\s*(\S+)\s*$", matches[0])
    if not name:
        raise SystemExit("generated terminal Secret has no name")
    return name.group(1)


def deployment(docs, component):
    matches = [doc for doc in docs if "\nkind: Deployment\n" in f"\n{doc}\n" and f"app.kubernetes.io/component: {component}" in doc]
    if len(matches) != 1:
        raise SystemExit(f"expected one {component} Deployment, found {len(matches)}")
    return matches[0]


def assert_ref(doc, secret_name, key):
    pattern = rf"(?ms)- name: CUBE_TERMINAL_INTERNAL_TOKEN\s+valueFrom:\s+secretKeyRef:\s+name: {re.escape(secret_name)}\s+key: {re.escape(key)}\s*$"
    if not re.search(pattern, doc):
        raise SystemExit(f"missing terminal Secret reference {secret_name}/{key}")


generated = documents(sys.argv[1])
generated_name = terminal_secret(generated)
assert_ref(deployment(generated, "master"), generated_name, "internal-token")
assert_ref(deployment(generated, "ops"), generated_name, "internal-token")

existing = documents(sys.argv[2])
if any(re.search(r"(?m)^  relay-token:", doc) for doc in existing if "\nkind: Secret\n" in f"\n{doc}\n"):
    raise SystemExit("chart rendered a Secret even though terminal.existingSecret was set")
assert_ref(deployment(existing, "master"), "operator-terminal", "relay-token")
assert_ref(deployment(existing, "ops"), "operator-terminal", "relay-token")

external = documents(sys.argv[3])
if any(re.search(r"(?m)^  relay-token:", doc) for doc in external if "\nkind: Secret\n" in f"\n{doc}\n"):
    raise SystemExit("external control-plane mode rendered the operator-managed terminal Secret")
assert_ref(deployment(external, "ops"), "external-terminal", "relay-token")

for doc in generated + existing + external:
    if "\nkind: ConfigMap\n" in f"\n{doc}\n" and "CUBE_TERMINAL_INTERNAL_TOKEN" in doc:
        raise SystemExit("terminal token reference leaked into a ConfigMap")

print("terminal generated/existing Secret wiring OK")
PY
