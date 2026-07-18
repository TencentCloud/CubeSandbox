# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Tests for env_utils.py — environment resolution and command construction.

Pure environment-variable handling, no CubeSandbox cluster or LLM credentials
needed. Mirrors the structure of the Claude Code integration test suite so
the two examples can be reviewed side-by-side.
"""

from __future__ import annotations

import argparse
import io
import os
import sys
import types
from unittest import mock

import pytest

import env_utils
from _opencode_common import ensure_success, positive_int, sandbox_identifier, stream_writer


# Snapshot/restore helper used by the older hand-rolled TestCase style. Kept
# here as a context manager so tests that need to set/tear down a known set
# of env vars (rather than ad-hoc monkeypatching) can do so without leaking
# state into the rest of the suite.
@pytest.fixture
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Wipe all integration-relevant env vars before each test.

    Listed explicitly so a leaked var cannot silently flip the result of a
    neighboring test (the order of pytest collection is not guaranteed).
    """
    for key in (
        "OPENCODE_PROVIDER",
        "OPENCODE_BASE_URL",
        "OPENCODE_MODEL",
        "ANTHROPIC_BASE_URL",
        "ANTHROPIC_MODEL",
        "OPENAI_BASE_URL",
        "OPENAI_MODEL",
        "GOOGLE_BASE_URL",
        "OPENCODE_LLM_HOST",
        "OPENCODE_CONFIG_DIR",
        "OPENCODE_DATA_DIR",
        "OPENCODE_WORKSPACE",
        "OPENCODE_SMALL_MODEL",
        "OPENCODE_CUSTOM_HEADERS",
        "DISABLE_TELEMETRY",
        "DISABLE_ERROR_REPORTING",
        "ANTHROPIC_API_KEY",
        "OPENAI_API_KEY",
        "DEEPSEEK_API_KEY",
        "GOOGLE_API_KEY",
        "GROQ_API_KEY",
        "MISTRAL_API_KEY",
        "OPENROUTER_API_KEY",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "NO_PROXY",
    ):
        monkeypatch.delenv(key, raising=False)


# ── internet_environment + provider ────────────────────────────────────────


class TestProvider:
    """Tests for env_utils.provider() — provider selection resolution."""

    def test_explicit_provider_overrides_env_keys(self, clean_env: None) -> None:
        # The explicit OPENCODE_PROVIDER env var must always win over any
        # inferred provider (the user explicitly opted out of inference).
        os.environ["OPENAI_API_KEY"] = "sk-openai"
        os.environ["OPENCODE_PROVIDER"] = "OpenAI"
        assert env_utils.provider() == "openai"

    def test_anthropic_key_resolves_to_anthropic(self, clean_env: None) -> None:
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant"
        assert env_utils.provider() == "anthropic"

    def test_openai_key_resolves_to_openai(self, clean_env: None) -> None:
        os.environ["OPENAI_API_KEY"] = "sk-oai"
        assert env_utils.provider() == "openai"

    def test_deepseek_key_resolves_to_deepseek(self, clean_env: None) -> None:
        os.environ["DEEPSEEK_API_KEY"] = "sk-ds"
        assert env_utils.provider() == "deepseek"

    def test_google_key_resolves_to_google(self, clean_env: None) -> None:
        os.environ["GOOGLE_API_KEY"] = "gkey"
        assert env_utils.provider() == "google"

    def test_no_env_signal_defaults_to_anthropic(self, clean_env: None) -> None:
        assert env_utils.provider() == "anthropic"

    def test_baseurl_anthropic_host_returns_anthropic(self, clean_env: None) -> None:
        # DeepSeek ships an Anthropic-compatible gateway at
        # api.deepseek.com/anthropic, but the host the heuristic looks at is
        # only the hostname (``api.deepseek.com``) — the path is ignored.
        # When the user is intentionally targetting the Anthropic wire
        # protocol, they put it in the host (e.g. via a CNAME). Anything
        # more exotic should set OPENCODE_PROVIDER explicitly.
        os.environ["OPENCODE_BASE_URL"] = "https://api.anthropic.com"
        assert env_utils.provider() == "anthropic"

    def test_baseurl_openai_substring_returns_openai(self, clean_env: None) -> None:
        os.environ["OPENCODE_BASE_URL"] = "https://api.openai.com/v1"
        assert env_utils.provider() == "openai"

    def test_baseurl_googleapis_returns_google(self, clean_env: None) -> None:
        os.environ["OPENCODE_BASE_URL"] = "https://generativelanguage.googleapis.com"
        assert env_utils.provider() == "google"

    def test_baseurl_openrouter_returns_openrouter(self, clean_env: None) -> None:
        os.environ["OPENCODE_BASE_URL"] = "https://openrouter.ai/api/v1"
        assert env_utils.provider() == "openrouter"

    def test_provider_returns_lowercase(self, clean_env: None) -> None:
        os.environ["OPENCODE_PROVIDER"] = "  Anthropic  "
        assert env_utils.provider() == "anthropic"


