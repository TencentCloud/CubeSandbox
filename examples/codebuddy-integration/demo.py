"""
CodeBuddy CLI × CubeSandbox Integration Demo

Demonstrates two capabilities:
1. Native sandbox routing: run `codebuddy --sandbox <url>` on host (simplest path)
2. In-sandbox mode: run `codebuddy -p` inside a MicroVM

Usage:
    cp .env.example .env   # fill in real values
    pip install -r requirements.txt

    # Native sandbox — simple chat test (default, works in any environment)
    python demo.py

    # Native sandbox — tool-execution variant (exercises bash tool in sandbox;
    # requires DNS-resolvable *.cube.app domain — not available in port-forwarded dev setups)
    python demo.py --native-sandbox --prompt "Use bash to create /workspace/hello.py that prints 'Hello from CubeSandbox!', run it with python3, and report the output."

    # In-sandbox mode (codebuddy inside MicroVM)
    python demo.py --in-sandbox
    python demo.py --in-sandbox --prompt "Use bash to list files in /workspace"

    # HTTP API demo
    python demo.py --http-api
"""

import argparse
import json
import os
import threading
import time
from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox

# ---------------------------------------------------------------------------
# envd compatibility patches (same as openai-agents-example):
# 1. envd only supports "root" user
# 2. envd may not support `stdin` kwarg in commands.run()
# ---------------------------------------------------------------------------
# Apply patches if using E2B SDK under the hood
try:
    import e2b.envd.rpc as _e2b_rpc
    _e2b_rpc.default_username = "root"
except Exception:
    pass


def load_env():
    load_dotenv()
    # The E2B SDK reads E2B_API_URL and E2B_API_KEY from the environment.
    # CubeSandbox is E2B-compatible and does not require a real API key,
    # but codebuddy's --sandbox flag reads E2B_API_KEY. Set a dummy value
    # so the native-sandbox mode works out of the box.
    os.environ.setdefault("E2B_API_KEY", "e2b_000000")
    required = ("E2B_API_URL", "E2B_API_KEY", "CUBE_TEMPLATE_ID", "CODEBUDDY_API_KEY")
    for key in required:
        if not os.environ.get(key):
            raise SystemExit(f"Missing env var: {key}")


def parse_codebuddy_output(stdout):
    """Parse codebuddy --output-format json output.

    codebuddy returns a JSON array of message objects:
      [0] user message (input_text)
      [1] file-history-snapshot
      [2] assistant message (output_text — the actual response)
      [3] result (summary with model, session_id, usage)

    Returns (result_text, meta_dict).
    """
    result_text = stdout
    meta = {}
    try:
        parsed = json.loads(stdout)
    except (json.JSONDecodeError, TypeError):
        # Output is not valid JSON (truncated, text format, or error message)
        return result_text.strip(), meta

    # Handle list format (codebuddy --output-format json)
    if isinstance(parsed, list):
        # Extract metadata from the result element (last item with type=result)
        for item in reversed(parsed):
            if isinstance(item, dict) and item.get("type") == "result":
                meta = {k: v for k, v in item.items() if k != "content"}
                break
        # Extract assistant response text
        for item in reversed(parsed):
            if isinstance(item, dict) and item.get("role") == "assistant":
                content = item.get("content", [])
                if isinstance(content, list):
                    for c in content:
                        if isinstance(c, dict) and c.get("type") in ("output_text", "text"):
                            result_text = c.get("text", "")
                            break
                break
    # Handle dict format (fallback)
    elif isinstance(parsed, dict):
        meta = parsed
        result_text = parsed.get("result", stdout)

    return result_text, meta


