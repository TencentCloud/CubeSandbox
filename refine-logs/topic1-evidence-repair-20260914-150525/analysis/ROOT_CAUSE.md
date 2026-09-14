# ROOT_CAUSE — evidence integrity repair

## Confirmed defects in old sprint validator/docs

1. `accept.py` P95 mismatch branch was literal `pass` (false ACCEPT_PASS on raw/summary tamper).
2. Gate2 acceptance keyed on string `GATE2_DONE exit=0` instead of per-command exit codes.
3. `audit/VERIFY.md` PowerShell variables corrupted (non-copyable).
4. Example `VERIFY.md` had trailing whitespace (`git diff --check` dirty).
5. E3 summary `PASS` despite only unidirectional `prefer_first` (plan required prefer_a/prefer_b).
6. E4 summary-only `PASS` without splitting live bad-score vs unit Filter-before-Score vs unobserved negatives.
7. Live harness `drain()` destroyed all `cli list` sandbox IDs, not registry-only.
8. Manifest drift / incomplete coverage in old ledger (HYPOTHESIS: regenerated before E5/final artifacts).

## Repair actions

- New strict validator recomputes stats from raw, checks IDs/arms/phases/SHAs/metrics/restore/manifest.
- E3 → PARTIAL; E4 → PARTIAL with evidence split; no new live samples.
- Harness copy fixed; not executed live.
- Manifest generated last; validator outputs excluded from self-hash.

## Limits

- Original Prometheus scrapes were not persisted; metrics deltas derived from old summaries and labeled.
- No live cluster restore mutation in this repair; RESTORE asserts clean baseline + registry-only policy.
