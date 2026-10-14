<script setup>
import { computed, onMounted, ref } from 'vue'
import {
  emptyQualityStatus,
  normalizeQualityStatus,
  resolveBaselineUrl
} from '../quality-status-source.js'

const props = defineProps({
  data: {
    type: Object,
    required: true
  },
  locale: {
    type: String,
    default: 'en'
  },
  source: {
    type: String,
    default: ''
  }
})

const runtimeUrl =
  props.source ||
  props.data?.sourceUrl ||
  ''

const status = ref(props.data?.payload || emptyQualityStatus())
const loading = ref(Boolean(runtimeUrl) && props.data?.live !== false)
const loadFailed = ref(false)

const isZh = computed(() => props.locale.startsWith('zh'))
const words = computed(() =>
  isZh.value
    ? {
        title: '每日性能测试报告',
        subtitle: 'CubeSandbox 每日 E2E 回归与核心性能基准报告。',
        latest: '最近运行',
        commit: '提交',
        generated: '数据快照',
        loading: '正在加载最新状态…',
        loadFailedWithData: '无法刷新动态状态，当前显示已有数据。',
        loadFailedEmpty: '状态数据暂时不可用，请稍后重试。',
        e2e: '端到端回归',
        perf: '核心性能基准',
        modules: '模块分布',
        module: '模块',
        metric: '指标',
        operation: '操作',
        nightly: '本次运行',
        baseline: '公开基准',
        baselineLink: '基准',
        delta: '变化',
        verdict: '结论',
        total: '总数',
        passed: '通过',
        failed: '失败',
        errors: '错误',
        skipped: '跳过',
        critical: '严重',
        warning: '警告',
        similar: '持平',
        improved: '提升'
      }
    : {
        title: 'Daily Performance Report',
        subtitle: 'Daily CubeSandbox E2E regression and core performance benchmark report.',
        latest: 'Last run',
        commit: 'Commit',
        generated: 'Snapshot',
        loading: 'Loading latest status…',
        loadFailedWithData: 'Could not refresh dynamic status. Showing existing data.',
        loadFailedEmpty: 'Status data is temporarily unavailable. Please try again later.',
        e2e: 'End-to-End Regression',
        perf: 'Core Operations Performance',
        modules: 'Module Distribution',
        module: 'Module',
        metric: 'Metric',
        operation: 'Operation',
        nightly: 'Nightly',
        baseline: 'Published baseline',
        baselineLink: 'Baseline',
        delta: 'Delta',
        verdict: 'Verdict',
        total: 'Total',
        passed: 'Passed',
        failed: 'Failed',
        errors: 'Errors',
        skipped: 'Skipped',
        critical: 'Critical',
        warning: 'Warning',
        similar: 'Similar',
        improved: 'Improved'
      }
)

const tests = computed(() => status.value.e2e?.tests || {})
const modules = computed(() => Array.isArray(status.value.e2e?.modules) ? status.value.e2e.modules : [])
const perf = computed(() => status.value.performance || {})
const metrics = computed(() => Array.isArray(perf.value.metrics) ? perf.value.metrics : [])
const counts = computed(() => perf.value.counts || {})
const baselineUrl = computed(() => resolveBaselineUrl(perf.value.baseline, props.locale))
const shouldFetchLive = computed(() => Boolean(runtimeUrl) && props.data?.live !== false)
const hasOverallStatus = computed(() => status.value.status && status.value.status !== 'unknown')
const hasE2EData = computed(() =>
  Boolean(status.value.e2e?.status || Object.keys(tests.value).length || modules.value.length)
)
const hasPerformanceData = computed(() =>
  Boolean(
    perf.value.status ||
      Object.keys(counts.value).length ||
      metrics.value.length ||
      baselineUrl.value
  )
)
const hasData = computed(() =>
  Boolean(
    status.value.date ||
      status.value.commit ||
      hasOverallStatus.value ||
      hasE2EData.value ||
      hasPerformanceData.value
  )
)

function shortCommit(value) {
  return value ? String(value).slice(0, 12) : '—'
}

