import { existsSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

export const SITE_ORIGIN = 'https://cubesandbox.com'

const docsRoot = join(dirname(fileURLToPath(import.meta.url)), '..')

const EXTERNAL_BLOG_SLUGS = [
  'blog/posts/2026-05-17-aws-nested-virt-cube-deploy',
  'blog/posts/2026-05-22-cube-pvm-on-opencloudos9'
]

export function normalizeSitemapPath(url) {
  return String(url || '')
    .replace(/^https?:\/\/[^/]+/i, '')
    .replace(/\.html$/i, '')
    .replace(/\/+$/, '')
    .replace(/^\//, '')
}

export function shouldDropFromSitemap(url) {
  const path = normalizeSitemapPath(url)
  if (path === '404' || path.endsWith('/404')) return true
  return EXTERNAL_BLOG_SLUGS.some((slug) => path === slug || path.endsWith(`/${slug}`))
}

export function pageToCleanPath(relativePath) {
  const path = relativePath.replace(/\.md$/i, '')
  if (path === 'index') return '/'
  if (path.endsWith('/index')) return `/${path.slice(0, -'index'.length)}`
  return `/${path}`
}

export function pageToMarkdownPath(relativePath) {
  if (relativePath === 'index.md') return '/index.md'
  if (relativePath.endsWith('/index.md')) {
    return `/${relativePath.slice(0, -'/index.md'.length)}.md`
  }
  return `/${relativePath}`
}

export function counterpartRelativePath(relativePath) {
  return relativePath.startsWith('zh/')
    ? relativePath.slice(3)
    : `zh/${relativePath}`
}

export function counterpartExists(relativePath) {
  return existsSync(join(docsRoot, counterpartRelativePath(relativePath)))
}

export function shouldOfferMarkdown(relativePath) {
  if (relativePath.includes('_template')) return false
  const clean = relativePath.replace(/\.md$/i, '')
  return !EXTERNAL_BLOG_SLUGS.some((slug) => clean === slug || clean.endsWith(`/${slug}`))
}

export function crawlerHeadTags({ relativePath }) {
  const cleanPath = pageToCleanPath(relativePath)
  const canonical = `${SITE_ORIGIN}${cleanPath === '/' ? '/' : cleanPath}`
  const tags = [
    ['link', { rel: 'canonical', href: canonical }],
    ['link', { rel: 'describedby', href: '/llms.txt' }]
  ]

  if (shouldOfferMarkdown(relativePath)) {
    tags.push([
      'link',
      {
        rel: 'alternate',
        type: 'text/markdown',
        href: pageToMarkdownPath(relativePath)
      }
    ])
  }

  const enPath = relativePath.startsWith('zh/') ? relativePath.slice(3) : relativePath
  const zhPath = relativePath.startsWith('zh/') ? relativePath : `zh/${relativePath}`
  const enHref = `${SITE_ORIGIN}${pageToCleanPath(enPath)}`
  const zhHref = `${SITE_ORIGIN}${pageToCleanPath(zhPath)}`

  if (!relativePath.startsWith('zh/') || counterpartExists(relativePath)) {
    tags.push(['link', { rel: 'alternate', hreflang: 'en', href: enHref }])
    tags.push(['link', { rel: 'alternate', hreflang: 'x-default', href: enHref }])
  }
  if (relativePath.startsWith('zh/') || counterpartExists(relativePath)) {
    tags.push(['link', { rel: 'alternate', hreflang: 'zh', href: zhHref }])
  }

  return tags
}

export const PINNED_GUIDE_PATHS = [
  'guide/introduction',
  'guide/quickstart',
  'guide/pvm-deploy',
  'guide/bare-metal-deploy',
  'guide/kubernetes'
]

function isChineseLlmsUrl(url) {
  return /\/zh(\/|\.md(?:$|[)#?]))/.test(url)
}

function languageNeutralPath(url) {
  let path = normalizeSitemapPath(url).replace(/\.md$/i, '')
  if (path.startsWith('zh/')) path = path.slice(3)
  return path
}

function pinRank(url) {
  const index = PINNED_GUIDE_PATHS.indexOf(languageNeutralPath(url))
  return index === -1 ? PINNED_GUIDE_PATHS.length : index
}

function compareLlmsUrls(a, b) {
  const pin = pinRank(a) - pinRank(b)
  if (pin !== 0) return pin
  return a.localeCompare(b)
}

function itemUrl(line) {
  return line.match(/\((https?:\/\/[^)]+)\)/)?.[1] ?? ''
}

function sortByPinThenUrl(items, getUrl) {
  return [...items].sort((a, b) => compareLlmsUrls(getUrl(a), getUrl(b)))
}

export function regroupLlmsTxtByLanguage(source) {
  const headingAt = source.search(/^## /m)
  if (headingAt < 0) return source

  const header = source.slice(0, headingAt).trimEnd()
  const items = source
    .slice(headingAt)
    .split('\n')
    .filter((line) => line.startsWith('- ['))

  if (items.length === 0) return source

  const english = []
  const chinese = []
  for (const item of items) {
    const url = itemUrl(item)
    if (isChineseLlmsUrl(url)) chinese.push(item)
    else english.push(item)
  }

  const sections = []
  if (english.length) {
    sections.push(`## English\n\n${sortByPinThenUrl(english, itemUrl).join('\n')}`)
  }
  if (chinese.length) {
    sections.push(`## 简体中文\n\n${sortByPinThenUrl(chinese, itemUrl).join('\n')}`)
  }

  return `${header}\n\n${sections.join('\n\n')}\n`
}

function splitLlmsFullDocs(source) {
  return source
    .split(/(?=^---\nurl: )/m)
    .map((block) => block.trim())
    .filter(Boolean)
}

function llmsFullDocUrl(block) {
  const direct = block.match(/^---\nurl: (https?:\S+)/m)
  if (direct) return direct[1]
  const folded = block.match(/^---\nurl: >-\n[ \t]+(https?:\S+)/m)
  return folded?.[1] ?? ''
}

export function reorderLlmsFullByLanguage(source) {
  const docs = splitLlmsFullDocs(source)
  if (docs.length === 0) return source

  const english = []
  const chinese = []
  for (const doc of docs) {
    const url = llmsFullDocUrl(doc)
    if (isChineseLlmsUrl(url)) chinese.push(doc)
    else english.push(doc)
  }

  const ordered = [
    ...sortByPinThenUrl(english, llmsFullDocUrl),
    ...sortByPinThenUrl(chinese, llmsFullDocUrl)
  ]
  return `${ordered.join('\n\n')}\n`
}
