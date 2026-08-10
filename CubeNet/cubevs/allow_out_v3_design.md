# allow_out_v3 设计（已被完整方案文档取代）

本文档原描述 `allow_out_v2 → allow_out_v3` 的 eBPF 数据面迁移。它现在是「CubeEgress 自定义
端口支持」完整方案的一部分。

**请改读 [`cubeegress_custom_port_design.md`](./cubeegress_custom_port_design.md)** —— 该文档覆盖端到端
全栈（`CubeAPI` 配置模型 → `CubeMaster` 校验/聚合 → `network-agent` 传输 →
`CubeEgress`(Lua + iptables) → `CubeNet/cubevs` eBPF `allow_out_v3`），并整合了提交
`9e41c62`（`CubeEgress rules support port configuration`）的全部改动。

本文件中 `allow_out_v3` 相关的 key/value 结构、`lpm_key_v3` 编码规则、数据面
`classify_egress_flow`（由原 `l7_scheme_for_flow` 与 `session_policy_allowed` 合并而来，单次
`(ip, port)/48` 查找）、用户态落表、兼容/平滑升级、字节序与 ABI 守卫等内容，均已并入
上述完整方案文档的 §6 / §7 / §8，与此处保持一致。
