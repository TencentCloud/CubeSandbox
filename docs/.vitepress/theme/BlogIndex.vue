<script setup>
import { computed } from 'vue'
import { useData } from 'vitepress'

const props = defineProps({
  posts: {
    type: Array,
    required: true
  },
  locale: {
    type: String,
    default: 'en'
  }
})

const { lang } = useData()

const i18n = computed(() => {
  const isZh = (props.locale || lang.value || 'en').startsWith('zh')
  return isZh
    ? { by: '作者：', more: '更多', empty: '暂无文章。', featured: '精选' }
    : { by: 'By', more: 'Read more', empty: 'No posts yet.', featured: 'Featured' }
})

const featuredPosts = computed(() =>
  props.posts.filter((p) => p.featured)
)

const nonFeaturedPosts = computed(() =>
  props.posts.filter((p) => !p.featured)
)

function postLink(post) {
  return post.isExternal ? post.externalUrl : post.url
}

function postTarget(post) {
  return post.isExternal ? '_blank' : undefined
}

function postRel(post) {
  return post.isExternal ? 'noopener noreferrer' : undefined
}
</script>

<template>
  <div class="blog-index">
    <div v-if="featuredPosts.length === 0 && nonFeaturedPosts.length === 0" class="blog-empty">
      {{ i18n.empty }}
    </div>

    <!-- Featured section -->
    <template v-if="featuredPosts.length">
      <article
        v-for="post in featuredPosts"
        :key="'featured-' + post.url"
        class="blog-card"
      >
        <h3 class="blog-card-title">
          <a
            :href="postLink(post)"
            :target="postTarget(post)"
            :rel="postRel(post)"
          >{{ post.title }}</a>
          <span class="blog-badge blog-badge-featured">{{ i18n.featured }}</span>
        </h3>
        <p class="blog-card-meta">
          <template v-if="post.author">
            {{ i18n.by }} <span class="blog-card-author">{{ post.author }}</span>
          </template>
          <template v-if="post.author && post.dateText"> | </template>
          <template v-if="post.dateText">
            {{ post.dateText }}
          </template>
        </p>
        <p v-if="post.excerpt" class="blog-card-excerpt">{{ post.excerpt }}</p>
        <a
          class="blog-card-more"
          :href="postLink(post)"
          :target="postTarget(post)"
          :rel="postRel(post)"
        >{{ i18n.more }}</a>
      </article>
    </template>

    <!-- Regular posts -->
    <template v-if="nonFeaturedPosts.length">
      <article v-for="post in nonFeaturedPosts" :key="post.url" class="blog-card">
        <h3 class="blog-card-title">
          <a
            :href="postLink(post)"
            :target="postTarget(post)"
            :rel="postRel(post)"
          >{{ post.title }}</a>
        </h3>
        <p class="blog-card-meta">
          <template v-if="post.author">
            {{ i18n.by }} <span class="blog-card-author">{{ post.author }}</span>
          </template>
          <template v-if="post.author && post.dateText"> | </template>
          <template v-if="post.dateText">
            {{ post.dateText }}
          </template>
        </p>
        <p v-if="post.excerpt" class="blog-card-excerpt">{{ post.excerpt }}</p>
        <a
          class="blog-card-more"
          :href="postLink(post)"
          :target="postTarget(post)"
          :rel="postRel(post)"
        >{{ i18n.more }}</a>
      </article>
    </template>
  </div>
</template>
