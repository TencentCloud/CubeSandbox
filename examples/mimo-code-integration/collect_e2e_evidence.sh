#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PYTHON="${PYTHON:-${SCRIPT_DIR}/.venv/bin/python}"
if [[ ! -x "${PYTHON}" ]]; then
  PYTHON="${PYTHON_FALLBACK:-python3}"
fi

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${SCRIPT_DIR}/output/speculative-${STAMP}"
mkdir -p "${RUN_DIR}"

snapshot_resources() {
  local destination="$1"
  "${PYTHON}" - "${SCRIPT_DIR}" "${destination}" <<'PY'
import json
import os
import sys
from pathlib import Path

from dotenv import load_dotenv
from cubesandbox import Config, Sandbox

script_dir = Path(sys.argv[1])
destination = Path(sys.argv[2])
load_dotenv(script_dir / ".env", override=False)
api_url = os.environ.get("E2B_API_URL") or os.environ.get("CUBE_API_URL")
api_key = os.environ.get("E2B_API_KEY") or os.environ.get("CUBE_API_KEY")
if not api_url:
    raise SystemExit("E2B_API_URL or CUBE_API_URL is required")
try:
    config = Config(api_url=api_url, api_key=api_key)
except TypeError:
    config = Config(api_url=api_url)

sandboxes = Sandbox.list(config=config)
snapshots = []
next_token = None
while True:
    items, next_token = Sandbox.list_snapshots(
        next_token=next_token,
        config=config,
    )
    snapshots.extend(item.snapshot_id for item in items)
    if not next_token:
        break

payload = {
    "sandbox_ids": sorted(
        str(item.get("sandboxID") or item.get("sandbox_id"))
        for item in sandboxes
    ),
    "snapshot_ids": sorted(snapshots),
}
destination.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
PY
}

verify_run_resources() {
  local inventory="$1"
  "${PYTHON}" - "${RUN_DIR}" "${inventory}" <<'PY'
import json
import sys
from pathlib import Path

run_dir = Path(sys.argv[1])
inventory = json.loads(Path(sys.argv[2]).read_text())
target_sandboxes = set()
target_snapshots = set()
for name in ("promotion.json", "rollback.json"):
    path = run_dir / name
    if not path.exists():
        continue
    evidence = json.loads(path.read_text())
    for key in ("planner_sandbox_id", "source_sandbox_id"):
        if evidence.get(key):
            target_sandboxes.add(evidence[key])
    target_sandboxes.update(
        candidate["sandbox_id"]
        for candidate in evidence.get("candidates", [])
        if candidate.get("sandbox_id")
    )
    if evidence.get("snapshot_id"):
        target_snapshots.add(evidence["snapshot_id"])

leaked_sandboxes = target_sandboxes.intersection(inventory["sandbox_ids"])
leaked_snapshots = target_snapshots.intersection(inventory["snapshot_ids"])
if leaked_sandboxes or leaked_snapshots:
    raise SystemExit(
        "run-owned resource leak detected: "
        f"sandboxes={sorted(leaked_sandboxes)}, "
        f"snapshots={sorted(leaked_snapshots)}"
    )
print(
    "Run-owned resources cleaned: "
    f"{len(target_sandboxes)} sandbox IDs and "
    f"{len(target_snapshots)} snapshot IDs are absent."
)
PY
}

scan_evidence_secrets() {
  "${PYTHON}" - "${SCRIPT_DIR}" "${RUN_DIR}" <<'PY'
import os
import sys
from pathlib import Path
from dotenv import load_dotenv

script_dir = Path(sys.argv[1])
run_dir = Path(sys.argv[2])
load_dotenv(script_dir / ".env", override=False)
secret = os.environ.get("MIMO_API_KEY", "")
if not secret:
    raise SystemExit("MIMO_API_KEY is required for secret scanning")

leaks = []
needle = secret.encode()
for path in run_dir.rglob("*"):
    if path.is_file() and needle in path.read_bytes():
        leaks.append(str(path.relative_to(run_dir)))
if leaks:
    raise SystemExit("real MiMo key found in evidence files: " + ", ".join(leaks))
print("Secret scan passed: the real MiMo key is absent from evidence.")
PY
}

