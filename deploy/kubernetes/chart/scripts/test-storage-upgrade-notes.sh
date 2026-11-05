#!/bin/sh
# Keep the one-time storage upgrade warning explicit in both languages.
# Documentation assertions only; no cluster, mounts or sandbox survival checks.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "$SCRIPT_DIR/../../../.." && pwd)"

python3 - "$REPO_ROOT" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
cases = (
    ("docs/guide/kubernetes/upgrade.md", "One-time storage upgrade", (
        "lacks the eight storage environment variables",
        "changes the `cube-node` Pod template",
        "even when all `hostPaths` values are unchanged or default",
        "default `RollingUpdate` strategy",
        "recreates the Big Pod on every compute node",
        "Drain compute nodes before this upgrade",
        "**Pod network** (`cubeNode.hostNetwork: false`): recreation destroys the netns",
        "interrupts existing sandbox networking. Drain is mandatory.",
        "**Host network (default)**: taps survive",
        "drain remains the supported procedure",
        "until sandbox survival through a cubelet restart is live-validated",
    )),
    ("docs/zh/guide/kubernetes/upgrade.md", "存储升级的一次性提示", (
        "尚未包含这八个存储环境变量",
        "会修改 `cube-node` Pod template",
        "即使所有 `hostPaths` 值未改动或仍为默认值",
        "默认的 `RollingUpdate` 策略",
        "重建每个计算节点上的 Big Pod",
        "本次升级前先排空计算节点上的沙箱",
        "**Pod 网络**（`cubeNode.hostNetwork: false`）：重建会销毁 netns",
        "中断现有沙箱网络，必须排空",
        "**宿主机网络（默认）**：tap 设备会保留",
        "在 cubelet 重启后沙箱存活得到实机验证前",
        "排空仍是支持的升级流程",
    )),
)

for path, title, required in cases:
    document = (root / path).read_text()
    warnings = re.findall(r"(?m)^::: warning " + re.escape(title)
                          + r"\n([\s\S]*?)^:::$", document)
    assert len(warnings) == 1, f"{path}: expected one visible storage upgrade warning"
    warning = " ".join(warnings[0].split())
    for phrase in ("`wait-node-prep`", "../node-operations.md", *required):
        assert phrase in warning, f"{path}: missing upgrade guidance: {phrase}"
    print(f"ok: {path}: default-path recreation, drain and both network modes")
PY