def run_native_sandbox(prompt, max_turns=10):
    """Run CodeBuddy on the host with --sandbox flag routing tool calls to CubeSandbox.

    This is the simplest integration path — no custom image or Python SDK needed.
    CodeBuddy natively speaks the E2B protocol, and CubeSandbox is E2B-compatible.
    """
    import subprocess

    api_url = os.environ["E2B_API_URL"]
    print(f"[native] CodeBuddy on host → sandbox at {api_url}")
    print(f"[native] Prompt: {prompt}")

    cmd = [
        "codebuddy",
        "--sandbox", api_url,
        "--sandbox-new",
        "-p", prompt,
        "--output-format", "json",
        "--max-turns", str(max_turns),
        "--permission-mode", "bypassPermissions",
    ]
    print(f"[native] Running: codebuddy --sandbox {api_url} --sandbox-new -p ...")
    t0 = time.monotonic()
    env = {**os.environ, "DISABLE_AUTOUPDATER": "1"}
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=600, env=env)
    elapsed = time.monotonic() - t0
    print(f"[native] Completed in {elapsed*1000:.0f} ms")

    stdout = result.stdout
    result_text, meta = parse_codebuddy_output(stdout)
    if meta:
        print(f"[native] Model: {meta.get('model', 'N/A')}")
        print(f"[native] Session: {meta.get('session_id', meta.get('sessionId', 'N/A'))}")
        if 'usage' in meta:
            print(f"[native] Token usage: {meta['usage']}")
    print(f"\n{'='*60}")
    print("Result:")
    print(result_text)

    if result.returncode != 0:
        print(f"[native] stderr: {result.stderr}")

    return stdout


def proxy_port():
    """Return CUBE_PROXY_PORT_HTTP if set."""
    return os.environ.get("CUBE_PROXY_PORT_HTTP")


def apply_proxy_headers(sb):
    """Inject CubeProxy Host header into the E2B SDK connection config.

    CubeProxy (openresty) routes by Host header: <port>-<id>.cube.app.
    Without this, the E2B SDK would try to resolve ``*.cube.app`` directly,
    which only works in production — not in port-forwarded dev setups.
    """
    port = proxy_port()
    if not port:
        return
    host = f"49983-{sb.sandbox_id}.cube.app"
    extra_headers = {"Host": host}
    for ns in (sb.commands, sb.files):
        cfg = ns._connection_config
        object.__setattr__(cfg, "_ConnectionConfig__extra_sandbox_headers", extra_headers)
        object.__setattr__(ns, "_thread_local", threading.local())


def create_sandbox(timeout=300):
    """Create a sandbox from the CodeBuddy template.

    In production, ``*.cube.app`` resolves directly and no proxy is needed.
    When ``CUBE_PROXY_PORT_HTTP`` is set (port-forwarded dev setups), route
    envd traffic through CubeProxy with the correct Host header so the E2B
    SDK can reach the sandbox despite the domain not resolving locally.
    """
    template = os.environ["CUBE_TEMPLATE_ID"]
    api_key = os.environ.get("CODEBUDDY_API_KEY", "")

    create_kwargs = dict(
        template=template,
        timeout=timeout,
        envs={"CODEBUDDY_API_KEY": api_key},
    )
    if proxy_port():
        create_kwargs["sandbox_url"] = f"http://127.0.0.1:{proxy_port()}"

    print(f"[info] Creating sandbox from template: {template}")
    t0 = time.monotonic()
    sb = Sandbox.create(**create_kwargs)

    apply_proxy_headers(sb)

    print(f"[info] Sandbox created in {(time.monotonic()-t0)*1000:.0f} ms  (id={sb.sandbox_id})")
    return sb


def seed_workspace(sb):
    """Seed /workspace with a buggy Python script for CodeBuddy to fix.

    The demo task is a *bug-hunt*: ``fib.py`` contains an off-by-one error in
    the Fibonacci loop.  CodeBuddy is expected to read the code, identify the
    bug, patch it, and verify that ``fibonacci(10)`` returns 55.
    """
    print("[seed] Writing demo files to /workspace ...")
    sb.files.write(
        "/workspace/fib.py",
        '"""Fibonacci demo with a subtle bug.\n'
        '\n'
        "Expected: fibonacci(10) == 55\n"
        "Actual:   fibonacci(10) == 34  (wrong!)\n"
        '"""\n'
        "\n"
        "\n"
        "def fibonacci(n):\n"
        '    """Return the n-th Fibonacci number (0-indexed)."""\n'
        "    if n <= 0:\n"
        "        return 0\n"
        "    if n == 1:\n"
        "        return 1\n"
        "    a, b = 0, 1\n"
        '    for _ in range(2, n):  # BUG: should be range(2, n + 1)\n'
        "        a, b = b, a + b\n"
        "    return b\n"
        "\n"
        "\n"
        'if __name__ == "__main__":\n'
        '    for i in range(11):\n'
        '        print(f"fibonacci({i}) = {fibonacci(i)}")\n',
        user="root",
    )
    sb.files.write(
        "/workspace/README.md",
        "# Bug-Hunt Demo\n\n"
        "`fib.py` contains a Fibonacci function with a subtle off-by-one bug.\n"
        "**Task:** Find the bug, fix it, then run `python3 fib.py` to verify\n"
        "that `fibonacci(10)` returns **55** (not 34).\n",
        user="root",
    )
    print("[seed] Done — /workspace/fib.py and /workspace/README.md ready")


