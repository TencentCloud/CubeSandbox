import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  emptyQualityStatus,
  localizeBaselineUrl,
  normalizeQualityStatus,
  resolveBaselineUrl,
  safeHttpUrl
} from '../.vitepress/quality-status-source.js'

test('normalizeQualityStatus accepts the public status payload shape', () => {
  const status = normalizeQualityStatus({
    generatedAt: '2026-09-14T17:30:00+08:00',
    date: '2026-09-14',
    commit: 'ba77166b93fa995837a4bca0ae33b51d69c0494c',
    status: 'passed',
    e2e: {
      status: 'passed',
      tests: { total: 2, passed: 2 },
      modules: [{ module: 'lifecycle', total: 2, passed: 2 }, null]
    },
    performance: {
      status: 'passed',
      baseline: {
        name: 'v0.3.0 baseline',
        url: 'https://cubesandbox.com/blog/posts/2026-06-01-cubesandbox-perf-benchmark'
      },
      counts: { similar: 1 },
      metrics: [{ label: '3.2 Sandbox Create Latency', verdict: 'similar' }, null]
    }
  })

  assert.equal(status.date, '2026-09-14')
  assert.equal(status.e2e.tests.total, 2)
  assert.equal(status.e2e.modules.length, 1)
  assert.equal(status.e2e.modules[0].module, 'lifecycle')
  assert.equal(status.performance.metrics.length, 1)
  assert.equal(
    status.performance.baseline.url,
    'https://cubesandbox.com/blog/posts/2026-06-01-cubesandbox-perf-benchmark'
  )
  assert.equal(status.performance.metrics[0].label, '3.2 Sandbox Create Latency')
})

test('normalizeQualityStatus accepts nightly job aliases', () => {
  const status = normalizeQualityStatus({
    generated_at: '2026-09-14T17:30:00+08:00',
    last_run: '2026-09-14',
    git_commit: 'abc123',
    overall: 'warning',
    jobs: {
      full_e2e: {
        type: 'e2e',
        tests: { total: 1, failed: 1 },
        module_distribution: [{ name: 'filesystem', failed: 1 }]
      },
      perf: {
        type: 'performance',
        comparison: {
          baseline: { key: 'main' },
          counts: { warning: 1 },
          metrics: [{ label: '4.2 Network RTT', verdict: 'warning' }]
        }
      }
    }
  })

  assert.equal(status.generatedAt, '2026-09-14T17:30:00+08:00')
  assert.equal(status.date, '2026-09-14')
  assert.equal(status.commit, 'abc123')
  assert.equal(status.status, 'warning')
  assert.equal(status.e2e.modules[0].name, 'filesystem')
  assert.equal(status.performance.counts.warning, 1)
})

test('normalizeQualityStatus ignores empty top-level placeholders', () => {
  const status = normalizeQualityStatus({
    date: '2026-09-14',
    e2e: {},
    performance: {},
    jobs: {
      full_e2e: { tests: { total: 3, passed: 3 } },
      perf: {
        comparison: {
          metrics: [{ label: '4.1 File Write Throughput', verdict: 'similar' }]
        }
      }
    }
  })

  assert.equal(status.e2e.tests.total, 3)
  assert.equal(status.performance.metrics[0].label, '4.1 File Write Throughput')
})

test('normalizeQualityStatus rejects invalid payloads and unsafe baseline URLs', () => {
  assert.equal(normalizeQualityStatus(null), null)
  assert.equal(normalizeQualityStatus('bad'), null)
  assert.equal(normalizeQualityStatus({ Code: 'AccessDenied' }), null)
  assert.equal(safeHttpUrl('javascript:alert(1)'), '')

  const status = normalizeQualityStatus({
    performance: {
      baseline: { url: 'javascript:alert(1)' }
    }
  })

  assert.equal(status.performance.baseline.url, '')
  assert.equal(status.performance.baseline.url_zh, '')
  assert.equal(status.performance.baseline.url_en, '')
})

test('resolveBaselineUrl prefers locale-specific URLs and rewrites known sites', () => {
  const enUrl = 'https://cubesandbox.com/blog/posts/2026-06-01-cubesandbox-perf-benchmark'
  const zhUrl = 'https://cubesandbox.com/zh/blog/posts/2026-06-01-cubesandbox-perf-benchmark'
  const githubEn =
    'https://github.com/TencentCloud/CubeSandbox/blob/master/docs/blog/posts/2026-06-01-cubesandbox-perf-benchmark.md'
  const githubZh =
    'https://github.com/TencentCloud/CubeSandbox/blob/master/docs/zh/blog/posts/2026-06-01-cubesandbox-perf-benchmark.md'

  assert.equal(resolveBaselineUrl({ url: enUrl, url_zh: zhUrl }, 'en'), enUrl)
  assert.equal(resolveBaselineUrl({ url: enUrl, url_zh: zhUrl }, 'zh'), zhUrl)
  assert.equal(resolveBaselineUrl({ url: enUrl }, 'zh'), zhUrl)
  assert.equal(resolveBaselineUrl({ url: zhUrl }, 'en'), enUrl)
  assert.equal(resolveBaselineUrl({ url_zh: zhUrl }, 'en'), enUrl)
  assert.equal(resolveBaselineUrl({ url_en: enUrl }, 'zh'), zhUrl)

  assert.equal(localizeBaselineUrl(githubZh, 'en'), githubEn)
  assert.equal(localizeBaselineUrl(githubEn, 'zh'), githubZh)
  assert.equal(localizeBaselineUrl(enUrl, 'zh'), zhUrl)
  assert.equal(localizeBaselineUrl(zhUrl, 'en'), enUrl)
  assert.equal(localizeBaselineUrl('https://cubesandbox.com/zh', 'en'), 'https://cubesandbox.com/')
  assert.equal(localizeBaselineUrl('https://cubesandbox.com/zh', 'zh'), 'https://cubesandbox.com/zh')
})

test('emptyQualityStatus returns the unknown status skeleton', () => {
  assert.deepEqual(emptyQualityStatus(), {
    generatedAt: null,
    date: '',
    commit: '',
    status: 'unknown',
    e2e: {},
    performance: {}
  })
})
