# VERIFY（≈5 min）

`powershell
 = D:\postgraduate\main-line\Job\Tencent\CubeSandbox\refine-logs\topic1-productization-sprint-20260914-104856
Get-Content \STATUS.json
Get-Content \RESTORE.json
python \audit\accept.py --ledger " --out \audit\acceptance_result.json
python -m unittest \audit\test_accept.py -v
# expect ACCEPT_PASS; original #1666 head still 2668a520…
`
