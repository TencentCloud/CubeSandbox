import { createContentLoader } from 'vitepress'

function formatDate(value) {
  if (!value) return { time: 0, text: '' }
  const d = value instanceof Date ? value : new Date(value)
  if (isNaN(d.getTime())) return { time: 0, text: String(value) }
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return { time: d.getTime(), text: `${y}.${m}.${day}` }
}

function pickExcerpt(frontmatter, excerpt, src) {
  if (frontmatter.description) return frontmatter.description
  if (excerpt) {
    return excerpt
      .replace(/<[^>]+>/g, '')
      .replace(/\s+/g, ' ')
      .trim()
      .slice(0, 220)
  }
  if (src) {
    return src
      .replace(/^---[\s\S]*?---/, '')
      .replace(/[#>*_`>\-]/g, '')
      .replace(/\[([^\]]+)\]\([^)]+\)/g, '$1')
      .replace(/\s+/g, ' ')
      .trim()
      .slice(0, 220)
  }
  return ''
}

export default createContentLoader('blog/posts/*.md', {
  excerpt: true,
  transform(raw) {
    return raw
      .map(({ url, frontmatter, excerpt, src }) => {
        const d = formatDate(frontmatter.date)
        return {
          title: frontmatter.title || url,
          url,
          author: frontmatter.author || '',
          date: d.time,
          dateText: d.text,
          excerpt: pickExcerpt(frontmatter, excerpt, src),
          featured: !!frontmatter.featured,
          weight: typeof frontmatter.weight === 'number' ? frontmatter.weight : 9999,
          externalUrl: frontmatter.externalUrl || null,
          isExternal: frontmatter.type === 'external' && !!frontmatter.externalUrl
        }
      })
      .sort((a, b) => {
        if (a.featured !== b.featured) return a.featured ? -1 : 1
        if (a.featured && b.featured) return a.weight - b.weight
        return b.date - a.date
      })
  }
})
