export const PARTICLE_TRAVEL = 1.25
export const MAX_PARTICLE_DELAY = 0.35
export const ASSEMBLY_DURATION = PARTICLE_TRAVEL + MAX_PARTICLE_DELAY
// Blend during the final approach; never hold a completed voxel silhouette.
export const SOLID_START = ASSEMBLY_DURATION - 0.28

export function clamp(value, min, max) {
  return Math.min(max, Math.max(min, value))
}

export function damp(current, target, delta, rate = 7) {
  return current + (target - current) * (1 - Math.exp(-rate * delta))
}

export function smoothstep(value) {
  const t = clamp(value, 0, 1)
  return clamp(t * t * t * (t * (t * 6 - 15) + 10), 0, 1)
}

// A stable seed gives thousands of distinct paths without random flicker on replay.
export function particleRecipe(index, targetY) {
  const noise = (salt) => {
    const value = Math.sin(index * 127.1 + salt * 311.7) * 43758.5453
    return value - Math.floor(value)
  }
  const radius = 0.1 + Math.sqrt(noise(1)) * 0.55
  return {
    angle: (index * 2.399963229728653) % (Math.PI * 2),
    radiusX: radius,
    radiusY: radius * 0.92,
    depth: (noise(3) - 0.35) * 0.65,
    spin: (noise(4) - 0.5) * 5,
    size: 0.38 + noise(5) * 0.35,
    delay: clamp((targetY + 0.5) * 0.2 + noise(6) * 0.145, 0, MAX_PARTICLE_DELAY)
  }
}

export function particleProgress(elapsed, delay) {
  return smoothstep((elapsed - delay) / PARTICLE_TRAVEL)
}
