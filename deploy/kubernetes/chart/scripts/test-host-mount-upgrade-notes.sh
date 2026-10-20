#!/bin/sh
# Render the custom Host Mount migration warning for installs and upgrades.
# This uses helm template only; no cluster or release is created.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

python3 - "$CHART_DIR" "$TMP_DIR" <<'PY'
import pathlib
import shutil
import subprocess
import sys

chart, tmp = map(pathlib.Path, sys.argv[1:])
test_chart = tmp / "chart"
shutil.copytree(chart, test_chart)

# Helm template omits NOTES.txt. Wrap the actual notes template in a ConfigMap
# in an isolated chart copy so it is rendered with the normal release context.
notes = (test_chart / "templates/NOTES.txt").read_text()
template_name = "templates/host-mount-notes-test.yaml"
(test_chart / template_name).write_text(
    '{{- define "test.hostMountNotes" -}}\n'
    + notes
    + '\n{{- end -}}\n'
    'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: host-mount-notes-test\n'
    'data:\n  notes: |{{ include "test.hostMountNotes" . | nindent 4 }}\n'
)

warning_title = "WARNING (custom hostPaths.dataShared requires Host Mount migration):"
custom_path = "/var/lib/cubesandbox/shared"


def render(upgrade, path=None):
    args = [
        "helm", "template", "host-mount-notes", str(test_chart),
        "--show-only", template_name,
        "--set-string", "mysql.password=test",
        "--set-string", "mysql.rootPassword=test",
        "--set-string", "redis.password=test",
    ]
    if upgrade:
        args.append("--is-upgrade")
    if path is not None:
        args += ["--set-string", "hostPaths.dataShared=" + path]
    rendered = subprocess.check_output(args, text=True)
    _, body = rendered.split("\n  notes: |\n", 1)
    return "\n".join(line[4:] if line.startswith("    ") else line
                     for line in body.splitlines()).strip()


defaults = []
for upgrade in (False, True):
    mode = "upgrade" if upgrade else "install"
    default = render(upgrade)
    assert warning_title not in default, f"{mode}: default path must not warn"
    assert default == render(upgrade, "/data/shared"), (
        f"{mode}: an explicit default path must preserve the default notes"
    )
    defaults.append(default)

    custom = render(upgrade, custom_path)
    assert custom.count(warning_title) == 1, f"{mode}: expected one migration warning"
    start = custom.index(warning_title)
    end = custom.index("\n\nPersistence ", start)
    warning = custom[start:end]
    for required in (
        "Earlier chart versions",
        f"Pod at {custom_path}",
        f'{custom_path}/... as metadata["host-mount"] source paths.',
        "callers MUST use /data/shared/...",
        "CubeMaster extra_conf.allowed_host_mount_prefixes MUST be updated",
        "/data/shared/ prefix (or narrower subdirectories)",
        "including when CubeMaster is",
        "external to this release",
        "Coordinate the caller and allowlist changes with the upgrade",
        "Host Mount requests will fail after upgrade",
        f"The host directory stays {custom_path}; no data is migrated.",
    ):
        assert required in warning, f"{mode}: missing migration guidance: {required}"
    assert custom[:start] + custom[end + 2:] == default, (
        f"{mode}: the warning must be the only change to custom-path notes"
    )
    print(f"ok: {mode} default notes unchanged; custom Host Mount migration warning")

assert defaults[0] == defaults[1], "default notes must match for install and upgrade"
PY
