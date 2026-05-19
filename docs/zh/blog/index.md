---
sidebar: false
title: 博客
---

<script setup>
import { data as posts } from './posts.data.js'
</script>

<BlogIndex :posts="posts" locale="zh" />
