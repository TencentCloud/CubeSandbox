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


# ── E2B per-host request transforms compatibility ───────────────────────────
#
# E2B's ``network.rules`` accepts a host-keyed mapping of transforms:
#
#     network = {
#         "rules": {
#             "api.example.com": [
#                 {"transform": {"headers": {"X-Header": "Content"}}}
#             ]
#         }
#     }
#
# CubeEgress expresses the same intent as a list of L7 rules with credential
# ``inject`` entries:
#
#     Rule(
#         name="e2b-transform-api.example.com-0",
#         match=Match(host="api.example.com"),
#         action=Action(
#             allow=True,
#             inject=[Inject(header="X-Header", secret="Content")],
#         ),
#     )
#
# ``_convert_e2b_per_host_rules`` is the bridge: given an E2B-shaped dict, it
# returns a list of dicts that ``_serialize_rule`` can consume directly.
# A bare list (already CubeEgress-shaped) is passed through untouched so
# existing callers keep working.


def _is_e2b_per_host_rules(rules: Any) -> bool:
    """Return True iff *rules* looks like E2B's per-host transform mapping.

    The current CubeSandbox ``rules`` field is a list; E2B's is a dict keyed
    by host. The two shapes are unambiguous, so we use the type alone as the
    discriminator.
    """
    return isinstance(rules, dict)


def _convert_e2b_transform_to_inject(transform: Dict[str, Any]) -> List[Inject]:
    """Convert one E2B ``transform`` block to a list of :class:`Inject`.

    Today only ``transform.headers`` is documented; additional transform
    kinds are reported as ``ValueError`` so that silent drops never produce
    a sandbox that's missing credential injection the caller asked for.
    """
    if not isinstance(transform, dict):
        raise ValueError(
            f"network.rules transform must be a dict, got {type(transform).__name__}"
        )
    headers = transform.get("headers")
    if headers is None:
        raise ValueError("network.rules transform requires a 'headers' field")
    if not isinstance(headers, dict):
        raise ValueError(
            f"network.rules transform.headers must be a dict, got {type(headers).__name__}"
        )
    unknown = set(transform.keys()) - {"headers"}
    if unknown:
        raise ValueError(
            f"network.rules transform has unsupported keys: {sorted(unknown)!r}; "
            "only 'headers' is supported by the CubeEgress compatibility layer"
        )
    injects: List[Inject] = []
    for name, value in headers.items():
        if not isinstance(name, str) or not name:
            raise ValueError("network.rules transform.headers keys must be non-empty strings")
        if not isinstance(value, str):
            raise ValueError(
                f"network.rules transform.headers[{name!r}] must be a string, "
                f"got {type(value).__name__}"
            )
        injects.append(Inject(header=name, secret=value))
    return injects


def _convert_e2b_per_host_rules(rules: Dict[str, Any]) -> List[Dict[str, Any]]:
    """Convert E2B's per-host transform mapping to CubeEgress rule dicts.

    Each ``host -> [transform_entry, ...]`` pair fans out into one Rule per
    entry, preserving the original list order so audit logs and rule
    evaluation remain stable. The generated rule names use the
    ``e2b-transform-<host>[-<index>]`` convention to make compat-layer
    output identifiable in audit logs.

    The result is a list of plain dicts (not :class:`Rule` instances) so it
    flows through ``_serialize_rule`` unchanged and matches the shape that
    callers can already pass via ``network={"rules": [...]}``.
    """
    converted: List[Dict[str, Any]] = []
    for host, entries in rules.items():
        if not isinstance(host, str) or not host:
            raise ValueError(
                "network.rules host keys must be non-empty strings"
            )
        if not isinstance(entries, list):
            raise ValueError(
                f"network.rules[{host!r}] must be a list of transform entries, "
                f"got {type(entries).__name__}"
            )
        for index, entry in enumerate(entries):
            if not isinstance(entry, dict):
                raise ValueError(
                    f"network.rules[{host!r}][{index}] must be a dict, "
                    f"got {type(entry).__name__}"
                )
            transform = entry.get("transform")
            if transform is None:
                raise ValueError(
                    f"network.rules[{host!r}][{index}] is missing the 'transform' field"
                )
            unknown = set(entry.keys()) - {"transform"}
            if unknown:
                raise ValueError(
                    f"network.rules[{host!r}][{index}] has unsupported keys: "
                    f"{sorted(unknown)!r}; only 'transform' is supported"
                )
            injects = _convert_e2b_transform_to_inject(transform)
            suffix = "" if len(entries) == 1 else f"-{index}"
            converted.append({
                "name": f"e2b-transform-{host}{suffix}",
                "match": {"host": host},
                "action": {
                    "allow": True,
                    "inject": [i.to_wire() for i in injects],
                },
            })
    return converted


def _normalize_rules_arg(rules: Any) -> List[Any]:
    """Return a list of rule items suitable for :func:`_serialize_rule`.

    Accepts:
    - ``None`` / empty → ``[]``.
    - ``dict`` → treated as E2B per-host transforms, expanded via
      :func:`_convert_e2b_per_host_rules`.
    - ``list`` / other iterable of ``Rule`` or rule-shaped dicts → returned
      as a list, untouched.
    """
    if not rules:
        return []
    if _is_e2b_per_host_rules(rules):
        return _convert_e2b_per_host_rules(rules)
    if isinstance(rules, list):
        return rules
    # Tolerate any other iterable so SDK callers can pass tuples/generators.
    try:
        return list(rules)
    except TypeError as exc:
        raise TypeError(
            f"network.rules must be a list or dict, got {type(rules).__name__}"
        ) from exc


DENY_ALL_IPV4_CIDR = "0.0.0.0/0"
ALLOW_OUT_DOMAIN_REQUIRES_DENY_ALL = (
    "When specifying allowed domains in allow_out, you must disable public "
    "outbound traffic or include '0.0.0.0/0' in deny_out to block all other traffic."
)


def _validate_allow_out_domains_require_deny_all(
    allow_out: list[str] | None,
    deny_out: list[str] | None,
    *,
    default_deny_all: bool = False,
) -> None:
    if not any(_is_domain_allow_out_target(target) for target in allow_out or []):
        return
    if default_deny_all or any(str(target).strip() == DENY_ALL_IPV4_CIDR for target in deny_out or []):
        return
    from ._exceptions import ApiError

    raise ApiError(ALLOW_OUT_DOMAIN_REQUIRES_DENY_ALL, 400)


def _is_domain_allow_out_target(target: object) -> bool:
    import ipaddress

    if not isinstance(target, str):
        return False
    target = target.strip()
    if not target or "/" in target:
        return False
    try:
        ipaddress.ip_address(target)
        return False
    except ValueError:
        pass
    if _is_dotted_decimal_like_target(target):
        return False

    domain = target.rstrip(".").lower()
    if domain.startswith("*."):
        domain = domain[2:]
    elif "*" in domain:
        return False
    return _is_valid_dns_domain_name(domain)


def _is_dotted_decimal_like_target(target: str) -> bool:
    parts = target.rstrip(".").split(".")
    return len(parts) == 4 and all(part and part.isdigit() for part in parts)


def _is_valid_dns_domain_name(domain: str) -> bool:
    labels = domain.split(".")
    return bool(domain) and len(domain) < 255 and all(
        label
        and len(label) <= 63
        and not label.startswith("-")
        and not label.endswith("-")
        and all(ch.isalnum() or ch == "-" for ch in label)
        for label in labels
    )
