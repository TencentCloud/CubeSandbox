#!/usr/bin/env python3
"""Go envd vs cube-envd: startup time, RSS, command latency.

Assumes envd-go2 (:49985) and envd-rust (:49984) containers are running.
"""
import json
import statistics
import subprocess
import threading
import time

import http.client

import capture  # reuse the raw connect client (module-level HOST/PORT)


def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()


def rss_kib_during_upload(container, size_mib=256, port=49984):
    """Stream a `size_mib` upload to /files while sampling the daemon's RSS.

    Returns (status, peak_rss_delta_kib, seconds). The payload never sits in
    memory when uploads stream to disk, so the delta stays at
    buffer-sized levels; a whole-body-buffering regression shows up as a
    delta equal to the upload size.
    """
    def rss_kib():
        out = subprocess.check_output(
            ["docker", "exec", container, "sh", "-c",
             "grep VmRSS /proc/$(pidof cube-envd)/status"])
        return int(out.decode().split()[1])

    baseline = rss_kib()
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=600)
    total = size_mib * 1024 * 1024
    conn.putrequest("POST", "/files?path=/home/user/rss_probe.bin&username=user")
    conn.putheader("Content-Type", "application/octet-stream")
    conn.putheader("Content-Length", str(total))
    conn.endheaders()
    peak = baseline
    stop = False

    def sampler():
        nonlocal peak
        while not stop:
            try:
                peak = max(peak, rss_kib())
            except Exception:
                pass
            time.sleep(0.2)

    t = threading.Thread(target=sampler)
    t.start()
    t0 = time.time()
    chunk = b"x" * (1024 * 1024)
    for _ in range(size_mib):
        conn.send(chunk)
    resp = conn.getresponse()
    resp.read()
    stop = True
    t.join()
    return resp.status, peak - baseline, time.time() - t0


def download_throughput(mib=100, port=49984):
    """Download a mib-MiB file from /files and return throughput in MiB/s.

    The file must already exist in the sandbox (created by the caller via
    docker exec dd).
    """
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=300)
    conn.putrequest(
        "GET", f"/files?path=/home/user/throughput_{mib}m.bin&username=user")
    conn.putheader("Authorization", "Basic dXNlcjo=")
    conn.endheaders()
    r = conn.getresponse()
    total = 0
    t0 = time.time()
    while True:
        chunk = r.read(1024 * 1024)
        if not chunk:
            break
        total += len(chunk)
    dt = time.time() - t0
    return r.status, total / dt / 1024 / 1024


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

    # data plane: upload must stream to disk (RSS delta stays at
    # buffer levels, not the upload size) and download throughput must not
    # regress against the Go baseline on the same host.
    status, delta_kib, secs = rss_kib_during_upload("envd-rust", 256, 49984)
    result["upload_256m_rust"] = {
        "status": status,
        "rss_delta_kib": delta_kib,
        "seconds": round(secs, 2),
    }
    for c in ("envd-go2", "envd-rust"):
        sh(f"docker exec {c} sh -c "
           "'dd if=/dev/zero of=/home/user/throughput_100m.bin bs=1M count=100 2>/dev/null'")
    _, tp_go = download_throughput(100, 49985)
    _, tp_rust = download_throughput(100, 49984)
    result["download_mibs_go"] = round(tp_go)
    result["download_mibs_rust"] = round(tp_rust)
    # Leftover artifacts would leak into the next conformance ListDir
    # capture (they live in /home/user next to the fixture files).
    for c in ("envd-go2", "envd-rust"):
        sh(f"docker exec {c} rm -f /home/user/rss_probe.bin "
           "/home/user/throughput_100m.bin")

    go_start = startup_ms("envd-go2", "/usr/bin/envd")
    rust_start = startup_ms("envd-rust", "/usr/bin/cube-envd")
    result["startup_ms_go"] = {"mean": round(statistics.mean(go_start), 1), "samples": go_start}
    result["startup_ms_rust"] = {"mean": round(statistics.mean(rust_start), 1), "samples": rust_start}

    with open("perf-results.json", "w") as f:
        json.dump(result, f, indent=2)
    print(json.dumps(result, indent=2))
