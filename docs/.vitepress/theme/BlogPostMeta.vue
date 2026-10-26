<script setup>
import { computed } from 'vue'
import { useData } from 'vitepress'

const { frontmatter, lang } = useData()

const isZh = computed(() => (lang.value || 'en').startsWith('zh'))

const labels = computed(() =>
  isZh.value
    ? { by: '作者：' }
    : { by: 'By' }
)

function formatDate(value) {
  if (!value) return ''
  const d = value instanceof Date ? value : new Date(value)
  if (isNaN(d.getTime())) return String(value)
  const y = d.getFullYear()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${y}.${m}.${day}`
}

const dateText = computed(() => formatDate(frontmatter.value.date))
const author = computed(() => frontmatter.value.author || '')
</script>

<template>
  <p v-if="author || dateText" class="blog-post-meta">
    <template v-if="author">
      {{ labels.by }} <span class="blog-post-meta-author">{{ author }}</span>
    </template>
    <template v-if="author && dateText"> | </template>
    <template v-if="dateText">
      {{ dateText }}
    </template>
  </p>
</template>
