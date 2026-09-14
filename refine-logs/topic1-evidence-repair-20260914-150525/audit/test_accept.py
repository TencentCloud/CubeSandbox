#!/usr/bin/env python3
"""Negative matrix for evidence-repair accept.py."""
from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

import accept


LEDGER = Path(__file__).resolve().parents[1]


def clone_ledger() -> Path:
    td = Path(tempfile.mkdtemp(prefix="topic1-neg-"))
    # copy essential tree
    for name in ("raw", "summaries", "audit", "lanes", "scripts", "analysis"):
        src = LEDGER / name
        if src.exists():
            shutil.copytree(src, td / name, ignore=shutil.ignore_patterns("_negatives", "__pycache__", "result-*.json"))
    for name in ("STATUS.json", "RESTORE.json", "PRE_STATE.json", "POST_STATE.json", "RESOURCE_REGISTRY.json"):
        shutil.copy2(LEDGER / name, td / name)
    # drop result files from copied audit if any
    for p in (td / "audit").glob("result-*.json"):
        p.unlink()
    return td


def rewrite_manifest(td: Path) -> None:
    # rebuild minimal manifest for mutated ledger so hash checks target intentional faults
    # For negatives that mutate content, we keep original manifest to trigger drift OR
    # regenerate excluding the mutated path depending on case.
    pass


