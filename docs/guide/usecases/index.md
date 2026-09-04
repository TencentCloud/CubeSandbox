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

| Title | Author     | Date | Tags |
| --- |------------| --- | --- |
| [trpc-agent-go: A Secure Code Execution Backend Powered by Cube Sandbox](./trpc-agent-go.md) | joeyczheng | 2026-06-03 | agent, code-execution, e2b, golang |
| [Lexmount AI: Putting the Browser Runtime Inside the Agent Sandbox](./lexmount-browser-agent.md) | Xiong Xiuzhang | 2026-08-13 | agent, browser, browser-runtime, production |
| [Hermes Agent: Running a Resident Agent Platform in CubeSandbox](./hermes-agent.md) | Chen Jinbo | 2026-08-20 | agent, persistence, skills, host-mount |
| [Lenovo Cloud Agent: Sandbox Migration from Daytona to CubeSandbox](./lenovo-cloud-agent.md) | Li Jian | 2026-08-20 | agent, migration, daytona, e2b-compat |
| [Horizon Insights: Sandboxing Financial Research Agents](./horizon-insights.md) | Wang Zhengkai | 2026-08-26 | agent, financial, host-mount, cubeegress |
| [unisound: Stress-Testing Density Limits for RL Rollout](./unisound-rl-rollout.md) | Unisound Atlas Intelligent Computing Team | 2026-09-01 | agent, rl, rollout, density |
| [Guangdong Rising: Building a Multi-Tenant Sandbox Platform](./guangdong-rising.md) | Feng Jiaqi | 2026-09-03 | agent, multi-tenant, sandbox-platform, lifecycle |
