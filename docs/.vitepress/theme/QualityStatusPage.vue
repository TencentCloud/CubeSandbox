<script setup>
import { computed, defineComponent, h, onBeforeUnmount, onMounted, ref } from 'vue'
import './quality-status.css'
import {
  DEFAULT_QUALITY_STATUS_JSON_URL,
  emptyQualityStatus,
  historyUrlFromLatest,
  normalizeQualityStatus,
  resolveBaselineUrl,
  sortNightsByDate
} from '../quality-status-source.js'

const props = defineProps({
  data: { type: Object, required: true },
  locale: { type: String, default: 'en' }
})

const COPY = {
  en: {
    kicker: 'CubeSandbox · Nightly status',
    titleA: 'Nightly ',
    titleEm: 'quality',
    titleB: ' status for master',
    lead: 'Each nightly run builds CubeSandbox from master, replays the SDK end-to-end suite and benchmarks core operations against the published baseline.',
    run: 'Run',
    commit: 'Commit',
    snapshot: 'Snapshot',
    e2e: 'E2E',
    perf: 'Perf',
    wall: (n) => `Last ${n} nights`,
    legend: { ok: 'E2E passed', fail: 'E2E failed', none: 'No run', warn: 'Perf warning', crit: 'Perf critical' },
    state: { ok: 'Passed', fail: 'Failed', none: 'No run', incomplete: 'Incomplete', warn: 'Warning', crit: 'Critical', perfOk: 'Within baseline' },
    noTests: 'no tests ran',
    counts: { passed: 'Passed', skipped: 'Skipped', failed: 'Failed', errors: 'Errors', total: 'Total' },
    fig: {
      e2e: 'E2E',
      e2eSub: (total) => `of ${total} passed`,
      e2eNote: (skipped, failed) => `${skipped} skipped · ${failed} failed`,
      perf: 'Perf',
      perfSub: 'critical vs baseline',
      perfNote: (warning, improved) => `${warning} warning · ${improved} improved`,
      streak: 'Streak',
      streakSub: 'nights without E2E failure',
      streakNote: (date) => (date ? `since ${date}` : 'no completed run yet'),
      window: 'Window',
      windowSub: 'nights E2E passed',
      windowNote: (count) => `${count} without a run`
    },
    better: (direction) => (direction === 'higher' ? 'higher is better' : 'lower is better'),
    e2eTitle: 'End-to-end regression',
    e2eLead: 'One bar per night and module. The outlined bar on the right is the latest run; faded bars are modules that were skipped entirely.',
    modCols: ['Module', 'Nights', 'Pass rate', 'Latest'],
    perfTitle: 'Core performance',
    perfLead: (n, band) => `Last ${n} nights per metric. The dashed line is the published baseline and the shaded band is ±${band}%, the range counted as similar.`,
    perfCols: ['Metric', 'Trend', 'Nightly', 'Baseline', 'Delta', 'Verdict'],
    filters: { all: 'All', critical: 'Critical', warning: 'Warning', improved: 'Improved', similar: 'Similar' },
    verdict: { improved: 'Improved', similar: 'Similar', warning: 'Warning', critical: 'Critical', missing: 'Missing' },
    metricsIn: (n, c) => `${n} metrics${c ? ` · ${c} critical` : ''}`,
    methodTitle: 'How to read this',
    methodLead: 'Three rules keep this page honest and safe to publish.',
    steps: [
      ['01', 'One night, one block', 'A block is one scheduled run on one master commit. Gaps mean no run happened, not a pass.'],
      ['02', 'Baseline, not yesterday', 'Performance is compared with the published benchmark, so slow drift cannot hide behind small nightly steps.'],
      ['03', 'Aggregates only', 'Totals, verdicts and timings. No hosts, environment ids, logs or private consoles.']
    ],
    baseline: 'Baseline',
    rawLatest: 'Latest JSON',
    rawHistory: 'History JSON',
    loading: 'Loading latest status…',
    empty: 'Status data is temporarily unavailable. Please try again later.',
    loadFailedWithData: 'Could not refresh dynamic status. Showing existing data.',
    noHistory: 'History is unavailable, so only the latest night is shown.',
    footer: 'Aggregates only · no hosts, environment ids, or logs'
  },
  zh: {
    kicker: 'CubeSandbox · 每日构建',
    titleA: 'master 的每日',
    titleEm: '质量',
    titleB: '状态',
    lead: '每晚从 master 构建 CubeSandbox，回放 SDK 端到端用例，并对照公开基准测试核心操作。',
    run: '运行',
    commit: '提交',
    snapshot: '快照',
    e2e: '端到端',
    perf: '性能',
    wall: (n) => `最近 ${n} 晚`,
    legend: { ok: '端到端通过', fail: '端到端失败', none: '未运行', warn: '性能警告', crit: '性能严重' },
    state: { ok: '通过', fail: '失败', none: '未运行', incomplete: '未完成', warn: '警告', crit: '严重', perfOk: '在基准范围内' },
    noTests: '没有用例运行',
    counts: { passed: '通过', skipped: '跳过', failed: '失败', errors: '错误', total: '总计' },
    fig: {
      e2e: '端到端',
      e2eSub: (total) => `/ ${total} 通过`,
      e2eNote: (skipped, failed) => `跳过 ${skipped} · 失败 ${failed}`,
      perf: '性能',
      perfSub: '项严重回退',
      perfNote: (warning, improved) => `警告 ${warning} · 提升 ${improved}`,
      streak: '连续',
      streakSub: '晚端到端无失败',
      streakNote: (date) => (date ? `自 ${date} 起` : '还没有完整运行'),
      window: '窗口',
      windowSub: '晚端到端通过',
      windowNote: (count) => `${count} 晚未运行`
    },
    better: (direction) => (direction === 'higher' ? '越高越好' : '越低越好'),
    e2eTitle: '端到端回归',
    e2eLead: '每个模块每晚一根竖条。右侧带框的是最近一次运行；浅色表示该模块整体跳过。',
    modCols: ['模块', '每晚', '通过率', '最近'],
    perfTitle: '核心性能',
    perfLead: (n, band) => `每项指标最近 ${n} 晚的走势。虚线是公开基准，阴影带是 ±${band}%，即「持平」区间。`,
    perfCols: ['指标', '走势', '本次', '基准', '变化', '结论'],
    filters: { all: '全部', critical: '严重', warning: '警告', improved: '提升', similar: '持平' },
    verdict: { improved: '提升', similar: '持平', warning: '警告', critical: '严重', missing: '缺失' },
    metricsIn: (n, c) => `${n} 项${c ? ` · 严重 ${c}` : ''}`,
    methodTitle: '如何阅读',
    methodLead: '三条规则，保证这一页可信、可以公开。',
    steps: [
      ['01', '一晚一块', '每个方块对应一次定时运行、一个 master 提交。空缺表示当晚没跑，不代表通过。'],
      ['02', '对照基准，而非昨天', '性能和公开基准比较，缓慢的退化不会藏在每晚的小幅波动里。'],
      ['03', '只放汇总', '只有总数、结论和耗时。不出现主机、环境编号、日志或内部控制台。']
    ],
    baseline: '基准',
    rawLatest: '最新 JSON',
    rawHistory: '历史 JSON',
    loading: '正在加载最新状态…',
    empty: '状态数据暂时不可用，请稍后重试。',
    loadFailedWithData: '无法刷新动态状态，当前显示已有数据。',
    noHistory: '缺少历史数据，只显示最近一晚。',
    footer: '只公开汇总 · 不含主机、环境编号或日志'
  }
}

