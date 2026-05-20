# Use Cases

::: warning Bilingual PR Required
Every contribution in this section must include both an English file under `docs/guide/usecases/` and a Chinese file under `docs/zh/guide/usecases/`. PRs that update only one language will not be merged.
:::

This section collects real-world Cube Sandbox adoption stories, production patterns, and solution write-ups. Good submissions explain the business context, why Cube Sandbox was chosen, and the outcome it enabled.

## What belongs here

- Real business scenarios powered by Cube Sandbox
- Architecture notes for production deployments
- Migration stories from other sandbox or code execution platforms
- Internal tools, agent workflows, and engineering enablement cases

## How to contribute

1. Copy `_template.md` in the current directory and rename it to an English kebab-case slug such as `browser-agent-in-production.md`.
2. Create both files at the same time:
   - `docs/guide/usecases/<slug>.md`
   - `docs/zh/guide/usecases/<slug>.md`
3. Keep the filename identical in both languages to keep the URLs aligned.
4. Fill in the required frontmatter fields and describe the scenario with concrete details.
5. Add your article to the table below in both the English and Chinese index pages.
6. Open a PR and mention any related example repo, architecture diagram, or demo if available.

## Naming and frontmatter

- Filenames must use English kebab-case.
- Chinese filenames are not allowed.
- Use the same slug in both language directories.
- Keep frontmatter keys aligned across both files.

```md
---
title: Browser Agent for Internal QA Workflows
author: your-github-id
date: 2026-05-14
tags:
  - browser
  - qa
  - production
lang: en-US
---
```

## Published articles

| Title | Author | Date | Tags |
| --- | --- | --- | --- |
| _Add your article here_ | - | - | - |
