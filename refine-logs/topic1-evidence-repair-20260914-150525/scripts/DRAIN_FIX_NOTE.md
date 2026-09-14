# drain() 修正说明

原 harness 在清空 `CREATED` 后，仍会解析 `cli list` 中全部 32 位十六进制 ID 并执行销毁。

本目录中的副本仅销毁本轮 registry / `CREATED` 已登记对象；若清理后仍有残留 ID，则中止新负载并转入只读诊断，不删除未登记对象。

该修正未在 live 集群上执行。