const PALETTE = {
  ok: ['var(--qs-ok-1)', 'var(--qs-ok-2)', 'var(--qs-ok-3)'],
  warn: ['var(--qs-wa-1)', 'var(--qs-wa-2)', 'var(--qs-wa-3)'],
  fail: ['var(--qs-er-1)', 'var(--qs-er-2)', 'var(--qs-er-3)'],
  none: ['var(--qs-line-strong)', 'var(--qs-line)', 'var(--qs-line)']
}

const TREND_NIGHTS = 14
const FILTERS = ['all', 'critical', 'warning', 'improved', 'similar']

function num(value) {
  const parsed = Number(value)
  return Number.isFinite(parsed) ? parsed : 0
}

function e2eTone(status) {
  const value = String(status || '').toLowerCase()
  if (value === 'passed' || value === 'success') return 'ok'
  if (value === 'failed' || value === 'error') return 'fail'
  return 'none'
}

function perfTone(status) {
  const value = String(status || '').toLowerCase()
  if (value === 'critical' || value === 'failed' || value === 'error') return 'fail'
  if (value === 'warning' || value === 'warn') return 'warn'
  if (value === 'passed' || value === 'success') return 'ok'
  return 'none'
}

function moduleTone(counts) {
  if (!counts) return 'none'
  if (num(counts.failed) + num(counts.errors) > 0) return 'fail'
  if (num(counts.passed) === 0 && num(counts.skipped) > 0) return 'skip'
  return 'ok'
}

