# VERIFY（证据修复 ledger）

```powershell
$WT = 'D:/postgraduate/main-line/Job/Tencent/CubeSandbox-worktrees/topic1-evidence-repair-20260914-150525'
$LEDGER = 'D:/postgraduate/main-line/Job/Tencent/CubeSandbox/refine-logs/topic1-evidence-repair-20260914-150525'
$env:GOCACHE = "$WT/CubeMaster/.gocache-repair"

git -c safe.directory=$WT -C $WT diff --check
python "$LEDGER/audit/accept.py" --ledger "$LEDGER" --out "$LEDGER/audit/result-1.json"
python "$LEDGER/audit/accept.py" --ledger "$LEDGER" --out "$LEDGER/audit/result-2.json"
python -m unittest "$LEDGER/audit/test_accept.py" -v
Get-FileHash "$LEDGER/audit/result-1.json","$LEDGER/audit/result-2.json" -Algorithm SHA256
```

期望：`ACCEPT_PASS`；两次 result SHA 一致；负例矩阵全部 `ACCEPT_FAIL`。