# ── llm_host ──────────────────────────────────────────────────────────────


class TestLLMHost:
    """Tests for env_utils.llm_host() — upstream API host resolution."""

    def test_explicit_host_wins_over_baseurl(self, clean_env: None) -> None:
        # OPENCODE_LLM_HOST is the documented override knob for shared
        # clusters that proxy the upstream. It must beat any *_BASE_URL.
        os.environ["OPENCODE_LLM_HOST"] = "llm.example.com"
        os.environ["OPENCODE_BASE_URL"] = "https://api.anthropic.com"
        assert env_utils.llm_host() == "llm.example.com"

    def test_opencode_base_url_used_when_no_explicit_host(self, clean_env: None) -> None:
        os.environ["OPENCODE_BASE_URL"] = "https://api.openai.com"
        assert env_utils.llm_host() == "api.openai.com"

    def test_anthropic_base_url_used_when_opencode_unset(self, clean_env: None) -> None:
        os.environ["ANTHROPIC_BASE_URL"] = "https://api.deepseek.com/anthropic"
        assert env_utils.llm_host() == "api.deepseek.com"

    def test_provider_default_when_nothing_set(self, clean_env: None) -> None:
        os.environ["OPENCODE_PROVIDER"] = "openai"
        assert env_utils.llm_host() == "api.openai.com"

    def test_provider_default_anthropic(self, clean_env: None) -> None:
        # Provider inference defaults to anthropic when no signal is present.
        assert env_utils.llm_host() == "api.anthropic.com"


# ── provider_inject ───────────────────────────────────────────────────────


class TestProviderInject:
    """Tests for env_utils.provider_inject() — CubeEgress auth-header shape."""

    def test_anthropic_injects_x_api_key_and_version(self) -> None:
        specs = env_utils.provider_inject("anthropic", "sk-test")
        assert len(specs) == 2
        assert specs[0] == {
            "header": "x-api-key",
            "secret": "sk-test",
            "format": "${SECRET}",
        }
        # The pinned anthropic-version header is a constant, not the secret —
        # we feed it through the same ${SECRET} format for shape consistency
        # but the rendered value is "2023-06-01".
        assert specs[1] == {
            "header": "anthropic-version",
            "secret": "2023-06-01",
            "format": "${SECRET}",
        }

    def test_non_anthropic_injects_bearer(self) -> None:
        specs = env_utils.provider_inject("openai", "sk-test")
        assert specs == [
            {"header": "Authorization", "secret": "sk-test", "format": "Bearer ${SECRET}"}
        ]

    def test_deepseek_injects_bearer(self) -> None:
        specs = env_utils.provider_inject("deepseek", "sk-ds")
        assert specs[0]["header"] == "Authorization"
        assert specs[0]["format"] == "Bearer ${SECRET}"

    def test_provider_case_insensitive(self) -> None:
        specs = env_utils.provider_inject("Anthropic", "sk")
        assert specs[0]["header"] == "x-api-key"


# ── provider_key_candidates + require_provider_key ─────────────────────────


class TestRequireProviderKey:
    """Tests for env_utils.require_provider_key() and provider_key_name()."""

    def test_raises_when_no_keys_set(self, clean_env: None) -> None:
        with pytest.raises(SystemExit):
            env_utils.require_provider_key()

    def test_returns_anthropic_key_when_set(self, clean_env: None) -> None:
        os.environ["ANTHROPIC_API_KEY"] = "sk-test"
        assert env_utils.require_provider_key() == "sk-test"

    def test_returns_codebuddy_api_key_when_set(self, clean_env: None) -> None:
        # OpenCode uses provider-specific keys (ANTHROPIC_API_KEY, OPENAI_API_KEY,
        # etc.) rather than a generic OPENCODE_API_KEY. The canonical name for
        # the anthropic provider is ANTHROPIC_API_KEY.
        os.environ["OPENCODE_PROVIDER"] = "anthropic"
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant-test"
        assert env_utils.require_provider_key() == "sk-ant-test"

    def test_error_lists_candidate_keys(self, clean_env: None) -> None:
        # The error message must mention the canonical key name so
        # operators know what to set. Without this, debugging a missing-key
        # failure is a guessing game.
        with pytest.raises(SystemExit) as exc_info:
            env_utils.require_provider_key()
        assert "ANTHROPIC_API_KEY" in str(exc_info.value)