function shortCommit(value) {
  return value ? String(value).slice(0, 12) : '—'
}

function stamp(value) {
  if (!value) return '—'
  return String(value).replace('T', ' ').replace(/:\d{2}([+-]\d{2}:\d{2}|Z)$/, ' $1')
}

function points(pts, scale) {
  return pts.map(([x, y]) => `${x * scale},${y * scale}`).join(' ')
}

const Cube = defineComponent({
  props: {
    left: { type: String, default: 'none' },
    right: { type: String, default: '' },
    tags: { type: Array, default: null }
  },
  setup(props) {
    return () => {
      if (props.left === 'none' && !props.right) {
        return h('svg', { viewBox: '0 0 20 22', 'aria-hidden': 'true' }, [
          h('path', {
            d: 'M10 1 19 6v10l-9 5-9-5V6z M1 6l9 5 9-5 M10 11v10',
            fill: 'none',
            stroke: 'var(--qs-line-strong)',
            'stroke-width': '1',
            'stroke-dasharray': '2 1.5'
          })
        ])
      }
      const scale = props.tags ? 8 : 1
      const left = PALETTE[props.left] || PALETTE.none
      const right = PALETTE[props.right || props.left] || left
      const nodes = [
        h('polygon', { points: points([[10, 1], [19, 6], [10, 11], [1, 6]], scale), fill: left[0] }),
        h('polygon', { points: points([[1, 6], [10, 11], [10, 21], [1, 16]], scale), fill: left[1] }),
        h('polygon', { points: points([[19, 6], [10, 11], [10, 21], [19, 16]], scale), fill: right[2] })
      ]
      if (props.tags) {
        nodes.push(
          h('text', { class: 'qs-face-tag', x: '42', y: '96', transform: 'rotate(30 42 96)', 'text-anchor': 'middle' }, props.tags[0]),
          h('text', { class: 'qs-face-tag', x: '118', y: '96', transform: 'rotate(-30 118 96)', 'text-anchor': 'middle' }, props.tags[1])
        )
      }
      return h('svg', { viewBox: props.tags ? '0 0 160 176' : '0 0 20 22', 'aria-hidden': 'true' }, nodes)
    }
  }
})

