<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import { useRoute } from 'vitepress'
import { FLOWS, archSVG, layoutArrowLabels } from './juicefsArchitectureData.js'

const route = useRoute()
const isZh = computed(() => route.path.startsWith('/zh/'))
const props = defineProps({
  mode: {
    type: String,
    default: 'architecture',
    validator: value => ['architecture', 'flows'].includes(value),
  },
})
const scenario = ref(0)
const backend = ref('cubefs')
const place = ref('local')
const step = ref(0)
const diagram = ref(null)

const copy = computed(() => isZh.value ? {
  scenarios: ['制作模板', '启动沙箱', '暂停恢复'],
  backends: { xfs: 'XFS', cubefs: 'JuiceFS' },
  local: '本机', cross: '跨机', previous: '上一步', next: '下一步', switch: '切换', step: '第', suffix: '步',
} : {
  scenarios: ['Build template', 'Start sandbox', 'Pause and restore'],
  backends: { xfs: 'XFS', cubefs: 'JuiceFS' },
  local: 'Local', cross: 'Cross-node', previous: 'Previous', next: 'Next', switch: 'navigate', step: 'Step', suffix: '',
})

const flowKey = computed(() => backend.value === 'xfs'
  ? 'xfs'
  : `cubefs-${scenario.value === 0 ? 'local' : place.value}`)
const flow = computed(() => FLOWS[flowKey.value][scenario.value])
const current = computed(() => flow.value.steps[step.value])
const svg = computed(() => props.mode === 'architecture'
  ? archSVG(null, [], null, { jfsOnly: true })
  : archSVG(current.value.on, current.value.arrows, {
      no: `${step.value + 1} / ${flow.value.steps.length}`,
      k: current.value.k,
      time: current.value.time,
    }, { xfs: backend.value === 'xfs', files: current.value.files || [] }))

function chooseScenario(index) {
  scenario.value = index
  place.value = 'local'
  step.value = 0
}
function chooseBackend(value) {
  backend.value = value
  place.value = 'local'
  step.value = 0
}
function choosePlace(value) {
  place.value = value
  step.value = 0
}
function previous() {
  if (props.mode === 'flows' && step.value > 0) step.value--
}
function next() {
  if (props.mode === 'flows' && step.value < flow.value.steps.length - 1) step.value++
}

watch(svg, async () => {
  await nextTick()
  const element = diagram.value?.querySelector('svg')
  if (!element) return
  element.setAttribute('role', 'img')
  element.setAttribute('aria-label', current.value?.k || 'Cube + JuiceFS architecture')
  layoutArrowLabels(element)
}, { immediate: true })
</script>

<template>
  <div class="jfs-design" tabindex="0" @keydown.left.prevent="previous" @keydown.right.prevent="next">
    <template v-if="props.mode === 'flows'">
      <div class="toolbar">
        <div class="toolbar flow-tabs">
          <div class="tabs">
            <button v-for="(name, index) in copy.scenarios" :key="name" :class="{ on: scenario === index }" @click="chooseScenario(index)">{{ name }}</button>
          </div>
          <div class="tabs">
            <button v-for="(name, key) in copy.backends" :key="key" :class="{ on: backend === key }" @click="chooseBackend(key)">{{ name }}</button>
          </div>
          <div v-if="backend === 'cubefs' && scenario !== 0" class="tabs">
            <button :class="{ on: place === 'local' }" @click="choosePlace('local')">{{ copy.local }}</button>
            <button :class="{ on: place === 'cross' }" @click="choosePlace('cross')">{{ copy.cross }}</button>
          </div>
        </div>
        <div class="controls">
        <span class="keyboard">← / → {{ copy.switch }}</span>
        <button :disabled="step === 0" @click="previous">{{ copy.previous }}</button>
        <button :disabled="step === flow.steps.length - 1" @click="next">{{ copy.next }}</button>
        </div>
      </div>
      <div class="steps">
        <button v-for="(item, index) in flow.steps" :key="item.k" class="pill" :class="{ on: step === index }" @click="step = index"><b>{{ index + 1 }}</b>{{ item.k }}</button>
      </div>
    </template>

    <div class="body" :class="{ architecture: props.mode === 'architecture' }">
      <div ref="diagram" class="svgbox" v-html="svg" />
      <aside v-if="props.mode === 'flows'" class="info" aria-live="polite">
        <div class="step-no">{{ copy.step }} {{ step + 1 }} / {{ flow.steps.length }} {{ copy.suffix }} · {{ current.time }}</div>
        <h3>{{ current.k }}</h3>
        <ul><li v-for="detail in current.d" :key="detail">{{ detail }}</li></ul>
      </aside>
    </div>
  </div>
</template>

