import { readFile } from 'node:fs/promises'
import {
  emptyContributorsData,
  normalizeContributors
} from './.vitepress/contributors-source.js'

// Build-time only handles the local dev override (CONTRIBUTORS_JSON_PATH).
// Otherwise the page fetches the JSON at runtime in the browser — nothing
// is baked into the static bundle.
export default {
  async load() {
    const localPath = process.env.CONTRIBUTORS_JSON_PATH
    if (localPath) {
      try {
        const normalized = normalizeContributors(
          JSON.parse(await readFile(localPath, 'utf8'))
        )
        if (normalized) return { ...normalized, live: false }
        console.warn(`[contributors] ${localPath} returned an unexpected payload`)
      } catch (error) {
        console.warn(`[contributors] failed to read ${localPath}:`, error)
      }
    }
    return { ...emptyContributorsData(), live: true }
  }
}
