# 根因与修复说明

## 已核实问题

1. 旧版 `accept.py` 在 raw 与 summary 的 P95 不一致时仍执行空分支，导致篡改后可能得到 `ACCEPT_PASS`。
2. Gate2 以日志中的 `GATE2_DONE exit=0` 字符串作为通过条件，未核对各命令的真实退出码。
3. 旧 ledger 中 `audit/VERIFY.md` 的 PowerShell 变量损坏，命令无法直接复制执行。
4. example `VERIFY.md` 存在 trailing whitespace，`git diff --check` 不通过。
5. E3 计划要求 `prefer_a` / `prefer_b` 两臂，实际仅有单向 `prefer_first`，却记为 `PASS`。
6. E4 仅凭摘要记为 `PASS`，未区分 live bad-score、Filter-before-Score 单测保证，以及未直接观测的负例。
7. live harness 的 `drain()` 会销毁 `cli list` 中全部 sandbox，而非仅清理本轮 registry 登记对象。
8. 旧 manifest 存在哈希漂移且覆盖不全（推测为 E5/终态追加后未重新生成，缺少命令时间线，标为假设）。

## 本次处理

- 重写验收脚本：由 raw 重算统计量，并检查 request ID、臂/阶段/行数、SHA、metrics reason、restore 字段与 manifest 覆盖；配套负例矩阵。
- E3、E4 结论改为 `PARTIAL`，并写明证据边界；未追加 live 样本。
- 在 ledger 内保留 registry-only 的 `drain()` 修正副本，未在 live 集群上执行以“证明”该修正。
- manifest 最后生成，并排除 validator 自身输出，避免自引用哈希。

## 边界

- 原始 Prometheus scrape 未随旧 sprint 落盘；本次 metrics 增量由旧 summary 派生，并在文件中标注来源。
- 本修复未对 live 集群做恢复类变更；`RESTORE.json` 记录的是与基线一致的清洁状态及 registry-only 清理策略。
