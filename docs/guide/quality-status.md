---
title: Daily Performance Report
---

<script setup>
import { data } from '../quality-status.data.js'
</script>

<QualityStatusPage :data="data" locale="en" />

## Read the Latest Report with Python

The report is public JSON and does not require an API key. This minimal example fetches the latest result and prints the overall status, E2E totals, and performance metrics:

```python
import json
from urllib.request import urlopen

REPORT_URL = (
    "https://cubesandbox-1253970226.cos.ap-singapore.myqcloud.com/"
    "page-data/quality-status.json"
)

with urlopen(REPORT_URL, timeout=10) as response:
    report = json.load(response)

print(f"Date: {report.get('date', 'unknown')}")
print(f"Overall status: {report.get('status', 'unknown')}")

tests = report.get("e2e", {}).get("tests", {})
print(
    "E2E: "
    f"{tests.get('passed', 0)}/{tests.get('total', 0)} passed, "
    f"{tests.get('failed', 0)} failed"
)

for metric in report.get("performance", {}).get("metrics", []):
    label = metric.get("label", "Unnamed metric")
    current = metric.get("current_text", metric.get("current", "—"))
    baseline = metric.get("baseline_text", metric.get("baseline", "—"))
    verdict = metric.get("verdict", "unknown")
    print(f"{label}: current={current}, baseline={baseline}, verdict={verdict}")
```

## Data Source

This page loads the latest public nightly status from the Tencent Cloud COS JSON object at `https://cubesandbox-1253970226.cos.ap-singapore.myqcloud.com/page-data/quality-status.json`.

To override the source for another deployment, configure `VITE_QUALITY_STATUS_JSON_URL` or `QUALITY_STATUS_JSON_URL`.

Browser-side refresh requires the COS object to allow cross-origin `GET` requests from the documentation domain. The docs build remains offline-safe and only embeds local preview data when `QUALITY_STATUS_JSON_PATH` is set.

For local preview, set `QUALITY_STATUS_JSON_PATH` to a JSON file with the same public schema.

Only aggregate nightly E2E and performance data should be published. Do not include internal environment IDs, run IDs, logs, credentials, or private Web UI links.

## JSON Contract

The public JSON object should contain the following top-level fields:

```json
{
  "generatedAt": "2026-09-14T17:30:00+08:00",
  "date": "2026-09-14",
  "commit": "ba77166b93fa995837a4bca0ae33b51d69c0494c",
  "status": "passed",
  "e2e": {
    "status": "passed",
    "tests": { "total": 128, "passed": 126, "failed": 0, "errors": 0, "skipped": 2 },
    "modules": [{ "module": "lifecycle", "total": 32, "passed": 32, "failed": 0, "errors": 0, "skipped": 0 }]
  },
  "performance": {
    "status": "passed",
    "baseline": {
      "name": "v0.3.0 baseline",
      "url": "https://cubesandbox.com/blog/posts/2026-06-01-cubesandbox-perf-benchmark",
      "url_zh": "https://cubesandbox.com/zh/blog/posts/2026-06-01-cubesandbox-perf-benchmark"
    },
    "counts": { "critical": 0, "warning": 1, "similar": 8, "improved": 2 },
    "metrics": [{ "label": "3.2 Sandbox Create Latency", "current_text": "1.24s", "baseline_text": "1.31s", "delta_pct": -5.3, "verdict": "improved" }]
  }
}
```

The status page picks the baseline link from the current locale. The Chinese page prefers `baseline.url_zh`; the English page prefers `baseline.url` or `baseline.url_en`. If the JSON contains only one URL, the page rewrites known public site and GitHub docs paths (`cubesandbox.com/guide/...` ↔ `/zh/guide/...`, and GitHub `docs/blog/posts/...` ↔ `docs/zh/blog/posts/...`).
