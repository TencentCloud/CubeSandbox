# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""Tests for the ``cubesandbox._policy`` module — L7 egress rule dataclasses."""

from __future__ import annotations

import pytest

from cubesandbox import Action, Inject, Match, Rule
from cubesandbox._policy import (
    SECRET_MAX_BYTES,
    _convert_e2b_per_host_rules,
    _normalize_inject_dict,
    _normalize_match_dict,
    _serialize_rule,
)


class TestMatchWireShape:
    def test_empty_match_produces_empty_dict(self):
        assert Match().to_wire() == {}

    def test_host_only(self):
        assert Match(host="api.example.com").to_wire() == {"host": "api.example.com"}

    def test_default_port_scheme_omitted(self):
        # No port/scheme → wire dict does not carry those keys, keeping
        # backward compatibility with servers that predate the L7 port field.
        wire = Match(host="api.example.com").to_wire()
        assert "port" not in wire
        assert "scheme" not in wire

    def test_explicit_port_and_scheme_serialized(self):
        wire = Match(host="api.example.com", port=8080, scheme="http").to_wire()
        assert wire == {"host": "api.example.com", "port": 8080, "scheme": "http"}

    def test_scheme_alone_uses_default_port(self):
        # SDK does not expand scheme-only rules on the client side — the wire
        # form is left untouched and the server (network-agent) fills in the
        # scheme's default port (http → 80, https → 443) when building the
        # cubevs L7 plan. Verify only that the wire shape is stable.
        wire = Match(host="api.example.com", scheme="https").to_wire()
        assert wire == {"host": "api.example.com", "scheme": "https"}

    def test_scheme_normalized_strip_lowercase(self):
        # Scheme matching is case-insensitive across the stack (Lua
        # normalize_scheme and cubevs both lowercase/strip) — the SDK
        # normalizes before validating and stores the canonical lowercase
        # form on the wire.
        wire = Match(host="api.example.com", scheme=" HTTPS ").to_wire()
        assert wire == {"host": "api.example.com", "scheme": "https"}


class TestMatchPortValidation:
    def test_port_without_scheme_rejected(self):
        with pytest.raises(ValueError, match="scheme"):
            Match(host="api.example.com", port=8080)

    def test_port_out_of_range_low(self):
        with pytest.raises(ValueError, match=r"\[1, 65535\]"):
            Match(host="api.example.com", port=0, scheme="http")

    def test_port_out_of_range_high(self):
        with pytest.raises(ValueError, match=r"\[1, 65535\]"):
            Match(host="api.example.com", port=65536, scheme="http")

    def test_port_negative(self):
        with pytest.raises(ValueError, match=r"\[1, 65535\]"):
            Match(host="api.example.com", port=-1, scheme="http")

    def test_port_wrong_type_string(self):
        with pytest.raises(ValueError, match="int"):
            Match(host="api.example.com", port="8080", scheme="http")  # type: ignore[arg-type]

    def test_port_wrong_type_bool_rejected(self):
        # bool is a subclass of int in Python — must be filtered out
        # explicitly, otherwise Match(port=True) would slip through as port=1.
        with pytest.raises(ValueError, match="int"):
            Match(host="api.example.com", port=True, scheme="http")  # type: ignore[arg-type]

    @pytest.mark.parametrize("scheme", ["htps", "", 1])
    def test_invalid_scheme_rejected(self, scheme):
        with pytest.raises(ValueError, match="http.*https"):
            Match(host="api.example.com", scheme=scheme)  # type: ignore[arg-type]


class TestNormalizeMatchDict:
    def test_dict_pass_through(self):
        out = _normalize_match_dict({"host": "foo", "port": 8080, "scheme": "http"})
        assert out == {"host": "foo", "port": 8080, "scheme": "http"}

    def test_dict_port_without_scheme_rejected(self):
        with pytest.raises(ValueError, match="scheme"):
            _normalize_match_dict({"host": "foo", "port": 8080})

    def test_dict_port_out_of_range_rejected(self):
        with pytest.raises(ValueError, match=r"\[1, 65535\]"):
            _normalize_match_dict({"host": "foo", "port": 0, "scheme": "http"})

    def test_dict_input_not_mutated(self):
        # Regression guard: normalization returns a new dict so caller-owned
        # data structures are not silently changed.
        original = {"host": "foo"}
        out = _normalize_match_dict(original)
        assert out is not original

    def test_dict_scheme_normalized(self):
        # Same normalize-then-validate semantics as the Match dataclass:
        # mixed case and surrounding whitespace are accepted, and the wire
        # form carries the canonical lowercase scheme.
        out = _normalize_match_dict({"host": "foo", "port": 8443, "scheme": " HTTPS "})
        assert out["scheme"] == "https"

    @pytest.mark.parametrize("scheme", ["ftp", "", False])
    def test_dict_invalid_scheme_rejected(self, scheme):
        with pytest.raises(ValueError, match="http.*https"):
            _normalize_match_dict({"host": "foo", "scheme": scheme})


class TestRuleSerializerHandlesPortScheme:
    def test_rule_dataclass_carries_port(self):
        rule = Rule(
            name="api-inject",
            match=Match(host="api.example.com", port=8443, scheme="https"),
            action=Action(allow=True),
        )
        wire = _serialize_rule(rule)
        assert wire["match"]["port"] == 8443
        assert wire["match"]["scheme"] == "https"

    def test_rule_dict_carries_port(self):
        wire = _serialize_rule({
            "name": "api-inject",
            "match": {"host": "api.example.com", "port": 8443, "scheme": "https"},
            "action": {"allow": True},
        })
        assert wire["match"]["port"] == 8443


class TestInjectSecretValidation:
    def test_secret_at_cap_accepted(self):
        inj = Inject(header="Authorization", secret="x" * SECRET_MAX_BYTES)
        assert inj.to_wire()["secret"] == "x" * SECRET_MAX_BYTES

    def test_secret_over_cap_rejected(self):
        with pytest.raises(ValueError, match="exceeds 2048 bytes"):
            Inject(header="Authorization", secret="x" * (SECRET_MAX_BYTES + 1))

    def test_dict_secret_over_cap_rejected(self):
        with pytest.raises(ValueError, match="exceeds 2048 bytes"):
            _normalize_inject_dict({"header": "Authorization", "secret": "x" * (SECRET_MAX_BYTES + 1)})

    def test_e2b_header_over_cap_rejected(self):
        with pytest.raises(ValueError, match="exceeds 2048 bytes"):
            _convert_e2b_per_host_rules({
                "api.example.com": [
                    {"transform": {"headers": {"Authorization": "x" * (SECRET_MAX_BYTES + 1)}}},
                ],
            })


class TestE2BPerHostRulesCompat:
    def test_transform_still_works_without_port(self):
        # E2B compat layer does not use port; generated rules should not carry
        # port/scheme so they get the legacy default {80, 443} treatment.
        rules = _convert_e2b_per_host_rules({
            "api.example.com": [
                {"transform": {"headers": {"Authorization": "Bearer x"}}}
            ],
        })
        assert len(rules) == 1
        assert "port" not in rules[0]["match"]
        assert "scheme" not in rules[0]["match"]

    def test_empty_transform_list_rejected(self):
        # An empty entries list fans out to zero rules and would silently
        # drop the host the caller keyed in — must raise, not no-op.
        with pytest.raises(ValueError, match="empty list"):
            _convert_e2b_per_host_rules({"api.example.com": []})
