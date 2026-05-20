# Blog Maintenance

This guide explains how to add, edit, and organise blog posts on the Cube Sandbox documentation site.

## File locations

All English blog posts live under `docs/blog/posts/`. Chinese translations live under `docs/zh/blog/posts/`. Each post is a single Markdown file.

```
docs/
├── blog/
│   └── posts/
│       └── 2026-05-18-my-post.md   ← English
docs/zh/
├── blog/
│   └── posts/
│       └── 2026-05-18-my-post.md   ← Chinese (same filename)
```

Use the date-prefixed naming convention `YYYY-MM-DD-slug.md` so posts sort naturally in the file system.

---

## Frontmatter reference

Every post starts with a YAML frontmatter block. The fields are:

```yaml
---
title: "Post title"
date: 2026-05-18          # publication date (YYYY-MM-DD)
author: "Author Name"     # displayed in the post meta line
description: "Post description shown as the excerpt in the blog list."

# --- optional fields ---

featured: true            # show this post in the Featured section at the top of the list
weight: 1                 # order among featured posts — lower number = higher position (default 9999)

type: external            # set to "external" for off-site posts
externalUrl: https://...  # required when type is external — "Read more" will open this URL
---
```

### Field details

| Field | Required | Default | Notes |
|---|---|---|---|
| `title` | yes | — | Shown in the list card and `<title>` tag |
| `date` | yes | — | Used for sorting non-featured posts (newest first) |
| `author` | no | — | Shown in the meta line below the title |
| `description` | no | — | Shown as excerpt in the list; falls back to body text |
| `featured` | no | `false` | Featured posts are pinned to the top of the list |
| `weight` | no | `9999` | Controls order within the Featured section |
| `type` | no | — | Set to `external` to make the post link off-site |
| `externalUrl` | no | — | Required when `type: external` |

---

## Creating a regular post

1. Create `docs/blog/posts/YYYY-MM-DD-slug.md`.
2. Add the frontmatter and write the post body in Markdown.
3. Create the matching Chinese file under `docs/zh/blog/posts/` with the same filename.

Example:

```markdown
---
title: "Example post title"
date: 2026-06-01
author: Author Name
description: Example post description.
---

# Example post title

Body text starts here …
```

---

## Creating a featured post

Add `featured: true` and optionally a `weight` to control its position:

```markdown
---
title: "Example post title"
date: 2026-06-01
author: Author Name
description: Example post description.
featured: true
weight: 1
---
```

Featured posts appear at the very top of the blog list regardless of date. Lower `weight` = higher position. Posts with the same `weight` are ordered by date.

---

## Creating an external post

Use `type: external` when the full article lives on another site. The Markdown file only needs a frontmatter block — the body is optional:

```markdown
---
title: "Example post title"
date: 2026-06-01
author: Author Name
description: Example post description.
type: external
externalUrl: https://example.com/your-post
---
```

Clicking the title or "Read more" opens `externalUrl` in a new tab.

---

## Ordering and sorting rules

1. **Featured posts** (any `weight`) always appear before non-featured posts, ordered by `weight` ascending.
2. **Non-featured posts** are ordered by `date` descending (newest first).
3. When searching, both sections are filtered by the keyword simultaneously.
