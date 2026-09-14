# VERIFY — 小实验补充结论包

实验名见 [`REVIEWER_SUPPLEMENT.md`](./REVIEWER_SUPPLEMENT.md) / [`STATUS.json`](./STATUS.json) 的 `experiments[].title`。

本页只核对 **本目录已入库** 的 STATUS / summaries。原始机房 ledger **未**随本 PR 发布。

```powershell
# From repo root:
$L = ".\docs\dev\topic1-live-supplement-20260914\small-experiments"
Get-Content "$L\STATUS.json"
Get-Content "$L\RESTORE.json"
Get-ChildItem "$L\summaries\*.json"
```

期望：五项状态与 `REVIEWER_SUPPLEMENT.md` 表一致；`formal_override=false`；`RESTORE=PASS`。

可运行的 sidecar / fault 演示见仓库内 `CubeMaster/examples/external-http-score/VERIFY.md`。
