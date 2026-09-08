import {
  BoxGeometry, Color, DynamicDrawUsage, Float32BufferAttribute,
  InstancedMesh, MeshBasicMaterial, Object3D, Raycaster, Vector3
} from 'three'
import { ASSEMBLY_DURATION, SOLID_START, particleProgress, particleRecipe, smoothstep } from './motion.js'

// Sample the actual SVG-derived geometry, so every cube ends on the brand mark.
// Instancing renders the entire particle field in one draw call.
export function buildParticles(logoRoot, columns = 56) {
  const rows = Math.round(columns * 2260 / 1790)
  const cell = 1 / rows
  const ray = new Raycaster(new Vector3(), new Vector3(0, 0, -1))
  const particles = []
  const hits = []
  logoRoot.updateMatrixWorld(true)
  for (let row = 0; row < rows; row++) {
    for (let column = 0; column < columns; column++) {
      const x = ((column + 0.5) / columns - 0.5) * 1790 / 2260
      const y = 0.5 - (row + 0.5) / rows
      ray.ray.origin.set(x, y, 2)
      hits.length = 0
      ray.intersectObject(logoRoot, true, hits)
      if (!hits.length) continue
      const hit = hits[0]
      const material = hit.object.material[hit.face.materialIndex]
      particles.push({
        x, y, z: hit.point.z - cell / 2,
        color: material.color.clone(),
        ...particleRecipe(particles.length, y)
      })
    }
  }

  const geometry = new BoxGeometry(cell * 0.94, cell * 0.94, cell * 0.94)
  const colors = []
  const normals = geometry.attributes.normal
  for (let i = 0; i < normals.count; i++) {
    const shade = normals.getZ(i) > 0 ? 1 : normals.getY(i) > 0 ? 0.88 : 0.62
    colors.push(shade, shade, shade)
  }
  geometry.setAttribute('color', new Float32BufferAttribute(colors, 3))
  const material = new MeshBasicMaterial({ vertexColors: true, transparent: true, toneMapped: false })
  const mesh = new InstancedMesh(geometry, material, particles.length)
  mesh.instanceMatrix.setUsage(DynamicDrawUsage)
  // Its animated extent is larger than the final silhouette; don't cull by rest pose.
  mesh.frustumCulled = false
  particles.forEach((particle, index) => mesh.setColorAt(index, new Color().copy(particle.color)))
  const dummy = new Object3D()

  return {
    mesh,
    count: particles.length,
    update(elapsed) {
      const blend = smoothstep((elapsed - SOLID_START) / (ASSEMBLY_DURATION - SOLID_START))
      mesh.visible = blend < 1
      if (!mesh.visible) return 1
      material.opacity = smoothstep(elapsed / 0.18) * (1 - blend)
      for (let i = 0; i < particles.length; i++) {
        const p = particles[i]
        const progress = particleProgress(elapsed, p.delay)
        const remaining = 1 - progress
        // A soft spiral, then an unhurried arrival. No linear explosion/reversal.
        const angle = p.angle + progress * 1.25
        dummy.position.set(
          p.x * progress + Math.cos(angle) * p.radiusX * remaining,
          p.y * progress + Math.sin(angle) * p.radiusY * remaining,
          p.z * progress + (p.depth + Math.sin(progress * Math.PI) * 0.15) * remaining
        )
        dummy.rotation.set(p.spin * remaining, p.spin * 0.7 * remaining, p.angle * remaining)
        const scale = p.size + (1 - p.size) * progress
        dummy.scale.setScalar(scale)
        dummy.updateMatrix()
        mesh.setMatrixAt(i, dummy.matrix)
      }
      mesh.instanceMatrix.needsUpdate = true
      return blend
    },
    dispose() { mesh.dispose(); geometry.dispose(); material.dispose() }
  }
}
