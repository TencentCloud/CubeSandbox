import { readFile } from 'node:fs/promises'
import {
  DEFAULT_QUALITY_STATUS_JSON_URL,
  emptyQualityStatus,
  normalizeQualityStatus
} from './.vitepress/quality-status-source.js'

export default {
  async load() {
    const sourceUrl =
      process.env.QUALITY_STATUS_JSON_URL ||
      process.env.VITE_QUALITY_STATUS_JSON_URL ||
      DEFAULT_QUALITY_STATUS_JSON_URL
    const localPath = process.env.QUALITY_STATUS_JSON_PATH || ''
    if (localPath) {
      try {
        const normalized = normalizeQualityStatus(
          JSON.parse(await readFile(localPath, 'utf8'))
        )
        if (normalized) return { payload: normalized, sourceUrl, live: false }
        console.warn(`[quality-status] ${localPath} returned an unexpected payload`)
      } catch (error) {
        console.warn(`[quality-status] failed to read ${localPath}:`, error)
      }
    }
    return { payload: emptyQualityStatus(), sourceUrl, live: true }
  }
}
