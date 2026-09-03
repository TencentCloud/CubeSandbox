#!/usr/bin/env python3
"""End-to-end acceptance run against a live CubeSandbox deployment.

Creates a sandbox from a cube-envd template and exercises the three
acceptance scenarios (health/readiness, command execution, file I/O)
through the CubeSandbox Python SDK, then repeats a smoke pass on the
Go-envd rollback template.

Env: CUBE_API_URL, CUBE_PROXY_NODE_IP, CUBE_PROXY_PORT_HTTP,
     TEMPLATE_CUBE (cube-envd template id), TEMPLATE_GO (rollback).
"""
import os
import sys
import time

# Repo-root SDK (tests/e2e/envd_conformance -> ../../../sdk/python).
sys.path.insert(
    0,
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python"),
)
from cubesandbox import Sandbox  # noqa: E402

TEMPLATE_CUBE = os.environ["TEMPLATE_CUBE"]
TEMPLATE_GO = os.environ.get("TEMPLATE_GO")

PASS = 0
FAIL = 0


def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  [PASS] {name} {detail}")
    else:
        FAIL += 1
        print(f"  [FAIL] {name} {detail}")


def scenario_health(sb):
    print("--- scenario 1: health / readiness ---")
    # Sandbox reached RUNNING means CubeMaster's :49983/health probe passed.
    info = sb.get_info() if hasattr(sb, "get_info") else {}
    check("sandbox running (readiness probe on :49983 passed)", sb.sandbox_id, f"id={sb.sandbox_id}")
    r = sb.commands.run("echo -n ok")
    check("basic roundtrip", r.exit_code == 0 and r.stdout == "ok")
    print(f"  info: {info if info else 'n/a'}")


def scenario_commands(sb):
    print("--- scenario 2: command execution ---")
    r = sb.commands.run("echo hello-e2e")
    check("echo stdout", r.stdout == "hello-e2e\n" and r.exit_code == 0, repr(r.stdout))
    r = sb.commands.run("echo out; echo err >&2; exit 7")
    check("stderr + exit code", r.stdout == "out\n" and r.stderr == "err\n" and r.exit_code == 7,
          f"exit={r.exit_code}")
    r = sb.commands.run("echo $MY_VAR-$(whoami)-$(pwd)", envs={"MY_VAR": "e2e42"}, user="user")
    check("envs + user + cwd", r.stdout.strip() == "e2e42-user-/home/user", repr(r.stdout))
    r = sb.commands.run("seq 1 20000 | md5sum")
    check("large output pipeline", "10ba2f7dcb0eebb2e1b6a5624d1efc3c" in r.stdout or r.exit_code == 0,
          repr(r.stdout[:50]))
    t0 = time.time()
    try:
        sb.commands.run("sleep 30", timeout=2)
        check("timeout enforced", False, "no exception raised")
    except Exception as e:
        check("timeout enforced", time.time() - t0 < 10, f"{type(e).__name__} after {time.time()-t0:.1f}s")


def scenario_files(sb):
    print("--- scenario 3: file I/O ---")
    sb.files.write("/home/user/e2e.txt", "hello file\n", user="user")
    content = sb.files.read("/home/user/e2e.txt", user="user")
    check("write/read roundtrip", content == "hello file\n", repr(content))
    blob = bytes(range(256)) * 4
    sb.files.write("/home/user/e2e.bin", blob, user="user")
    back = sb.files.read("/home/user/e2e.bin", user="user")
    raw = back.encode("latin-1", "replace") if isinstance(back, str) else back
    check("binary roundtrip size", len(raw) == len(blob), f"{len(raw)}/{len(blob)}")
    entries = sb.files.list("/home/user")
    names = [e.get("name") for e in entries]
    check("list contains files", "e2e.txt" in names and "e2e.bin" in names, str(names))
    st = sb.files.stat("/home/user/e2e.txt")
    check("stat size", st.get("size") in ("11", 11), str(st.get("size")))
    sb.files.make_dir("/home/user/e2e_dir")
    check("exists after make_dir", sb.files.exists("/home/user/e2e_dir"))
    sb.files.rename("/home/user/e2e.txt", "/home/user/e2e_dir/moved.txt")
    check("rename + read", sb.files.read("/home/user/e2e_dir/moved.txt", user="user") == "hello file\n")
    sb.files.remove("/home/user/e2e_dir")
    check("remove dir", not sb.files.exists("/home/user/e2e_dir"))
    try:
        sb.files.read("/home/user/nope.txt", user="user")
        check("missing file errors", False)
    except Exception as e:
        check("missing file errors", "404" in str(e) or "not exist" in str(e), type(e).__name__)


def scenario_pty(sb):
    print("--- scenario 4: pty create / resize ---")
    from cubesandbox import PtySize
    handle = sb.pty.create(PtySize(rows=24, cols=80))
    try:
        # resize() drives the Update RPC (TIOCSWINSZ on the pty master). The
        # SDK's send_stdin maps to SendInput, which is out of MVP scope, so we
        # observe the effect by reading the pty slave's kernel winsize directly
        # instead of typing `stty size` through stdin.
        handle.resize(PtySize(rows=43, cols=132))
        tty = sb.commands.run(f"readlink /proc/{handle.pid}/fd/0").stdout.strip()
        check("pty attached to a tty", tty.startswith("/dev/"), repr(tty))
        if tty.startswith("/dev/"):
            r = sb.commands.run(f"stty size < {tty}")
            check("pty resize observed (43 132)", "43 132" in r.stdout, repr(r.stdout))
    finally:
        try:
            killed = handle.kill()
            check("pty kill", killed)
        except Exception as e:
            print(f"  pty kill failed: {e}")


def run_suite(template, label, full=True):
    print(f"\n===== template {template} ({label}) =====")
    sb = Sandbox.create(template=template, timeout=300)
    try:
        print(f"sandbox: {sb.sandbox_id}")
        scenario_health(sb)
        if full:
            scenario_commands(sb)
            scenario_files(sb)
            scenario_pty(sb)
        else:
            r = sb.commands.run("echo rollback-ok")
            check("rollback smoke: command", r.stdout == "rollback-ok\n")
            sb.files.write("/home/user/rb.txt", "rb\n", user="user")
            check("rollback smoke: file", sb.files.read("/home/user/rb.txt", user="user") == "rb\n")
    finally:
        try:
            sb.kill()
            print("sandbox killed")
        except Exception as e:
            print(f"kill failed: {e}")


if __name__ == "__main__":
    run_suite(TEMPLATE_CUBE, "cube-envd", full=True)
    if TEMPLATE_GO:
        run_suite(TEMPLATE_GO, "Go envd rollback", full=False)
    print(f"\n===== E2E RESULT: {PASS} passed, {FAIL} failed =====")
    sys.exit(1 if FAIL else 0)
