# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
L7 egress policy types — host/path/SNI matching, audit, credential injection.

These dataclasses are pure data holders on the SDK side; matching and
evaluation happen server-side.

Wire format: ``to_wire()`` on each type emits the camelCase JSON shape that
nests under ``network.rules`` in the POST /sandboxes payload.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, List, Literal, Optional


Scheme = Literal["http", "https"]

Method = Literal[
    "GET", "HEAD", "POST", "PUT", "PATCH",
    "DELETE", "OPTIONS", "CONNECT", "TRACE",
]

AuditLevel = Literal["full", "metadata", "none"]


@dataclass
class Match:
    """
    Rule match conditions. All fields optional; empty Match matches any request.

    Multi-field semantics: AND across fields, OR within ``method``.
    Comparisons on sni/host/scheme are case-insensitive.
    """
    sni: Optional[str] = None
    host: Optional[str] = None
    method: Optional[List[Method]] = None
    path: Optional[str] = None
    scheme: Optional[Scheme] = None

    def to_wire(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {}
        if self.sni is not None:
            out["sni"] = self.sni
        if self.host is not None:
            out["host"] = self.host
        if self.method is not None:
            out["method"] = list(self.method)
        if self.path is not None:
            out["path"] = self.path
        if self.scheme is not None:
            out["scheme"] = self.scheme
        return out


@dataclass
class Inject:
    """
    Credential injection. Only honored when ``Action.allow=True`` and the
    request is HTTPS with matching SNI/Host (server enforces).
    """
    header: str
    secret: str
    format: Optional[str] = None

    def render(self) -> str:
        """Render the final injected header value (preview helper)."""
        fmt = self.format or "${SECRET}"
        return fmt.replace("${SECRET}", self.secret)

    def to_wire(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {"header": self.header, "secret": self.secret}
        if self.format is not None:
            out["format"] = self.format
        return out


@dataclass
class Action:
    """
    Rule action.

    - ``allow=True``: pass the request through; optional credential injection.
    - ``allow=False``: reject (HTTP 403); ``inject`` is ignored if set.
    - ``audit`` defaults to ``"metadata"`` server-side when omitted.
    """
    allow: bool
    inject: Optional[List[Inject]] = None
    audit: Optional[AuditLevel] = None

    def to_wire(self) -> Dict[str, Any]:
        out: Dict[str, Any] = {"allow": self.allow}
        if self.audit is not None:
            out["audit"] = self.audit
        if self.inject is not None:
            out["inject"] = [i.to_wire() for i in self.inject]
        return out


@dataclass
class Rule:
    """
    Egress rule. ``name`` is a human-readable label used for audit logging.
    """
    name: str
    match: Match
    action: Action

    def to_wire(self) -> Dict[str, Any]:
        return {
            "name": self.name,
            "match": self.match.to_wire(),
            "action": self.action.to_wire(),
        }


# ── dict → wire normalization (lets callers pass plain dicts) ────────────────

# All match keys today are wire-identical (no camelCase rename needed
# after dropping sni_suffix/path_prefix). Kept as a no-op pass-through
# so callers passing a plain dict don't accidentally have their input
# mutated.


def _normalize_match_dict(m: Dict[str, Any]) -> Dict[str, Any]:
    return dict(m)


def _normalize_inject_dict(i: Dict[str, Any]) -> Dict[str, Any]:
    # No snake_case keys to translate today; pass through verbatim.
    return dict(i)


def _normalize_action_dict(a: Dict[str, Any]) -> Dict[str, Any]:
    out: Dict[str, Any] = {}
    for k, v in a.items():
        if k == "inject" and v is not None:
            out["inject"] = [_normalize_inject_dict(x) for x in v]
        else:
            out[k] = v
    return out


def _serialize_rule(rule: Any) -> Dict[str, Any]:
    """Serialize a Rule dataclass or a dict-shaped rule to the wire JSON.

    Accepts:
    - ``Rule`` instances → delegates to ``Rule.to_wire()``.
    - ``dict`` with the same wire keys (sni / host / method / path /
      scheme).
    """
    if isinstance(rule, Rule):
        return rule.to_wire()
    if not isinstance(rule, dict):
        raise TypeError(f"rule must be Rule or dict, got {type(rule).__name__}")

    out: Dict[str, Any] = {}
    if "name" in rule:
        out["name"] = rule["name"]
    if "match" in rule and rule["match"] is not None:
        out["match"] = _normalize_match_dict(rule["match"])
    if "action" in rule and rule["action"] is not None:
        out["action"] = _normalize_action_dict(rule["action"])
    return out
