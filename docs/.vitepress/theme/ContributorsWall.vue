<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useData } from 'vitepress'
import './contributors.css'
import {
  CONTRIBUTORS_JSON_URL,
  emptyContributorsData,
  normalizeContributors
} from '../contributors-source.js'

const props = defineProps({
  data: {
    type: Object,
    required: true
  },
  locale: {
    type: String,
    default: 'en'
  }
})

const { lang } = useData()
const query = ref('')
const heroRef = ref(null)
const gridRef = ref(null)
const activeGridRef = ref(null)
const px = ref(0)
const py = ref(0)

const liveData = ref(props.data || emptyContributorsData())
const loading = ref(Boolean(props.data?.live))
const loadFailed = ref(false)

const isZh = computed(() =>
  (props.locale || lang.value || 'en').startsWith('zh')
)

const i18n = computed(() =>
  isZh.value
    ? {
        kicker: '贡献者致谢',
        titleA: '筑造',
        titleB: 'Cube',
        titleC: '的人',
        lead: '每一次提交、每一行文档、每一个 issue，都是垒起 CubeSandbox 的一块方砖。这面墙，属于他们。',
        contributors: '贡献者',
        commits: '提交',
        stars: 'GitHub 星标',
        index: '全部贡献者',
        activeTitle: '活跃贡献者',
        search: '输入名字过滤…',
        loading: '正在加载贡献者数据…',
        empty: '暂时无法加载贡献者数据，请稍后刷新重试。',
        none: '没有匹配的贡献者。',
        github: '在 GitHub 上查看全部',
        contribute: '成为贡献者',
        updated: '数据快照',
        joinKicker: '加入我们',
        joinTitle: '从一块砖开始',
        steps: [
          { n: '01', t: '认领一个 issue', d: '从 good first issue 上手，bug、文档、翻译都算数。' },
          { n: '02', t: '提交你的 PR', d: '按照贡献指南提交，社区 review 后合入 master。' },
          { n: '03', t: '在这面墙上见', d: '合并的贡献会在下次数据刷新时出现在这里。' }
        ]
      }
    : {
        kicker: 'Contributors Tribute',
        titleA: 'The ',
        titleB: 'Cube',
        titleC: ' Builders',
        lead: 'Every commit, every doc, every issue is a block in the wall. CubeSandbox stands because these people stacked them.',
        contributors: 'Contributors',
        commits: 'Commits',
        stars: 'GitHub Stars',
        index: 'All contributors',
        activeTitle: 'Active contributors',
        search: 'Filter by name…',
        loading: 'Loading contributors…',
        empty: 'Contributor data could not be loaded. Please refresh and try again.',
        none: 'No matching contributors.',
        github: 'View all contributors on GitHub',
        contribute: 'Become a contributor',
        updated: 'Snapshot',
        joinKicker: 'Join the builders',
        joinTitle: 'Start with one block',
        steps: [
          { n: '01', t: 'Pick an issue', d: 'Start with a good first issue — bugs, docs and translations all count.' },
          { n: '02', t: 'Open your PR', d: 'Follow the contributing guide; the community reviews and merges.' },
          { n: '03', t: 'Meet you on the wall', d: 'Merged contributions show up here on the next data refresh.' }
        ]
      }
)

const numberLocale = computed(() => (isZh.value ? 'zh-CN' : 'en-US'))

function formatNumber(value) {
  return Number(value || 0).toLocaleString(numberLocale.value)
}

const stats = computed(() => {
  const raw = liveData.value?.stats || {}
  return [
    { key: 'contributors', label: i18n.value.contributors, value: raw.contributors || 0 },
    { key: 'commits', label: i18n.value.commits, value: raw.commits || 0 },
    { key: 'stars', label: i18n.value.stars, value: raw.stars || 0 }
  ]
})

const displayed = ref([0, 0, 0])

const contributors = computed(() =>
  Array.isArray(liveData.value?.contributors) ? liveData.value.contributors : []
)

const activeContributors = computed(() =>
  Array.isArray(liveData.value?.activeContributors) ? liveData.value.activeContributors : []
)