const Sparkline = defineComponent({
  props: {
    values: { type: Array, required: true },
    baseline: { type: Number, default: null },
    band: { type: Number, default: 5 },
    verdict: { type: String, default: '' }
  },
  setup(props) {
    return () => {
      const values = props.values
      const present = values.filter((value) => value != null)
      if (values.length < 2 || present.length < 2) return h('span', { class: 'qs-spark-empty' }, '—')
      const width = 240
      const height = 32
      const pad = 3
      const base = props.baseline
      const pool = base != null ? [...present, base * (1 - props.band / 100 - 0.01), base * (1 + props.band / 100 + 0.01)] : present
      const lo = Math.min(...pool)
      const hi = Math.max(...pool)
      const span = hi - lo || 1
      const y = (value) => pad + (height - pad * 2) * (1 - (value - lo) / span)
      const x = (index) => (index / (values.length - 1)) * width
      const segments = []
      values.forEach((value, index) => {
        if (value == null) {
          if (segments.length && segments[segments.length - 1].length) segments.push([])
          return
        }
        if (!segments.length) segments.push([])
        segments[segments.length - 1].push({ x: x(index), y: y(value) })
      })
      const drawn = segments
        .filter((segment) => segment.length)
        .map((segment) => (segment.length === 1 ? [{ x: segment[0].x - 2, y: segment[0].y }, { x: segment[0].x + 2, y: segment[0].y }] : segment))
      const fmt = (segment) => segment.map((point) => `${point.x.toFixed(1)},${point.y.toFixed(1)}`).join(' ')
      const cls = props.verdict === 'critical' ? ' bad' : props.verdict === 'warning' ? ' warn' : ''
      const last = values[values.length - 1]
      const svg = [
        base != null
          ? h('g', [
              h('rect', {
                class: 'band',
                x: '0',
                y: y(base * (1 + props.band / 100)),
                width,
                height: y(base * (1 - props.band / 100)) - y(base * (1 + props.band / 100))
              }),
              h('line', { class: 'base', x1: '0', x2: width, y1: y(base), y2: y(base), 'vector-effect': 'non-scaling-stroke' })
            ])
          : null,
        ...drawn.map((segment, index) =>
          h('g', { key: index }, [
            h('polygon', { class: `area${cls}`, points: `${segment[0].x.toFixed(1)},${height} ${fmt(segment)} ${segment[segment.length - 1].x.toFixed(1)},${height}` }),
            h('polyline', { class: `line${cls}`, points: fmt(segment), 'vector-effect': 'non-scaling-stroke' })
          ])
        )
      ]
      return h('div', { class: 'qs-spark-wrap' }, [
        h('svg', { class: 'qs-spark', viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none', 'aria-hidden': 'true' }, svg),
        last != null ? h('span', { class: `qs-spark-dot${cls}`, style: { top: `${((y(last) / height) * 100).toFixed(1)}%` } }) : null
      ])
    }
  }
})

const isZh = computed(() => props.locale.startsWith('zh'))
const t = computed(() => (isZh.value ? COPY.zh : COPY.en))
const latestUrl = DEFAULT_QUALITY_STATUS_JSON_URL
const historyUrl = historyUrlFromLatest(latestUrl)
const publicLatestUrl = latestUrl
const publicHistoryUrl = historyUrl

const status = ref(props.data?.payload || emptyQualityStatus())
const nights = ref([])
const loading = ref(true)
const loadFailed = ref(false)
const filter = ref('all')
const openDate = ref('')

const tests = computed(() => status.value.e2e?.tests || {})
const perf = computed(() => status.value.performance || {})
const counts = computed(() => perf.value.counts || {})
const metrics = computed(() => (Array.isArray(perf.value.metrics) ? perf.value.metrics : []))
const band = computed(() => num(perf.value.thresholds?.similar_pct) || 5)
const warnPct = computed(() => num(perf.value.thresholds?.warning_pct) || band.value)
const baselineUrl = computed(() => resolveBaselineUrl(perf.value.baseline, props.locale))
const hasData = computed(() => Boolean(status.value.date || status.value.commit || metrics.value.length || tests.value.total))

const wallNights = computed(() => {
  if (nights.value.length) return nights.value
  if (!status.value.date) return []
  return [{
    date: status.value.date,
    status: status.value.status || 'unknown',
    commit: status.value.commit,
    e2e: status.value.e2e ? { status: status.value.e2e.status, tests: status.value.e2e.tests } : undefined
  }]
})
const trendNights = computed(() => nights.value.slice(-TREND_NIGHTS))
const latestE2E = computed(() => e2eTone(status.value.e2e?.status))
const latestPerf = computed(() => perfTone(perf.value.status))
const overallTone = computed(() => {
  if (status.value.status === 'failed') return 'fail'
  if (status.value.status === 'warning') return 'warn'
  if (status.value.status === 'passed') return 'ok'
  return 'none'
})

const streak = computed(() => {
  let count = 0
  let since = ''
  for (let index = wallNights.value.length - 1; index >= 0; index -= 1) {
    const night = wallNights.value[index]
    const tone = night.status === 'no_run' ? 'none' : e2eTone(night.e2e?.status)
    if (tone === 'fail') break
    if (tone === 'ok') {
      count += 1
      since = night.date
    }
  }
  return { count, since }
})
const passedNights = computed(() => wallNights.value.filter((night) => night.status !== 'no_run' && e2eTone(night.e2e?.status) === 'ok').length)
const noRunNights = computed(() => wallNights.value.filter((night) => night.status === 'no_run').length)

const moduleRows = computed(() => {
  const names = []
  const seen = new Set()
  for (const row of status.value.e2e?.modules || []) {
    if (row?.module && !seen.has(row.module)) {
      seen.add(row.module)
      names.push(row.module)
    }
  }
  for (const night of nights.value) {
    for (const name of Object.keys(night.e2e?.modules || {})) {
      if (!seen.has(name)) {
        seen.add(name)
        names.push(name)
      }
    }
  }
  const latestModules = new Map((status.value.e2e?.modules || []).map((row) => [row.module, row]))
  return names.map((name) => {
    const row = latestModules.get(name)
    const history = wallNights.value.map((night) => {
      if (night.status === 'no_run') return 'none'
      if (nights.value.length) return moduleTone(night.e2e?.modules?.[name])
      return moduleTone(row)
    })
    const ran = history.filter((tone) => tone !== 'none')
    const good = ran.filter((tone) => tone === 'ok' || tone === 'skip').length
    return {
      name,
      row,
      history,
      rate: ran.length ? `${((good / ran.length) * 100).toFixed(1)}%` : '—',
      strong: Boolean(ran.length && good === ran.length),
      latestTone: moduleTone(row)
    }
  })
})

const visibleMetrics = computed(() =>
  metrics.value.filter((metric) => filter.value === 'all' || String(metric.verdict || '').toLowerCase() === filter.value)
)
const metricGroups = computed(() => {
  const titles = new Map((perf.value.sections || []).map((section) => [section.key, section.title]))
  const groups = new Map()
  for (const metric of visibleMetrics.value) {
    const key = metric.section || String(metric.label || '').match(/^\s*(\d+\.\d+)/)?.[1] || '—'
    if (!groups.has(key)) groups.set(key, { key, title: titles.get(key) || key, rows: [] })
    groups.get(key).rows.push(metric)
  }
  return [...groups.values()]
})

function metricName(metric) {
  return String(metric.label || '').replace(/^\s*\d+\.\d+\s*/, '')
}

function seriesFor(metric) {
  const key = metric.key || ''
  const series = trendNights.value.map((night) => {
    const value = night.performance?.values?.[key]
    return typeof value === 'number' && Number.isFinite(value) ? value : null
  })
  if (!series.length && typeof metric.current === 'number') series.push(metric.current)
  return series
}

function deltaClass(metric) {
  const delta = metric.delta_pct
  if (delta == null) return 'dim'
  if (delta <= -band.value) return 'down'
  if (delta >= warnPct.value) return String(metric.verdict || '').toLowerCase() === 'critical' ? 'up crit' : 'up'
  return 'dim'
}

function deltaText(metric) {
  const delta = metric.delta_pct
  if (delta == null || !Number.isFinite(Number(delta))) return '—'
  const sign = delta > 0 ? '+' : delta < 0 ? '−' : ''
  return `${sign}${Math.abs(delta).toFixed(1)}%`
}

function chipClass(verdict) {
  if (verdict === 'improved') return 'ok'
  if (verdict === 'critical') return 'bad'
  if (verdict === 'warning') return 'warn'
  return ''
}

function nightMarker(night) {
  if (!night.performance) return undefined
  const tone = perfTone(night.performance.status)
  if (tone === 'fail') return 'fail'
  if (tone === 'warn') return 'warn'
  return undefined
}

function nightState(night) {
  const words = t.value
  if (night.status === 'no_run') return words.state.none
  const tone = e2eTone(night.e2e?.status)
  if (tone === 'ok') return `${words.e2e} · ${words.state.ok}`
  if (tone === 'fail') {
    const failedModules = Object.entries(night.e2e?.modules || {})
      .filter(([, counts]) => num(counts.failed) + num(counts.errors) > 0)
      .map(([name]) => name)
    const total = num(night.e2e?.tests?.total)
    const detail = failedModules.length ? failedModules.join(', ') : total ? '' : words.noTests
    return `${words.e2e} · ${words.state.fail}${detail ? ` · ${detail}` : ''}`
  }
  return `${words.e2e} · ${words.state.incomplete}`
}

function nightPerfText(night) {
  const words = t.value
  if (!night.performance) return ''
  const tone = perfTone(night.performance.status)
  if (tone === 'none') return ''
  if (tone === 'ok') return words.state.perfOk
  const worst = night.performance.worst
  const prefix = tone === 'fail' ? words.state.crit : words.state.warn
  if (!worst?.label) return prefix
  const delta = num(worst.delta_pct)
  return `${prefix} · ${worst.label} ${delta > 0 ? '+' : ''}${delta.toFixed(1)}%`
}

function nightLabel(night) {
  const perfText = nightPerfText(night)
  const commit = night.commit ? `, ${t.value.commit} ${shortCommit(night.commit)}` : ''
  return `${night.date} ${nightState(night)}${perfText ? `, ${t.value.perf} ${perfText}` : ''}${commit}`
}

function toggleNight(date) {
  openDate.value = openDate.value === date ? '' : date
}

function closeNightOnOutside(event) {
  if (!event.target.closest('.qs-block')) openDate.value = ''
}

function e2eWord() {
  if (latestE2E.value === 'ok') return t.value.state.ok
  if (latestE2E.value === 'fail') return t.value.state.fail
  return t.value.state.incomplete
}

function perfWord() {
  if (latestPerf.value === 'fail') return t.value.state.crit
  if (latestPerf.value === 'warn') return t.value.state.warn
  if (latestPerf.value === 'ok') return t.value.state.ok
  return t.value.state.none
}

async function fetchJson(url) {
  const response = await fetch(url, { headers: { Accept: 'application/json' }, signal: AbortSignal.timeout(15000) })
  if (!response.ok) throw new Error(`HTTP ${response.status}`)
  return response.json()
}

async function loadStatus() {
  loading.value = true
  loadFailed.value = false
  try {
    const [latestResult, historyResult] = await Promise.allSettled([
      fetchJson(latestUrl),
      historyUrl ? fetchJson(historyUrl) : Promise.resolve(null)
    ])
    if (latestResult.status === 'fulfilled') {
      const normalized = normalizeQualityStatus(latestResult.value)
      if (!normalized) throw new Error('unexpected payload')
      status.value = normalized
    } else {
      throw latestResult.reason
    }
    if (historyResult.status === 'fulfilled' && Array.isArray(historyResult.value?.nights)) {
      nights.value = sortNightsByDate(historyResult.value.nights)
    }
  } catch (error) {
    console.warn('[quality-status] live fetch failed:', error)
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  loadStatus()
  document.addEventListener('click', closeNightOnOutside)
})
onBeforeUnmount(() => document.removeEventListener('click', closeNightOnOutside))
</script>

<template>
  <div class="qs-page" :data-lang="isZh ? 'zh' : 'en'" data-testid="quality-status">
    <div class="qs-wrap">
      <p v-if="loading" class="qs-empty">{{ t.loading }}</p>
      <p v-else-if="!hasData" class="qs-empty">{{ t.empty }}</p>
      <template v-else>
        <p v-if="loadFailed" class="qs-note">{{ t.loadFailedWithData }}</p>
        <section class="qs-hero">
          <div>
            <p class="qs-label qs-accent">{{ t.kicker }}</p>
            <h1>{{ t.titleA }}<em>{{ t.titleEm }}</em>{{ t.titleB }}</h1>
            <p class="qs-verdict">
              <span class="qs-beacon" :style="{ '--qs-tone': PALETTE[overallTone][1] }" />
              <span>
                {{ t.e2e }} <b class="qs-word" :class="latestE2E">{{ e2eWord() }}</b>
                · {{ t.perf }}
                <b class="qs-word" :class="latestPerf">{{ perfWord() }}<template v-if="num(counts.critical) > 0"> ({{ num(counts.critical) }})</template></b>
              </span>
            </p>
            <p class="qs-lead">{{ t.lead }}</p>
            <p class="qs-meta">
              <span>{{ t.run }} <b>{{ status.date || '—' }}</b></span>
              <span>{{ t.commit }} <b>{{ shortCommit(status.commit) }}</b></span>
              <span>{{ t.snapshot }} <b>{{ stamp(status.generatedAt) }}</b></span>
            </p>
          </div>
          <div class="qs-hero-cube">
            <Cube :left="latestE2E === 'none' ? 'none' : latestE2E" :right="latestPerf" :tags="['E2E', 'PERF']" />
            <div class="qs-shadow" />
          </div>
        </section>

        <section v-if="wallNights.length" class="qs-wall">
          <div class="qs-wall-head">
            <p class="qs-label">{{ t.wall(wallNights.length) }}</p>
            <div class="qs-legend">
              <span><Cube left="ok" />{{ t.legend.ok }}</span>
              <span><Cube left="fail" />{{ t.legend.fail }}</span>
              <span><Cube left="none" />{{ t.legend.none }}</span>
              <span><i class="qs-perf-mark warn" />{{ t.legend.warn }}</span>
              <span><i class="qs-perf-mark fail" />{{ t.legend.crit }}</span>
            </div>
          </div>
          <div class="qs-blocks" :style="{ '--qs-n': wallNights.length }">
            <div
              v-for="(night, index) in wallNights"
              :key="night.date"
              class="qs-block"
              :class="{ 'qs-last': index === wallNights.length - 1, 'is-open': openDate === night.date }"
              :data-perf="nightMarker(night)"
            >
              <button
                type="button"
                class="qs-cube-btn"
                :aria-expanded="openDate === night.date"
                :aria-label="nightLabel(night)"
                @click.stop="toggleNight(night.date)"
              >
                <Cube :left="night.status === 'no_run' ? 'none' : e2eTone(night.e2e?.status)" />
              </button>
              <div class="qs-tip" @click.stop>
                <div class="qs-tip-head">
                  <span>
                    <span class="qs-tip-date">{{ night.date }}</span>
                    <span class="qs-tip-state">{{ nightState(night) }}</span>
                  </span>
                  <code v-if="night.commit" class="qs-tip-commit">{{ shortCommit(night.commit) }}</code>
                </div>
                <span v-if="night.status !== 'no_run' && night.e2e?.tests" class="qs-tip-rows">
                  <span class="qs-tc ok"><i />{{ t.counts.passed }}<b>{{ num(night.e2e.tests.passed) }}</b></span>
                  <span class="qs-tc skip"><i />{{ t.counts.skipped }}<b>{{ num(night.e2e.tests.skipped) }}</b></span>
                  <span class="qs-tc fail"><i />{{ t.counts.failed }}<b :class="{ hot: num(night.e2e.tests.failed) }">{{ num(night.e2e.tests.failed) }}</b></span>
                  <span v-if="num(night.e2e.tests.errors) > 0" class="qs-tc err"><i />{{ t.counts.errors }}<b class="hot">{{ num(night.e2e.tests.errors) }}</b></span>
                  <span class="qs-tc total">{{ t.counts.total }}<b>{{ num(night.e2e.tests.total) }}</b></span>
                </span>
                <span v-if="nightPerfText(night)" class="qs-tip-perf" :class="perfTone(night.performance?.status)">
                  <i />{{ t.perf }} · {{ nightPerfText(night) }}
                </span>
              </div>
            </div>
          </div>
          <div class="qs-axis">
            <span>{{ wallNights[0].date }}</span>
            <span>{{ wallNights[wallNights.length - 1].date }}</span>
          </div>
          <p v-if="!nights.length" class="qs-note">{{ t.noHistory }}</p>
        </section>

        <section class="qs-figures">
          <div class="qs-figure">
            <p class="qs-label">{{ t.fig.e2e }}</p>
            <b>{{ num(tests.passed) }}</b>
            <span class="qs-sub">{{ t.fig.e2eSub(num(tests.total)) }}</span>
            <span class="qs-sub">{{ t.fig.e2eNote(num(tests.skipped), num(tests.failed) + num(tests.errors)) }}</span>
          </div>
          <div class="qs-figure" :class="{ 'is-bad': num(counts.critical), 'is-warn': !num(counts.critical) && num(counts.warning) }">
            <p class="qs-label">{{ t.fig.perf }}</p>
            <b>{{ num(counts.critical) }}</b>
            <span class="qs-sub">{{ t.fig.perfSub }}</span>
            <span class="qs-sub">{{ t.fig.perfNote(num(counts.warning), num(counts.improved)) }}</span>
          </div>
          <div class="qs-figure">
            <p class="qs-label">{{ t.fig.streak }}</p>
            <b>{{ streak.count }}</b>
            <span class="qs-sub">{{ t.fig.streakSub }}</span>
            <span class="qs-sub">{{ t.fig.streakNote(streak.since) }}</span>
          </div>
          <div class="qs-figure">
            <p class="qs-label">{{ t.fig.window }}</p>
            <b>{{ passedNights }}<small>/{{ wallNights.length }}</small></b>
            <span class="qs-sub">{{ t.fig.windowSub }}</span>
            <span class="qs-sub">{{ t.fig.windowNote(noRunNights) }}</span>
          </div>
        </section>

        <section v-if="moduleRows.length" class="qs-section">
          <div class="qs-section-head">
            <div>
              <p class="qs-label qs-accent">01</p>
              <h2>{{ t.e2eTitle }}</h2>
            </div>
            <p>{{ t.e2eLead }}</p>
          </div>
          <div class="qs-rows">
            <div class="qs-row qs-mod-row qs-head">
              <span>{{ t.modCols[0] }}</span>
              <span>{{ t.modCols[1] }}</span>
              <span class="r">{{ t.modCols[2] }}</span>
              <span class="r">{{ t.modCols[3] }}</span>
            </div>
            <div v-for="row in moduleRows" :key="row.name" class="qs-row qs-mod-row">
              <div class="qs-name">
                {{ row.name }}
                <small>
                  {{ num(row.row?.passed) }}/{{ num(row.row?.total) }}
                  <template v-if="num(row.row?.skipped)"> · {{ num(row.row.skipped) }} {{ t.counts.skipped.toLowerCase() }}</template>
                </small>
              </div>
              <div class="qs-ticks" :style="{ '--qs-n': row.history.length }">
                <span v-for="(tone, index) in row.history" :key="wallNights[index].date" class="qs-tick" :class="tone === 'ok' ? '' : tone" :title="wallNights[index].date" />
              </div>
              <div class="qs-num" :class="{ strong: row.strong }">{{ row.rate }}</div>
              <span class="qs-chip" :class="row.latestTone === 'fail' ? 'bad' : row.latestTone === 'ok' ? 'ok' : ''">
                {{ row.latestTone === 'fail' ? t.state.fail : row.latestTone === 'skip' ? t.counts.skipped : row.latestTone === 'ok' ? t.state.ok : t.state.none }}
              </span>
            </div>
          </div>
        </section>

        <section v-if="metrics.length" class="qs-section">
          <div class="qs-section-head">
            <div>
              <p class="qs-label qs-accent">02</p>
              <h2>{{ t.perfTitle }}</h2>
            </div>
            <p>{{ t.perfLead(trendNights.length || 1, band) }}</p>
          </div>
          <div class="qs-filter" role="group">
            <button v-for="value in FILTERS" :key="value" type="button" :aria-pressed="filter === value" @click="filter = value">
              {{ t.filters[value] }} {{ value === 'all' ? metrics.length : num(counts[value]) }}
            </button>
          </div>
          <div v-for="group in metricGroups" :key="group.key" class="qs-group">
            <div class="qs-group-head">
              <h3>{{ group.title }}</h3>
              <span>{{ t.metricsIn(group.rows.length, group.rows.filter((row) => row.verdict === 'critical').length) }}</span>
            </div>
            <div class="qs-rows">
              <div class="qs-row qs-perf-row qs-head">
                <span v-for="(col, index) in t.perfCols" :key="col" :class="{ r: index >= 2 }">{{ col }}</span>
              </div>
              <div v-for="metric in group.rows" :key="metric.key || metric.label" class="qs-row qs-perf-row">
                <div class="qs-name">
                  {{ metricName(metric) }}
                  <small v-if="metric.unit">{{ metric.unit }}<template v-if="metric.better"> · {{ t.better(metric.better) }}</template></small>
                </div>
                <Sparkline :values="seriesFor(metric)" :baseline="metric.baseline ?? null" :band="band" :verdict="String(metric.verdict || '').toLowerCase()" />
                <div class="qs-num strong">{{ metric.current_text || '—' }}</div>
                <div class="qs-num dim">{{ String(metric.baseline_text || '—').replace(/\*/g, '') }}</div>
                <div class="qs-num" :class="deltaClass(metric)">{{ deltaText(metric) }}</div>
                <span class="qs-chip" :class="chipClass(String(metric.verdict || '').toLowerCase())">{{ t.verdict[String(metric.verdict || '').toLowerCase()] || metric.verdict || '—' }}</span>
              </div>
            </div>
          </div>
          <p v-if="baselineUrl" class="qs-note">
            {{ t.baseline }}:
            <a :href="baselineUrl" target="_blank" rel="noopener noreferrer">{{ perf.baseline?.name || perf.baseline?.key || baselineUrl }}</a>
          </p>
          <p v-if="perf.note" class="qs-note">{{ perf.note }}</p>
        </section>

        <section class="qs-section">
          <div class="qs-section-head">
            <div>
              <p class="qs-label qs-accent">03</p>
              <h2>{{ t.methodTitle }}</h2>
            </div>
            <p>{{ t.methodLead }}</p>
          </div>
          <ol class="qs-method">
            <li v-for="step in t.steps" :key="step[0]">
              <em>{{ step[0] }}</em>
              <strong>{{ step[1] }}</strong>
              <span>{{ step[2] }}</span>
            </li>
          </ol>
        </section>

        <footer class="qs-foot">
          <div class="qs-foot-links">
            <a :href="publicLatestUrl" target="_blank" rel="noopener noreferrer">{{ t.rawLatest }}</a>
            <a :href="publicHistoryUrl" target="_blank" rel="noopener noreferrer">{{ t.rawHistory }}</a>
          </div>
          <span>{{ t.footer }}</span>
        </footer>
      </template>
    </div>
  </div>
</template>
