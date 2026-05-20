# 博客维护

本文介绍如何在 Cube Sandbox 文档站点上新建、编辑和管理博客文章。

## 文件位置

英文博客文章存放于 `docs/blog/posts/`，中文版存放于 `docs/zh/blog/posts/`，两者文件名保持一致。

```
docs/
├── blog/
│   └── posts/
│       └── 2026-05-18-my-post.md   ← 英文
docs/zh/
├── blog/
│   └── posts/
│       └── 2026-05-18-my-post.md   ← 中文（同名文件）
```

文件名建议使用 `YYYY-MM-DD-slug.md` 的日期前缀格式，便于在文件系统中自然排序。

---

## Frontmatter 字段说明

每篇文章以 YAML frontmatter 开头，支持以下字段：

```yaml
---
title: "文章标题"
date: 2026-05-18          # 发布日期（YYYY-MM-DD）
author: "作者姓名"         # 显示在文章元信息行
description: "文章摘要，展示在博客列表卡片中。"

# --- 可选字段 ---

featured: true            # 将此文章放置在列表顶部的精选区域
weight: 1                 # 精选文章的排列顺序，数字越小越靠前（默认 9999）

type: external            # 设为 "external" 表示这是一篇外链文章
externalUrl: https://...  # type 为 external 时必填，"更多" 按钮将跳转到此地址
---
```

### 字段详情

| 字段 | 是否必填 | 默认值 | 说明 |
|---|---|---|---|
| `title` | 是 | — | 在列表卡片和页面 `<title>` 中显示 |
| `date` | 是 | — | 用于非精选文章的排序（越新越靠前） |
| `author` | 否 | — | 显示在标题下方的元信息行 |
| `description` | 否 | — | 列表摘要；不填则从正文截取 |
| `featured` | 否 | `false` | 精选文章固定显示在列表顶部 |
| `weight` | 否 | `9999` | 控制精选区内的排列顺序 |
| `type` | 否 | — | 设为 `external` 可跳转到外部链接 |
| `externalUrl` | 否 | — | `type: external` 时必填 |

---

## 新建普通文章

1. 创建 `docs/blog/posts/YYYY-MM-DD-slug.md`
2. 填写 frontmatter，然后用 Markdown 撰写正文
3. 在 `docs/zh/blog/posts/` 下创建同名中文文件

示例：

```markdown
---
title: "这是一段示例标题"
date: 2026-06-01
author: 作者姓名
description: 这是一段示例描述。
---

# 这是一段示例标题

正文从这里开始……
```

---

## 新建精选文章

添加 `featured: true`，并通过 `weight` 控制显示顺序：

```markdown
---
title: "这是一段示例标题"
date: 2026-06-01
author: 作者姓名
description: 这是一段示例描述。
featured: true
weight: 1
---
```

精选文章始终显示在博客列表顶部，不受发布日期影响。`weight` 越小排位越靠前；`weight` 相同时按日期倒序排列。

---

## 新建外链文章

当文章全文发布在其他平台时，使用 `type: external`。Markdown 文件只需要 frontmatter，正文可省略：

```markdown
---
title: "这是一段示例标题"
date: 2026-06-01
author: 作者姓名
description: 这是一段示例描述。
type: external
externalUrl: https://example.com/your-post
---
```

点击标题或"更多"按钮将在新标签页打开 `externalUrl`。

---

## 排序规则

1. **精选文章**（任意 `weight`）始终排在非精选文章之前，按 `weight` 升序排列。
2. **普通文章**按 `date` 降序排列（最新的在最前面）。
3. 搜索时，精选区和普通区同时按关键词过滤。
