---
title: 部署相关排障
lang: zh-CN
---

# 部署相关排障

| 标题 | 描述 | 相关 Issues |
| --- | --- | --- |
| `/data/cubelet` 必须是 XFS（reflink） | `cubelet` 把 `/data/cubelet` 作为容器可写层的存储目录，依赖 XFS 的 reflink 特性。在 Ubuntu / Debian / WSL 等 ext4 根盘的环境上部署，one-click 前置检查会以 `not XFS` 报错退出。Workaround：用 loopback `.img` 格式化为 XFS 后挂到 `/data/cubelet`；生产建议挂独立 XFS 数据盘（100–300 GiB）；新装机器推荐 OpenCloudOS 9 / RHEL 系。 | [#311](https://github.com/TencentCloud/CubeSandbox/issues/311), [#245](https://github.com/TencentCloud/CubeSandbox/issues/245) |