class TestProviderKeyName:
    """Tests for env_utils.provider_key_name() — env-var name lookup."""

    def test_returns_first_set_key(self, clean_env: None) -> None:
        os.environ["OPENCODE_PROVIDER"] = "anthropic"
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant-test"
        os.environ["OPENCODE_API_KEY"] = "oc-test"
        assert env_utils.provider_key_name() == "ANTHROPIC_API_KEY"

    def test_returns_canonical_when_no_keys_set(self, clean_env: None) -> None:
        os.environ["OPENCODE_PROVIDER"] = "anthropic"
        # No keys → still return the canonical first candidate so callers
        # can distinguish "no key set" from "unknown provider".
        assert env_utils.provider_key_name() == "ANTHROPIC_API_KEY"

    def test_openai_provider_returns_openai_key_env(self, clean_env: None) -> None:
        os.environ["OPENCODE_PROVIDER"] = "openai"
        os.environ["OPENAI_API_KEY"] = "sk-oai"
        assert env_utils.provider_key_name() == "OPENAI_API_KEY"

    def test_deepseek_provider_returns_deepseek_key_env(self, clean_env: None) -> None:
        os.environ["OPENCODE_PROVIDER"] = "deepseek"
        os.environ["DEEPSEEK_API_KEY"] = "sk-ds"
        assert env_utils.provider_key_name() == "DEEPSEEK_API_KEY"


# ── build_opencode_env ────────────────────────────────────────────────────


class TestBuildOpencodeEnv:
    """Tests for env_utils.build_opencode_env() — exec-env construction."""

    def test_with_secrets_includes_only_active_provider_key(self, clean_env: None) -> None:
        # CI hosts regularly have multiple provider keys lying around. The
        # direct flavor must forward exactly the one matching the active
        # provider — never both, never all.
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant"
        os.environ["OPENAI_API_KEY"] = "sk-oai"
        env = env_utils.build_opencode_env(include_secrets=True)
        assert env["ANTHROPIC_API_KEY"] == "sk-ant"
        assert "OPENAI_API_KEY" not in env

    def test_without_secrets_omits_provider_key(self, clean_env: None) -> None:
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant"
        env = env_utils.build_opencode_env(include_secrets=False)
        assert "ANTHROPIC_API_KEY" not in env

    def test_sets_opencode_config_and_data_dirs(self, clean_env: None) -> None:
        env = env_utils.build_opencode_env()
        assert env["OPENCODE_CONFIG_DIR"] == "/workspace/.opencode/config"
        assert env["OPENCODE_DATA_DIR"] == "/workspace/.opencode/data"
        # XDG base dirs are forced to /workspace so the user-owned exec
        # account (``user``) can write there.
        assert env["XDG_CONFIG_HOME"] == "/workspace"
        assert env["XDG_DATA_HOME"] == "/workspace"

    def test_passthrough_env_forwarded(self, clean_env: None) -> None:
        os.environ["HTTP_PROXY"] = "http://proxy:8080"
        os.environ["HTTPS_PROXY"] = "http://proxy:8080"
        os.environ["OPENCODE_MODEL"] = "anthropic/claude-sonnet-4-6"
        env = env_utils.build_opencode_env()
        assert env["HTTP_PROXY"] == "http://proxy:8080"
        assert env["HTTPS_PROXY"] == "http://proxy:8080"
        assert env["OPENCODE_MODEL"] == "anthropic/claude-sonnet-4-6"

    def test_proxy_userinfo_stripped_before_forwarding(self, clean_env: None) -> None:
        # Host proxy credentials (http://user:pass@corp-proxy:8080) would
        # otherwise leak into the VM where the LLM agent can read them via
        # printenv.
        os.environ["HTTP_PROXY"] = "http://user:pass@corp-proxy:8080"
        os.environ["HTTPS_PROXY"] = "https://alice:s3cret@proxy.example.com:3128"
        env = env_utils.build_opencode_env()
        assert env["HTTP_PROXY"] == "http://corp-proxy:8080"
        assert env["HTTPS_PROXY"] == "https://proxy.example.com:3128"
        assert "user:pass" not in env["HTTP_PROXY"]
        assert "alice:s3cret" not in env["HTTPS_PROXY"]

    def test_proxy_without_userinfo_passes_through(self, clean_env: None) -> None:
        os.environ["HTTPS_PROXY"] = "http://proxy:8080"
        env = env_utils.build_opencode_env()
        assert env["HTTPS_PROXY"] == "http://proxy:8080"

    def test_disable_telemetry_defaults_to_one(self, clean_env: None) -> None:
        env = env_utils.build_opencode_env()
        assert env["DISABLE_TELEMETRY"] == "1"
        assert env["DISABLE_ERROR_REPORTING"] == "1"
        assert env["OPENCODE_DISABLE_AUTOUPDATE"] == "1"


