#!/usr/bin/env python3
"""
cubebox appsnapshot 脚本
用于为 cubebox 容器制作 app snapshot

使用方法:
    python3 cubebox_appsnapshot.py --cubebox_id <container_id>

示例:
    python3 cubebox_appsnapshot.py --cubebox_id abc123def456
"""

import argparse
import json
import re
import shlex
import shutil
import subprocess
import sys
from pathlib import Path

# 快照存储目录
CUBE_SNAPSHOT_DIR = Path("/data/cubelet/root/cube-snapshot/")
# cube-runtime 路径
CUBE_RUNTIME_PATH = "/usr/local/services/cubetoolbox/cube-shim/bin/cube-runtime"


# cubebox ID 合法字符：字母数字、下划线、短横线、点号
_SAFE_ID_RE = re.compile(r'^[a-zA-Z0-9_.-]+$')


def _validate_cubebox_id(cubebox_id):
    """校验 cubebox_id 不包含路径穿越字符"""
    if not cubebox_id or not _SAFE_ID_RE.match(cubebox_id):
        print(f"invalid cubebox_id: {cubebox_id!r} (must match {_SAFE_ID_RE.pattern})", file=sys.stderr)
        sys.exit(1)


def _parse_json_if_possible(value):
    """尝试将字符串解析为 JSON，失败则返回原值"""
    try:
        return json.loads(value)
    except Exception:
        return value


def _ensure_json_string(v):
    """确保值是 JSON 字符串格式"""
    if v is None:
        return ""
    if isinstance(v, str):
        return v
    return json.dumps(v, separators=(",", ":"))


def _format_cmd_for_shell(cmd_list):
    """将命令列表格式化为可执行的 shell 命令字符串"""
    return " ".join(shlex.quote(part) for part in cmd_list)


def get_cubebox_snapshot_spec(cubebox_id):
    """
    通过 cubecli 查询 cubebox 信息，并抽取快照制作所需字段。

    返回字典：
      - resource: 来自 Spec.annotations["cube.vmmres"]（若为 JSON 字符串则解析为对象）
      - disk:     来自 Spec.annotations["cube.disk"]（若为 JSON 字符串则解析为对象）
      - pmem:     来自 Spec.annotations["cube.pmem"]（若为 JSON 字符串则解析为对象）
      - kernel:   来自 Spec.annotations["cube.vm.kernel.path"]（字符串）
    """
    try:
        res = subprocess.run(
            ["cubecli", "c", "info", cubebox_id],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
            universal_newlines=True
        )
    except subprocess.CalledProcessError as e:
        print(e.stderr, file=sys.stderr)
        sys.exit(1)

    # 处理空输出（ID 不存在或返回空）
    if res.stdout is None or res.stdout.strip() == "":
        print(f"cubecli ctr info {cubebox_id} returned empty output (invalid cubebox_id?)", file=sys.stderr)
        sys.exit(1)

    try:
        info = json.loads(res.stdout)
    except Exception as e:
        print(f"parse cubecli output to JSON failed: {e}", file=sys.stderr)
        sys.exit(1)

    annotations = ((info or {}).get("Spec") or {}).get("annotations") or {}
    resource_raw = annotations.get("cube.vmmres")
    disk_raw = annotations.get("cube.disk")
    pmem_raw = annotations.get("cube.pmem")
    kernel = annotations.get("cube.vm.kernel.path")

    result = {
        "resource": _parse_json_if_possible(resource_raw) if resource_raw is not None else None,
        "disk": _parse_json_if_possible(disk_raw) if disk_raw is not None else None,
        "pmem": _parse_json_if_possible(pmem_raw) if pmem_raw is not None else None,
        "kernel": kernel
    }
    return result


