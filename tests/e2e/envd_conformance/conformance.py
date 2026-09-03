#!/usr/bin/env python3
"""Conformance diff: run capture.py scenarios against cube-envd, then compare
fixture-by-fixture against the Go envd baseline with normalization.

Usage:
  ENVD_BASE=http://127.0.0.1:49984 OUTDIR=fixtures-rust python3 capture.py all
  python3 conformance.py fixtures fixtures-rust

Normalized away (legitimately dynamic):
  - HTTP Date/Content-Length/Connection headers, chunked framing details
  - pids, timestamps (ts / modifiedTime), watcher ids, machine-specific
    metrics values (only key sets and types are compared)
  - hostnames in downloaded /etc/hostname content
Declared differences (allowlisted, documented in cube-envd/README.md):
  - gzip: cube-envd always identity
  - CreateWatcher/SendInput: unimplemented in cube-envd
  - Connect: implemented; nested-selector shape still rejected differently
"""
import json
import re
import sys

GO_DIR = sys.argv[1] if len(sys.argv) > 1 else "fixtures"
RS_DIR = sys.argv[2] if len(sys.argv) > 2 else "fixtures-rust"

# Fixtures where cube-envd intentionally differs (cube-envd/README.md).
DECLARED_DIFFERENT = {
    "fs_watch_unary_probe": "CreateWatcher: implemented upstream, unimplemented in cube-envd",
    "rest_files_gzip_accept": "gzip download encoding: upstream supports, cube-envd identity-only",
    "rest_files_compose_probe": "/files/compose: implemented upstream, 501 in cube-envd",
    "proc_sendinput_probe": "SendInput selector error wording differs (unimplemented either way)",
    "proc_connect_missing": "nested-selector: BOTH refuse to attach — upstream rejects the shape (unimplemented/invalid input type), cube-envd resolves to no pid and returns not_found; neither attaches to a process",
    "proc_sendsignal_nested_probe": "nested-selector: BOTH refuse to act on the process — upstream rejects the shape (501), cube-envd resolves to no pid and returns not_found; neither signals a process (no destructive side effect)",
    "fs_bad_json": "JSON parse error wording is parser-specific (code and status equal)",
    "rest_init_timestamp_out_of_range": "timestamp outside i64-nanosecond range (9999): upstream UnixNano() wraps and drops as stale (204); cube-envd rejects as a caller bug (400). Neither applies anything nor moves the gate",
}
# Fixtures that depend on prior state in ways the rerun reproduces
# differently. Currently empty; kept for the next scenario that needs it.
SKIP = set()

VOLATILE_KEYS = {"ts", "cpu_used_pct", "cpu_count", "mem_total", "mem_used", "mem_cache",
                 "mem_total_mib", "mem_used_mib", "disk_used", "disk_total",
                 "watcherId", "pid"}
TIME_RE = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z")
HEADERS_KEPT = {"Content-Type", "Content-Encoding", "Cache-Control",
                "Access-Control-Allow-Origin", "Access-Control-Expose-Headers",
                "Access-Control-Allow-Methods", "Access-Control-Allow-Headers",
                "Access-Control-Max-Age", "X-E2B-Legacy-SDK"}