# ── opencode_command (command builder) ────────────────────────────────────


class TestOpencodeCommand:
    """Tests for env_utils.opencode_command() — headless invocation builder."""

    def test_minimal_command(self) -> None:
        # The minimal headless invocation is `opencode run <prompt>`. The
        # --dangerously-skip-permissions flag is appended by default because
        # OpenCode otherwise blocks on a permission prompt that cannot be
        # answered over the exec channel.
        cmd = env_utils.opencode_command("hello")
        assert cmd == "opencode run --dangerously-skip-permissions hello"

    def test_continue_flag(self) -> None:
        cmd = env_utils.opencode_command("hello", continue_session=True)
        assert "-c" in cmd

    def test_resume_flag(self) -> None:
        cmd = env_utils.opencode_command("hello", resume="abc-123")
        assert "-s abc-123" in cmd

    def test_session_id_flag(self) -> None:
        cmd = env_utils.opencode_command("hello", session_id="uuid-1")
        assert "--session-id uuid-1" in cmd

    def test_model_flag(self) -> None:
        cmd = env_utils.opencode_command("hello", model="anthropic/claude-sonnet-4-6")
        assert "-m anthropic/claude-sonnet-4-6" in cmd

    def test_dangerously_skip_permissions_off(self) -> None:
        cmd = env_utils.opencode_command("hello", dangerously_skip_permissions=False)
        assert "--dangerously-skip-permissions" not in cmd

    def test_prompt_is_shell_quoted(self) -> None:
        # shlex.quote wraps prompts with shell metachars so they reach
        # OpenCode as a single arg even if the user passes 'hello world; rm -rf /'.
        cmd = env_utils.opencode_command("hello world; rm -rf /")
        assert "'hello world; rm -rf /'" in cmd


# ── shell_join + helpers ──────────────────────────────────────────────────


class TestShellJoin:
    """Tests for env_utils.shell_join() — ``&&`` chain builder."""

    def test_joins_non_empty_parts_with_and(self) -> None:
        assert env_utils.shell_join("a", "b", "c") == "a && b && c"

    def test_skips_empty_parts(self) -> None:
        assert env_utils.shell_join("a", "", "c") == "a && c"


class TestStripUrlUserinfo:
    """Tests for env_utils.strip_url_userinfo() — credential removal."""

    def test_strips_user_and_password(self) -> None:
        assert env_utils.strip_url_userinfo("http://u:p@h:8080") == "http://h:8080"

    def test_strips_user_only(self) -> None:
        assert env_utils.strip_url_userinfo("http://u@h:8080") == "http://h:8080"

    def test_keeps_port(self) -> None:
        assert (
            env_utils.strip_url_userinfo("https://u:p@proxy.example.com:3128/path")
            == "https://proxy.example.com:3128/path"
        )

    def test_handles_bare_host_with_userinfo(self) -> None:
        # Some proxies are exported as just "user:p@host:port" with no scheme.
        assert env_utils.strip_url_userinfo("u:p@h:8080") == "https://h:8080"

    def test_no_userinfo_returns_unchanged(self) -> None:
        assert env_utils.strip_url_userinfo("http://proxy:8080") == "http://proxy:8080"

    def test_empty_returns_unchanged(self) -> None:
        assert env_utils.strip_url_userinfo("") == ""
        assert env_utils.strip_url_userinfo("   ") == "   "

    def test_at_sign_in_path_only_returns_unchanged(self) -> None:
        # An '@' that is not in the authority section should not be mangled.
        assert (
            env_utils.strip_url_userinfo("http://proxy:8080/path@version")
            == "http://proxy:8080/path@version"
        )


