import { defineAsyncComponent } from 'vue'
import DefaultTheme from 'vitepress/theme'
import Layout from './Layout.vue'
import BlogIndex from './BlogIndex.vue'
import './blog.css'
import './home.css'

export default {
  extends: DefaultTheme,
  Layout,
  enhanceApp({ app }) {
    app.component('BlogIndex', BlogIndex)
    app.component(
      'ContributorsWall',
      defineAsyncComponent(() => import('./ContributorsWall.vue'))
    )
  }
}