const filtered = computed(() => {
  const needle = query.value.trim().toLowerCase()
  if (!needle) return contributors.value
  return contributors.value.filter((row) =>
    String(row.login || '').toLowerCase().includes(needle)
  )
})

const filteredActive = computed(() => {
  const needle = query.value.trim().toLowerCase()
  if (!needle) return activeContributors.value
  return activeContributors.value.filter((row) =>
    String(row.login || '').toLowerCase().includes(needle)
  )
})

const searching = computed(() => query.value.trim().length > 0)

const githubContributorsUrl = computed(
  () => `${liveData.value?.htmlUrl || 'https://github.com/tencentcloud/CubeSandbox'}/graphs/contributors`
)

const contributeUrl = computed(() =>
  isZh.value
    ? 'https://github.com/tencentcloud/CubeSandbox/blob/master/CONTRIBUTING_zh.md'
    : 'https://github.com/tencentcloud/CubeSandbox/blob/master/CONTRIBUTING.md'
)

const updatedText = computed(() => {
  const raw = liveData.value?.generatedAt
  if (!raw) return ''
  const date = new Date(raw)
  if (Number.isNaN(date.getTime())) return ''
  const formatted = date.toLocaleDateString(numberLocale.value, {
    year: 'numeric',
    month: 'short',
    day: 'numeric'
  })
  return `${i18n.value.updated} — ${formatted}`
})

function avatarUrl(row) {
  const src = String(row.avatarUrl || '')
  if (!src) return ''
  const joiner = src.includes('?') ? '&' : '?'
  return `${src}${joiner}s=128`
}

/* avatars fade in over a placeholder icon; loading="lazy" keeps offscreen
   avatars from fetching until they approach the viewport */
const loadedAvatars = ref(new Set())

function markAvatarLoaded(login) {
  const next = new Set(loadedAvatars.value)
  next.add(login)
  loadedAvatars.value = next
}

function stagger(index) {
  return `${(index % 9) * 40}ms`
}

const watermarkStyle = computed(() => ({
  '--px': `${px.value}px`,
  '--py': `${py.value}px`
}))

function onHeroMove(event) {
  const rect = heroRef.value?.getBoundingClientRect()
  if (!rect) return
  px.value = ((event.clientX - rect.left) / rect.width - 0.5) * 18
  py.value = ((event.clientY - rect.top) / rect.height - 0.5) * 12
}

function onHeroLeave() {
  px.value = 0
  py.value = 0
}

/* focus-by-dimming only when the pointer actually rests on a grid —
   continuous movement never triggers it */
let focusTimer = null
let lastX = 0
let lastY = 0

function onGridMove(event) {
  const grid = event.currentTarget
  if (!grid) return
  const moved = Math.hypot(event.clientX - lastX, event.clientY - lastY)
  lastX = event.clientX
  lastY = event.clientY
  if (moved < 4) return
  grid.classList.remove('ct-focus')
  if (focusTimer) clearTimeout(focusTimer)
  focusTimer = setTimeout(() => {
    grid.classList.add('ct-focus')
  }, 600)
}

function onGridLeave(event) {
  if (focusTimer) clearTimeout(focusTimer)
  event.currentTarget?.classList.remove('ct-focus')
}

/* wheel-scrolling slides the grid under a resting pointer — the card in
   focus is no longer the one under the cursor, so drop focus and re-arm
   the dwell timer */
function onGridWheel(event) {
  const grid = event.currentTarget
  if (!grid) return
  grid.classList.remove('ct-focus')
  if (focusTimer) clearTimeout(focusTimer)
  focusTimer = setTimeout(() => {
    grid.classList.add('ct-focus')
  }, 600)
}

function runCountUp(reduce) {
  const targets = stats.value.map((s) => s.value)
  if (reduce) {
    displayed.value = targets
    return
  }
  const start = performance.now()
  const duration = 1500
  const tick = (t) => {
    const p = Math.min(1, (t - start) / duration)
    const eased = 1 - Math.pow(1 - p, 3)
    displayed.value = targets.map((v) => Math.round(v * eased))
    if (p < 1) requestAnimationFrame(tick)
  }
  requestAnimationFrame(tick)
}

