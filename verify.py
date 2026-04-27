#!/usr/bin/env python3
"""
verify.py — 在 254（或任意远程机器）上运行，验证完整数据流链路。

在远程机器上：
    pip3 install httpx requests -q
    python3 verify.py
"""
import os, sys

# ---- 配置（也可以用环境变量） ----
os.environ.setdefault("CUBE_API_URL",       "http://9.135.79.34:3000")
os.environ.setdefault("CUBE_TEMPLATE_ID",   "tpl-6265796cee124256b4dcd6a1")
os.environ.setdefault("CUBE_PROXY_NODE_IP", "9.135.79.34")   # 绕过 *.cube.app DNS

sys.path.insert(0, os.path.dirname(__file__))
from cube_e2b_code_interpreter import Sandbox

def main():
    print("=" * 55)
    print("  cube_e2b_code_interpreter — 验证数据流")
    print(f"  API:   {os.environ['CUBE_API_URL']}")
    print(f"  PROXY: {os.environ['CUBE_PROXY_NODE_IP']}")
    print("=" * 55)

    with Sandbox.create() as sb:
        print(f"\n[1] Sandbox 创建成功")
        print(f"    id:     {sb.sandbox_id}")
        print(f"    host:   {sb.get_host(49999)}")
        print(f"    proxy:  {sb._cfg.proxy_node_ip} (直连，无需 DNS)")

        # ---- 完全对齐 e2b_code_interpreter 用法 ----
        print("\n[2] run_code — 变量持久化测试")
        sb.run_code("x = 1")
        execution = sb.run_code("x += 1; x")
        print(f"    execution.text = {execution.text!r}  (期望: '2')")
        assert execution.text == "2", f"期望 '2'，实际 {execution.text!r}"
        print("    ✅ PASS")

        print("\n[3] run_code — stdout 流式输出测试")
        lines_received = []
        execution2 = sb.run_code(
            "for i in range(3): print(f'line {i}')",
            on_stdout=lambda msg: lines_received.append(msg.text)
        )
        print(f"    stdout lines: {execution2.logs.stdout}")
        print(f"    callback got: {lines_received}")
        # envd 可能合并多行为一条，检查内容包含即可
        all_stdout = "".join(execution2.logs.stdout)
        assert "line 0" in all_stdout and "line 2" in all_stdout
        print("    ✅ PASS")

        print("\n[4] run_code — 异常捕获测试")
        execution3 = sb.run_code("1/0")
        print(f"    error.name  = {execution3.error.name!r}")
        print(f"    error.value = {execution3.error.value!r}")
        assert execution3.error is not None
        print("    ✅ PASS")

        print("\n[5] run_code — 复杂表达式")
        execution4 = sb.run_code("sum(range(101))")
        print(f"    execution.text = {execution4.text!r}  (期望: '5050')")
        assert execution4.text == "5050"
        print("    ✅ PASS")

    print("\n" + "=" * 55)
    print("  全部通过 ✅  Sandbox 已自动销毁")
    print("=" * 55)

if __name__ == "__main__":
    main()
