#!/usr/bin/env python3
"""Strict acceptance validator for topic1 evidence-repair ledger."""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import sys
from pathlib import Path

REQUIRED_REQ_FIELDS = [
    "run_id", "experiment_id", "arm", "phase", "seq", "request_id",
    "ts_submit", "ts_api", "ts_ready", "latency_api_ms", "latency_ready_ms",
    "http", "outcome", "failure_reason", "sandbox_id", "final_node",
    "commit_sha", "binary_sha256", "config_sha256",
]

E2_ARMS = ("control", "healthy", "delayed", "timeout")
RESTORE_REQUIRED = [
    "binary_restored", "conf_restored", "binary_sha", "conf_sha",
    "sandbox_count", "host_cubelet", "stub_gone", "passed", "status",
    "registry_only_cleanup", "pre_state_sandbox_baseline",
]


def sha256_file(p: Path) -> str:
    h = hashlib.sha256()
    h.update(p.read_bytes())
    return h.hexdigest()


def load_json(p: Path):
    return json.loads(p.read_text(encoding="utf-8"))


def pct(xs, p):
    if not xs:
        return None
    ys = sorted(xs)
    if len(ys) == 1:
        return float(ys[0])
    k = (len(ys) - 1) * (p / 100.0)
    f = math.floor(k)
    c = math.ceil(k)
    if f == c:
        return float(ys[int(k)])
    return float(ys[f] * (c - k) + ys[c] * (k - f))


def nearly(a, b, tol=1e-6):
    if a is None and b is None:
        return True
    if a is None or b is None:
        return False
    return abs(float(a) - float(b)) <= tol


def read_jsonl(p: Path):
    rows = []
    for i, ln in enumerate(p.read_text(encoding="utf-8").splitlines(), 1):
        if not ln.strip():
            continue
        try:
            rows.append(json.loads(ln))
        except Exception as e:
            raise ValueError(f"{p}: line {i} json error: {e}")
    return rows