def run_codebuddy_in_sandbox(sb, prompt, max_turns=10):
    """Run CodeBuddy in-sandbox mode."""
    # Build the command — use JSON output for parseable results
    # Prefix env vars so the sandbox-internal codebuddy can authenticate and
    # run without update checks or interactive prompts.
    api_key = os.environ.get("CODEBUDDY_API_KEY", "")
    auth_token = os.environ.get("CODEBUDDY_AUTH_TOKEN")

    env_prefix = ""
    if auth_token:
        env_prefix = f'CODEBUDDY_AUTH_TOKEN={json.dumps(auth_token)} '

    if not auth_token:
        print("Tip: Set CODEBUDDY_AUTH_TOKEN for in-sandbox auth (enterprise accounts only). "
              "Otherwise, codebuddy will require interactive login.")

    cmd = (
        f'{env_prefix}'
        f'CODEBUDDY_API_KEY={json.dumps(api_key)} '
        f'DISABLE_AUTOUPDATER=1 '
        f'CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy '
        f'codebuddy -p {json.dumps(prompt)} '
        f'--output-format json '
        f'--max-turns {max_turns} '
        f'--permission-mode bypassPermissions'
    )
    print(f"[in-sandbox] Running: codebuddy -p ...")
    t0 = time.monotonic()
    try:
        result = sb.commands.run(cmd, user="root", timeout=300)
    except Exception as e:
        print(f"[in-sandbox] Command failed: {e}")
        print("[in-sandbox] Tip: CodeBuddy inside the sandbox needs product auth.")
        print("         Either copy ~/.codebuddy/local_storage/ into the sandbox,")
        print("         or use --native-sandbox mode (runs on host, no auth needed).")
        return ""
    elapsed = time.monotonic() - t0
    print(f"[in-sandbox] Completed in {elapsed*1000:.0f} ms")

    stdout = result.stdout if hasattr(result, "stdout") else str(result)
    if isinstance(stdout, bytes):
        stdout = stdout.decode("utf-8")

    # Try to parse JSON output
    try:
        result_text, meta = parse_codebuddy_output(stdout)
        if meta:
            print(f"[in-sandbox] Model: {meta.get('model', 'N/A')}")
            print(f"[in-sandbox] Session: {meta.get('session_id', meta.get('sessionId', 'N/A'))}")
            if 'usage' in meta:
                print(f"[in-sandbox] Token usage: {meta['usage']}")
        print(f"\n{'='*60}")
        print("Result:")
        print(result_text)
    except (json.JSONDecodeError, TypeError):
        print(f"\n{'='*60}")
        print("Output (raw):")
        print(stdout)

    return stdout


