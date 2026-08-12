# Design: Set Alias on an Existing Template

> Single source of truth for the `PUT /templates/:id/alias` feature (PR #1120,
> building on #749). Code comments cite this document by section (e.g.
> `design §3.3`); keep the sections stable when editing.

## 1. Motivation

PR #749 attaches a template alias **at create time only**. This feature lets an
operator **set, reassign, or clear** the alias of an *already-existing*
template via `PUT /templates/{templateID}/alias`, closing the gap.

## 2. Layer ownership

| Layer | Responsibility |
|---|---|
| **CubeMaster (Go)** | Owns the alias rule, the READY requirement, the release+claim+confirm transaction, and the error → `ret_code` mapping. **Single source of truth.** |
| **CubeAPI (Rust)** | Forwards the call; maps `ret_code` → HTTP status. Does **not** re-validate the alias. |
| **SDKs (Go / Node / Python)** | Thin wrappers over the endpoint. No client-side alias validation. |
| **openapi.yml** | Documents the public REST surface (request/response/status codes). |

## 3. Contract

### 3.1 Alias semantics

`alias` is **optional**. Absent / `null` / `""` (after trim) means **clear** —
`display_name` is set to `''`, so the `alias_key` generated column becomes
`NULL` and is exempt from the unique index. A non-empty value means **claim**.

### 3.2 Endpoint

```
PUT /templates/{templateID}/alias
Content-Type: application/json
{ "alias": "<string|null>" }
→ 200 TemplateDetail   (the post-update template, same shape as GET /templates/:id)
```

### 3.3 Error mapping

| Condition | Store sentinel | `ret_code` | HTTP |
|---|---|---|---|
| Invalid alias format | `ErrInvalidAlias` | `130400` MasterParamsError | **400** |
| Target is a snapshot | `ErrAliasNotApplicableToSnapshot` | `130400` MasterParamsError | **400** |
| Not found / `DELETING` | `ErrTemplateNotFound` | `130404` NotFound | **404** |
| Target not `READY` | `ErrTemplateNotReady` | `130409` Conflict | **409** |
| Duplicate (concurrent claim) | `IsDuplicateAliasError` | `130409` Conflict | **409** |
| Anything else | — | `130593` MasterInternalError | **500** |

CubeAPI derives the HTTP status from `ret_code` via `map_err` (`is_params_error`
→ 400, `is_not_found` → 404, `is_conflict` → 409); adding a new sentinel only
needs a `ret_code` that one of those classifiers recognizes.

### 3.4 Validation rule (single source)

An alias must match `^[a-z0-9][a-z0-9-]{0,63}$` and **not** start with `tpl-` /
`snap-` (including the bare prefixes). Enforced **only** by CubeMaster's
`validateTemplateAlias` (`request_validation.go`); CubeAPI and the SDKs do not
re-validate, so the rule cannot drift between layers. `validateTemplateAlias` is
shared with the create/normalize path, so this also tightens create-time aliases
— the bare prefixes `tpl-`/`snap-` (which collide with ID prefixes) are now
rejected on create too, not just on set.

### 3.5 READY requirement

- **Claim** requires the target to be `READY`; any other status →
  `ErrTemplateNotReady` (409) — an alias must never resolve to a building or
  failed template, and the `READY` predicate is enforced *inside* the claim
  transaction (`claimTemplateAlias(..., requireReady=true)`), not only by the
  out-of-transaction `GetDefinition` check.
- **Clear** is allowed for any non-`DELETING` template (including `FAILED`),
  so an alias stuck on a failed template can be released without deleting the
  template.

Note: READY constrains only the *target* of an operator `set_alias`. It does
**not** by itself stop a *different* template's in-flight create/redo from
reclaiming an alias at build completion — that is handled by the job-sync +
finalize re-read invariant in §3.6.

### 3.6 Concurrency / atomicity

`claimTemplateAlias` runs **release + claim + confirm** in one transaction:

1. **Release** — `UPDATE display_name='' WHERE alias_key=? AND template_id<>target`.
2. **Claim** — `UPDATE display_name=alias WHERE template_id=target` (operator
   path additionally requires `status='READY'`; create/redo path requires
   `status<>'DELETING'` so it can claim at `PARTIALLY_READY`).
3. **Confirm** — `SELECT COUNT(*) WHERE template_id=target AND alias_key=alias`
   (plus the same status predicate). If `0`, the transaction rolls back the
   release (another template's alias is never silently cleared): the operator
   path distinguishes "exists but not READY" (409) from "gone" (404). A SELECT
   (not `RowsAffected`) avoids MySQL's rows-changed conflation with idempotent
   re-claims.

Two concurrent swaps between the same pair of templates can deadlock (MySQL
`1205`/`1213`, PostgreSQL `40P01`/`55P03`); the transaction is retried once.

**Anti-steal-back invariant (operator vs create/redo).** A create/redo
completion claims the alias captured in the job's `RequestJSON` *at submit
time*. To keep an operator `set_alias` from being silently reverted by a build
that was already running (or starts later), `SetTemplateAlias` syncs the
operator's change into the affected jobs' `RequestJSON`
(`syncTemplateImageJobAlias` for the target/old holder;
`clearAliasFromOtherInProgressJobs` for other in-progress builds), **and the
finalize path re-reads the alias from the DB** (`currentJobAlias`) instead of
the in-memory frozen value — so an in-flight create/redo honors the operator's
set/clear/transfer. This sync is best-effort (failures are logged, the API
still returns 200); the narrow residual race is an operator `set_alias` landing
between a job's last `RequestJSON` sync and its finalize re-read.

### 3.7 Snapshots

A snapshot's `display_name` is an informational label, not a unique alias; its
`alias_key` is always `NULL` (STORED generated column), so it can never satisfy
the unique index. Set-alias on a snapshot → `ErrAliasNotApplicableToSnapshot`
(400).

## 4. Touch-point checklist (run on every behavior change)

When changing set-alias behavior, update **all** of:

- [ ] CubeMaster store: sentinel(s) + `SetTemplateAlias` + its doc comment
- [ ] CubeMaster handler: the error `switch` + the "Error mapping" doc comment
      (`template.go`)
- [ ] CubeAPI `map_err` — only if a new `ret_code` is introduced
- [ ] `openapi.yml` response status/descriptions for this path
- [ ] CLI flags / help (`cubemastercli tpl set-alias`)
- [ ] SDK methods (Go/Node/Python) — only if request/response *shape* changes
- [ ] Go unit tests (`store_test.go`, `template_test.go`) + Rust unit tests
- [ ] e2e (`tests/e2e/sdk_compat/cases/templates/test_alias.py`)
- [ ] This design doc + the PR description

## 5. Known debt (tracked separately, project-wide — not this feature's scope)

- **Error mapping is scattered** across the Go handler `switch`, the Rust
  `map_err`, and `openapi.yml` text. A typed-error refactor (`errors.As`) would
  centralize classification. Affects all endpoints, not just alias.
- **`openapi.yml` is partly hand-maintained.** Registering the served
  `/cluster/*` and `/nodes/*` routes in utoipa `paths()` would make the spec
  regenerable from code, eliminating hand-edit drift. Affects the whole API.
