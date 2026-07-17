#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Offline unit tests for examples/codebuddy-integration/env_utils.py.

No CubeSandbox cluster or LLM credentials needed — every function tested here
is a pure resolution helper around environment variables. Run:

    cd examples/codebuddy-integration
    python3 -m unittest test_env_utils.py -v
"""

from __future__ import annotations

import argparse
import os
import sys
import types
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import env_utils  # noqa: E402


class InternetEnvironmentTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_INTERNET_ENVIRONMENT",
            "CODEBUDDY_BASE_URL",
            "ANTHROPIC_BASE_URL",
            "CODEBUDDY_LLM_HOST",
            "CODEBUDDY_PROVIDER",
            "CODEBUDDY_MODEL",
            "ANTHROPIC_MODEL",
            "CODEBUDDY_API_KEY",
            "ANTHROPIC_API_KEY",
            "OPENAI_API_KEY",
            "DEEPSEEK_API_KEY",
            "CODEBUDDY_AUTH_TOKEN",
            "CODEBUDDY_CONFIG_DIR",
            "CODEBUDDY_WORKSPACE",
            "DISABLE_TELEMETRY",
            "DISABLE_ERROR_REPORTING",
            "DISABLE_AUTOUPDATER",
            "DISABLE_FEEDBACK_COMMAND",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_default_internet_environment(self) -> None:
        self._clear()
        self.assertEqual(env_utils.internet_environment(), "io")

    def test_internet_environment_normalizes_case(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "  INTERNAL  "
        self.assertEqual(env_utils.internet_environment(), "internal")

    def test_internet_environment_rejects_unknown(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "mars"
        with self.assertRaises(SystemExit):
            env_utils.internet_environment()


class ProviderTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_INTERNET_ENVIRONMENT",
            "CODEBUDDY_BASE_URL",
            "ANTHROPIC_BASE_URL",
            "CODEBUDDY_PROVIDER",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_explicit_provider_overrides_everything(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_PROVIDER"] = "OpenAI"
        self.assertEqual(env_utils.provider(), "openai")

    def test_io_default_returns_codebuddy_io(self) -> None:
        self._clear()
        self.assertEqual(env_utils.provider(), "codebuddy_io")

    def test_internal_with_anthropic_url(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "internal"
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.anthropic.com"
        self.assertEqual(env_utils.provider(), "anthropic")

    def test_internal_with_openai_url(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "internal"
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.openai.com/v1"
        self.assertEqual(env_utils.provider(), "openai")

    def test_internal_with_unknown_url_falls_back_to_codebuddy_io(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "internal"
        os.environ["CODEBUDDY_BASE_URL"] = "https://example.com"
        self.assertEqual(env_utils.provider(), "codebuddy_io")


class LLMHostTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_INTERNET_ENVIRONMENT",
            "CODEBUDDY_BASE_URL",
            "ANTHROPIC_BASE_URL",
            "CODEBUDDY_LLM_HOST",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_explicit_host_wins(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_LLM_HOST"] = "llm.example.com"
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.anthropic.com"
        self.assertEqual(env_utils.llm_host(), "llm.example.com")

    def test_anthropic_base_url_used_for_default(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "internal"
        os.environ["ANTHROPIC_BASE_URL"] = "https://api.deepseek.com/anthropic"
        self.assertEqual(env_utils.llm_host(), "api.deepseek.com")

    def test_provider_default_when_nothing_set(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "internal"
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.anthropic.com"
        self.assertEqual(env_utils.llm_host(), "api.anthropic.com")


class KeyInjectionTests(unittest.TestCase):
    def test_anthropic_injects_x_api_key(self) -> None:
        specs = env_utils.provider_inject("anthropic", "sk-test")
        self.assertEqual(len(specs), 2)
        self.assertEqual(specs[0], {
            "header": "x-api-key",
            "secret": "sk-test",
            "format": "${SECRET}",
        })
        self.assertEqual(specs[1]["header"], "anthropic-version")

    def test_non_anthropic_injects_bearer(self) -> None:
        specs = env_utils.provider_inject("openai", "sk-test")
        self.assertEqual(len(specs), 1)
        self.assertEqual(specs[0], {
            "header": "Authorization",
            "secret": "sk-test",
            "format": "Bearer ${SECRET}",
        })

    def test_provider_case_insensitive(self) -> None:
        specs = env_utils.provider_inject("Anthropic", "sk")
        self.assertEqual(specs[0]["header"], "x-api-key")


class KeyCandidatesTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_BASE_URL", "ANTHROPIC_API_KEY",
            "OPENAI_API_KEY", "DEEPSEEK_API_KEY", "GEMINI_API_KEY",
            "CODEBUDDY_API_KEY", "CODEBUDDY_AUTH_TOKEN",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_anthropic_candidates_start_with_anthropic_key(self) -> None:
        self._clear()
        candidates = env_utils.provider_key_candidates("anthropic")
        self.assertEqual(candidates[0], "ANTHROPIC_API_KEY")

    def test_codebuddy_io_falls_back_to_anthropic_when_base_url_matches(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.anthropic.com"
        candidates = env_utils.provider_key_candidates("codebuddy_io")
        self.assertIn("ANTHROPIC_API_KEY", candidates)
        self.assertIn("CODEBUDDY_API_KEY", candidates)

    def test_codebuddy_io_does_not_fall_back_when_base_url_is_neutral(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_BASE_URL"] = "https://example.com"
        candidates = env_utils.provider_key_candidates("codebuddy_io")
        self.assertNotIn("ANTHROPIC_API_KEY", candidates)


class RequireProviderKeyTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_INTERNET_ENVIRONMENT",
            "CODEBUDDY_BASE_URL",
            "CODEBUDDY_API_KEY", "CODEBUDDY_AUTH_TOKEN",
            "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_raises_when_no_keys_set(self) -> None:
        self._clear()
        with self.assertRaises(SystemExit):
            env_utils.require_provider_key()

    def test_returns_anthropic_key_when_set(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "internal"
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.anthropic.com"
        os.environ["ANTHROPIC_API_KEY"] = "sk-test"
        self.assertEqual(env_utils.require_provider_key(), "sk-test")

    def test_returns_codebuddy_api_key_when_set(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_API_KEY"] = "cb-test"
        self.assertEqual(env_utils.require_provider_key(), "cb-test")


class BuildEnvTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_INTERNET_ENVIRONMENT",
            "CODEBUDDY_PROVIDER",
            "CODEBUDDY_CONFIG_DIR",
            "CODEBUDDY_BASE_URL",
            "ANTHROPIC_BASE_URL",
            "ANTHROPIC_API_KEY",
            "OPENAI_API_KEY",
            "DEEPSEEK_API_KEY",
            "GEMINI_API_KEY",
            "CODEBUDDY_API_KEY",
            "CODEBUDDY_AUTH_TOKEN",
            "DISABLE_TELEMETRY",
            "DISABLE_ERROR_REPORTING",
            "DISABLE_AUTOUPDATER",
            "DISABLE_FEEDBACK_COMMAND",
            "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
            "CODEBUDDY_MODEL", "MAX_THINKING_TOKENS",
            "CODEBUDDY_CUSTOM_HEADERS",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_with_secrets_includes_active_provider_key_only(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "internal"
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.anthropic.com"
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant"
        os.environ["OPENAI_API_KEY"] = "sk-oai"
        env = env_utils.build_codebuddy_env(include_secrets=True)
        self.assertEqual(env["ANTHROPIC_API_KEY"], "sk-ant")
        self.assertNotIn("OPENAI_API_KEY", env)
        self.assertEqual(env["CODEBUDDY_INTERNET_ENVIRONMENT"], "internal")
        self.assertEqual(env["DISABLE_TELEMETRY"], "1")

    def test_without_secrets_omits_keys(self) -> None:
        self._clear()
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant"
        env = env_utils.build_codebuddy_env(include_secrets=False)
        self.assertNotIn("ANTHROPIC_API_KEY", env)

    def test_passthrough_env_forwarded(self) -> None:
        self._clear()
        os.environ["HTTP_PROXY"] = "http://proxy:8080"
        os.environ["HTTPS_PROXY"] = "http://proxy:8080"
        os.environ["CODEBUDDY_MODEL"] = "claude-sonnet-4-6"
        env = env_utils.build_codebuddy_env()
        self.assertEqual(env["HTTP_PROXY"], "http://proxy:8080")
        self.assertEqual(env["HTTPS_PROXY"], "http://proxy:8080")
        self.assertEqual(env["CODEBUDDY_MODEL"], "claude-sonnet-4-6")

    def test_proxy_userinfo_stripped_before_forwarding(self) -> None:
        # Host proxy credentials must not leak into the sandbox where the LLM
        # agent could read them out via env or printenv.
        self._clear()
        os.environ["HTTP_PROXY"] = "http://user:pass@corp-proxy:8080"
        os.environ["HTTPS_PROXY"] = "https://alice:s3cret@proxy.example.com:3128"
        env = env_utils.build_codebuddy_env()
        self.assertEqual(env["HTTP_PROXY"], "http://corp-proxy:8080")
        self.assertEqual(env["HTTPS_PROXY"], "https://proxy.example.com:3128")
        self.assertNotIn("user:pass", env["HTTP_PROXY"])
        self.assertNotIn("alice:s3cret", env["HTTPS_PROXY"])

    def test_proxy_without_userinfo_passes_through(self) -> None:
        self._clear()
        os.environ["HTTPS_PROXY"] = "http://proxy:8080"
        env = env_utils.build_codebuddy_env()
        self.assertEqual(env["HTTPS_PROXY"], "http://proxy:8080")

    def test_only_first_matching_key_forwarded(self) -> None:
        # When CODEBUDDY_INTERNET_ENVIRONMENT=io (provider=codebuddy_io) with a
        # custom BASE_URL containing "anthropic", provider_key_candidates() extends
        # the list with ANTHROPIC_API_KEY.  If both CODEBUDDY_API_KEY and
        # ANTHROPIC_API_KEY are set on the host, the first match
        # (CODEBUDDY_API_KEY, higher priority for codebuddy_io) enters the
        # sandbox — confirming the loop breaks after the first hit.
        self._clear()
        os.environ["CODEBUDDY_INTERNET_ENVIRONMENT"] = "io"
        os.environ["CODEBUDDY_BASE_URL"] = "https://api.anthropic.com"
        os.environ["CODEBUDDY_API_KEY"] = "cb-primary"
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant-secondary"
        env = env_utils.build_codebuddy_env(include_secrets=True)
        self.assertEqual(env["CODEBUDDY_API_KEY"], "cb-primary")
        self.assertNotIn("ANTHROPIC_API_KEY", env)


class StripUrlUserinfoTests(unittest.TestCase):
    def test_strips_user_and_password(self) -> None:
        self.assertEqual(
            env_utils.strip_url_userinfo("http://u:p@h:8080"),
            "http://h:8080",
        )

    def test_strips_user_only(self) -> None:
        self.assertEqual(
            env_utils.strip_url_userinfo("http://u@h:8080"),
            "http://h:8080",
        )

    def test_keeps_port(self) -> None:
        self.assertEqual(
            env_utils.strip_url_userinfo("https://u:p@proxy.example.com:3128/path"),
            "https://proxy.example.com:3128/path",
        )

    def test_handles_bare_host_with_userinfo(self) -> None:
        # Some proxies are exported as just "user:p@host:port" with no scheme.
        self.assertEqual(
            env_utils.strip_url_userinfo("u:p@h:8080"),
            "https://h:8080",
        )

    def test_no_userinfo_returns_unchanged(self) -> None:
        self.assertEqual(
            env_utils.strip_url_userinfo("http://proxy:8080"),
            "http://proxy:8080",
        )

    def test_empty_returns_unchanged(self) -> None:
        self.assertEqual(env_utils.strip_url_userinfo(""), "")
        self.assertEqual(env_utils.strip_url_userinfo("   "), "   ")

    def test_at_sign_in_path_only_returns_unchanged(self) -> None:
        # An '@' that is not in the authority section should not be mangled.
        self.assertEqual(
            env_utils.strip_url_userinfo("http://proxy:8080/path@version"),
            "http://proxy:8080/path@version",
        )

class CommandBuilderTests(unittest.TestCase):
    def test_minimal_command(self) -> None:
        cmd = env_utils.codebuddy_command("hello")
        self.assertEqual(cmd, "codebuddy -p -y hello")

    def test_continue_flag(self) -> None:
        cmd = env_utils.codebuddy_command("hello", continue_session=True)
        self.assertIn("-c", cmd)

    def test_resume_flag(self) -> None:
        cmd = env_utils.codebuddy_command("hello", resume="abc-123")
        self.assertIn("--resume abc-123", cmd)

    def test_session_id_flag(self) -> None:
        cmd = env_utils.codebuddy_command("hello", session_id="uuid-1")
        self.assertIn("--session-id uuid-1", cmd)

    def test_model_flag(self) -> None:
        cmd = env_utils.codebuddy_command("hello", model="claude-sonnet-4-6")
        self.assertIn("--model claude-sonnet-4-6", cmd)

    def test_dangerously_skip_permissions_off(self) -> None:
        cmd = env_utils.codebuddy_command("hello", dangerously_skip_permissions=False)
        self.assertNotIn("-y", cmd)

    def test_prompt_is_shell_quoted(self) -> None:
        cmd = env_utils.codebuddy_command("hello world; rm -rf /")
        # shlex.quote wraps in single quotes for safety
        self.assertIn("'hello world; rm -rf /'", cmd)


class ShellJoinTests(unittest.TestCase):
    def test_joins_non_empty_parts_with_and(self) -> None:
        self.assertEqual(
            env_utils.shell_join("a", "b", "c"),
            "a && b && c",
        )

    def test_skips_empty_parts(self) -> None:
        self.assertEqual(
            env_utils.shell_join("a", "", "c"),
            "a && c",
        )


class HomeAndWorkspaceTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_CONFIG_DIR", "CODEBUDDY_WORKSPACE",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_default_home(self) -> None:
        self._clear()
        self.assertEqual(env_utils.codebuddy_home(), "/workspace/.codebuddy")

    def test_custom_home(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_CONFIG_DIR"] = "/var/lib/codebuddy"
        self.assertEqual(env_utils.codebuddy_home(), "/var/lib/codebuddy")

    def test_default_workspace(self) -> None:
        self._clear()
        self.assertEqual(env_utils.codebuddy_workspace(), "/workspace")


class HostFromUrlTests(unittest.TestCase):
    def test_extracts_host(self) -> None:
        self.assertEqual(
            env_utils._host_from_url("https://api.anthropic.com/v1"),
            "api.anthropic.com",
        )

    def test_handles_bare_hostname(self) -> None:
        self.assertEqual(
            env_utils._host_from_url("api.anthropic.com"),
            "api.anthropic.com",
        )

    def test_empty_returns_empty(self) -> None:
        self.assertEqual(env_utils._host_from_url(""), "")
        self.assertEqual(env_utils._host_from_url("   "), "")


class OptionalAndIntEnvTests(unittest.TestCase):
    def test_optional_returns_default_when_unset(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(env_utils.optional("X", "fallback"), "fallback")

    def test_optional_returns_empty_when_unset_and_no_default(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(env_utils.optional("X"), "")

    def test_required_raises_when_unset(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(SystemExit):
                env_utils.required("X")

    def test_int_env_returns_default_when_unset(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(env_utils.int_env("X", 42), 42)

    def test_int_env_parses_value(self) -> None:
        with mock.patch.dict(os.environ, {"X": "123"}):
            self.assertEqual(env_utils.int_env("X", 0), 123)

    def test_int_env_rejects_non_integer(self) -> None:
        with mock.patch.dict(os.environ, {"X": "abc"}):
            with self.assertRaises(SystemExit):
                env_utils.int_env("X", 0)


class CodebuddyModelTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_INTERNET_ENVIRONMENT",
            "CODEBUDDY_PROVIDER",
            "CODEBUDDY_MODEL",
            "ANTHROPIC_MODEL",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_explicit_model_wins(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_MODEL"] = "claude-opus-4-5"
        self.assertEqual(env_utils.codebuddy_model(), "claude-opus-4-5")

    def test_anthropic_model_fallback(self) -> None:
        # When CODEBUDDY_MODEL is unset and provider is anthropic, fall back
        # to ANTHROPIC_MODEL.
        self._clear()
        os.environ["CODEBUDDY_PROVIDER"] = "anthropic"
        os.environ["ANTHROPIC_MODEL"] = "claude-sonnet-4-7"
        self.assertEqual(env_utils.codebuddy_model(), "claude-sonnet-4-7")

    def test_no_default_for_non_anthropic_raises(self) -> None:
        # OpenAI and other non-Anthropic providers have no safe cross-provider
        # default, so omitting both CODEBUDDY_MODEL and ANTHROPIC_MODEL raises.
        self._clear()
        os.environ["CODEBUDDY_PROVIDER"] = "openai"
        with self.assertRaises(SystemExit):
            env_utils.codebuddy_model()

    def test_anthropic_default_when_nothing_set(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_PROVIDER"] = "anthropic"
        # no CODEBUDDY_MODEL, no ANTHROPIC_MODEL → must use the shipped default
        self.assertEqual(env_utils.codebuddy_model(), "claude-sonnet-4-6")


class ProviderKeyNameTests(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in (
            "CODEBUDDY_INTERNET_ENVIRONMENT",
            "CODEBUDDY_PROVIDER",
            "CODEBUDDY_BASE_URL",
            "CODEBUDDY_API_KEY",
            "ANTHROPIC_API_KEY",
            "OPENAI_API_KEY",
            "DEEPSEEK_API_KEY",
        )}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def _clear(self) -> None:
        for k in self._saved:
            os.environ.pop(k, None)

    def test_returns_first_set_key(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_PROVIDER"] = "anthropic"
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant-test"
        os.environ["CODEBUDDY_API_KEY"] = "cb-test"
        self.assertEqual(env_utils.provider_key_name(), "ANTHROPIC_API_KEY")

    def test_falls_back_to_codebuddy_key(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_PROVIDER"] = "anthropic"
        os.environ["CODEBUDDY_API_KEY"] = "cb-fallback"
        # ANTHROPIC_API_KEY is not set → first candidate is ANTHROPIC_API_KEY
        # (always added by provider_key_candidates), which returns empty string
        # from os.environ.get; the second candidate CODEBUDDY_API_KEY matches.
        self.assertEqual(env_utils.provider_key_name(), "CODEBUDDY_API_KEY")

    def test_unknown_provider_returns_default(self) -> None:
        self._clear()
        os.environ["CODEBUDDY_PROVIDER"] = "anthropic"
        # no keys set → returns the canonical first candidate name
        self.assertEqual(env_utils.provider_key_name(), "ANTHROPIC_API_KEY")


class LoadLocalDotenvTests(unittest.TestCase):
    def test_load_local_dotenv_does_not_raise(self) -> None:
        # Smoke test: the function should not raise even when no .env exists.
        env_utils.load_local_dotenv()


class PositiveIntTests(unittest.TestCase):
    def test_parses_positive_integer(self) -> None:
        from _codebuddy_common import positive_int
        self.assertEqual(positive_int("42"), 42)

    def test_rejects_zero(self) -> None:
        from _codebuddy_common import positive_int
        with self.assertRaises(argparse.ArgumentTypeError):
            positive_int("0")

    def test_rejects_negative(self) -> None:
        from _codebuddy_common import positive_int
        with self.assertRaises(argparse.ArgumentTypeError):
            positive_int("-5")

    def test_rejects_non_integer(self) -> None:
        from _codebuddy_common import positive_int
        with self.assertRaises(argparse.ArgumentTypeError):
            positive_int("abc")


class EnvPositiveIntTests(unittest.TestCase):
    """Verify env-var fallback also rejects zero / negative / non-integer values.

    argparse evaluates ``default=`` before ``type=``, so the bare
    ``default=int_env(...)`` pattern that used to be there would silently let
    ``CODEBUDDY_SANDBOX_TIMEOUT=0`` reach the SDK. These tests pin down
    ``_env_positive_int`` as the right helper.
    """

    def test_unset_returns_default(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(env_utils._env_positive_int("X", 42), 42)

    def test_empty_returns_default(self) -> None:
        with mock.patch.dict(os.environ, {"X": ""}):
            self.assertEqual(env_utils._env_positive_int("X", 42), 42)

    def test_positive_passes_through(self) -> None:
        with mock.patch.dict(os.environ, {"X": "120"}):
            self.assertEqual(env_utils._env_positive_int("X", 1), 120)

    def test_zero_raises(self) -> None:
        with mock.patch.dict(os.environ, {"X": "0"}):
            with self.assertRaises(SystemExit):
                env_utils._env_positive_int("X", 1)

    def test_negative_raises(self) -> None:
        with mock.patch.dict(os.environ, {"X": "-5"}):
            with self.assertRaises(SystemExit):
                env_utils._env_positive_int("X", 1)

    def test_non_integer_raises(self) -> None:
        with mock.patch.dict(os.environ, {"X": "abc"}):
            with self.assertRaises(SystemExit):
                env_utils._env_positive_int("X", 1)


class CommonHelpersTests(unittest.TestCase):
    """Tests for _codebuddy_common helpers that are not exercised by env_utils."""

    def test_ensure_success_zero_exit(self) -> None:
        from _codebuddy_common import ensure_success
        result = unittest.mock.MagicMock(exit_code=0)
        # must not raise
        ensure_success(result, "do something")

    def test_ensure_success_none_exit(self) -> None:
        from _codebuddy_common import ensure_success
        result = unittest.mock.MagicMock(exit_code=None, stdout="ok", stderr="")
        ensure_success(result, "do something")  # must not raise

    def test_ensure_success_non_zero_exit(self) -> None:
        from _codebuddy_common import ensure_success
        result = unittest.mock.MagicMock(
            exit_code=1, stdout="out", stderr="error"
        )
        with self.assertRaises(SystemExit) as ctx:
            ensure_success(result, "do something")
        # SystemExit raised by ensure_success carries the formatted message as
        # args[0], not an integer exit code.
        self.assertIn("Failed to do something (exit 1)", str(ctx.exception))

    def test_sandbox_identifier_prefers_sandbox_id(self) -> None:
        from _codebuddy_common import sandbox_identifier
        # Use a plain object so the attribute lookup is a real getattr, not a
        # auto-generated MagicMock attribute (which would always succeed and
        # silently mask the priority path).
        sandbox = types.SimpleNamespace(sandbox_id="sb-abc", id="legacy-id")
        self.assertEqual(sandbox_identifier(sandbox), "sb-abc")

    def test_sandbox_identifier_falls_back_to_id(self) -> None:
        from _codebuddy_common import sandbox_identifier
        sandbox = types.SimpleNamespace(id="legacy-id")
        self.assertEqual(sandbox_identifier(sandbox), "legacy-id")

    def test_sandbox_identifier_returns_unknown_when_neither_attr(self) -> None:
        from _codebuddy_common import sandbox_identifier
        sandbox = types.SimpleNamespace()
        self.assertEqual(sandbox_identifier(sandbox), "unknown")

    def test_stream_writer_extracts_line(self) -> None:
        from _codebuddy_common import stream_writer
        import io
        buf = io.StringIO()
        writer = stream_writer(buf)
        writer("hello")
        self.assertEqual(buf.getvalue(), "hello")

    def test_stream_writer_handles_chunk_with_line_attr(self) -> None:
        from _codebuddy_common import stream_writer
        import io
        buf = io.StringIO()
        writer = stream_writer(buf)
        chunk = unittest.mock.MagicMock()
        chunk.line = "chunked output"
        writer(chunk)
        self.assertEqual(buf.getvalue(), "chunked output")


if __name__ == "__main__":
    unittest.main(verbosity=2)