function setupReveal(reduce) {
  const grids = [activeGridRef.value, gridRef.value].filter(Boolean)
  if (!grids.length) return
  if (reduce || !('IntersectionObserver' in window)) {
    grids.forEach((grid) => grid.classList.add('ct-plain'))
    return
  }
  const io = new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          entry.target.classList.add('ct-in')
          io.unobserve(entry.target)
          // zero the stagger delay once revealed so hover/dim stays snappy
          setTimeout(() => entry.target.style.setProperty('--d', '0ms'), 700)
        }
      }
    },
    { threshold: 0.05, rootMargin: '0px 0px -6% 0px' }
  )
  grids.forEach((grid) =>
    grid.querySelectorAll('.ct-card').forEach((el) => io.observe(el))
  )
}

async function fetchLive() {
  loading.value = true
  loadFailed.value = false
  try {
    const response = await fetch(CONTRIBUTORS_JSON_URL, {
      headers: { Accept: 'application/json' },
      signal: AbortSignal.timeout(15000)
    })
    if (!response.ok) throw new Error(`HTTP ${response.status}`)
    const normalized = normalizeContributors(await response.json())
    if (!normalized) throw new Error('unexpected payload')
    liveData.value = { ...normalized, live: false }
  } catch (error) {
    console.warn('[contributors] live fetch failed:', error)
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches

  if (props.data?.live) {
    await fetchLive()
    await nextTick()
  }
  runCountUp(reduce)
  setupReveal(reduce)
})

watch(searching, async (active) => {
  await nextTick()
  if (!active) return
  ;[activeGridRef.value, gridRef.value]
    .filter(Boolean)
    .forEach((grid) => grid.classList.add('ct-plain'))
})
</script>

