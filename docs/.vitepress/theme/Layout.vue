<script setup>
import DefaultTheme from 'vitepress/theme'
import BlogPostMeta from './BlogPostMeta.vue'
import { useRoute, useData } from 'vitepress'
import { computed, onMounted, watch } from 'vue'

const { Layout } = DefaultTheme

const route = useRoute()
const { frontmatter } = useData()

const isBlogPost = computed(() =>
  /\/(zh\/)?blog\/posts\//.test(route.path)
)

const isZh = computed(() => route.path.startsWith('/zh/'))

const contribute = computed(() => isZh.value
  ? { text: '如果你有关于 CubeSandbox 的文章想贡献，欢迎提交 PR！', link: 'https://github.com/TencentCloud/CubeSandbox/contribute', linkText: '前往 GitHub 贡献' }
  : { text: 'Have an article about CubeSandbox you\'d like to share?', link: 'https://github.com/TencentCloud/CubeSandbox/contribute', linkText: 'Contribute on GitHub' }
)

// For external-link blog posts: open the external URL in a new tab and
// navigate back so the blank VitePress page is never shown.
const handleExternalPost = () => {
  const url = frontmatter.value?.externalUrl
  if (!url || typeof window === 'undefined') return
  window.open(url, '_blank', 'noopener,noreferrer')
  if (window.history.length > 1) {
    window.history.back()
  } else {
    // No history to go back to (e.g. direct URL access) — go to blog index.
    const blogIndex = route.path.startsWith('/zh/') ? '/zh/blog/' : '/blog/'
    window.location.replace(blogIndex)
  }
}
onMounted(handleExternalPost)
watch(() => frontmatter.value?.externalUrl, (url) => {
  if (url) handleExternalPost()
})
</script>

<template>
  <Layout>
    <template v-if="isBlogPost" #doc-before>
      <BlogPostMeta />
    </template>
    <template v-if="isBlogPost" #doc-after>
      <div class="blog-contribute">
        <span class="blog-contribute-text">{{ contribute.text }}</span>
        <a
          :href="contribute.link"
          target="_blank"
          rel="noopener noreferrer"
          class="blog-contribute-link"
        >{{ contribute.linkText }} →</a>
      </div>
    </template>
  </Layout>
</template>