capture_after_on_exit() {
  local status=$?
  trap - EXIT
  if [[ ! -f "${RUN_DIR}/resources-after.json" ]]; then
    snapshot_resources "${RUN_DIR}/resources-after.json" || true
  fi
  if [[ -f "${RUN_DIR}/resources-after.json" ]]; then
    verify_run_resources "${RUN_DIR}/resources-after.json" || true
  fi
  scan_evidence_secrets || true
  exit "${status}"
}
trap capture_after_on_exit EXIT

printf 'Evidence directory: %s\n' "${RUN_DIR}"

(
  cd "${SCRIPT_DIR}"
  "${PYTHON}" -m unittest discover -s tests -v
  "${PYTHON}" -m py_compile \
    ./*.py \
    ./tests/*.py \
    ./fixtures/normalize-slug/project/*.py \
    ./fixtures/normalize-slug/project/tests/*.py
  bash -n build-template.sh collect_e2e_evidence.sh
) 2>&1 | tee "${RUN_DIR}/offline-checks.log"

snapshot_resources "${RUN_DIR}/resources-before.json"

AUDIT_PATH="$(
  "${PYTHON}" - "${SCRIPT_DIR}" <<'PY'
import os
import sys
from pathlib import Path
from dotenv import load_dotenv

load_dotenv(Path(sys.argv[1]) / ".env", override=False)
print(
    os.environ.get(
        "MIMO_EGRESS_AUDIT_PATH",
        "/data/log/cube-egress/access.jsonl",
    )
)
PY
)"
AUDIT_START_LINE=0
if [[ -n "${AUDIT_PATH}" && -r "${AUDIT_PATH}" ]]; then
  AUDIT_START_LINE="$(wc -l < "${AUDIT_PATH}")"
fi

(
  cd "${SCRIPT_DIR}"
  "${PYTHON}" network_policy.py --skip-agent
) 2>&1 | tee "${RUN_DIR}/network-preflight.log"

(
  cd "${SCRIPT_DIR}"
  "${PYTHON}" speculative_mimo_code.py \
    --task "${SCRIPT_DIR}/fixtures/normalize-slug/task.json" \
    --candidates 2 \
    --concurrency 2 \
    --evidence-file "${RUN_DIR}/promotion.json"
) 2>&1 | tee "${RUN_DIR}/promotion.log"

grep -Fq "CUBE_MIMO_PROMOTION_OK" "${RUN_DIR}/promotion.log"

(
  cd "${SCRIPT_DIR}"
  "${PYTHON}" speculative_mimo_code.py \
    --task "${SCRIPT_DIR}/fixtures/normalize-slug/task.json" \
    --candidates 2 \
    --concurrency 2 \
    --force-promotion-failure \
    --evidence-file "${RUN_DIR}/rollback.json"
) 2>&1 | tee "${RUN_DIR}/rollback.log"

grep -Fq "CUBE_MIMO_ROLLBACK_OK" "${RUN_DIR}/rollback.log"

snapshot_resources "${RUN_DIR}/resources-after.json"
verify_run_resources "${RUN_DIR}/resources-after.json"

if [[ -n "${AUDIT_PATH}" && -r "${AUDIT_PATH}" ]]; then
  "${PYTHON}" - "${SCRIPT_DIR}" "${AUDIT_PATH}" \
    "${RUN_DIR}/egress-audit-summary.json" "${AUDIT_START_LINE}" \
    "${RUN_DIR}" <<'PY'
import json
import sys
from pathlib import Path

audit_path = Path(sys.argv[2])
destination = Path(sys.argv[3])
start_line = int(sys.argv[4])
run_dir = Path(sys.argv[5])
evidence = [
    json.loads((run_dir / name).read_text())
    for name in ("promotion.json", "rollback.json")
]
target_rules = {
    item["egress_rule_name"]
    for item in evidence
    if item.get("egress_rule_name")
}

def selected_fields(payload):
    http = payload.get("http") or {}
    policy = payload.get("policy") or {}
    sandbox = payload.get("sandbox") or {}
    tls = payload.get("tls") or {}
    credentials = payload.get("credentials") or {}
    return {
        "ts": payload.get("ts"),
        "event": payload.get("event"),
        "sandbox": {"src_ip": sandbox.get("src_ip")},
        "http": {
            key: http.get(key)
            for key in (
                "host",
                "method",
                "path",
                "status",
                "req_bytes",
                "resp_bytes",
                "user_agent",
            )
        },
        "policy": {
            key: policy.get(key)
            for key in ("decision", "matched_rule", "duration_us")
        },
        "credentials": {"injected": credentials.get("injected", [])},
        "tls": {key: tls.get(key) for key in ("sni", "version")},
    }

matches = []
lines = audit_path.read_text(encoding="utf-8", errors="replace").splitlines()
new_lines = lines[start_line:] if start_line <= len(lines) else lines
for line in new_lines:
    if "api.xiaomimimo.com" not in line:
        continue
    try:
        payload = json.loads(line)
    except json.JSONDecodeError:
        continue
    matched_rule = (payload.get("policy") or {}).get("matched_rule")
    if matched_rule not in target_rules:
        continue
    matches.append(selected_fields(payload))

if not matches:
    raise SystemExit(
        "no CubeEgress audit records matched this rollout's rule names"
    )
qualified_rules = {
    item["policy"]["matched_rule"]
    for item in matches
    if item["policy"]["decision"] == "allow"
    and "api-key" in item["credentials"]["injected"]
}
missing_rules = target_rules.difference(qualified_rules)
if missing_rules:
    raise SystemExit(
        "missing allowed, credential-injected audit evidence for rules: "
        + ", ".join(sorted(missing_rules))
    )
destination.write_text(json.dumps(matches, indent=2, sort_keys=True) + "\n")
print(f"Wrote {len(matches)} redacted MiMo audit records.")
PY
else
  printf '%s\n' \
    "MIMO_EGRESS_AUDIT_PATH is unset or unreadable; attach a separately redacted" \
    "CubeEgress audit excerpt when submitting the PR evidence." \
    > "${RUN_DIR}/egress-audit-note.txt"
fi

scan_evidence_secrets

"${PYTHON}" - "${RUN_DIR}" <<'PY'
import json
import sys
from pathlib import Path

run_dir = Path(sys.argv[1])
promotion = json.loads((run_dir / "promotion.json").read_text())
rollback = json.loads((run_dir / "rollback.json").read_text())
run_sandboxes = {
    evidence[key]
    for evidence in (promotion, rollback)
    for key in ("planner_sandbox_id", "source_sandbox_id")
}
run_sandboxes.update(
    candidate["sandbox_id"]
    for evidence in (promotion, rollback)
    for candidate in evidence["candidates"]
)
run_snapshots = {promotion["snapshot_id"], rollback["snapshot_id"]}

lines = [
    "# MiMo Speculative Rollout E2E Evidence",
    "",
    f"- Task profile: `{promotion['task']['name']}`",
    f"- Allowed paths: `{', '.join(promotion['task']['allowed_paths'])}`",
    f"- Promotion outcome: `{promotion['outcome']}`",
    f"- Rollback outcome: `{rollback['outcome']}`",
    f"- Parent session (promotion): `{promotion['parent_session_id']}`",
    f"- Snapshot (promotion): `{promotion['snapshot_id']}`",
    f"- Winner: `{promotion['winner']}`",
    f"- Candidate results: `{len(promotion['candidates'])}`",
    f"- Run-owned sandboxes cleaned: `{len(run_sandboxes)}`; remaining: `0`",
    f"- Run-owned snapshots cleaned: `{len(run_snapshots)}`; remaining: `0`",
    "- Real MiMo key in evidence: `no`",
    "",
    "See the JSON and log files in this directory for candidate sandbox/session IDs,",
    "scores, promotion details, rollback proof, and resource cleanup.",
]
(run_dir / "SUMMARY.md").write_text("\n".join(lines) + "\n")
PY

trap - EXIT
printf 'CUBE_MIMO_E2E_EVIDENCE_OK\nSummary: %s\n' "${RUN_DIR}/SUMMARY.md"
