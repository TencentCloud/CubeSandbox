---
title: 模板相关排障
lang: zh-CN
---

# 模板相关排障

| 标题 | 描述 | 相关 Issues |
| --- | --- | --- |
| 自定义模板制作 `tpl create-from-image` 总是超时 | 两类主因：① 自定义镜像里没有 envd 或未在容器启动时拉起（默认 readiness probe 打的就是 envd 的 `49983/health`，未起则 `connection refused` 到超时）；② 在 AWS EC2 等嵌套虚拟化环境部署，受 XSAVE 等指令集缺失导致 MicroVM panic、嵌套页错误慢启动撞 `VsockServerReady` / probe budget。解法分别是按 [自带镜像接入](../tutorials/bring-your-own-image.md) 教程做镜像、以及切到 PVM 部署。 | [#312](https://github.com/TencentCloud/CubeSandbox/issues/312), [#95](https://github.com/TencentCloud/CubeSandbox/issues/95), [#94](https://github.com/TencentCloud/CubeSandbox/issues/94), [#161](https://github.com/TencentCloud/CubeSandbox/issues/161), [#253](https://github.com/TencentCloud/CubeSandbox/issues/253) |
| 磁盘空间不足导致模板制作失败 | 制作模板时需要把 OCI 镜像解压并写入到磁盘文件，会占用大量临时空间。当 `/tmp`、`/data/cubelet` 或 `/usr/local/services/cubetoolbox/` 所在分区空间不足时，模板可能卡在 `UNPACKING` / `BUILDING_EXT4` 阶段，或表现为 mkfs.ext4 校验和不匹配、inode 类型冲突等错误。 | [#240](https://github.com/TencentCloud/CubeSandbox/issues/240), [#251](https://github.com/TencentCloud/CubeSandbox/issues/251) |
| 沙箱网段和局域网冲突导致创建模板超时 | one-click 部署默认沙箱网段是 `192.168.0.0/18`。如果宿主机局域网也使用 `192.168.1.x`，Cube 可能给沙箱分配到和真实局域网重叠的 IP 导致模板创建或端口探测以 `context deadline exceeded` 失败。将 Cubelet CIDR 改成不冲突的网段，并在重启前清理旧 TAP 网卡和 `cube-dev`。 | [指南](./local-network-cidr-conflict.md) |