export const DEFAULT_QUALITY_STATUS_JSON_URL =
  'https://cubesandbox-1253970226.cos.ap-singapore.myqcloud.com/page-data/quality-status.json'

export function safeHttpUrl(value) {
  if (!value) return ''
  try {
    const url = new URL(String(value))
    return ['http:', 'https:'].includes(url.protocol) ? url.href : ''
  } catch {
    return ''
  }
}

export function emptyQualityStatus() {
  return {
    generatedAt: null,
    date: '',
    commit: '',
    status: 'unknown',
    e2e: {},
    performance: {}
  }
}

function nonEmptyObject(value) {
  return Boolean(value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length)
}

function firstObject(candidates) {
  return candidates.find(nonEmptyObject) || {}
}

export function normalizeQualityStatus(payload) {
  if (!payload || typeof payload !== 'object') return null
  const hasRecognizedField = Boolean(
    payload.generatedAt ||
      payload.generated_at ||
      payload.date ||
      payload.lastRun ||
      payload.last_run ||
      payload.commit ||
      payload.git_commit ||
      payload.source?.sha ||
      payload.status ||
      payload.overall ||
      payload.e2e ||
      payload.performance ||
      payload.perf ||
      payload.jobs ||
      payload.modules
  )
  if (!hasRecognizedField) return null
  const jobs = payload.jobs && typeof payload.jobs === 'object' ? payload.jobs : {}
  const e2e = firstObject([
    payload.e2e,
    jobs.full_e2e,
    Object.values(jobs).find((job) => job?.type === 'e2e')
  ])
  const performance = firstObject([
    payload.performance,
    payload.perf,
    jobs.perf,
    Object.values(jobs).find((job) => job?.type === 'performance')
  ])
  const comparison = performance.comparison && typeof performance.comparison === 'object'
    ? performance.comparison
    : performance
  const baseline = performance.baseline || comparison.baseline || {}
  const modules = e2e.modules || e2e.module_distribution || payload.modules || []
  const metrics = comparison.metrics || performance.metrics || []
  return {
    generatedAt: payload.generatedAt || payload.generated_at || null,
    date: payload.date || payload.lastRun || payload.last_run || '',
    commit: payload.commit || payload.git_commit || payload.source?.sha || '',
    status: payload.status || payload.overall || 'unknown',
    e2e: {
      status: e2e.status || '',
      tests: e2e.tests || {},
      modules: Array.isArray(modules) ? modules.filter((row) => row && typeof row === 'object') : []
    },
    performance: {
      status: performance.status || '',
      baseline: {
        ...baseline,
        url: safeHttpUrl(baseline.url)
      },
      counts: comparison.counts || performance.counts || {},
      note: comparison.note || performance.note || '',
      metrics: Array.isArray(metrics) ? metrics.filter((row) => row && typeof row === 'object') : []
    }
  }
}
