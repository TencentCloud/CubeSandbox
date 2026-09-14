# ROOT_CAUSE

本轮工程目标均达标，无未达标指标的三轮根因循环。

若后续复现出现 healthy P95 退化：
1. 测量：核对 warm-up、arm 隔离、fault 清零、二进制 SHA
2. 定位：拆 client / scorer duration histogram / ready
3. 最小修复：仅限 example 启动/连接复用/文档 timeout 一致性