class TestHostFromUrl:
    """Tests for env_utils._host_from_url() — hostname extraction."""

    def test_extracts_host(self) -> None:
        assert env_utils._host_from_url("https://api.anthropic.com/v1") == "api.anthropic.com"

    def test_handles_bare_hostname(self) -> None:
        assert env_utils._host_from_url("api.anthropic.com") == "api.anthropic.com"

    def test_empty_returns_empty(self) -> None:
        assert env_utils._host_from_url("") == ""
        assert env_utils._host_from_url("   ") == ""


class TestHomeAndWorkspace:
    """Tests for env_utils.opencode_home / data_dir / workspace resolution."""

    def test_default_config_dir(self, clean_env: None) -> None:
        assert env_utils.opencode_home() == "/workspace/.opencode/config"

    def test_custom_config_dir(self, clean_env: None) -> None:
        os.environ["OPENCODE_CONFIG_DIR"] = "/var/lib/opencode"
        assert env_utils.opencode_home() == "/var/lib/opencode"

    def test_default_data_dir(self, clean_env: None) -> None:
        assert env_utils.opencode_data_dir() == "/workspace/.opencode/data"

    def test_custom_data_dir(self, clean_env: None) -> None:
        os.environ["OPENCODE_DATA_DIR"] = "/var/lib/opencode-data"
        assert env_utils.opencode_data_dir() == "/var/lib/opencode-data"

    def test_default_workspace(self, clean_env: None) -> None:
        assert env_utils.opencode_workspace() == "/workspace"


class TestOpencodeModel:
    """Tests for env_utils.opencode_model() — model selection."""

    def test_explicit_model_wins(self, clean_env: None) -> None:
        os.environ["OPENCODE_MODEL"] = "openai/gpt-5-mini"
        assert env_utils.opencode_model() == "openai/gpt-5-mini"

    def test_anthropic_model_fallback(self, clean_env: None) -> None:
        # When OPENCODE_MODEL is unset and provider is anthropic, fall back
        # to ANTHROPIC_MODEL for parity with how Anthropic SDK users
        # configure the model in their shell.
        os.environ["OPENCODE_PROVIDER"] = "anthropic"
        os.environ["ANTHROPIC_MODEL"] = "claude-sonnet-4-7"
        assert env_utils.opencode_model() == "claude-sonnet-4-7"

    def test_no_default_for_non_anthropic_raises(self, clean_env: None) -> None:
        # OpenAI and other non-Anthropic providers have no safe
        # cross-provider default, so omitting both OPENCODE_MODEL and
        # ANTHROPIC_MODEL raises.
        os.environ["OPENCODE_PROVIDER"] = "openai"
        with pytest.raises(SystemExit):
            env_utils.opencode_model()

    def test_anthropic_default_when_nothing_set(self, clean_env: None) -> None:
        os.environ["OPENCODE_PROVIDER"] = "anthropic"
        # No OPENCODE_MODEL, no ANTHROPIC_MODEL → must use the shipped default
        assert env_utils.opencode_model() == "claude-sonnet-4-6"


class TestLoadLocalDotenv:
    """Tests for env_utils.load_local_dotenv() — .env auto-discovery."""

    def test_load_local_dotenv_does_not_raise(self) -> None:
        # Smoke test: the function should not raise even when no .env exists.
        env_utils.load_local_dotenv()


# ── required / optional / int_env / _env_positive_int ──────────────────────


