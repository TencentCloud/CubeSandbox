# 生态集成

::: warning 必须同时提交中英文
本栏目所有投稿都必须同时包含 `docs/guide/integrations/` 下的英文文件和 `docs/zh/guide/integrations/` 下的中文文件。只更新单一语言的 PR 不会被合并。
:::

这里收录 Cube Sandbox 面向 Agent 框架、开发工具与生态平台的集成指南。每个集成对象应单独占用一个文件，方便贡献者提交聚焦、低冲突的 PR。

## 适合收录的内容

- LangChain、Dify、OpenClaw、Claude Code 等 Agent 框架集成
- SDK 接线方式与平台相关配置说明
- 带示例仓库的端到端集成方案
- 兼容性说明、限制条件与推荐配置

## 如何贡献

1. 复制当前目录下的 `_template.md`，并改名为英文 kebab-case 文件名，例如 `langchain.md` 或 `claude-code.md`。
2. 同时创建这两个文件：
   - `docs/guide/integrations/<slug>.md`
   - `docs/zh/guide/integrations/<slug>.md`
3. 中英文文件名必须保持一致，便于双语站点保持 URL 对应关系。
4. 一个集成对象一篇文章，不要把多个 Agent 或框架合并到同一篇里。
5. 在中英文两个索引页的文章列表中各追加一行。
6. 发起 PR 时请附带示例代码、仓库链接或截图，帮助 reviewer 验证指南可用性。

## 命名与 frontmatter 规范

- 文件名必须使用英文 kebab-case。
- 不允许使用中文文件名。
- 中英文目录必须使用相同 slug。
- 两个语言版本的 frontmatter key 应保持一致。

```md
---
title: LangChain 集成指南
author: your-github-id
date: 2026-05-14
tags:
  - integration
  - langchain
lang: zh-CN
---
```

## 已发布文章

| 标题 | 作者 | 日期 | 标签 |
| --- | --- | --- | --- |
| _在这里补充你的文章_ | - | - | - |