def run_codebuddy_http(sb, message="Hello! What can you help me with?"):
    """Run CodeBuddy in HTTP API mode."""
    # Start codebuddy --serve in background
    # Prefix env vars so the sandbox-internal codebuddy can authenticate and
    # run without update checks or interactive prompts.
    api_key = os.environ.get("CODEBUDDY_API_KEY", "")
    auth_token = os.environ.get("CODEBUDDY_AUTH_TOKEN")

    env_prefix = ""
    if auth_token:
        env_prefix = f'CODEBUDDY_AUTH_TOKEN={json.dumps(auth_token)} '

    if not auth_token:
        print("Tip: Set CODEBUDDY_AUTH_TOKEN for in-sandbox auth (enterprise accounts only). "
              "Otherwise, codebuddy will require interactive login.")

    print("[http] Starting codebuddy --serve on port 8080 ...")
    sb.commands.run(
        f'nohup env {env_prefix}'
        f'CODEBUDDY_API_KEY={json.dumps(api_key)} '
        f'DISABLE_AUTOUPDATER=1 '
        f'CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy '
        f'codebuddy --serve --port 8080 --hostname 0.0.0.0 '
        f'--permission-mode bypassPermissions '
        f'> /tmp/codebuddy.log 2>&1 &',
        user="root"
    )

    # Wait for server to be ready
    print("[http] Waiting for server to be ready ...")
    for i in range(30):
        time.sleep(1)
        try:
            health = sb.commands.run('curl -s http://localhost:8080/health', user="root")
            health_out = health.stdout if hasattr(health, "stdout") else str(health)
            if isinstance(health_out, bytes):
                health_out = health_out.decode("utf-8")
            if "ok" in health_out.lower() or "healthy" in health_out.lower():
                print(f"[http] Server ready after {i+1}s")
                break
        except Exception:
            pass
    else:
        print("[http] Server did not become ready in 30s")
        log = sb.commands.run('cat /tmp/codebuddy.log', user="root")
        log_text = log.stdout if hasattr(log, "stdout") else str(log)
        if isinstance(log_text, bytes):
            log_text = log_text.decode("utf-8")
        print(log_text)
        return

    # Call the chat API
    print(f"[http] Sending message: {message}")
    payload = json.dumps({"message": message})
    api_cmd = (
        f'curl -s -X POST http://localhost:8080/api/chat '
        f'-H "Content-Type: application/json" '
        f"-d '{payload}'"
    )
    result = sb.commands.run(api_cmd, user="root")
    stdout = result.stdout if hasattr(result, "stdout") else str(result)
    if isinstance(stdout, bytes):
        stdout = stdout.decode("utf-8")

    print(f"\n{'='*60}")
    print("HTTP API Response:")
    try:
        parsed = json.loads(stdout)
        print(json.dumps(parsed, indent=2, ensure_ascii=False))
    except json.JSONDecodeError:
        print(stdout)

    return stdout


def main():
    parser = argparse.ArgumentParser(description="CodeBuddy CLI × CubeSandbox demo")
    parser.add_argument("--prompt", default="Reply with exactly: Hello from CodeBuddy CLI in CubeSandbox native sandbox mode.",
                        help="Prompt for CodeBuddy. For a tool-execution example, use: "
                             "'Use bash to create /workspace/hello.py that prints ...Hello from CubeSandbox!..., "
                             "run it with python3, and report the output.' (requires DNS-resolvable *.cube.app domain)")
    parser.add_argument("--max-turns", type=int, default=3, help="Maximum CodeBuddy turns (default: 3).")
    parser.add_argument("--template", default=None, help="Cube template ID (or set CUBE_TEMPLATE_ID)")
    parser.add_argument("--timeout", type=int, default=300, help="Sandbox timeout in seconds")

    mode_group = parser.add_mutually_exclusive_group()
    mode_group.add_argument("--native-sandbox", action="store_true", default=True,
                            dest="native_sandbox",
                            help="Run native sandbox mode (CodeBuddy on host, tool calls in MicroVM) [default]")
    mode_group.add_argument("--in-sandbox", action="store_true",
                            help="Run in-sandbox mode (CodeBuddy inside MicroVM)")
    mode_group.add_argument("--http-api", action="store_true", help="Run HTTP API mode demo")
    args = parser.parse_args()

    load_env()

    if args.template:
        os.environ["CUBE_TEMPLATE_ID"] = args.template

    # Native sandbox mode: CodeBuddy runs on host, no sandbox creation needed
    if args.native_sandbox:
        run_native_sandbox(args.prompt, max_turns=args.max_turns)
        return

    sb = create_sandbox(timeout=args.timeout)

    # Seed the workspace with a small demo project for CodeBuddy to inspect.
    seed_workspace(sb)

    try:
        if args.http_api:
            run_codebuddy_http(sb)
        else:
            run_codebuddy_in_sandbox(sb, args.prompt, max_turns=args.max_turns)
    finally:
        print("\n[cleanup] Destroying sandbox ...")
        t0 = time.monotonic()
        sb.kill()
        print(f"[cleanup] Done in {(time.monotonic()-t0)*1000:.0f} ms")


if __name__ == "__main__":
    main()
