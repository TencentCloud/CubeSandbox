export const CONTRIBUTORS_JSON_URL =
  'https://raw.githubusercontent.com/Cube-Operation/cube-automations/refs/heads/master/data/contributors.json'

export const CONTRIBUTORS_REPO = 'tencentcloud/CubeSandbox'
export const CONTRIBUTORS_REPO_URL = 'https://github.com/tencentcloud/CubeSandbox'

export function emptyContributorsData() {
  return {
    generatedAt: null,
    repo: CONTRIBUTORS_REPO,
    htmlUrl: CONTRIBUTORS_REPO_URL,
    stats: {
      contributors: 0,
      commits: 0,
      stars: 0
    },
    activeContributors: [],
    contributors: []
  }
}

export function normalizeContributors(payload) {
  if (!payload || !Array.isArray(payload.contributors) || !payload.stats) {
    return null
  }
  const pick = (row) => ({
    login: String(row.login),
    htmlUrl: String(row.htmlUrl),
    avatarUrl: String(row.avatarUrl || ''),
    contributions: Number(row.contributions) || 0
  })
  return {
    generatedAt: payload.generatedAt || null,
    repo: payload.repo || CONTRIBUTORS_REPO,
    htmlUrl: payload.htmlUrl || CONTRIBUTORS_REPO_URL,
    stats: {
      contributors: Number(payload.stats.contributors) || 0,
      commits: Number(payload.stats.commits) || 0,
      stars: Number(payload.stats.stars) || 0
    },
    activeContributors: (Array.isArray(payload.activeContributors)
      ? payload.activeContributors
      : []
    )
      .filter((row) => row && row.login && row.htmlUrl)
      .map(pick),
    contributors: payload.contributors
      .filter((row) => row && row.login && row.htmlUrl)
      .map(pick)
  }
}
