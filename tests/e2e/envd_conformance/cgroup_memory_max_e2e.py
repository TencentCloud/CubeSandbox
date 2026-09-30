"""Check that `-cgroup-memory-max-bytes` injects the cgroup memory cap.

Run as root:
  python3 cgroup_memory_max_e2e.py /path/to/cube-envd [/sys/fs/cgroup] [/path/to/cube-entrypoint.sh]

The cgroup v2 `user`/`ptys` subtrees carry `memory.high` + `memory.max`, derived
from guest memory and clamped by the enclosing cgroup. Upstream envd offers no
way to request a different value; cube-envd accepts one through
`CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES` (envd's own process environment) and, since
this change, through `-cgroup-memory-max-bytes`.

Cases 1-5 start the daemon directly, each under its own cgroup root, and read
the value the kernel really received rather than what the daemon logged:

  1. `-cgroup-memory-max-bytes X`                -> user/ptys memory.max == X
  2. `CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES=Y` only  -> == Y
  3. both, with different values                 -> == X (flag wins)
  4. a request above the safe ceiling            -> clamped, with a warning
  5. `CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES=0`       -> derived cap, with a warning
  6. malformed/zero/negative/missing flag values -> usage error, exit 2

Case 7 is the deployment path: it runs `docker/cube-entrypoint.sh` with the
candidate binary, the flag in `ENVD_EXTRA_ARGS` and the environment variable
deliberately set to a different value. `ENVD_EXTRA_ARGS` is the surface the
bring-your-own-image tutorial documents, and only this case proves that a
deployment can actually set the cap — argv alone does not.

The ceiling is recomputed here from `/proc/meminfo` and the enclosing cgroup's
`memory.max`, independently of the daemon's own arithmetic, so case 4 fails if
either side changes. Needs a writable cgroup v2 root (root, in a guest or on a
host with cgroup2 mounted); only the subtrees it creates are removed.
"""

import http.client
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time

CHECKS = []
MIB = 1024 * 1024


