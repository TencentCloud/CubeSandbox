# 开发者文档

本章节面向**在 CubeSandbox 代码库上工作**的工程师——贡献者、维护者，以及内部服务（CubeMaster、CubeProxy、CubeAPI、Cubelet）的集成者。这里汇集了安全改动系统所需遵循的约定、契约与内部参考。

如果你是*使用* CubeSandbox（部署、制作模板、调用 API），建议从[指南](../guide/introduction)开始。[架构](../architecture/overview)章节讲解系统设计；本章节则更深入一层，聚焦代码编写与服务协作所遵循的规则。

## 约定

- [Redis Key 命名规范](./redis-key-spec)——所有服务在共享 Redis 实例上必须遵循的统一命名空间：命名格式、归属划分、已注册 Key 清单、TTL 策略，以及各服务的 key 构造模块。

## 调度

- [调度插件系统设计](./scheduler-plugin-design)——集群调度性能评估与高性能调度插件系统的设计文档：现状走读结论、指标定义、注册表与 Profile 配置机制、三种内置策略 Profile，以及 benchmark 方案。
- [调度代码阅读指南](./scheduler-code-reading-guide)——实现上述方案所需的调度代码导读：推荐阅读顺序、关键文件与符号、测试模板与工程规范。

## 适合放在这里的内容

- 跨服务的数据契约与命名约定（key、消息主题、schema）
- 内部模块边界与新增行为的入口（如 key 构造、缓存层）
- 针对内部服务的编码规范与贡献规则
- 预留未来补充：内部 API、测试约定、贡献指南

::: tip 双语同步
开发者文档同时维护英文（`docs/dev/`）与中文（`docs/zh/dev/`）。新增或修改页面时请保持两语言同步，并使用相同文件名以保证 URL 对齐。
:::
