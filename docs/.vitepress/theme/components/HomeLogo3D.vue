<script>
// Browser-session state survives SPA navigation; no storage or SSR user state.
let hasVisited = false
</script>

<script setup>
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useData } from 'vitepress'
import logoUrl from '../../../assets/cube-sandbox-logo.svg?url'

const props = defineProps({ enabled: { type: Boolean, default: true } })
const { isDark } = useData()
const host = ref(null)
const ready = ref(false)
const reduced = ref(false)
let scene
let media
let mounted = false
let generation = 0
let controller

function stop() {
  generation++
  controller?.abort()
  controller = undefined
  scene?.dispose()
  scene = undefined
  ready.value = false
}

async function start() {
  stop()
  if (!mounted || !props.enabled || reduced.value) return
  const current = generation
  const started = performance.now()
  const shouldPlay = !hasVisited
  hasVisited = true
  controller = new AbortController()
  const signal = controller.signal
  try {
    const [{ createScene }, source] = await Promise.all([
      import('./home-logo/createScene.js'),
      fetch(logoUrl, { signal }).then((response) => {
        if (!response.ok) throw new Error('Logo could not be loaded')
        return response.text()
      })
    ])
    if (!mounted || current !== generation) return
    scene = createScene({
      host: host.value,
      pointerHost: host.value.closest('.VPHomeHero') || host.value,
      source,
      intro: shouldPlay && performance.now() - started < 1500,
      dark: isDark.value,
      onFailure: stop
    })
    ready.value = true
  } catch {
    // Keep the server-rendered brand asset on network or WebGL failure.
    if (current === generation) stop()
  }
}

function preferenceChanged() {
  reduced.value = media.matches
  if (reduced.value) { hasVisited = true; stop() }
  else start()
}

onMounted(() => {
  mounted = true
  media = window.matchMedia('(prefers-reduced-motion: reduce)')
  media.addEventListener('change', preferenceChanged)
  preferenceChanged()
})
watch(isDark, (value) => scene?.setDark(value))
watch(() => props.enabled, start)
onBeforeUnmount(() => {
  mounted = false
  media?.removeEventListener('change', preferenceChanged)
  stop()
})
</script>

<template>
  <div class="home-logo" :class="{ 'is-ready': ready }">
    <div class="home-logo__stage" aria-hidden="true">
      <div class="home-logo__glow" />
      <img class="home-logo__fallback" :src="logoUrl" alt="" width="1790" height="2260" fetchpriority="high">
      <div ref="host" class="home-logo__canvas" />
      <div class="home-logo__shadow" />
    </div>

  </div>
</template>

<style scoped>
.home-logo { width: 100%; --logo-height: 404px; }
.home-logo__stage { position: relative; isolation: isolate; height: var(--logo-height); overflow: hidden; }
.home-logo__canvas { position: absolute; inset: 0; opacity: 0; transition: opacity 180ms ease; }
.home-logo__canvas :deep(canvas) { display: block; width: 100%; height: 100%; }
.home-logo__fallback { position: absolute; top: 50%; left: 50%; height: 71.94%; width: auto; max-width: 78%; object-fit: contain; transform: translate(-50%, -50%); transition: opacity 180ms ease; }
.is-ready .home-logo__canvas { opacity: 1; }
.is-ready .home-logo__fallback { opacity: 0; }
.home-logo__glow { position: absolute; inset: 22% 17%; z-index: -1; border-radius: 50%; background: #72a8e7; opacity: 0.12; filter: blur(44px); }
.home-logo__shadow { position: absolute; bottom: 4%; left: 29%; width: 42%; height: 3%; border-radius: 50%; background: #264c80; opacity: 0.15; filter: blur(9px); z-index: -1; }
.dark .home-logo__glow { opacity: 0.16; }
.dark .home-logo__shadow { background: #090e19; opacity: 0.5; }
@media (min-width: 1280px) { .home-logo { --logo-height: 428px; } }
@media (max-width: 959px) { .home-logo { --logo-height: 320px; max-width: 440px; margin: auto; } }
@media (max-width: 639px) { .home-logo { --logo-height: 256px; } }
@media (prefers-reduced-motion: reduce) { .home-logo__canvas, .home-logo__fallback { transition: none; } }
</style>
