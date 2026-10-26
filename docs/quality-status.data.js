import {
  DEFAULT_QUALITY_STATUS_JSON_URL,
  emptyQualityStatus
} from './.vitepress/quality-status-source.js'

export default {
  async load() {
    return {
      payload: emptyQualityStatus(),
      sourceUrl: DEFAULT_QUALITY_STATUS_JSON_URL,
      live: true
    }
  }
}
