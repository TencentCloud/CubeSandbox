# cubesandbox Python SDK — Test Report

**Date:** 2026-05-07  
**Platform:** Linux 5.4 (x64), Python 3.11.6, pytest 9.0.3  
**Working directory:** `sdk/python/`  
**Command:** `PYTHONPATH=. python3 -m pytest tests/test_sandbox.py -v`

## Summary

| Total | Passed | Failed | Skipped |
|------:|-------:|-------:|--------:|
| 58    | 58     | 0      | 0       |

✅ **All 58 tests passed.**

---

## Results by Test Class

### TestHealth (1/1)

| Test | Result |
|------|--------|
| test_health_ok | ✅ PASS |

### TestListSandboxesV1 (2/2)

| Test | Result |
|------|--------|
| test_list_returns_array | ✅ PASS |
| test_list_empty | ✅ PASS |

### TestListSandboxesV2 (2/2)

| Test | Result |
|------|--------|
| test_list_v2_running | ✅ PASS |
| test_list_v2_paused | ✅ PASS |

### TestCreate (8/8)

| Test | Result |
|------|--------|
| test_create_minimal | ✅ PASS |
| test_create_with_timeout | ✅ PASS |
| test_create_with_env_vars | ✅ PASS |
| test_create_with_metadata | ✅ PASS |
| test_create_requires_template | ✅ PASS |
| test_create_explicit_template | ✅ PASS |
| test_create_api_error | ✅ PASS |
| test_create_template_not_found | ✅ PASS |
| test_create_auth_error | ✅ PASS |

### TestGetInfo (3/3)

| Test | Result |
|------|--------|
| test_get_info_running | ✅ PASS |
| test_get_info_paused | ✅ PASS |
| test_get_info_not_found | ✅ PASS |

### TestKill (4/4)

| Test | Result |
|------|--------|
| test_kill_success | ✅ PASS |
| test_kill_not_found | ✅ PASS |
| test_context_manager_kills_on_exit | ✅ PASS |
| test_context_manager_suppresses_kill_error | ✅ PASS |

### TestPause (2/2)

| Test | Result |
|------|--------|
| test_pause_success | ✅ PASS |
| test_pause_not_found | ✅ PASS |

### TestResume (3/3)

| Test | Result |
|------|--------|
| test_resume_success | ✅ PASS |
| test_resume_default_timeout | ✅ PASS |
| test_resume_not_found | ✅ PASS |

### TestConnect (3/3)

| Test | Result |
|------|--------|
| test_connect_success | ✅ PASS |
| test_connect_not_found | ✅ PASS |
| test_connect_sends_timeout | ✅ PASS |

### TestProperties (4/4)

| Test | Result |
|------|--------|
| test_get_host | ✅ PASS |
| test_get_host_custom_port | ✅ PASS |
| test_domain_fallback_to_config | ✅ PASS |
| test_repr | ✅ PASS |

### TestExecutionModel (7/7)

| Test | Result |
|------|--------|
| test_text_returns_main_result | ✅ PASS |
| test_text_none_when_no_results | ✅ PASS |
| test_text_none_when_no_main | ✅ PASS |
| test_error_captured | ✅ PASS |
| test_logs_defaults_empty | ✅ PASS |
| test_repr_with_text | ✅ PASS |
| test_repr_with_error | ✅ PASS |

### TestParseStream (15/15)

| Test | Result |
|------|--------|
| test_parses_result | ✅ PASS |
| test_parses_result_not_main | ✅ PASS |
| test_parses_stdout | ✅ PASS |
| test_parses_stderr | ✅ PASS |
| test_parses_error | ✅ PASS |
| test_parses_execution_count | ✅ PASS |
| test_ignores_bad_json | ✅ PASS |
| test_ignores_empty_line | ✅ PASS |
| test_ignores_unknown_type | ✅ PASS |
| test_stdout_callback_called | ✅ PASS |
| test_stderr_callback_called | ✅ PASS |
| test_result_callback_called | ✅ PASS |
| test_error_callback_called | ✅ PASS |
| test_multiple_stdout_lines | ✅ PASS |
| test_multiple_results_last_main | ✅ PASS |

### TestConfig (3/3)

| Test | Result |
|------|--------|
| test_defaults | ✅ PASS |
| test_trailing_slash_stripped | ✅ PASS |
| test_env_override | ✅ PASS |

---

## Coverage Summary

| Module | Coverage |
|--------|----------|
| `cubesandbox/sandbox.py` | lifecycle, execute, context, config, error handling |
| `cubesandbox/_models.py` | Execution, Result, OutputMessage, ExecutionError, Context |
| `cubesandbox/_stream.py` | ndjson stream parsing, all event types, callbacks |
| `cubesandbox/_config.py` | env var resolution, trailing-slash stripping |
| `cubesandbox/_exceptions.py` | ApiError, AuthenticationError, SandboxNotFoundError, TemplateNotFoundError |

All tests use mocks (no real CubeAPI endpoint required).