<style scoped>
.jfs-design {
  --bg: var(--vp-c-bg); --card: var(--vp-c-bg-soft); --fg: var(--vp-c-text-1); --muted: var(--vp-c-text-2); --border: var(--vp-c-divider); --muted-bg: var(--vp-c-bg-alt); --ring: var(--vp-c-brand-1); --s3: #65758b;
  --c-vm: #7c5ce7; --c-cl: #3478f6; --c-jfs: #16a06d; --c-xfs: #d48806; --c-redis: #dc4055; --c-master: #7950b6; --c-tc: #1677b8; --c-minio: #c0392b;
  margin: 22px 0 32px; padding: 16px 18px 18px; border: 1px solid var(--border); border-radius: 12px; background: var(--card); outline: none;
}
.jfs-design:focus-visible { box-shadow: 0 0 0 2px var(--vp-c-brand-1); }
.toolbar { display: flex; justify-content: space-between; align-items: center; gap: 10px; flex-wrap: wrap; }
.flow-tabs { justify-content: flex-start; }
.tabs { display: inline-flex; padding: 3px; gap: 2px; border: 1px solid var(--border); border-radius: 8px; background: var(--bg); }
button { border: 1px solid var(--border); border-radius: 7px; padding: 6px 11px; color: var(--muted); background: var(--bg); font: inherit; font-size: 12px; cursor: pointer; }
.tabs button { border: 0; padding: 5px 10px; background: transparent; }
button:hover:not(:disabled) { color: var(--fg); }
/* Keep the selected control legible when the pointer is over it. */
button.on,
button.on:hover:not(:disabled) { color: #fff; background: rgb(9, 9, 11); }
button:disabled { opacity: .35; cursor: default; }
.controls { display: flex; align-items: center; gap: 7px; }
.keyboard { color: var(--muted); font-size: 12px; }
.steps { display: flex; flex-wrap: wrap; gap: 6px; margin: 10px 0 6px; }
.pill { border-radius: 99px; padding: 5px 10px; text-align: left; }
.pill b { color: var(--fg); margin-right: 5px; }
.pill.on b { color: var(--bg); }
.body { display: grid; grid-template-columns: minmax(0, 1fr) 270px; gap: 15px; min-height: 0; }
.body.architecture { display: block; }
.svgbox { min-width: 0; overflow-x: auto; }
.svgbox :deep(svg) { display: block; min-width: 650px; width: 94%; height: auto; margin: 0 auto; }
.svgbox :deep(text) { font-family: inherit; fill: var(--fg); }
.svgbox :deep(.t-sub) { fill: var(--muted); font-size: 11.5px; }
.svgbox :deep(.t-title) { font-size: 14.5px; font-weight: 600; }
.svgbox :deep(.t-frame) { font-size: 13px; fill: var(--muted); font-weight: 600; }
.svgbox :deep(.frame) { fill: color-mix(in srgb, var(--c, var(--muted)) 3%, transparent); stroke: color-mix(in srgb, var(--c, var(--muted)) 30%, var(--border)); stroke-width: 1.5; stroke-dasharray: 4 4; }
.svgbox :deep(.box) { transition: opacity .3s; }
.svgbox :deep(.box.dim) { opacity: .12; }
.svgbox :deep(.box rect.bg) { fill: color-mix(in srgb, var(--c) 8%, var(--card)); stroke: color-mix(in srgb, var(--c) 40%, var(--border)); stroke-width: 1.5; transition: all .3s; }
.svgbox :deep(.box rect.bar) { fill: var(--c); }
.svgbox :deep(.box .t-title) { fill: color-mix(in srgb, var(--c) 55%, var(--fg)); }
.svgbox :deep(.box.on rect.bg) { fill: color-mix(in srgb, var(--c) 16%, var(--card)); stroke: var(--c); stroke-width: 2.2; filter: drop-shadow(0 4px 10px color-mix(in srgb, var(--c) 30%, transparent)); }
.svgbox :deep(.box.clone rect.bg) { stroke-dasharray: 5 3.5; }
.svgbox :deep(.link) { fill: none; stroke: color-mix(in srgb, var(--c, var(--ring)) 45%, transparent); stroke-width: 1.4; stroke-dasharray: 3 4; }
.svgbox :deep(.arrow) { fill: none; stroke: var(--c); stroke-width: 2.4; stroke-dasharray: 7 6; animation: dash .9s linear infinite; }
.svgbox :deep(.head) { fill: var(--c); }
.svgbox :deep(.lbl rect) { fill: var(--c); }
.svgbox :deep(.lbl text) { fill: #fff; font-size: 12.5px; font-weight: 600; }
.svgbox :deep(.label-leader) { fill: none; stroke: var(--c); stroke-width: 1.2; stroke-dasharray: 2 3; opacity: .7; }
@keyframes dash { to { stroke-dashoffset: -26; } }
.info { border-left: 1px solid var(--border); padding: 10px 0 0 16px; overflow: auto; }
.step-no { color: var(--muted); font-size: 12px; }
.info h3 { margin: 2px 0 8px; border: 0; font-size: 16px; }
.info ul { margin: 0; padding-left: 18px; color: var(--muted); font-size: 13px; line-height: 1.6; }
.info li { margin: 7px 0; }
@media (max-width: 900px) { .body { grid-template-columns: 1fr; }.info { border-top: 1px solid var(--border); border-left: 0; padding: 12px 0 0; }.keyboard { display: none; } }
@media (prefers-reduced-motion: reduce) { .svgbox :deep(.arrow) { animation: none; } }
</style>
