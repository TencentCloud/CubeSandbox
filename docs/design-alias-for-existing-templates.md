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

The target **must be `READY`**; any other status → `ErrTemplateNotReady`
(409). Rationale:

- An alias must never resolve to a building or failed template.
- Requiring `READY` prevents the create-time claim
  (`claimAliasForReadyTemplate`, run at build completion) from silently
  overwriting an operator-set alias — the failure mode that made the earlier
  "non-READY allowed" behavior unsafe.
- Consistent with the create path, which claims only after the build reaches a
  non-FAILED status.

### 3.6 Concurrency / atomicity

`claimTemplateAlias` runs **release + claim + confirm** in one transaction:

1. **Release** — `UPDATE display_name='' WHERE alias_key=? AND template_id<>target`
   (clears the alias from any other holder).
2. **Claim** — `UPDATE display_name=alias WHERE template_id=target`.
3. **Confirm** — `SELECT COUNT(*) WHERE template_id=target AND alias_key=alias`.
   If `0` (target hard-deleted between `GetDefinition` and the claim), return
   `ErrTemplateNotFound` so the **whole transaction — including the release —
   rolls back** (another template's alias is never silently cleared). The
   `SELECT` (not `RowsAffected`) avoids MySQL's rows-changed conflation with
   idempotent re-claims.

Two concurrent swaps between the same pair of templates can deadlock
(MySQL `1205`/`1213`, PostgreSQL `40P01`/`55P03`); the transaction is retried
once (`isDeadlockError`).

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