class TestNegatives(unittest.TestCase):
    def assert_fail(self, td: Path, substr: str):
        res = accept.accept(td)
        self.assertEqual(res["result"], "ACCEPT_FAIL", msg=res)
        joined = "\n".join(res["errors"])
        self.assertIn(substr, joined, msg=joined)

    def test_raw_latency_change_summary_unchanged(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/control/requests.jsonl"
        rows = [json.loads(l) for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        for r in rows:
            if r.get("phase") == "measure":
                r["latency_api_ms"] = int(r["latency_api_ms"]) + 1000
                break
        req.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")
        self.assert_fail(td, "api_p")

    def test_summary_change_raw_unchanged(self):
        td = clone_ledger()
        sp = td / "raw/E2_http_hot_path/control/summary.json"
        s = json.loads(sp.read_text(encoding="utf-8"))
        s["api_p95_ms"] = float(s["api_p95_ms"]) + 999
        sp.write_text(json.dumps(s, indent=2) + "\n", encoding="utf-8")
        self.assert_fail(td, "api_p95")

    def test_delete_row(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/healthy/requests.jsonl"
        lines = [l for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        req.write_text("\n".join(lines[:-1]) + "\n", encoding="utf-8")
        self.assert_fail(td, "measure rows")

    def test_duplicate_request_id(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/delayed/requests.jsonl"
        lines = [l for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        rows = [json.loads(l) for l in lines]
        rows[-1]["request_id"] = rows[0]["request_id"]
        req.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")
        self.assert_fail(td, "duplicate request_id")

    def test_wrong_arm(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/timeout/requests.jsonl"
        rows = [json.loads(l) for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        rows[0]["arm"] = "healthy"
        req.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")
        self.assert_fail(td, "row arm=")

    def test_missing_warmup(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/control/requests.jsonl"
        rows = [json.loads(l) for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        rows = [r for r in rows if r.get("phase") != "warmup"]
        req.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")
        self.assert_fail(td, "warmup")

    def test_seq_gap(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/control/requests.jsonl"
        rows = [json.loads(l) for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        for r in rows:
            if r.get("phase") == "measure" and r.get("seq") == 5:
                r["seq"] = 7
        req.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")
        self.assert_fail(td, "seq")

    def test_missing_sha(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/control/requests.jsonl"
        rows = [json.loads(l) for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        rows[0]["binary_sha256"] = None
        req.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")
        self.assert_fail(td, "binary_sha256")

    def test_sha_drift_across_rows(self):
        td = clone_ledger()
        req = td / "raw/E2_http_hot_path/healthy/requests.jsonl"
        rows = [json.loads(l) for l in req.read_text(encoding="utf-8").splitlines() if l.strip()]
        rows[-1]["commit_sha"] = "deadbeef" * 5
        req.write_text("\n".join(json.dumps(r) for r in rows) + "\n", encoding="utf-8")
        self.assert_fail(td, "commit_sha drift")

    def test_timeout_reason_to_success(self):
        td = clone_ledger()
        mp = td / "raw/E2_http_hot_path/timeout/metrics_delta.json"
        m = json.loads(mp.read_text(encoding="utf-8"))
        d = m["delta"]
        if "reason:timeout" in d:
            d["reason:success"] = d.pop("reason:timeout")
        else:
            d["reason:success"] = 22
        mp.write_text(json.dumps(m, indent=2) + "\n", encoding="utf-8")
        # also mirror into summary
        sp = td / "raw/E2_http_hot_path/timeout/summary.json"
        s = json.loads(sp.read_text(encoding="utf-8"))
        s["metrics_delta"] = d
        sp.write_text(json.dumps(s, indent=2) + "\n", encoding="utf-8")
        self.assert_fail(td, "reason")

    def test_reason_count_mismatch(self):
        td = clone_ledger()
        mp = td / "raw/E2_http_hot_path/timeout/metrics_delta.json"
        m = json.loads(mp.read_text(encoding="utf-8"))
        m["delta"]["reason:timeout"] = 3
        mp.write_text(json.dumps(m, indent=2) + "\n", encoding="utf-8")
        self.assert_fail(td, "reason:timeout")

    def test_filter_resurrection_forged(self):
        td = clone_ledger()
        forged = {
            "scorer_restored_filtered_node": True,
            "accepted_by_scheduler": True,
            "claim_accepted": True,
            "node_id": "filtered-node-x",
        }
        p = td / "raw/E4_filter_before_score/FORGED_RESURRECTION.json"
        p.write_text(json.dumps(forged, indent=2) + "\n", encoding="utf-8")
        self.assert_fail(td, "filtered")

    def test_restore_missing_field(self):
        td = clone_ledger()
        rp = td / "RESTORE.json"
        r = json.loads(rp.read_text(encoding="utf-8"))
        r.pop("stub_gone", None)
        rp.write_text(json.dumps(r, indent=2) + "\n", encoding="utf-8")
        self.assert_fail(td, "stub_gone")

    def test_restore_sandbox_baseline(self):
        td = clone_ledger()
        rp = td / "RESTORE.json"
        r = json.loads(rp.read_text(encoding="utf-8"))
        r["sandbox_count"] = 99
        rp.write_text(json.dumps(r, indent=2) + "\n", encoding="utf-8")
        self.assert_fail(td, "sandbox_count")

    def test_manifest_hash_drift(self):
        td = clone_ledger()
        # mutate a declared file without updating manifest
        p = td / "summaries/business_signal_live.json"
        s = json.loads(p.read_text(encoding="utf-8"))
        s["note"] = (s.get("note") or "") + " tampered"
        p.write_text(json.dumps(s, indent=2) + "\n", encoding="utf-8")
        self.assert_fail(td, "hash drift")

    def test_manifest_missing_member(self):
        td = clone_ledger()
        man_p = td / "audit/EVIDENCE_MANIFEST.json"
        man = json.loads(man_p.read_text(encoding="utf-8"))
        # remove one key
        keys = list(man["files"].keys())
        man["files"].pop(keys[0])
        man_p.write_text(json.dumps(man, indent=2) + "\n", encoding="utf-8")
        # file still on disk -> undeclared OR we removed declaration making undeclared
        res = accept.accept(td)
        self.assertEqual(res["result"], "ACCEPT_FAIL")

    def test_gate2_pass_string_but_nonzero(self):
        td = clone_ledger()
        g2j = td / "lanes/E/gate2_results.json"
        data = json.loads(g2j.read_text(encoding="utf-8"))
        data["commands"][0]["exit"] = 1
        data["commands"][0]["expect_zero"] = True
        g2j.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
        (td / "lanes/E/gate2.log").write_text("GATE2_DONE exit=0\n", encoding="utf-8")
        self.assert_fail(td, "Gate2")


if __name__ == "__main__":
    unittest.main(verbosity=2)
