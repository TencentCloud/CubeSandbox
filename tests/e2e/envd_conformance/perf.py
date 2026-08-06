#!/usr/bin/env python3
"""Go envd vs cube-envd: startup time, RSS, command latency.

Assumes envd-go2 (:49985) and envd-rust (:49984) containers are running.
"""
import json
import statistics
import subprocess
import time

import capture  # reuse the raw connect client (module-level HOST/PORT)


def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()


def startup_ms(container, binary, runs=10):
    times = []
    for _ in range(runs):
        sh(f"docker exec {container} sh -c 'pkill -f \"port 50000\" 2>/dev/null; true'")
        time.sleep(0.2)
        script = (
            f"start=$(date +%s%N); {binary} -port 50000 -isnotfc >/dev/null 2>&1 & "
            "for i in $(seq 1 2000); do "
            "  if curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:50000/health 2>/dev/null | grep -q 204; then "
            "    end=$(date +%s%N); echo $(( (end - start) / 1000000 )); break; "
            "  fi; "
            "done"
        )
        out = sh(f"docker exec {container} sh -c '{script}'")
        if out:
            times.append(int(out.splitlines()[-1]))
        sh(f"docker exec {container} sh -c 'pkill -f \"port 50000\"; true'")
        time.sleep(0.1)
    return times


def rss_kib(container, pattern):
    pid = sh(f"docker exec {container} sh -c \"pgrep -f '{pattern}' | head -1\"")
    if not pid:
        return None
    out = sh(f"docker exec {container} sh -c 'grep VmRSS /proc/{pid}/status'")
    return int(out.split()[1]) if out else None


def cmd_latency_ms(port, runs=100):
    capture.PORT = port
    times = []
    for _ in range(runs):
        t0 = time.time()
        r = capture.connect_stream("process.Process/Start", capture.start_req("echo hi"), timeout=10)
        dt = (time.time() - t0) * 1000
        ends = [f for f in r["frames"] if f["flags"] & 2]
        assert ends, f"stream did not finish: {r}"
        times.append(dt)
    times.sort()
    return {
        "p50_ms": round(times[len(times) // 2], 1),
        "p95_ms": round(times[int(len(times) * 0.95)], 1),
        "mean_ms": round(statistics.mean(times), 1),
    }


if __name__ == "__main__":
    result = {}
    result["rss_kib_go"] = rss_kib("envd-go2", "/usr/bin/envd -port")
    result["rss_kib_rust"] = rss_kib("envd-rust", "cube-envd -port")

    result["cmd_latency_go"] = cmd_latency_ms(49985)
    result["cmd_latency_rust"] = cmd_latency_ms(49984)

    go_start = startup_ms("envd-go2", "/usr/bin/envd")
    rust_start = startup_ms("envd-rust", "/usr/bin/cube-envd")
    result["startup_ms_go"] = {"mean": round(statistics.mean(go_start), 1), "samples": go_start}
    result["startup_ms_rust"] = {"mean": round(statistics.mean(rust_start), 1), "samples": rust_start}

    with open("perf-results.json", "w") as f:
        json.dump(result, f, indent=2)
    print(json.dumps(result, indent=2))