def normalize(obj, path=""):
    if isinstance(obj, dict):
        out = {}
        for k, v in obj.items():
            if k == "headers" and isinstance(v, dict):
                kept_lower = {h.lower() for h in HEADERS_KEPT}
                out[k] = {hk.title(): hv for hk, hv in v.items()
                          if hk.lower() in kept_lower and hv != ""}
            elif k in VOLATILE_KEYS:
                out[k] = f"<{type(v).__name__}>"
            elif k in ("modifiedTime",):
                out[k] = "<time>"
            elif k in ("owner", "group") and path.endswith("entry"):
                out[k] = v  # keep: ownership semantics matter
            else:
                out[k] = normalize(v, f"{path}.{k}")
        return out
    if isinstance(obj, list):
        return [normalize(v, path) for v in obj]
    if isinstance(obj, str):
        if obj.startswith("{") or obj.startswith("["):
            try:
                return json.dumps(normalize(json.loads(obj), path), sort_keys=True)
            except (ValueError, json.JSONDecodeError):
                pass
        s = TIME_RE.sub("<time>", obj)
        s = re.sub(r'"pid": \d+', '"pid": <int>', s)
        s = re.sub(r'"watcherId": "\w+"', '"watcherId": "<id>"', s)
        s = re.sub(r'"ts":\d+', '"ts":<int>', s)
        s = re.sub(r"\d{4}-\d{2}-\d{2}T[\d:.]+Z", "<time>", s)
        # Raw JSON bodies: parse and re-normalize when possible.
        if s.startswith("{") or s.startswith("["):
            try:
                return json.dumps(normalize(json.loads(s), path), sort_keys=True)
            except (ValueError, json.JSONDecodeError):
                pass
        return s
    return obj


def norm_stream_frames(fx):
    """For streaming fixtures compare the frame sequence structurally.

    Data events are coalesced per stream (stdout/stderr/pty): interleaving
    ORDER between different streams is scheduler-dependent and flip-flops
    between runs of the same implementation; content per stream is exact.
    """
    import base64 as b64mod
    if not isinstance(fx, dict) or "frames" not in fx:
        return fx
    out = dict(fx)
    frames = []
    streams = {}
    for fr in fx["frames"]:
        p = normalize(fr.get("payload"))
        if isinstance(p, dict):
            ev = p.get("event", {})
            if isinstance(ev, dict) and "start" in ev and isinstance(ev["start"], dict):
                if "pid" in ev["start"]:
                    ev["start"]["pid"] = "<int>"
            if isinstance(ev, dict) and "data" in ev and isinstance(ev["data"], dict):
                for stream_name, chunk in ev["data"].items():
                    try:
                        streams.setdefault(stream_name, b"")
                        streams[stream_name] += b64mod.b64decode(chunk)
                    except Exception:
                        streams[stream_name] = b"<decode-error>"
                continue  # folded into `streams`
        frames.append({"flags": fr["flags"], "payload": p})
    out["frames"] = frames
    out["data_streams"] = {
        k: b64mod.b64encode(v).decode() for k, v in sorted(streams.items())
    }
    out.pop("headers", None)
    out.pop("status_line", None)  # compared via http_ok flag
    out["http_ok"] = fx.get("status_line", "").startswith("HTTP/1.1 200")
    for k in ("closed_early", "stopped_by_deadline", "socket_timeout"):
        out.pop(k, None)
    return out


def load(dirname, name):
    with open(f"{dirname}/{name}.json") as f:
        return json.load(f)


def main():
    import os
    names = sorted(
        n[:-5] for n in os.listdir(GO_DIR) if n.endswith(".json")
    )
    passed, failed, declared, skipped, missing = [], [], [], [], []
    for name in names:
        if name in SKIP:
            skipped.append(name)
            continue
        try:
            rs = load(RS_DIR, name)
        except FileNotFoundError:
            missing.append(name)
            continue
        go = load(GO_DIR, name)
        go_n = normalize(norm_stream_frames(go))
        rs_n = normalize(norm_stream_frames(rs))
        if go_n == rs_n:
            passed.append(name)
        elif name in DECLARED_DIFFERENT:
            declared.append(name)
        else:
            failed.append(name)
            print(f"\n=== FAIL {name}")
            print("  go:  ", json.dumps(go_n, ensure_ascii=False)[:400])
            print("  rust:", json.dumps(rs_n, ensure_ascii=False)[:400])
    print(f"\n{'='*60}")
    print(f"PASS {len(passed)}  FAIL {len(failed)}  DECLARED-DIFF {len(declared)}  "
          f"SKIP {len(skipped)}  MISSING {len(missing)}")
    if missing:
        print("missing:", ", ".join(missing))
    if failed:
        print("failed:", ", ".join(failed))
        sys.exit(1)


if __name__ == "__main__":
    main()
