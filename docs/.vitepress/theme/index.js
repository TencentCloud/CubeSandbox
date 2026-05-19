import DefaultTheme from 'vitepress/theme'
import Layout from './Layout.vue'
import BlogIndex from './BlogIndex.vue'
import './blog.css'

export default {
  extends: DefaultTheme,
  Layout,
  enhanceApp({ app }) {
    app.component('BlogIndex', BlogIndex)
  }
}
