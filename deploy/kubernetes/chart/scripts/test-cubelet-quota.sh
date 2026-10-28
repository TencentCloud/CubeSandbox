#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
# Helm rendering and staged dynamicconf regression tests; no cluster required.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(cd -- "$CHART_DIR/../../.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

render() {
  helm template quota-test "$CHART_DIR" \
    --set-string mysql.password=test --set-string mysql.rootPassword=test \
    --set-string redis.password=test "$@"
}

render > "$TMP_DIR/default.yaml"
render --set cubeNode.quota.mcpu_limit=32000 \
  --set-string cubeNode.quota.mem_limit=64Gi \
  --set cubeNode.quota.mvm_limit=100 \
  --set cubeNode.quota.creation_concurrent_num=4 \
  --set cubeNode.quota.paused_resource_release_ratio=0.5 > "$TMP_DIR/overrides.yaml"
render --set cubeNode.quota.mcpu_limit=0 \
  --set-string cubeNode.quota.mem_limit= \
  --set cubeNode.quota.paused_resource_release_ratio=0 > "$TMP_DIR/zero.yaml"

python3 - "$REPO_ROOT" "$TMP_DIR" <<'PY'
import os
from pathlib import Path
import re
import subprocess
import sys

root, tmp = map(Path, sys.argv[1:])
script = root / "deploy/kubernetes/images/scripts/configure-cubelet-quota.sh"
source = (root / "Cubelet/dynamicconf/conf.yaml").read_text()
target = tmp / "conf.yaml"
prefix = "CUBELET_QUOTA_"
clean_env = {k: v for k, v in os.environ.items() if not k.startswith(prefix)}

def rendered_env(name):
    manifest = (tmp / name).read_text()
    pairs = re.findall(r'- name: (CUBELET_QUOTA_\w+)\n\s+value: "([^"]*)"', manifest)
    assert len(pairs) == len(dict(pairs)), "quota env must occur only in cubelet"
    if pairs:
        node = next(doc for doc in manifest.split("\n---\n")
                    if "kind: DaemonSet\n" in doc
                    and "app.kubernetes.io/component: cube-node\n" in doc)
        cubelet = node.split("        - name: cubelet\n", 1)[1].split("\n        - name:", 1)[0]
        assert cubelet.count("- name: " + prefix) == len(pairs)
    return dict(pairs)

def apply(env, ok=True):
    result = subprocess.run(["bash", str(script), str(target)],
                            env={**clean_env, **env}, capture_output=True, text=True)
    assert (result.returncode == 0) == ok, result.stderr

assert rendered_env("default.yaml") == {}
overrides = rendered_env("overrides.yaml")
assert overrides == {prefix + k.upper(): v for k, v in {
    "mcpu_limit": "32000", "mem_limit": "64Gi", "mvm_limit": "100",
    "creation_concurrent_num": "4", "paused_resource_release_ratio": "0.5",
}.items()}, "Helm must expose all host.quota overrides"
zero = rendered_env("zero.yaml")
assert zero == {prefix + "MCPU_LIMIT": "0", prefix + "MEM_LIMIT": "",
                prefix + "PAUSED_RESOURCE_RELEASE_RATIO": "0"}

target.write_text(source)
apply({})
assert target.read_text() == source, "default configuration must be untouched"

# Simulate image staging on initial start and again after Pod recreation.
expected = source.replace("mcpu_limit: 0", "mcpu_limit: 32000") \
    .replace('mem_limit: ""', 'mem_limit: "64Gi"') \
    .replace("mvm_limit: 0", "mvm_limit: 100") \
    .replace("creation_concurrent_num: 0", "creation_concurrent_num: 4") \
    .replace("paused_resource_release_ratio: 0.0", "paused_resource_release_ratio: 0.5")
for _ in range(2):
    target.write_text(source)
    target.chmod(0o640)
    apply(overrides)
    assert target.read_text() == expected, "overrides must survive re-staging"
    assert target.stat().st_mode & 0o777 == 0o640
    apply(overrides)
    assert target.read_text() == expected, "application must be idempotent"

apply(zero)
assert '    mcpu_limit: 0\n' in target.read_text()
assert '    mem_limit: ""\n' in target.read_text()
assert '    paused_resource_release_ratio: 0\n' in target.read_text()
assert '    mvm_limit: 100\n' in target.read_text(), "omitted fields must survive"

# Older bundled configs can lack the new ratio field; add it within host.quota.
target.write_text(source.replace("    paused_resource_release_ratio: 0.0\n", ""))
apply({prefix + "PAUSED_RESOURCE_RELEASE_RATIO": "0.5"})
assert "    paused_resource_release_ratio: 0.5\n  gc:" in target.read_text()

for key, value in [("MCPU_LIMIT", "-1"), ("MVM_LIMIT", "1.5"),
                   ("CREATION_CONCURRENT_NUM", "no"), ("MEM_LIMIT", "bad\nvalue"),
                   ("MEM_LIMIT", "1K"),
                   ("PAUSED_RESOURCE_RELEASE_RATIO", "1.1"),
                   ("PAUSED_RESOURCE_RELEASE_RATIO", "NaN")]:
    target.write_text(source)
    apply({prefix + key: value}, ok=False)
    assert target.read_text() == source, "invalid input must not modify the config"

target.write_text("host:\n  gc: {}\n")
apply(overrides, ok=False)
assert target.read_text() == "host:\n  gc: {}\n"
print("PASS: quota rendering, startup re-staging, zero/empty values and validation")
PY

for setting in 'cubeNode.quota=invalid' 'cubeNode.quota.typo=1' \
  'cubeNode.quota.mcpu_limit=-1' 'cubeNode.quota.mvm_limit=1.5' \
  'cubeNode.quota.creation_concurrent_num=false' 'cubeNode.quota.mem_limit=invalid' \
  'cubeNode.quota.mem_limit=1K' \
  'cubeNode.quota.paused_resource_release_ratio=1.1' \
  'cubeNode.quota.paused_resource_release_ratio=NaN'; do
  if render --set "$setting" > "$TMP_DIR/invalid.yaml" 2> "$TMP_DIR/error"; then
    echo "FAIL: Helm accepted $setting" >&2
    exit 1
  fi
  grep -q 'cubeNode.quota' "$TMP_DIR/error"
done
echo 'PASS: Helm rejects invalid quota overrides'