def check(name, ok, detail=""):
    CHECKS.append(ok)
    print(f"{'PASS' if ok else 'FAIL'}  {name}{('  — ' + detail) if detail else ''}", flush=True)


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def wait_healthy(port, timeout=15.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            connection = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
            connection.request("GET", "/health")
            status = connection.getresponse().status
            connection.close()
            if status in (200, 204):
                return True
        except OSError:
            time.sleep(0.2)
    return False


def read_memory_max(path):
    """Numeric `memory.max`, or None for `max`/absent."""
    try:
        raw = path.read_text().strip()
    except OSError:
        return None
    if raw == "max":
        return None
    return int(raw)


def unified_cgroup_path(base):
    """The daemon's own unified path relative to `base`, like the daemon resolves it."""
    for line in Path("/proc/self/cgroup").read_text().splitlines():
        if line.startswith("0::"):
            relative = line[3:].strip().lstrip("/")
            candidate = base / relative if relative else base
            return candidate if candidate.is_dir() else base
    return base


def mem_total_bytes():
    for line in Path("/proc/meminfo").read_text().splitlines():
        if line.startswith("MemTotal:"):
            return int(line.split()[1]) * 1024
    raise SystemExit("no MemTotal in /proc/meminfo")


def safe_default(root):
    """`min(guest, enclosing parent) - min(total/8, 128 MiB)`, recomputed here.

    `root` is the daemon's configured cgroup root, because that is what the
    daemon treats as its enclosing cgroup: each case gets a fresh root with no
    `memory.max` of its own, so the parent limit it reads is usually absent and
    the guest's `MemTotal` is the ceiling. Reading the *base* cgroup here
    instead would disagree whenever the surrounding sandbox is capped below
    guest memory, and the clamp case would then fail for the wrong reason.
    """
    guest = mem_total_bytes()
    parent = read_memory_max(unified_cgroup_path(root) / "memory.max")
    effective = min(guest, parent) if parent is not None else guest
    return effective - min(effective // 8, 128 * MIB)


def case_root(base, label):
    return base / f"cube-envd-mem-{label}"


def subtree_value(root, name, file="memory.max"):
    return read_memory_max(root / name / file)


def stop(process):
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()


def remove_tree(root):
    """Remove the subtrees the daemon created; leaves may need the daemon to exit."""
    for _ in range(20):
        if not root.exists():
            return
        for child in sorted(root.rglob("*"), key=lambda p: -len(p.parts)):
            if child.is_dir():
                try:
                    child.rmdir()
                except OSError:
                    pass
        try:
            root.rmdir()
            return
        except OSError:
            time.sleep(0.2)


def case(binary, label, base, expected, extra_args=(), env=None, log_expect=None):
    """Start the daemon under its own root and compare the kernel-visible cap."""
    port = free_port()
    workdir = Path(tempfile.mkdtemp(prefix=f"cube-envd-mem-{label}-"))
    root = case_root(base, label)
    log = workdir / "envd.log"
    root.mkdir(exist_ok=True)
    environment = {
        **os.environ,
        # Each case gets its own root through the *environment* form: the flag
        # form of `-cgroup-root` is a deliberate non-feature, so this test must
        # not depend on it.
        "CUBE_ENVD_CGROUP_ROOT": str(root),
        **(env or {}),
    }
    daemon = subprocess.Popen(
        [str(binary), "-port", str(port), *extra_args],
        stdout=log.open("w"), stderr=subprocess.STDOUT, env=environment,
    )
    try:
        if not wait_healthy(port):
            check(f"{label}: daemon healthy", False, f"see {log}")
            return
        for name in ("user", "ptys"):
            value = subtree_value(root, name)
            check(f"{label}: {name}/memory.max == {expected}", value == expected, f"got {value}")
        high = subtree_value(root, "user", "memory.high")
        check(f"{label}: user/memory.high matches memory.max", high == expected, f"got {high}")
        if log_expect:
            text = log.read_text(errors="replace")
            check(f"{label}: log says {log_expect!r}", log_expect in text)
    finally:
        stop(daemon)
        shutil.rmtree(workdir, ignore_errors=True)
        remove_tree(root)


def entrypoint_case(binary, entrypoint, base, requested, environment):
    """The deployment path: `ENVD_EXTRA_ARGS` through the real entrypoint.

    The entrypoint is the documented tuning surface and it forwards flags, so
    this is the case that proves a deployment can set the cap; argv alone does
    not. The environment variable is deliberately a different value, which also
    pins that the flag wins in the path a deployment actually uses.
    """
    port = free_port()
    workdir = Path(tempfile.mkdtemp(prefix="cube-envd-mem-entrypoint-"))
    root = base / "cube-envd-mem-entrypoint"
    log = workdir / "envd.log"
    root.mkdir(exist_ok=True)
    env = {
        **os.environ,
        "ENVD_BIN": str(binary),
        "ENVD_PORT": str(port),
        "ENVD_LOG_FILE": str(log),
        "ENVD_EXTRA_ARGS": f"-cgroup-memory-max-bytes {requested}",
        "CUBE_ENVD_CGROUP_ROOT": str(root),
        "CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES": str(environment),
    }
    # New session: the entrypoint starts envd as its child, so one killpg takes
    # both down (the script itself only waits on the daemon).
    runner = subprocess.Popen(
        ["sh", str(entrypoint)], env=env, start_new_session=True,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        if not wait_healthy(port):
            check("entrypoint: daemon healthy", False, f"see {log}")
            return
        check("entrypoint: starts envd and /health answers", True, f"port {port}")
        for name in ("user", "ptys"):
            value = subtree_value(root, name)
            check(
                f"entrypoint: {name}/memory.max == {requested} (flag beats the environment)",
                value == requested,
                f"got {value}",
            )
        text = log.read_text(errors="replace") if log.exists() else ""
        check(
            "entrypoint: ENVD_EXTRA_ARGS reached envd",
            f"requested=Some({requested})" in text,
            next((ln for ln in text.splitlines() if "mode=enabled" in ln), text[-160:]),
        )
    finally:
        try:
            os.killpg(os.getpgid(runner.pid), 15)
        except (ProcessLookupError, PermissionError):
            pass
        stop(runner)
        shutil.rmtree(workdir, ignore_errors=True)
        remove_tree(root)


def invalid_flag_values(binary):
    """A bad flag value must be a usage error, never a silent default."""
    for label, argv in [
        ("zero", ["-cgroup-memory-max-bytes", "0"]),
        ("garbage", ["-cgroup-memory-max-bytes", "abc"]),
        ("negative", ["-cgroup-memory-max-bytes", "-1"]),
        ("missing", ["-cgroup-memory-max-bytes"]),
    ]:
        result = subprocess.run(
            [str(binary), "-port", "0", *argv], capture_output=True, text=True, timeout=30
        )
        ok = result.returncode == 2 and "cgroup-memory-max-bytes" in result.stderr
        check(f"invalid value {label}: exit 2 with usage", ok, f"exit {result.returncode}")


def main():
    binary = Path(sys.argv[1] if len(sys.argv) > 1 else "target/x86_64-linux-musl/release/cube-envd")
    base = Path(sys.argv[2] if len(sys.argv) > 2 else "/sys/fs/cgroup")
    entrypoint = Path(sys.argv[3]) if len(sys.argv) > 3 else Path("docker/cube-entrypoint.sh")
    if not binary.exists():
        raise SystemExit(f"cube-envd binary not found: {binary}")
    if not (base / "cgroup.controllers").exists():
        raise SystemExit(f"{base} is not a cgroup v2 root")
    if os.geteuid() != 0:
        raise SystemExit("this check writes cgroup subtrees; run it as root")

    safe = safe_default(case_root(base, "flag"))
    requested = min(256 * MIB, safe // 2)
    environment = requested // 2
    print(f"# safe ceiling {safe} bytes; requesting {requested} / env {environment}\n", flush=True)

    case(binary, "flag", base, requested,
         extra_args=["-cgroup-memory-max-bytes", str(requested)])
    case(binary, "env", base, environment,
         env={"CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES": str(environment)})
    case(binary, "both", base, requested,
         extra_args=["-cgroup-memory-max-bytes", str(requested)],
         env={"CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES": str(environment)})
    case(binary, "clamp", base, safe_default(case_root(base, "clamp")),
         extra_args=["-cgroup-memory-max-bytes", str(1 << 50)], log_expect="clamping to")
    case(binary, "invalid-env", base, safe_default(case_root(base, "invalid-env")),
         env={"CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES": "0"}, log_expect="ignoring invalid")
    if entrypoint.exists():
        entrypoint_case(binary, entrypoint, base, requested, environment)
    else:
        check("entrypoint script found", False, str(entrypoint))
    invalid_flag_values(binary)

    print(f"\n{sum(CHECKS)} passed, {len(CHECKS) - sum(CHECKS)} failed")
    if not all(CHECKS):
        raise SystemExit(1)


main()
