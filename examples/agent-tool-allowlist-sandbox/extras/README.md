# Advanced demos (optional)

Happy path is `../run.py`. Scripts here stack pause/resume, CIDR egress,
fan-out, and reference loops on top of the same template.

Run from the **example root** so imports resolve:

```bash
cd ..
python -m extras.tool_allowlist_limits   # or:
python extras/tool_allowlist_deny.py
```

| Script | Marker |
|--------|--------|
| `tool_allowlist_limits.py` | `LIMITS_DEMO_OK` |
| `tool_allowlist_deny.py` | host deny only |
| `tool_allowlist_allow.py` | artifact-ok |
| `tool_allowlist_guest_runner.py` | `GUEST_RUNNER_OK` |
| `tool_allowlist_checkpoint.py` | `CHECKPOINT_OK` |
| `tool_allowlist_egress.py` | `EGRESS_STACK_OK` |
| `tool_allowlist_fanout.py` | `FANOUT_OK` |
| `tool_agent_loop.py` | `AGENT_LOOP_OK` |
| `verify_template.py` | older smoke; prefer `../run.py` → `RUN_OK` |
