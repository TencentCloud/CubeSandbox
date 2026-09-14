# VERIFY（产品化结论包）

本页只核对 **本目录已入库的结论文件**。可运行的 operator 最短路径请用仓库内示例：

[`CubeMaster/examples/external-http-score/VERIFY.md`](../../../../CubeMaster/examples/external-http-score/VERIFY.md)

```powershell
# From repo root (docs path relative to this file's package):
Get-Content .\docs\dev\topic1-live-supplement-20260914\productization\STATUS.json
Get-Content .\docs\dev\topic1-live-supplement-20260914\productization\RESTORE.json
Get-ChildItem .\docs\dev\topic1-live-supplement-20260914\productization\summaries\*.json
```

期望：`STATUS.json` 中 operator / hot-path / signal / Filter 相关项为 PASS 或已声明的 PARTIAL/NO_IMPROVEMENT；`RESTORE.json` 为 PASS；`binpack_probe` 保持 NO_IMPROVEMENT（不覆盖正式 `VALID_NO_IMPROVEMENT`）。

**不**要求也不提供对私有 `refine-logs/...` 账本的 `accept.py` 重跑。
