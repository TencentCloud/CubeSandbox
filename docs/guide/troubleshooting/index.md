# Troubleshooting

::: warning Bilingual PR Required
Every contribution in this section must include both an English file under `docs/guide/troubleshooting/` and a Chinese file under `docs/zh/guide/troubleshooting/`. PRs that update only one language will not be merged.
:::

This section collects practical troubleshooting write-ups for Cube Sandbox deployments and daily operations. Preferred submissions are concrete, reproducible, and directly actionable.

## What belongs here

- Deployment failures and environment-specific pitfalls
- Runtime errors, networking issues, and authentication problems
- Upgrade regressions, version compatibility notes, and recovery steps
- FAQ-style operational guidance grounded in real incidents

## How to contribute

1. Copy `_template.md` in the current directory and rename it to an English kebab-case slug such as `e2b-api-401-timeout.md`.
2. Create both files at the same time:
   - `docs/guide/troubleshooting/<slug>.md`
   - `docs/zh/guide/troubleshooting/<slug>.md`
3. Keep the filename identical in both languages to keep the URLs aligned.
4. Fill in the required frontmatter fields and complete the troubleshooting sections.
5. Add your article to the table below in both the English and Chinese index pages.
6. Open a PR with enough context for reviewers to validate the issue and fix.

## Naming and frontmatter

- Filenames must use English kebab-case.
- Chinese filenames are not allowed.
- Use the same slug in both language directories.
- Keep frontmatter keys aligned across both files.

```md
---
title: E2B API 401 Timeout Behind Reverse Proxy
author: your-github-id
date: 2026-05-14
tags:
  - api
  - auth
  - reverse-proxy
lang: en-US
---
```

## Published articles

| Title | Author | Date | Tags |
| --- | --- | --- | --- |
| _Add your article here_ | - | - | - |
