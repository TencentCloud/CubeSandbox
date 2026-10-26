---
sidebar: false
title: Blog
---

<script setup>
import { data as posts } from './posts.data.js'
</script>

<BlogIndex :posts="posts" locale="en" />
