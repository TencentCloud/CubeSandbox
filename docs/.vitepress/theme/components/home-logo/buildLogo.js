import {
  Box3, ExtrudeGeometry, Group, Mesh, MeshBasicMaterial,
  MeshStandardMaterial, Vector3
} from 'three'
import { SVGLoader } from 'three/addons/loaders/SVGLoader.js'

// Original SVG paint order. Thin seam details travel with their supporting face.
const STRUCTURES = [
  [0, 1, 2], [3], [4], [5], [6, 7], [8], [9], [10], [11],
  [12], [13], [14], [15], [16, 18], [17, 19]
]
const SCALE = 1 / 2260
const CENTER = { x: 3195, y: 1910 }

export function buildLogo(source) {
  const { paths } = new SVGLoader().parse(source)
  if (paths.length !== 20) throw new Error('Logo paths changed; update the structural grouping.')
  const root = new Group()
  const geometries = new Set()
  const materials = new Set()
  const faces = new Map()
  const sides = new Map()
  const pieces = []

  function materialFor(color, side) {
    const cache = side ? sides : faces
    const key = color.getHexString()
    if (!cache.has(key)) {
      const material = side
        ? new MeshStandardMaterial({ color, roughness: 0.58, metalness: 0.22 })
        : new MeshBasicMaterial({ color, toneMapped: false })
      cache.set(key, material)
      materials.add(material)
    }
    return cache.get(key)
  }

  const dispose = () => {
    geometries.forEach((geometry) => geometry.dispose())
    materials.forEach((material) => material.dispose())
  }

  try {
    STRUCTURES.forEach((indices, index) => {
      const group = new Group()
      for (const pathIndex of indices) {
        const path = paths[pathIndex]
        const depth = pathIndex >= 18 ? 0.5 : 38
        // Pinned Three.js r183 supports createShapes; update with the loader on upgrades.
        for (const shape of SVGLoader.createShapes(path)) {
          const geometry = new ExtrudeGeometry(shape, {
            depth, bevelEnabled: false, curveSegments: 16, steps: 1
          })
          geometries.add(geometry)
          geometry.translate(-CENTER.x, -CENTER.y, -depth)
          geometry.scale(SCALE, SCALE, SCALE)
          const mesh = new Mesh(geometry, [
            materialFor(path.color, false), materialFor(path.color, true)
          ])
          // Reflect through the object transform so the renderer corrects winding.
          mesh.scale.y = -1
          // Preserve the SVG painter's order without coplanar depth fighting.
          mesh.position.z = pathIndex * 0.00008
          group.add(mesh)
        }
      }
      const center = new Box3().setFromObject(group).getCenter(new Vector3())
      group.children.forEach((mesh) => mesh.position.sub(center))
      group.position.copy(center)
      root.add(group)
      pieces.push({ group, target: center.clone() })
    })
  } catch (error) {
    dispose()
    throw error
  }

  return {
    root, pieces, dispose,
    setDepthOnly(value) {
      materials.forEach((material) => {
        material.colorWrite = !value
        material.depthWrite = value || !material.transparent
      })
    },
    setOpacity(value) {
      root.visible = value > 0
      materials.forEach((material) => {
        material.opacity = value
        if (material.transparent !== (value < 1)) {
          material.transparent = value < 1
          material.needsUpdate = true
        }
        material.depthWrite = value === 1
      })
    }
  }
}
