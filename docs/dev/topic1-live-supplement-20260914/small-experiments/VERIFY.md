# VERIFY — 小实验补充（≈5 min）

实验名见 [`../REVIEWER_SUPPLEMENT.md`](../REVIEWER_SUPPLEMENT.md) / [`../STATUS.json`](../STATUS.json) 的 `experiments[].title`。

```powershell
$L = "D:\postgraduate\main-line\Job\Tencent\CubeSandbox\refine-logs\topic1-small-experiments-20260913-210144"
Get-Content "$L\STATUS.json"
Get-Content "$L\RESTORE.json"
python -c @"
import json, pathlib
L = pathlib.Path(r'$L')
st = json.loads((L/'STATUS.json').read_text(encoding='utf-8'))
for e in st.get('experiments') or []:
    print(e['title'], '->', e['status'], f\"({e['id']})\")
print('restore', st.get('restore'), '| formal_override', st.get('formal_override'))
"@
Get-FileHash "$L\audit\MANIFEST.sha256" | Format-List
```

期望：五项状态与 `REVIEWER_SUPPLEMENT.md` 表一致；`formal_override=false`；`RESTORE=PASS`。
