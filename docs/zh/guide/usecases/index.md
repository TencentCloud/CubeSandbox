# 应用案例

::: warning 必须同时提交中英文
本栏目所有投稿都必须同时包含 `docs/guide/usecases/` 下的英文文件和 `docs/zh/guide/usecases/` 下的中文文件。只更新单一语言的 PR 不会被合并。
:::

这里收录 Cube Sandbox 在真实业务中的落地方案、生产实践与经验总结。高质量案例应当说明业务背景、为什么选择 Cube Sandbox，以及最终带来了什么结果。

## 适合收录的内容

- 基于 Cube Sandbox 的真实业务场景
- 面向生产部署的方案设计与架构经验
- 从其他沙箱或代码执行平台迁移到 Cube 的故事
- 内部工具、Agent 工作流与研发效能实践

## 如何贡献

1. 复制当前目录下的 `_template.md`，并改名为英文 kebab-case 文件名，例如 `browser-agent-in-production.md`。
2. 同时创建这两个文件：
   - `docs/guide/usecases/<slug>.md`
   - `docs/zh/guide/usecases/<slug>.md`
3. 中英文文件名必须保持一致，便于双语站点保持 URL 对应关系。
4. 按要求填写 frontmatter，并用具体信息描述业务场景与方案。
5. 在中英文两个索引页的文章列表中各追加一行。
6. 发起 PR 时如有示例仓库、架构图或演示链接，请一并说明。

## 命名与 frontmatter 规范

- 文件名必须使用英文 kebab-case。
- 不允许使用中文文件名。
- 中英文目录必须使用相同 slug。
- 两个语言版本的 frontmatter key 应保持一致。

```md
---
title: 面向内部 QA 流程的 Browser Agent 方案
author: your-github-id
date: 2026-05-14
tags:
  - browser
  - qa
  - production
lang: zh-CN
---
```

## 已发布文章

| 标题 | 作者 | 日期 | 标签 |
| --- | --- | --- | --- |
| _在这里补充你的文章_ | - | - | - |
