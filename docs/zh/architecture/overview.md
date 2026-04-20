# 架构概览

Cube Sandbox 遵循清晰的自上而下分层架构。

## 分层架构

![Cube Sandbox 架构图](../../assets/cube-sandbox-arch.png)

## 核心组件

1. **CubeAPI**: 兼容 E2B REST API 网关，替换 URL 等环境变量即可从 E2B 云无缝切换到 Cube Sandbox。
2. **CubeMaster**: 编排调度器，接收 E2B API 请求并分发到对应 Cubelet，负责资源调度、集群状态维护。
3. **CubeProxy**: 反向代理与请求路由组件，通过解析 Host 头中 `<port>-<sandbox_id>.<domain>` 的格式，将来自 SDK 客户端的请求转发到对应的沙箱实例。
4. **Cubelet**: 计算节点本地调度组件，管理单节点所有沙箱实例的完整生命周期。
5. **CubeVS**: 基于 eBPF 内核态转发，网络层面提供完整的隔离机制与安全策略支持。
6. **CubeHypervisor & CubeShim**: Cube 沙箱的虚拟化层。CubeHypervisor 负责管理 KVM MicroVM，CubeShim 实现 containerd Shim v2 接口，将沙箱集成到容器运行时。