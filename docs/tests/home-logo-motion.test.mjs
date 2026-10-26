import test from 'node:test'
import assert from 'node:assert/strict'
import { ASSEMBLY_DURATION, MAX_PARTICLE_DELAY, PARTICLE_TRAVEL, SOLID_START, particleProgress, damp, particleRecipe, smoothstep } from '../.vitepress/theme/components/home-logo/motion.js'

test('cubes are almost settled during blending and fully settled when the logo resolves', () => {
  for (let index = 0; index < 4000; index++) {
    const motion = particleRecipe(index, (index % 100) / 100 - 0.5)
    assert.equal(particleProgress(0, motion.delay), 0)
    assert.equal(particleProgress(motion.delay, motion.delay), 0)
    let previous = 0
    for (let step = 0; step <= 100; step++) {
      const progress = particleProgress(step / 100 * ASSEMBLY_DURATION, motion.delay)
      assert.ok(progress >= previous && progress <= 1)
      previous = progress
    }
    assert.ok(particleProgress(SOLID_START, motion.delay) > 0.9)
    assert.equal(particleProgress(ASSEMBLY_DURATION, motion.delay), 1)
    assert.equal(particleProgress(100, motion.delay), 1)
  }
})

test('the solid transition overlaps arrival instead of adding a post-arrival wait', () => {
  const latestArrival = PARTICLE_TRAVEL + MAX_PARTICLE_DELAY
  assert.ok(SOLID_START < latestArrival)
  assert.equal(ASSEMBLY_DURATION, latestArrival)
  assert.ok(ASSEMBLY_DURATION - SOLID_START <= 0.3)
})

test('pointer response converges equally at 30, 60 and 120 fps', () => {
  const result = (fps) => {
    let angle = -0.14
    for (let i = 0; i < fps; i++) angle = damp(angle, 0.14, 1 / fps)
    return angle
  }
  assert.ok(Math.abs(result(30) - result(120)) < 1e-12)
  assert.ok(Math.abs(result(60) - 0.14) < 0.001)
  assert.equal(damp(0.14, 0, 0), 0.14)
})

test('thousands of cube paths have bounded rotation and reproducible starts', () => {
  const poses = Array.from({ length: 4000 }, (_, i) => particleRecipe(i, 0))
  assert.deepEqual(poses, Array.from({ length: 4000 }, (_, i) => particleRecipe(i, 0)))
  assert.equal(new Set(poses.map((pose) => pose.angle)).size, 4000)
  for (const pose of poses) {
    assert.ok(pose.radiusX <= 0.65 && pose.radiusY <= 0.61)
    assert.ok(Math.abs(pose.spin) <= 2.5)
    assert.ok(pose.angle >= 0 && pose.angle <= 2 * Math.PI)
  }
})

test('arrival easing has gentle endpoints and clamps out-of-range times', () => {
  assert.equal(smoothstep(-1), 0)
  assert.equal(smoothstep(2), 1)
  assert.ok(smoothstep(0.001) < 0.000001)
  assert.ok(1 - smoothstep(0.999) < 0.000001)
})
