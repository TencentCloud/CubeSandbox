---
title: 每日性能测试报告
---

<script setup>
import { data } from '../../quality-status.data.js'
</script>

<QualityStatusPage :data="data" locale="zh" />

## 使用 Python 获取最新报告

报告以公开 JSON 提供，无需 API Key。下面的最小示例会获取最新结果，并输出整体状态、E2E 测试统计和性能指标：

```python
import json
from urllib.request import urlopen

REPORT_URL = (
    "https://cubesandbox-1253970226.cos.ap-singapore.myqcloud.com/"
    "page-data/quality-status.json"
)

# 获取并解析最新的每日测试报告。
with urlopen(REPORT_URL, timeout=10) as response:
    report = json.load(response)

print(f"日期：{report.get('date', '未知')}")
print(f"整体状态：{report.get('status', '未知')}")

# 汇总 E2E 测试结果。
tests = report.get("e2e", {}).get("tests", {})
print(
    "E2E："
    f"通过 {tests.get('passed', 0)}/{tests.get('total', 0)}，"
    f"失败 {tests.get('failed', 0)}"
)

# 逐项输出当前性能、公开基线和对比结论。
for metric in report.get("performance", {}).get("metrics", []):
    label = metric.get("label", "未命名指标")
    current = metric.get("current_text", metric.get("current", "—"))
    baseline = metric.get("baseline_text", metric.get("baseline", "—"))
    verdict = metric.get("verdict", "未知")
    print(f"{label}：本次={current}，基线={baseline}，结论={verdict}")
```

## 数据源

本页默认从腾讯云 COS JSON 对象动态加载最新公开 nightly 状态：`https://cubesandbox-1253970226.cos.ap-singapore.myqcloud.com/page-data/quality-status.json`。

如需在其他部署中覆盖数据源，可以配置 `VITE_QUALITY_STATUS_JSON_URL` 或 `QUALITY_STATUS_JSON_URL`。

浏览器端实时刷新需要 COS 对象允许文档站域名跨域 `GET` 请求。文档构建保持离线安全，只有设置 `QUALITY_STATUS_JSON_PATH` 时才会内置本地预览数据。

本地预览时，可以使用 `QUALITY_STATUS_JSON_PATH` 指向同一公开 schema 的 JSON 文件。

只应发布聚合后的 nightly E2E 和性能数据。不要在公开 JSON 中包含内部环境 ID、run ID、日志、凭据或私有 Web UI 链接。

## JSON 约定

公开 JSON 对象应包含以下顶层字段：

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

Status 页面会按当前语言选择基线链接。中文页优先使用 `baseline.url_zh`，英文页优先使用 `baseline.url` 或 `baseline.url_en`。如果 JSON 只有一个 URL，页面会改写已知的官网和 GitHub 文档路径（`cubesandbox.com/guide/...` ↔ `/zh/guide/...`，以及 GitHub `docs/blog/posts/...` ↔ `docs/zh/blog/posts/...`）。
