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


def _validate_scheme(value: Any, field: str) -> Optional[str]:
    """Normalize *value* (strip + lowercase) and validate against http/https.

    Returns the normalized scheme, or ``None`` when *value* is ``None``.
    Scheme matching is case-insensitive across the stack (Lua
    ``normalize_scheme`` and cubevs both lowercase/strip), so the SDK
    normalizes before the membership check and propagates the clean
    lowercase value downstream.
    """
    if value is None:
        return None
    if not isinstance(value, str):
        raise ValueError(f"{field} must be 'http' or 'https', got {value!r}")
    normalized = value.strip().lower()
    if normalized not in ("http", "https"):
        raise ValueError(f"{field} must be 'http' or 'https', got {value!r}")
    return normalized


@dataclass
class Match:
    """
    Rule match conditions. All fields optional; empty Match matches any request.

    Multi-field semantics: AND across fields, OR within ``method``.
    Comparisons on sni/host/scheme are case-insensitive.

    ``port`` + ``scheme`` together drive which TCP port CubeEgress intercepts
    on the sandbox side. Both must be set together or both omitted:

    - Both omitted → default set ``{80/http, 443/https}`` (backward compatible).
    - Both set → CubeEgress intercepts only that (host, port, scheme) tuple.

    Every rule sharing the same ``(host, port)`` MUST agree on ``scheme`` —
    a port can only route to one nginx listener (HTTP → 8080, HTTPS → 8443).
    The server rejects the whole policy if it detects a mismatch.
    """
    sni: Optional[str] = None
    host: Optional[str] = None
    method: Optional[List[Method]] = None
    path: Optional[str] = None
    scheme: Optional[Scheme] = None
    port: Optional[int] = None

    def __post_init__(self) -> None:
        # Client-side pre-validation. Not strictly required (the server would
        # reject the same shape) but catches typos before the network round-trip
        # and produces a Pythonic error path (ValueError) instead of a 400.
        # _validate_scheme returns the normalized scheme; store it so
        # to_wire() emits the canonical lowercase form.
        self.scheme = _validate_scheme(self.scheme, "Match.scheme")
        if self.port is not None:
            if not isinstance(self.port, int) or isinstance(self.port, bool):
                raise ValueError(f"Match.port must be an int, got {type(self.port).__name__}")
            if self.port < 1 or self.port > 65535:
                raise ValueError(f"Match.port must be in [1, 65535], got {self.port}")
            if self.scheme is None:
                raise ValueError("Match.port requires Match.scheme to be set")
        if self.scheme is not None and self.port is None:
            # Legacy shape: scheme alone (no port) is still accepted server-side
            # as a match qualifier that filters by HTTP vs HTTPS on the default
            # {80, 443} set. This is not the new port-scoped feature, so we do
            # NOT raise — but callers relying on it get the classic behavior.
            pass

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
        if self.port is not None:
            out["port"] = self.port
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
    out = dict(m)
    # Best-effort client-side validation for the port + scheme pair. Mirrors
    # Match.__post_init__ so dict-shaped and dataclass-shaped rules get the
    # same behavior. Errors here would otherwise surface as a 400 from the
    # server; catching them locally gives users a Pythonic ValueError with a
    # stack trace pointing at the offending call site.
    port = out.get("port")
    scheme = _validate_scheme(out.get("scheme"), "match.scheme")
    if scheme is not None:
        # Propagate the normalized (stripped, lowercased) scheme so the wire
        # form is canonical regardless of caller casing/whitespace.
        out["scheme"] = scheme
    if port is not None:
        if not isinstance(port, int) or isinstance(port, bool):
            raise ValueError(
                f"match.port must be an int, got {type(port).__name__}"
            )
        if port < 1 or port > 65535:
            raise ValueError(f"match.port must be in [1, 65535], got {port}")
        if scheme is None:
            raise ValueError("match.port requires match.scheme to be set")
    return out


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
        if not entries:
            # An empty list would fan out to zero rules and silently drop the
            # host the caller keyed in — the exact "silent drop" this
            # compatibility layer exists to prevent.
            raise ValueError(
                f"network.rules[{host!r}] is an empty list; every host must "
                "declare at least one transform entry"
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


def _build_network_update_body(network: Dict[str, Any]) -> Dict[str, Any]:
    """Translate ``Sandbox.update_network``'s single argument into the API body.

    The update endpoint takes the policy at the top level and carries
    ``allow_internet_access`` alongside it, so this flattens what
    :func:`_build_network_payload` nests. Only keys the caller actually supplied
    are emitted: the flag is presence-based, matching E2B, so omitting it is
    distinct from sending ``true``.
    """
    unknown = set(network) - _NETWORK_UPDATE_KEYS
    if unknown:
        raise ValueError(
            f"network has unsupported keys: {sorted(unknown)!r}; "
            f"supported keys are {sorted(_NETWORK_UPDATE_KEYS)!r}"
        )
    allow_internet_access = network.get("allow_internet_access", True)
    if not isinstance(allow_internet_access, bool):
        raise ValueError(
            "network.allow_internet_access must be a bool, got "
            f"{type(allow_internet_access).__name__}"
        )
    body = _build_network_payload(network, allow_internet_access=allow_internet_access)
    if "allow_internet_access" in network:
        body["allowInternetAccess"] = allow_internet_access
    return body


_NETWORK_UPDATE_KEYS = frozenset(
    {
        "allow_out",
        "deny_out",
        "rules",
        "allow_internet_access",
        "allow_public_traffic",
        "mask_request_host",
    }
)


def _build_network_payload(
    network: Dict[str, Any],
    *,
    allow_internet_access: bool = True,
) -> Dict[str, Any]:
    """Translate the SDK's snake_case ``network`` argument into the API body.

    Shared by sandbox creation and :meth:`Sandbox.update_network` so both accept
    exactly the same argument shape and run the same client-side validation.
    """
    _validate_allow_out_domains_require_deny_all(
        network.get("allow_out"),
        network.get("deny_out"),
        default_deny_all=not allow_internet_access,
    )
    net: Dict[str, Any] = {}
    if "allow_out" in network:
        net["allowOut"] = network["allow_out"]
    if "deny_out" in network:
        net["denyOut"] = network["deny_out"]
    if "allow_public_traffic" in network:
        net["allowPublicTraffic"] = network["allow_public_traffic"]
    if "mask_request_host" in network:
        net["maskRequestHost"] = network["mask_request_host"]
    if network.get("rules"):
        # ``rules`` accepts either CubeEgress's list-of-Rule shape or E2B's
        # per-host transform mapping (``{host: [{transform: {...}}]}``).
        # ``_normalize_rules_arg`` collapses both into a list of rule dicts
        # that ``_serialize_rule`` understands.
        normalized_rules = _normalize_rules_arg(network["rules"])
        if normalized_rules:
            net["rules"] = [_serialize_rule(r) for r in normalized_rules]
    return net


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
