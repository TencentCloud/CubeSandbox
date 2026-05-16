# 故障排障

::: warning 必须同时提交中英文
本栏目所有投稿都必须同时包含 `docs/guide/troubleshooting/` 下的英文文件和 `docs/zh/guide/troubleshooting/` 下的中文文件。只更新单一语言的 PR 不会被合并。
:::

这里收录 Cube Sandbox 在部署、使用与运维过程中遇到的真实问题与解决方案。我们更欢迎可复现、可验证、可直接落地的排障经验。

## 适合收录的内容

- 部署失败与环境相关坑位
- 运行时报错、网络问题、鉴权问题
- 升级回归、版本兼容性说明与恢复方法
- 来自真实事故或高频咨询的 FAQ 式运维指南

## 如何贡献

1. 复制当前目录下的 `_template.md`，并改名为英文 kebab-case 文件名，例如 `e2b-api-401-timeout.md`。
2. 同时创建这两个文件：
   - `docs/guide/troubleshooting/<slug>.md`
   - `docs/zh/guide/troubleshooting/<slug>.md`
3. 中英文文件名必须保持一致，便于双语站点保持 URL 对应关系。
4. 按要求填写 frontmatter，并完成排障说明各章节。
5. 在中英文两个索引页的文章列表中各追加一行。
6. 提交 PR 时请补充足够背景，方便 reviewer 验证问题与修复方式。

## 命名与 frontmatter 规范

- 文件名必须使用英文 kebab-case。
- 不允许使用中文文件名。
- 中英文目录必须使用相同 slug。
- 两个语言版本的 frontmatter key 应保持一致。

```md
---
title: 反向代理场景下 E2B API 401 超时
author: your-github-id
date: 2026-05-14
tags:
  - api
  - auth
  - reverse-proxy
lang: zh-CN
---
```

## 已发布文章

| 标题 | 作者 | 日期 | 标签 |
| --- | --- | --- | --- |
| _在这里补充你的文章_ | - | - | - |
