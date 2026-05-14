# Integrations

::: warning Bilingual PR Required
Every contribution in this section must include both an English file under `docs/guide/integrations/` and a Chinese file under `docs/zh/guide/integrations/`. PRs that update only one language will not be merged.
:::

This section collects integration guides for agent frameworks, developer tools, and ecosystem platforms built on top of Cube Sandbox. Each integration should live in its own file so contributors can submit focused PRs without editing a shared monolithic page.

## What belongs here

- Agent framework integrations such as LangChain, Dify, OpenClaw, or Claude Code
- SDK wiring guides and platform-specific setup notes
- End-to-end integration patterns with example repositories
- Compatibility notes, caveats, and recommended configuration defaults

## How to contribute

1. Copy `_template.md` in the current directory and rename it to an English kebab-case slug such as `langchain.md` or `claude-code.md`.
2. Create both files at the same time:
   - `docs/guide/integrations/<slug>.md`
   - `docs/zh/guide/integrations/<slug>.md`
3. Keep the filename identical in both languages to keep the URLs aligned.
4. One integration per file. Do not merge multiple agents or frameworks into the same article.
5. Add your article to the table below in both the English and Chinese index pages.
6. Open a PR with any sample code, repo links, or screenshots that help reviewers validate the guide.

## Naming and frontmatter

- Filenames must use English kebab-case.
- Chinese filenames are not allowed.
- Use the same slug in both language directories.
- Keep frontmatter keys aligned across both files.

```md
---
title: LangChain Integration Guide
author: your-github-id
date: 2026-05-14
tags:
  - integration
  - langchain
lang: en-US
---
```

## Published articles

| Title | Author | Date | Tags |
| --- | --- | --- | --- |
| _Add your article here_ | - | - | - |
