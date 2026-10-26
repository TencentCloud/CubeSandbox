---
name: sdk-e2e-cases
description: Write, review, and execute CubeSandbox SDK compatibility E2E pytest cases. Use when the user asks to add, design, review, debug, or run SDK E2E cases under tests/e2e/sdk_compat, or mentions lifecycle, network policy, sandbox templates, backend compatibility, pytest markers, or live E2E validation.
---

# SDK E2E Cases

Use this skill for CubeSandbox SDK compatibility E2E work in `tests/e2e/sdk_compat`.

## Required First Steps

1. Read the relevant local docs before changing tests:
   - `tests/e2e/sdk_compat/README.md`
   - `tests/e2e/sdk_compat/docs/case-authoring.md`
   - `tests/e2e/sdk_compat/docs/test-coverage.md`
   - `tests/e2e/sdk_compat/docs/framework-design.md`
2. Inspect nearby cases in the same domain directory before writing a new case.
3. Decide whether the request is case authoring, review, execution, or debugging, then follow the matching workflow below.

## Authoring Workflow

1. Classify the behavior domain: `commands`, `filesystem`, `host-mount`, `lifecycle`, `network`, `run_code`, or `concurrency`.
2. Decide whether the behavior is a shared SDK contract or backend-specific behavior.
3. Use capability and environment markers for unsupported prerequisites. Do not hide backend differences with loose assertions.
4. Prefer existing fixtures and helpers over new abstractions:
   - `sdk_sandbox`
   - `sdk_e2e_config`
   - `sdk_backend`
   - `sandbox_create_options`
   - `sandbox_template_id`
5. Keep assertions contract-focused and diagnostics useful. Include IDs, states, URLs, status codes, and short response snippets where helpful, but do not log secrets or traffic access token values.
6. Add or update docs only when the case introduces a new pattern, marker, environment variable, template requirement, or coverage category.

## Review Workflow

Review SDK E2E changes for:

- Correctness: the test asserts product/SDK contracts, not incidental implementation details.
- Backend compatibility: shared cases run across selected backends, and unsupported behavior uses capability markers.
- Lifecycle semantics: `running` means the sandbox data plane should be usable; do not mask backend bugs with broad retries.
- Resource cleanup: created sandboxes are cleaned with existing fixtures/helpers unless intentionally preserved by framework behavior.
- Security: do not print API keys, traffic access tokens, machine IPs, or sensitive headers.
- Stability: avoid fixed sleeps, public internet assumptions without markers, and unbounded polling.
- Maintainability: prefer existing helpers, readable markers, and focused tests.

## Execution Workflow

From `tests/e2e/sdk_compat`, use the smallest command that validates the change:

```bash
pytest --collect-only -q
pytest --run-e2e -m "p0 and not slow"
pytest --run-e2e cases/lifecycle/test_create.py -q
pytest --run-e2e cases/network/test_policy.py -q
```

For live runs, check that required environment variables are documented and intentionally set. Use `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` only when debugging failures.

## Additional Reference

For concrete marker templates, review checklists, and command patterns, read `REFERENCE.md`.
