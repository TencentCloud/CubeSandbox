# 验收命令

工作树与 ledger 路径如下（可按本机位置替换）。

```powershell
$WT = 'D:/postgraduate/main-line/Job/Tencent/CubeSandbox-worktrees/topic1-evidence-repair-20260914-150525'
$LEDGER = "$WT/refine-logs/topic1-evidence-repair-20260914-150525"
$env:GOCACHE = "$WT/CubeMaster/.gocache-repair"

git -c safe.directory=$WT -C $WT diff --check
python "$LEDGER/audit/accept.py" --ledger "$LEDGER" --out "$LEDGER/audit/result-1.json"
python "$LEDGER/audit/accept.py" --ledger "$LEDGER" --out "$LEDGER/audit/result-2.json"
python -m unittest "$LEDGER/audit/test_accept.py" -v
Get-FileHash "$LEDGER/audit/result-1.json","$LEDGER/audit/result-2.json" -Algorithm SHA256
```

预期结果：`ACCEPT_PASS`；两次 result 的 SHA256 相同；负例用例均返回 `ACCEPT_FAIL`。
