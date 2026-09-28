#!/bin/sh
# Guard: cube.cubeImage rewrites only official Cube TCR hosts
# (cube-sandbox-int / cube-sandbox-cn). Private repositories stay as
# declared so values-cn.yaml can mix with a partial image override.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"

# Component image tag the chart renders by default (values.yaml `tag: vX.Y.Z`).
# Derive it at runtime so this guard survives release bumps and never
# hard-codes a release tag that scripts/bump-image.sh --check would flag.
TAG="$(awk '/^[[:space:]]+tag:[[:space:]]+v[0-9]/{print $2; exit}' "$CHART_DIR/values.yaml")"
if [ -z "$TAG" ]; then
  echo "error: no component image tag (tag: vX.Y.Z) in values.yaml" >&2
  exit 1
fi

python3 - "$CHART_DIR" "$TAG" <<'PY'
import pathlib
import re
import subprocess
import sys

chart = sys.argv[1]
tag = sys.argv[2]
custom = f"{tag}-custom"

common = [
    "helm",
    "template",
    "cube-image-registry-guard",
    chart,
    "--set-string",
    "mysql.password=test",
    "--set-string",
    "mysql.rootPassword=test",
    "--set-string",
    "redis.password=test",
]


def render(*extra: str) -> str:
    proc = subprocess.run(
        common + list(extra),
        check=True,
        capture_output=True,
        text=True,
    )
    return proc.stdout


# Primary (non-init) container name per component. The guard asserts only the
# image of this container, so a future initContainer / sidecar does not break
# the assertions (the registry-rewrite behavior it checks is per-image).
PRIMARY_CONTAINER = {"ops": "cube-ops", "master": "cube-master", "mysql": "mysql"}


def find_sts_or_deploy_image(text: str, component: str) -> str:
    container = PRIMARY_CONTAINER[component]
    docs = [d for d in text.split("\n---\n") if d.strip()]
    matches = []
    for doc in docs:
        body = f"\n{doc}\n"
        if "\nkind: Deployment\n" not in body and "\nkind: StatefulSet\n" not in body:
            continue
        if f"\n    app.kubernetes.io/component: {component}\n" not in body:
            continue
        m = re.search(
            r"- name:\s+" + re.escape(container) + r"\s*\n(\s+)image:\s*([^\n]+)",
            doc,
        )
        if not m:
            raise SystemExit(
                f"container '{container}' not found in {component} Deployment/StatefulSet"
            )
        matches.append(m.group(2).strip().strip('"'))
    if len(matches) != 1:
        raise SystemExit(f"expected one image for {component}, found {matches}")
    return matches[0]


def expect(label: str, got: str, want: str) -> None:
    if got != want:
        raise SystemExit(f"{label}: expected {want!r}, got {got!r}")
    print(f"ok: {label} -> {got}")


INT = "cube-sandbox-int.tencentcloudcr.com"
CN = "cube-sandbox-cn.tencentcloudcr.com"
MIRROR = "my-mirror.example.com"
PRIVATE = "private.example.com"

# The official-host allowlist lives in templates/_helpers.tpl
# (cube.officialImageRegistries). Assert it still names both hosts so this
# guard cannot pass if the template allowlist drifts (e.g. cn dropped).
helpers_text = (pathlib.Path(chart) / "templates" / "_helpers.tpl").read_text()
for host in (INT, CN):
    if host not in helpers_text:
        raise SystemExit(f"cube.officialImageRegistries no longer mentions {host}")
print("ok: cube.officialImageRegistries still lists both official hosts")

# 1. Chart defaults stay on TCR int.
default = render()
expect(
    "default cube-ops",
    find_sts_or_deploy_image(default, "ops"),
    f"{INT}/cube-sandbox/cube-ops:{tag}",
)
expect(
    "default cube-master",
    find_sts_or_deploy_image(default, "master"),
    f"{INT}/cube-sandbox/cube-master:{tag}",
)
expect(
    "default mysql (cube.image, no rewrite)",
    find_sts_or_deploy_image(default, "mysql"),
    "mysql:8.0",
)

# 2. values-cn.yaml rewrites official Cube hosts and third-party images.
cn_file = str(pathlib.Path(chart) / "values-cn.yaml")
cn = render("-f", cn_file)
expect(
    "values-cn cube-ops",
    find_sts_or_deploy_image(cn, "ops"),
    f"{CN}/cube-sandbox/cube-ops:{tag}",
)
if f"{CN}/cube-sandbox/cube-kernel:{tag}" not in cn:
    raise SystemExit("values-cn must rewrite official cube-kernel to CN")
print(f"ok: values-cn cube-kernel -> {CN}/cube-sandbox/cube-kernel:{tag}")
expect(
    "values-cn mysql",
    find_sts_or_deploy_image(cn, "mysql"),
    f"{CN}/cube-sandbox/mysql:8.0",
)

# 3. values-cn + one private repo: private host stays, uncovered official images go CN.
partial = render(
    "-f",
    cn_file,
    "--set-string",
    f"images.ops.repository={PRIVATE}/cube-sandbox/cube-ops",
    "--set-string",
    f"images.ops.tag={custom}",
)
expect(
    "cn + private cube-ops",
    find_sts_or_deploy_image(partial, "ops"),
    f"{PRIVATE}/cube-sandbox/cube-ops:{custom}",
)
expect(
    "cn + private cube-ops leaves cube-master on CN",
    find_sts_or_deploy_image(partial, "master"),
    f"{CN}/cube-sandbox/cube-master:{tag}",
)
if f"{PRIVATE}/cube-sandbox/cube-kernel:" in partial:
    raise SystemExit("cn + private ops must not rewrite cube-kernel to the private host")
if f"{CN}/cube-sandbox/cube-kernel:{tag}" not in partial:
    raise SystemExit("cn + private ops must keep official cube-kernel on CN")
print("ok: cn + private cube-ops leaves cube-kernel on CN")

# 4. Full-mirror imageRegistry still rewrites official defaults.
mirrored = render("--set-string", f"global.imageRegistry={MIRROR}")
expect(
    "full-mirror cube-ops",
    find_sts_or_deploy_image(mirrored, "ops"),
    f"{MIRROR}/cube-sandbox/cube-ops:{tag}",
)
expect(
    "full-mirror cube-master",
    find_sts_or_deploy_image(mirrored, "master"),
    f"{MIRROR}/cube-sandbox/cube-master:{tag}",
)
expect(
    "full-mirror mysql stays on default (cube.image)",
    find_sts_or_deploy_image(mirrored, "mysql"),
    "mysql:8.0",
)

# 5. Full-mirror + private ops: private host is not rewritten.
mixed = render(
    "--set-string",
    f"global.imageRegistry={MIRROR}",
    "--set-string",
    f"images.ops.repository={PRIVATE}/cube-sandbox/cube-ops",
    "--set-string",
    "images.ops.tag=custom",
)
expect(
    "full-mirror + private cube-ops",
    find_sts_or_deploy_image(mixed, "ops"),
    f"{PRIVATE}/cube-sandbox/cube-ops:custom",
)
expect(
    "full-mirror + private cube-ops still mirrors cube-master",
    find_sts_or_deploy_image(mixed, "master"),
    f"{MIRROR}/cube-sandbox/cube-master:{tag}",
)

print("ok: cube.cubeImage rewrites only official Cube TCR hosts")
PY

echo "All cube.cubeImage registry guard tests passed"
