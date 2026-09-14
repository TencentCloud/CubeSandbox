---
title: 质量状态
---

<script setup>
import { data } from '../../quality-status.data.js'
</script>

<QualityStatusPage :data="data" locale="zh" />

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
    "baseline": { "name": "v0.7.1 baseline", "url": "https://cubesandbox.com/guide/performance-benchmark" },
    "counts": { "critical": 0, "warning": 1, "similar": 8, "improved": 2 },
    "metrics": [{ "label": "3.2 Sandbox Create Latency", "current_text": "1.24s", "baseline_text": "1.31s", "delta_pct": -5.3, "verdict": "improved" }]
  }
}
```