function text(value) {
  return value === undefined || value === null || value === '' ? '—' : String(value)
}

function dateTimeText(value) {
  if (!value) return '—'
  return String(value).replace('T', ' ').replace(/([+-]\d{2}:\d{2}|Z)$/, ' $1')
}

function metricValue(row, key) {
  const textKey = `${key}_text`
  if (row[textKey]) return row[textKey]
  if (key === 'current' && row.currentText) return row.currentText
  if (key === 'baseline' && row.baselineText) return row.baselineText
  return text(row[key])
}

function deltaText(row) {
  if (row.deltaText) return row.deltaText
  if (row.delta_pct === undefined || row.delta_pct === null) return '—'
  const pct = Number(row.delta_pct)
  return Number.isFinite(pct) ? `${pct.toFixed(1)}%` : '—'
}

function tone(value) {
  const current = String(value || '').toLowerCase()
  if (['success', 'passed', 'improved'].includes(current)) return 'ok'
  if (['failed', 'error', 'critical'].includes(current)) return 'err'
  if (['warning', 'warn', 'running'].includes(current)) return 'warn'
  return 'mute'
}

async function fetchLatest() {
  loading.value = true
  loadFailed.value = false
  try {
    const response = await fetch(runtimeUrl, {
      headers: { Accept: 'application/json' },
      signal: AbortSignal.timeout(15000)
    })
    if (!response.ok) throw new Error(`HTTP ${response.status}`)
    const normalized = normalizeQualityStatus(await response.json())
    if (!normalized) throw new Error('unexpected payload')
    status.value = normalized
  } catch (error) {
    console.warn('[quality-status] live fetch failed:', error)
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  if (!shouldFetchLive.value) {
    loading.value = false
    return
  }
  fetchLatest()
})
</script>