def gen_cubebox_appsnapshot_cmd(cubebox_id, spec, snapshot_path, cube_runtime_path=None):
    """
    生成 cube-runtime snapshot --app-snapshot 命令（list 形式）。
    - vm-id: cubebox_id
    - path: snapshot_path
    """
    resource = spec.get("resource")
    disk = spec.get("disk")
    pmem = spec.get("pmem")
    kernel = spec.get("kernel") or ""

    # 使用传入的路径或默认路径
    runtime_path = cube_runtime_path if cube_runtime_path else CUBE_RUNTIME_PATH

    cmd = [
        runtime_path,
        "snapshot",
        "--app-snapshot",
        "--vm-id", str(cubebox_id),
        "--path", str(snapshot_path),
        "--resource", _ensure_json_string(resource),
        "--disk", _ensure_json_string(disk),
        "--pmem", _ensure_json_string(pmem),
        "--kernel", kernel,
        "--force",
    ]
    return cmd


def add_cubebox_snap(cubebox_id, snapshot_dir=None, cube_runtime_path=None):
    """
    为 cubebox 制作 app snapshot 的主函数

    Args:
        cubebox_id: cubebox 容器 ID
        snapshot_dir: 快照存储目录（可选，默认为 CUBE_SNAPSHOT_DIR）
        cube_runtime_path: cube-runtime 路径（可选，默认为 CUBE_RUNTIME_PATH）
    """
    _validate_cubebox_id(cubebox_id)
    spec = get_cubebox_snapshot_spec(cubebox_id)

    # 解析 CPU/Memory 计算目标路径
    resource_obj = spec.get("resource")
    if not resource_obj:
        print(f"resource is empty for cubebox {cubebox_id}", file=sys.stderr)
        sys.exit(1)
    cpu = resource_obj.get("cpu")
    memory = resource_obj.get("memory")

    # 使用传入的目录或默认目录
    base_dir = Path(snapshot_dir) if snapshot_dir else CUBE_SNAPSHOT_DIR

    # 目标路径和临时路径
    target_path = base_dir / "cubebox" / cubebox_id[:-2] / f"{cpu}C{memory}M"
    temp_path = base_dir / "cubebox" / cubebox_id[:-2] / f"{cpu}C{memory}M.tmp"

    # 清理可能存在的临时目录
    if temp_path.exists():
        print(f"temp path {temp_path} already exists, removing...")
        shutil.rmtree(temp_path)

    # 在临时目录制作快照
    cmd = gen_cubebox_appsnapshot_cmd(cubebox_id, spec, temp_path, cube_runtime_path)
    print(f"exec {_format_cmd_for_shell(cmd)}")

    try:
        res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True)
    except Exception as e:
        print(f"execute cmd failed: {e}", file=sys.stderr)
        sys.exit(1)

    if res.stdout:
        print(res.stdout)

    if res.returncode != 0:
        print(f"command exited with code {res.returncode}", file=sys.stderr)
        sys.exit(res.returncode)

    # 制作成功后，删除旧目录并移动临时目录到目标路径
    if target_path.exists():
        print(f"target path {target_path} already exists, removing...")
        shutil.rmtree(target_path)
    print(f"moving {temp_path} to {target_path}")
    shutil.move(str(temp_path), str(target_path))


def main():
    parser = argparse.ArgumentParser(
        prog="cubebox_appsnapshot.py",
        description="为 cubebox 容器制作 app snapshot",
        formatter_class=argparse.RawTextHelpFormatter
    )
    parser.add_argument(
        "--cubebox_id",
        type=str,
        required=True,
        help="cubebox 容器 ID (container0 id)"
    )
    parser.add_argument(
        "--snapshot_dir",
        type=str,
        default=None,
        help=f"快照存储目录 (默认: {CUBE_SNAPSHOT_DIR})"
    )
    parser.add_argument(
        "--cube_runtime",
        type=str,
        default=None,
        help=f"cube-runtime 路径 (默认: {CUBE_RUNTIME_PATH})"
    )

    args = parser.parse_args()

    add_cubebox_snap(args.cubebox_id, args.snapshot_dir, args.cube_runtime)


if __name__ == "__main__":
    main()