<template>
  <div class="ct-page">
    <header
      ref="heroRef"
      class="ct-hero"
      @mousemove="onHeroMove"
      @mouseleave="onHeroLeave"
    >
      <div class="ct-watermark" :style="watermarkStyle" aria-hidden="true">
        <img src="/logo.svg" alt="">
      </div>

      <p class="ct-kicker">{{ i18n.kicker }}</p>
      <h1 class="ct-title">{{ i18n.titleA }}<em class="ct-title-accent">{{ i18n.titleB }}</em>{{ i18n.titleC }}</h1>
      <p class="ct-lead">{{ i18n.lead }}</p>

      <dl class="ct-facts" aria-label="stats">
        <div v-for="(item, i) in stats" :key="item.key" class="ct-fact">
          <dd class="ct-fact-value">{{ formatNumber(displayed[i]) }}</dd>
          <dt class="ct-fact-label">{{ item.label }}</dt>
        </div>
      </dl>
    </header>

    <section v-if="loading" class="ct-loading" aria-live="polite">
      <div class="ct-loading-circles" aria-hidden="true">
        <span v-for="i in 5" :key="i"></span>
      </div>
      <p class="ct-loading-text">{{ i18n.loading }}</p>
    </section>

    <template v-else>
    <section v-if="activeContributors.length" class="ct-active">
      <div class="ct-wall-head">
        <h2 class="ct-wall-title">
          {{ i18n.activeTitle }}
          <span class="ct-count">{{ formatNumber(activeContributors.length) }}</span>
        </h2>
      </div>
      <div
        v-if="filteredActive.length"
        ref="activeGridRef"
        class="ct-grid"
        :class="{ 'ct-plain': searching }"
        @mousemove="onGridMove"
        @mouseleave="onGridLeave"
        @wheel.passive="onGridWheel"
      >
        <a
          v-for="(row, index) in filteredActive"
          :key="row.login"
          class="ct-card"
          :href="row.htmlUrl"
          target="_blank"
          rel="noopener noreferrer"
          :title="`@${row.login}`"
          :style="{ '--d': stagger(index) }"
        >
          <span class="ct-ring">
            <span class="ct-avatar-wrap">
              <span class="ct-avatar-ph" aria-hidden="true">
                <svg viewBox="0 0 24 24" width="30" height="30" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round">
                  <circle cx="12" cy="8" r="3.6" />
                  <path d="M4.5 20c.8-3.8 3.9-5.8 7.5-5.8s6.7 2 7.5 5.8" />
                </svg>
              </span>
              <img
                class="ct-avatar"
                :class="{ 'ct-avatar-on': loadedAvatars.has(row.login) }"
                :src="avatarUrl(row)"
                :alt="row.login"
                width="76"
                height="76"
                loading="lazy"
                decoding="async"
                @load="markAvatarLoaded(row.login)"
                @error="markAvatarLoaded(row.login)"
              >
            </span>
          </span>
          <span class="ct-login">@{{ row.login }}</span>
        </a>
      </div>
    </section>

    <section class="ct-wall">
      <div class="ct-wall-head">
        <h2 class="ct-wall-title">
          {{ i18n.index }}
          <span v-if="contributors.length" class="ct-count">
            {{ formatNumber(contributors.length) }}
          </span>
        </h2>
        <label v-if="contributors.length || activeContributors.length" class="ct-search">
          <span class="ct-search-label">{{ i18n.search }}</span>
          <input
            v-model="query"
            type="search"
            class="ct-search-input"
            :placeholder="i18n.search"
          >
        </label>
      </div>

      <p v-if="!contributors.length && !activeContributors.length" class="ct-empty">{{ i18n.empty }}</p>
      <p v-else-if="!filtered.length && !filteredActive.length" class="ct-empty">{{ i18n.none }}</p>

      <div
        v-else-if="filtered.length"
        ref="gridRef"
        class="ct-grid"
        :class="{ 'ct-plain': searching }"
        @mousemove="onGridMove"
        @mouseleave="onGridLeave"
        @wheel.passive="onGridWheel"
      >
        <a
          v-for="(row, index) in filtered"
          :key="row.login"
          class="ct-card"
          :href="row.htmlUrl"
          target="_blank"
          rel="noopener noreferrer"
          :title="`@${row.login}`"
          :style="{ '--d': stagger(index) }"
        >
          <span class="ct-ring">
            <span class="ct-avatar-wrap">
              <span class="ct-avatar-ph" aria-hidden="true">
                <svg viewBox="0 0 24 24" width="30" height="30" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round">
                  <circle cx="12" cy="8" r="3.6" />
                  <path d="M4.5 20c.8-3.8 3.9-5.8 7.5-5.8s6.7 2 7.5 5.8" />
                </svg>
              </span>
              <img
                class="ct-avatar"
                :class="{ 'ct-avatar-on': loadedAvatars.has(row.login) }"
                :src="avatarUrl(row)"
                :alt="row.login"
                width="76"
                height="76"
                loading="lazy"
                decoding="async"
                @load="markAvatarLoaded(row.login)"
                @error="markAvatarLoaded(row.login)"
              >
            </span>
          </span>
          <span class="ct-login">@{{ row.login }}</span>
        </a>
      </div>
    </section>
    </template>

    <section class="ct-join">
      <p class="ct-kicker">{{ i18n.joinKicker }}</p>
      <h2 class="ct-join-title">{{ i18n.joinTitle }}</h2>
      <ol class="ct-steps">
        <li v-for="step in i18n.steps" :key="step.n" class="ct-step">
          <span class="ct-step-num">{{ step.n }}</span>
          <span class="ct-step-title">{{ step.t }}</span>
          <span class="ct-step-desc">{{ step.d }}</span>
        </li>
      </ol>
    </section>

    <footer class="ct-footer">
      <p v-if="updatedText" class="ct-updated">{{ updatedText }}</p>
      <nav class="ct-links">
        <a class="ct-link" :href="githubContributorsUrl" target="_blank" rel="noopener noreferrer">
          {{ i18n.github }} <span class="ct-arrow" aria-hidden="true">↗</span>
        </a>
        <a class="ct-link" :href="contributeUrl" target="_blank" rel="noopener noreferrer">
          {{ i18n.contribute }} <span class="ct-arrow" aria-hidden="true">↗</span>
        </a>
      </nav>
    </footer>
  </div>
</template>