def accept(ledger: Path) -> dict:
    errors = []
    warnings = []

    def err(msg):
        errors.append(msg)

    status_path = ledger / "STATUS.json"
    restore_path = ledger / "RESTORE.json"
    manifest_path = ledger / "audit" / "EVIDENCE_MANIFEST.json"
    if not status_path.exists():
        return {"result": "ACCEPT_FAIL", "errors": ["missing STATUS.json"]}
    status = load_json(status_path)

    if status.get("github_writes") is not False:
        err("github_writes must be false")
    if status.get("original_pr_heads_unchanged") is not True:
        err("original_pr_heads_unchanged must be true")
    if status.get("business_signal_live") != "PARTIAL":
        err(f"STATUS.business_signal_live must be PARTIAL, got {status.get('business_signal_live')}")
    if status.get("filter_before_score") not in {"PARTIAL", "PASS_WITH_GAPS"}:
        err(f"STATUS.filter_before_score must be PARTIAL/PASS_WITH_GAPS, got {status.get('filter_before_score')}")
    if status.get("restore") != "PASS":
        err("STATUS.restore must be PASS")

    if not restore_path.exists():
        err("missing RESTORE.json")
        restore = {}
    else:
        restore = load_json(restore_path)
        for k in RESTORE_REQUIRED:
            if k not in restore:
                err(f"RESTORE missing field {k}")
        if restore.get("status") != "PASS" or restore.get("passed") is not True:
            err("RESTORE not PASS")
        if restore.get("registry_only_cleanup") is not True:
            err("RESTORE.registry_only_cleanup must be true")
        if restore.get("stub_gone") is not True:
            err("RESTORE.stub_gone must be true")
        if restore.get("sandbox_count") != restore.get("pre_state_sandbox_baseline"):
            err("RESTORE sandbox_count != pre_state_sandbox_baseline")

    # E2
    e2_ids = set()
    for arm in E2_ARMS:
        req_p = ledger / "raw" / "E2_http_hot_path" / arm / "requests.jsonl"
        sum_p = ledger / "raw" / "E2_http_hot_path" / arm / "summary.json"
        met_p = ledger / "raw" / "E2_http_hot_path" / arm / "metrics_delta.json"
        if not req_p.exists():
            err(f"missing E2 requests {arm}")
            continue
        if not sum_p.exists():
            err(f"missing E2 summary {arm}")
            continue
        rows = read_jsonl(req_p)
        summary = load_json(sum_p)
        warm = [r for r in rows if r.get("phase") == "warmup"]
        meas = [r for r in rows if r.get("phase") == "measure"]
        if len(warm) != 2:
            err(f"E2 {arm} warmup rows want 2 got {len(warm)}")
        if len(meas) != 20:
            err(f"E2 {arm} measure rows want 20 got {len(meas)}")
        # phase order: all warmup before measure in file order
        phases = [r.get("phase") for r in rows]
        if phases != ["warmup"] * len(warm) + ["measure"] * len(meas):
            err(f"E2 {arm} phase order invalid: {phases}")
        # seq contiguous for measure
        seqs = [r.get("seq") for r in meas]
        if seqs != list(range(20)):
            err(f"E2 {arm} measure seq gap/mismatch: {seqs}")
        # required fields + uniqueness
        for r in rows:
            for k in REQUIRED_REQ_FIELDS:
                if k not in r:
                    err(f"E2 {arm} missing field {k} in request_id={r.get('request_id')}")
            rid = r.get("request_id")
            if rid in e2_ids:
                err(f"duplicate request_id {rid}")
            e2_ids.add(rid)
            if r.get("arm") != arm:
                err(f"E2 file arm={arm} but row arm={r.get('arm')} id={rid}")
            if r.get("experiment_id") != "E2_http_hot_path":
                err(f"E2 bad experiment_id {r.get('experiment_id')}")
            if not r.get("commit_sha"):
                err(f"E2 {arm} missing commit_sha id={rid}")
            if not r.get("binary_sha256"):
                err(f"E2 {arm} missing binary_sha256 id={rid}")
            if not r.get("config_sha256"):
                err(f"E2 {arm} missing config_sha256 id={rid}")
        # SHA consistency within arm
        commits = {r.get("commit_sha") for r in rows}
        bins = {r.get("binary_sha256") for r in rows}
        confs = {r.get("config_sha256") for r in rows}
        if len(commits) != 1:
            err(f"E2 {arm} commit_sha drift {commits}")
        if len(bins) != 1:
            err(f"E2 {arm} binary_sha256 drift {bins}")
        if len(confs) != 1:
            err(f"E2 {arm} config_sha256 drift {confs}")
        # recompute summary
        api = [float(r["latency_api_ms"]) for r in meas if r.get("latency_api_ms") is not None]
        ready = [float(r["latency_ready_ms"]) for r in meas if r.get("latency_ready_ms") is not None]
        fail_n = sum(1 for r in meas if r.get("outcome") != "success")
        if summary.get("fail_n") != fail_n:
            err(f"E2 {arm} fail_n mismatch summary={summary.get('fail_n')} raw={fail_n}")
        if not nearly(summary.get("api_p50_ms"), pct(api, 50)):
            err(f"E2 {arm} api_p50 mismatch summary={summary.get('api_p50_ms')} recomputed={pct(api,50)}")
        if not nearly(summary.get("api_p95_ms"), pct(api, 95)):
            err(f"E2 {arm} api_p95 mismatch summary={summary.get('api_p95_ms')} recomputed={pct(api,95)}")
        if not nearly(summary.get("api_max_ms"), max(api) if api else None):
            err(f"E2 {arm} api_max mismatch")
        if not nearly(summary.get("ready_p50_ms"), pct(ready, 50)):
            err(f"E2 {arm} ready_p50 mismatch")
        if not nearly(summary.get("ready_p95_ms"), pct(ready, 95)):
            err(f"E2 {arm} ready_p95 mismatch")
        if not nearly(summary.get("ready_max_ms"), max(ready) if ready else None):
            err(f"E2 {arm} ready_max mismatch")
        # metrics reason
        if not met_p.exists():
            err(f"missing metrics_delta for {arm}")
        else:
            md = load_json(met_p).get("delta") or {}
            if arm == "timeout":
                if md.get("reason:timeout") is None:
                    err("timeout arm missing reason:timeout delta")
                elif md.get("reason:success"):
                    err("timeout arm must not claim reason:success delta")
                # outcomes should match reason timeout and equal warmup+measure when scorer on
                rt = md.get("reason:timeout")
                ot = md.get("cube_scheduler_external_http_score_outcomes_total")
                if rt is not None and ot is not None and not nearly(rt, ot):
                    err(f"timeout reason delta != outcomes_total ({rt} vs {ot})")
                if rt is not None and not nearly(rt, float(len(rows))):
                    err(f"timeout reason:timeout delta {rt} != request rows {len(rows)}")
            if arm == "healthy":
                if md.get("reason:success") is None:
                    err("healthy arm missing reason:success")
                if md.get("reason:timeout"):
                    err("healthy arm unexpected reason:timeout")
            if arm == "control" and md:
                # control may be empty
                pass

    # rollup must match arm summaries
    rollup_p = ledger / "summaries" / "http_hot_path_latency.json"
    if rollup_p.exists():
        rollup = load_json(rollup_p)
        for arm_sum in rollup.get("arms") or []:
            arm = arm_sum.get("arm")
            disk = load_json(ledger / "raw" / "E2_http_hot_path" / arm / "summary.json")
            for k in ("api_p50_ms", "api_p95_ms", "api_max_ms", "ready_p50_ms", "ready_p95_ms", "ready_max_ms", "fail_n", "n"):
                if not nearly(arm_sum.get(k), disk.get(k)) and arm_sum.get(k) != disk.get(k):
                    err(f"rollup {arm}.{k} != raw summary")

    # E3 PARTIAL
    e3s = ledger / "summaries" / "business_signal_live.json"
    if not e3s.exists():
        err("missing business_signal_live summary")
    else:
        e3 = load_json(e3s)
        if e3.get("status") != "PARTIAL":
            err(f"E3 status must be PARTIAL, got {e3.get('status')}")
        phases = e3.get("phases_observed") or []
        if "prefer_a" in phases or "prefer_b" in phases:
            err("E3 unexpectedly claims prefer_a/prefer_b without repair samples")
        if phases != ["prefer_first"] and set(phases) != {"prefer_first"}:
            # allow only prefer_first
            if set(phases) - {"prefer_first"}:
                err(f"E3 unexpected phases {phases}")
        e3_raw = ledger / "raw" / "E3_business_signal" / "requests.jsonl"
        if e3_raw.exists():
            rows = read_jsonl(e3_raw)
            ids = [r.get("request_id") for r in rows]
            if len(ids) != len(set(ids)):
                err("E3 duplicate request_id")
            for r in rows:
                for k in REQUIRED_REQ_FIELDS:
                    if k not in r:
                        err(f"E3 missing field {k}")

    # E4 nuanced
    e4s = ledger / "summaries" / "filter_before_score.json"
    if not e4s.exists():
        err("missing filter_before_score summary")
    else:
        e4 = load_json(e4s)
        if e4.get("status") not in {"PARTIAL", "PASS_WITH_GAPS"}:
            err(f"E4 status must be PARTIAL/PASS_WITH_GAPS got {e4.get('status')}")
        if "live_bad_score" not in e4 or "filter_before_score_unit" not in e4 or "unobserved_negatives" not in e4:
            err("E4 summary missing evidence split fields")
        if e4.get("status") == "PASS" and not e4.get("unobserved_negatives"):
            err("E4 cannot be summary-only PASS")

    # Filter anti-resurrection forged evidence must not exist as accepted claim
    forged = ledger / "raw" / "E4_filter_before_score" / "FORGED_RESURRECTION.json"
    if forged.exists():
        fr = load_json(forged)
        if fr.get("claim_accepted") is True:
            err("Filter anti-resurrection forged claim_accepted=true must fail")
        if fr.get("scorer_restored_filtered_node") is True and fr.get("accepted_by_scheduler") is True:
            err(f"{forged}: scorer cannot restore filtered node; evidence invalid")

    # Gate2 real exits
    g2 = ledger / "lanes" / "E" / "gate2.log"
    g2j = ledger / "lanes" / "E" / "gate2_results.json"
    if not g2.exists() or not g2j.exists():
        err("missing Gate2 log/results")
    else:
        results = load_json(g2j)
        if not results.get("commands"):
            err("Gate2 commands empty")
        for c in results["commands"]:
            name = c.get("name")
            if c.get("blocked"):
                if c.get("status") != "BLOCKED_ENVIRONMENT":
                    err(f"Gate2 {name} blocked but status not BLOCKED_ENVIRONMENT")
                continue
            if "PASS" in str(c.get("summary_only")):
                err(f"Gate2 {name} summary_only PASS without exit")
            if c.get("exit") is None:
                err(f"Gate2 {name} missing exit code")
            if c.get("expect_zero") and c.get("exit") != 0:
                err(f"Gate2 {name} exit={c.get('exit')} expected 0")
        # string GATE2_DONE alone is insufficient — require results file exits
        text = g2.read_text(encoding="utf-8", errors="replace")
        if "GATE2_DONE" in text and not results.get("commands"):
            err("Gate2 DONE string without command exits")

    # harness drain fix present
    harness = ledger / "scripts" / "remote_live_harness.py"
    if harness.exists():
        ht = harness.read_text(encoding="utf-8", errors="replace")
        if "registry-only drain" not in ht and "unregistered sandboxes present" not in ht:
            err("harness drain fix markers missing")
        # ensure old global destroy loop not present after CREATED.clear
        if re_search_old_drain(ht):
            err("harness still destroys all cli-list sandboxes")

    # manifest
    if not manifest_path.exists():
        err("missing EVIDENCE_MANIFEST.json")
    else:
        man = load_json(manifest_path)
        files = man.get("files") or {}
        include_roots = man.get("include_globs") or []
        exclude = set(man.get("exclude_paths") or [])
        # every declared file exists and hash matches
        for rel, meta in files.items():
            fp = ledger / rel
            if not fp.exists():
                err(f"manifest member missing: {rel}")
                continue
            dig = sha256_file(fp)
            if dig != meta.get("sha256"):
                err(f"manifest hash drift: {rel}")
        def excluded(rel: str) -> bool:
            if rel in exclude:
                return True
            for x in exclude:
                if x.endswith("/") and rel.startswith(x):
                    return True
                if x.endswith("*") and rel.startswith(x.rstrip("*")):
                    return True
            base = Path(rel).name
            if base in {"result-1.json", "result-2.json", "NEGATIVE_RESULTS.json"}:
                return True
            if "__pycache__" in rel or rel.endswith(".pyc"):
                return True
            if "/_negatives/" in f"/{rel}/":
                return True
            return False

        # no undeclared evidence files under tracked roots
        for sub in ("raw", "summaries", "audit", "lanes", "scripts", "analysis"):
            root = ledger / sub
            if not root.exists():
                continue
            for p in root.rglob("*"):
                if not p.is_file():
                    continue
                rel = p.relative_to(ledger).as_posix()
                if excluded(rel):
                    continue
                if rel == "audit/EVIDENCE_MANIFEST.json":
                    continue
                if rel not in files:
                    err(f"undeclared evidence file: {rel}")

    result = "ACCEPT_PASS" if not errors else "ACCEPT_FAIL"
    return {
        "result": result,
        "errors": errors,
        "warnings": warnings,
        "error_count": len(errors),
    }


def re_search_old_drain(ht: str) -> bool:
    # Detect the unsafe pattern: after CREATED.clear(), loop cli_list IDs into destroy
    return ("CREATED.clear()" in ht) and ("for sid in re.findall" in ht) and ("destroy(sid)" in ht[ht.find("CREATED.clear()"):ht.find("CREATED.clear()")+400])


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--ledger", required=True)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    ledger = Path(args.ledger)
    res = accept(ledger)
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(res, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(res["result"], "errors=", res["error_count"])
    for e in res["errors"]:
        print(" -", e)
    return 0 if res["result"] == "ACCEPT_PASS" else 2


if __name__ == "__main__":
    sys.exit(main())