<template>
  <div class="qs-page">
    <section class="qs-hero">
      <div>
        <p class="qs-kicker">CubeSandbox</p>
        <h1>{{ words.title }}</h1>
        <p>{{ words.subtitle }}</p>
      </div>
      <span v-if="hasOverallStatus" class="qs-badge" :class="`qs-${tone(status.status)}`">{{ text(status.status) }}</span>
    </section>

    <p v-if="loading" class="qs-note">{{ words.loading }}</p>
    <p v-else-if="loadFailed" class="qs-note qs-warn-text">
      {{ hasData ? words.loadFailedWithData : words.loadFailedEmpty }}
    </p>

    <div v-if="hasData" class="qs-stats">
      <div class="qs-card">
        <span>{{ words.latest }}</span>
        <strong>{{ text(status.date) }}</strong>
      </div>
      <div class="qs-card">
        <span>{{ words.commit }}</span>
        <strong><code>{{ shortCommit(status.commit) }}</code></strong>
      </div>
      <div class="qs-card">
        <span>{{ words.generated }}</span>
        <strong>{{ dateTimeText(status.generatedAt) }}</strong>
      </div>
    </div>

    <section v-if="hasE2EData" class="qs-section">
      <h2>
        {{ words.e2e }}
        <span class="qs-badge" :class="`qs-${tone(status.e2e?.status)}`">{{ text(status.e2e?.status) }}</span>
      </h2>
      <div class="qs-stats">
        <div v-for="key in ['total', 'passed', 'failed', 'errors', 'skipped']" :key="key" class="qs-card">
          <span>{{ words[key] }}</span>
          <strong>{{ text(tests[key]) }}</strong>
        </div>
      </div>
    </section>

    <section v-if="modules.length" class="qs-section">
      <h2>{{ words.modules }}</h2>
      <div class="qs-table-wrap">
        <table>
          <thead>
            <tr>
              <th>{{ words.module }}</th>
              <th>{{ words.total }}</th>
              <th>{{ words.passed }}</th>
              <th>{{ words.failed }}</th>
              <th>{{ words.errors }}</th>
              <th>{{ words.skipped }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, index) in modules" :key="row.module || row.name || index">
              <td><code>{{ row.module || row.name }}</code></td>
              <td>{{ text(row.total) }}</td>
              <td>{{ text(row.passed) }}</td>
              <td>{{ text(row.failed) }}</td>
              <td>{{ text(row.errors) }}</td>
              <td>{{ text(row.skipped) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section v-if="hasPerformanceData" class="qs-section">
      <h2>
        {{ words.perf }}
        <span class="qs-badge" :class="`qs-${tone(perf.status)}`">{{ text(perf.status) }}</span>
      </h2>
      <div class="qs-stats">
        <div v-for="key in ['critical', 'warning', 'similar', 'improved']" :key="key" class="qs-card">
          <span>{{ words[key] }}</span>
          <strong>{{ text(counts[key]) }}</strong>
        </div>
      </div>
      <p v-if="baselineUrl" class="qs-note">
        {{ words.baselineLink }}:
        <a :href="baselineUrl" target="_blank" rel="noopener noreferrer">{{ perf.baseline.name || perf.baseline.key || baselineUrl }}</a>
      </p>
      <div v-if="metrics.length" class="qs-table-wrap">
        <table>
          <thead>
            <tr>
              <th>{{ words.operation }}</th>
              <th>{{ words.nightly }}</th>
              <th>{{ words.baseline }}</th>
              <th>{{ words.delta }}</th>
              <th>{{ words.verdict }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, index) in metrics" :key="row.label || index">
              <td>{{ row.label }}</td>
              <td>{{ metricValue(row, 'current') }}</td>
              <td>{{ metricValue(row, 'baseline') }}</td>
              <td>{{ deltaText(row) }}</td>
              <td><span class="qs-badge" :class="`qs-${tone(row.verdict)}`">{{ text(row.verdict) }}</span></td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-if="perf.note" class="qs-note">{{ perf.note }}</p>
    </section>
  </div>
</template>

<style scoped>
.qs-page {
  display: grid;
  gap: 24px;
}
.qs-hero {
  display: flex;
  justify-content: space-between;
  gap: 20px;
  align-items: flex-start;
  padding: 24px;
  border: 1px solid var(--vp-c-divider);
  border-radius: 18px;
  background: var(--vp-c-bg-soft);
}
.qs-kicker {
  margin: 0 0 8px;
  color: var(--vp-c-brand-1);
  font-size: 12px;
  font-weight: 700;
  letter-spacing: .12em;
  text-transform: uppercase;
}
.qs-hero h1 {
  margin: 0;
}
.qs-hero p {
  margin: 8px 0 0;
  color: var(--vp-c-text-2);
}
.qs-section {
  display: grid;
  gap: 14px;
}
.qs-stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 12px;
}
.qs-card {
  padding: 14px;
  border: 1px solid var(--vp-c-divider);
  border-radius: 14px;
  background: var(--vp-c-bg-soft);
}
.qs-card span {
  display: block;
  color: var(--vp-c-text-2);
  font-size: 12px;
  text-transform: uppercase;
}
.qs-card strong {
  display: block;
  margin-top: 6px;
  font-size: 18px;
}
.qs-table-wrap {
  overflow-x: auto;
  border: 1px solid var(--vp-c-divider);
  border-radius: 14px;
}
.qs-table-wrap table {
  display: table;
  width: 100%;
  margin: 0;
}
.qs-note {
  margin: 0;
  color: var(--vp-c-text-2);
}
.qs-warn-text {
  color: var(--vp-c-warning-1);
}
.qs-badge {
  display: inline-flex;
  align-items: center;
  border-radius: 999px;
  padding: 3px 10px;
  border: 1px solid var(--vp-c-divider);
  font-size: 12px;
  font-weight: 700;
}
.qs-ok {
  color: var(--vp-c-green-1);
  border-color: var(--vp-c-green-2);
}
.qs-err {
  color: var(--vp-c-danger-1);
  border-color: var(--vp-c-danger-2);
}
.qs-warn {
  color: var(--vp-c-warning-1);
  border-color: var(--vp-c-warning-2);
}
.qs-mute {
  color: var(--vp-c-text-2);
}
</style>
