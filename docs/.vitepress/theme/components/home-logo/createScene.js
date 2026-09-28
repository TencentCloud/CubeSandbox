import {
  AmbientLight, DirectionalLight, Group, OrthographicCamera,
  Scene, SRGBColorSpace, WebGLRenderer
} from 'three'
import { buildLogo } from './buildLogo.js'
import { buildParticles } from './buildParticles.js'
import { ASSEMBLY_DURATION, clamp, damp } from './motion.js'

export function createScene({ host, pointerHost, source, intro, dark, onFailure }) {
  const renderer = new WebGLRenderer({ alpha: true, antialias: true, powerPreference: 'low-power' })
  renderer.outputColorSpace = SRGBColorSpace
  renderer.setClearColor(0x000000, 0)
  renderer.domElement.setAttribute('aria-hidden', 'true')
  let logo
  let particles
  try {
    logo = buildLogo(source)
    particles = buildParticles(logo.root, window.matchMedia('(pointer: coarse)').matches ? 42 : 56)
  } catch (error) {
    logo?.dispose()
    renderer.dispose()
    throw error
  }

  const scene = new Scene()
  const camera = new OrthographicCamera(-0.7, 0.7, 0.7, -0.7, 0.1, 20)
  camera.position.z = 5
  const tilt = new Group()
  const float = new Group()
  tilt.add(float)
  float.add(logo.root)
  float.add(particles.mesh)
  scene.add(tilt)
  const ambient = new AmbientLight(0xd4e6ff, 2)
  const key = new DirectionalLight(0xffffff, 3)
  key.position.set(-2, 3, 5)
  const rim = new DirectionalLight(0x87baff, 2)
  rim.position.set(3, 1, 2)
  scene.add(ambient, key, rim)

  const finePointer = window.matchMedia('(hover: hover) and (pointer: fine)')
  let elapsed = intro ? 0 : ASSEMBLY_DURATION
  let floatTime = 0
  let paused = false
  let visible = false
  let disposed = false
  let frame = 0
  let previous = 0
  let width = 1
  let height = 1
  let quality = finePointer.matches ? 1.5 : 1.25
  let sampleTime = 0
  let sampleFrames = 0
  let slowWindows = 0
  const pointer = { x: 0, y: 0 }

  function paint(delta = 0) {
    const assembled = elapsed >= ASSEMBLY_DURATION
    const blend = particles.update(elapsed)
    logo.setOpacity(blend)
    if (paused) {
      tilt.rotation.set(0, 0, 0)
      float.position.y = 0
      float.rotation.z = 0
    } else {
      tilt.rotation.y = damp(tilt.rotation.y, assembled ? pointer.x * 0.17 + Math.sin(floatTime * 0.35) * 0.025 : 0, delta)
      tilt.rotation.x = damp(tilt.rotation.x, assembled ? -pointer.y * 0.1 + Math.sin(floatTime * 0.4) * 0.015 : 0, delta)
      float.position.y = Math.sin(floatTime * Math.PI / 3) * 0.02
      float.rotation.z = Math.sin(floatTime * Math.PI / 4) * 0.009
    }
    if (blend > 0 && blend < 1) {
      // Fade the visible surface, not every overlapping SVG face. A depth-only
      // pass prevents covered curves and internal walls showing through the logo.
      logo.root.visible = false
      renderer.render(scene, camera)
      const particlesVisible = particles.mesh.visible
      particles.mesh.visible = false
      renderer.autoClear = false
      try {
        renderer.clearDepth()
        logo.root.visible = true
        logo.setDepthOnly(true)
        renderer.render(scene, camera)
        logo.setDepthOnly(false)
        renderer.render(scene, camera)
      } finally {
        logo.setDepthOnly(false)
        renderer.autoClear = true
        particles.mesh.visible = particlesVisible
      }
    } else {
      renderer.render(scene, camera)
    }
  }

  function running() {
    return !disposed && !paused && visible && !document.hidden
  }

  function tick(now) {
    frame = 0
    if (!running()) return
    const rawDelta = previous ? (now - previous) / 1000 : 0
    const delta = Math.min(rawDelta, 0.05)
    previous = now
    elapsed += delta
    if (elapsed >= ASSEMBLY_DURATION) floatTime += delta
    try {
      paint(delta)
    } catch {
      onFailure()
      return
    }
    // Two sustained slow windows are needed before degrading, ignoring startup.
    if (elapsed > ASSEMBLY_DURATION + 2 && rawDelta > 0) {
      sampleTime += rawDelta
      sampleFrames++
      if (sampleTime >= 3) {
        slowWindows = sampleFrames / sampleTime < 24 ? slowWindows + 1 : 0
        sampleTime = 0
        sampleFrames = 0
        if (slowWindows >= 2) {
          if (quality > 1) {
            quality = 1
            renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, quality))
            renderer.setSize(width, height, false)
            slowWindows = 0
          } else {
            onFailure()
            return
          }
        }
      }
    }
    frame = requestAnimationFrame(tick)
  }

  function syncRunning() {
    cancelAnimationFrame(frame)
    frame = 0
    previous = 0
    sampleTime = 0
    sampleFrames = 0
    if (running()) frame = requestAnimationFrame(tick)
  }

  function resize() {
    if (disposed) return
    width = Math.max(1, host.clientWidth)
    height = Math.max(1, host.clientHeight)
    // Extra horizontal room at narrow widths keeps all fragments inside the stage.
    const viewHeight = Math.max(1.39, 1.4 / (width / height))
    camera.top = viewHeight / 2
    camera.bottom = -viewHeight / 2
    camera.left = -viewHeight * width / height / 2
    camera.right = -camera.left
    camera.updateProjectionMatrix()
    renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, quality))
    renderer.setSize(width, height, false)
    paint()
  }

  function pointerMove(event) {
    if (!finePointer.matches || event.pointerType === 'touch') return
    const rect = pointerHost.getBoundingClientRect()
    pointer.x = clamp((event.clientX - rect.left) / rect.width * 2 - 1, -1, 1)
    pointer.y = clamp(1 - (event.clientY - rect.top) / rect.height * 2, -1, 1)
  }
  function pointerLeave() { pointer.x = 0; pointer.y = 0 }
  function contextLost(event) { event.preventDefault(); onFailure() }
  function setDark(value) { ambient.intensity = value ? 2.6 : 2; rim.intensity = value ? 3 : 2; paint() }

  const resizeObserver = new ResizeObserver(resize)
  const intersectionObserver = new IntersectionObserver(([entry]) => {
    visible = entry.isIntersecting
    syncRunning()
  })

  function dispose() {
    if (disposed) return
    disposed = true
    cancelAnimationFrame(frame)
    resizeObserver.disconnect()
    intersectionObserver.disconnect()
    pointerHost.removeEventListener('pointermove', pointerMove)
    pointerHost.removeEventListener('pointerleave', pointerLeave)
    document.removeEventListener('visibilitychange', syncRunning)
    renderer.domElement.removeEventListener('webglcontextlost', contextLost)
    logo.dispose()
    particles.dispose()
    renderer.dispose()
    renderer.forceContextLoss()
    renderer.domElement.remove()
  }

  try {
    host.appendChild(renderer.domElement)
    resize()
    setDark(dark)
    resizeObserver.observe(host)
    intersectionObserver.observe(host)
    pointerHost.addEventListener('pointermove', pointerMove, { passive: true })
    pointerHost.addEventListener('pointerleave', pointerLeave)
    document.addEventListener('visibilitychange', syncRunning)
    renderer.domElement.addEventListener('webglcontextlost', contextLost)
  } catch (error) {
    dispose()
    throw error
  }

  return {
    setDark,
    setPaused(value) {
      paused = value
      // Pausing always presents the fully assembled, neutral brand mark.
      if (paused) { elapsed = ASSEMBLY_DURATION; floatTime = 0 }
      paint()
      syncRunning()
    },
    replay() {
      if (paused) return
      elapsed = 0
      floatTime = 0
      pointerLeave()
      tilt.rotation.set(0, 0, 0)
      paint()
      syncRunning()
    },
    dispose
  }
}