class TestOptionalAndIntEnv:
    """Tests for env_utils.optional / required / int_env."""

    def test_optional_returns_default_when_unset(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            assert env_utils.optional("X", "fallback") == "fallback"

    def test_optional_returns_empty_when_unset_and_no_default(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            assert env_utils.optional("X") == ""

    def test_required_raises_when_unset(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            with pytest.raises(SystemExit):
                env_utils.required("X")

    def test_int_env_returns_default_when_unset(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            assert env_utils.int_env("X", 42) == 42

    def test_int_env_parses_value(self) -> None:
        with mock.patch.dict(os.environ, {"X": "123"}):
            assert env_utils.int_env("X", 0) == 123

    def test_int_env_rejects_non_integer(self) -> None:
        with mock.patch.dict(os.environ, {"X": "abc"}):
            with pytest.raises(SystemExit):
                env_utils.int_env("X", 0)


class TestEnvPositiveInt:
    """Verify env-var fallback also rejects zero / negative / non-integer values.

    argparse evaluates ``default=`` before ``type=``, so the bare
    ``default=int_env(...)`` pattern would silently let
    ``OPENCODE_SANDBOX_TIMEOUT=0`` reach the SDK. These tests pin down
    ``_env_positive_int`` as the right helper.
    """

    def test_unset_returns_default(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            assert env_utils._env_positive_int("X", 42) == 42

    def test_empty_returns_default(self) -> None:
        with mock.patch.dict(os.environ, {"X": ""}):
            assert env_utils._env_positive_int("X", 42) == 42

    def test_positive_passes_through(self) -> None:
        with mock.patch.dict(os.environ, {"X": "120"}):
            assert env_utils._env_positive_int("X", 1) == 120

    def test_zero_raises(self) -> None:
        with mock.patch.dict(os.environ, {"X": "0"}):
            with pytest.raises(SystemExit):
                env_utils._env_positive_int("X", 1)

    def test_negative_raises(self) -> None:
        with mock.patch.dict(os.environ, {"X": "-5"}):
            with pytest.raises(SystemExit):
                env_utils._env_positive_int("X", 1)

    def test_non_integer_raises(self) -> None:
        with mock.patch.dict(os.environ, {"X": "abc"}):
            with pytest.raises(SystemExit):
                env_utils._env_positive_int("X", 1)


# ── _opencode_common ──────────────────────────────────────────────────────


class TestPositiveInt:
    """Tests for _opencode_common.positive_int — argparse type guard."""

    def test_parses_positive_integer(self) -> None:
        assert positive_int("42") == 42

    def test_rejects_zero(self) -> None:
        with pytest.raises(argparse.ArgumentTypeError):
            positive_int("0")

    def test_rejects_negative(self) -> None:
        with pytest.raises(argparse.ArgumentTypeError):
            positive_int("-5")

    def test_rejects_non_integer(self) -> None:
        with pytest.raises(argparse.ArgumentTypeError):
            positive_int("abc")


class TestEnsureSuccess:
    """Tests for _opencode_common.ensure_success — exit-code → SystemExit."""

    def test_zero_exit_does_not_raise(self) -> None:
        result = mock.MagicMock(exit_code=0)
        # must not raise
        ensure_success(result, "do something")

    def test_none_exit_does_not_raise(self) -> None:
        result = mock.MagicMock(exit_code=None, stdout="ok", stderr="")
        ensure_success(result, "do something")  # must not raise

    def test_non_zero_exit_raises(self) -> None:
        result = mock.MagicMock(exit_code=1, stdout="out", stderr="error")
        with pytest.raises(SystemExit) as exc_info:
            ensure_success(result, "do something")
        assert "Failed to do something (exit 1)" in str(exc_info.value)


class TestSandboxIdentifier:
    """Tests for _opencode_common.sandbox_identifier — attribute priority."""

    def test_prefers_sandbox_id(self) -> None:
        sandbox = types.SimpleNamespace(sandbox_id="sb-abc", id="legacy-id")
        assert sandbox_identifier(sandbox) == "sb-abc"

    def test_falls_back_to_id(self) -> None:
        sandbox = types.SimpleNamespace(id="legacy-id")
        assert sandbox_identifier(sandbox) == "legacy-id"

    def test_returns_unknown_when_neither_attr(self) -> None:
        sandbox = types.SimpleNamespace()
        assert sandbox_identifier(sandbox) == "unknown"


class TestStreamWriter:
    """Tests for _opencode_common.stream_writer — chunk adapter."""

    def test_writes_plain_string(self) -> None:
        buf = io.StringIO()
        writer = stream_writer(buf)
        writer("hello")
        assert buf.getvalue() == "hello"

    def test_handles_chunk_with_line_attr(self) -> None:
        buf = io.StringIO()
        writer = stream_writer(buf)
        chunk = mock.MagicMock()
        chunk.line = "chunked output"
        writer(chunk)
        assert buf.getvalue() == "chunked output"