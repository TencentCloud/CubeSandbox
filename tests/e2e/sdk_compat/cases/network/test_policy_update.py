# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""In-place egress policy updates on a running sandbox.

What separates these cases from cases/network/test_policy.py: there the policy is
fixed at create time, so only the initial verdict is under test. Here the policy
changes under a live VM, which adds two things worth proving — new connections
follow the new policy, and connections that already exist are re-evaluated rather
than grandfathered.
"""

from __future__ import annotations

import os

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import (
    NETWORK_ALLOW_DENY,
    NETWORK_DNS_ALLOW,
    NETWORK_DYNAMIC_UPDATE,
    PAUSE_RESUME,
    ROLLBACK_CLONE,
)
from framework.lifecycle import (
    wait_until_data_plane_ready,
    wait_until_paused,
    wait_until_running,
)
from framework.network_probe import (
    ALTERNATE_TCP_TARGET_IP,
    TCP_TARGET_IP,
    assert_tcp_blocked,
    assert_tcp_reachable,
    require_conclusive_flow_outcome,
    start_established_flow,
    tcp_probe_command,
    wait_established_flow_outcome,
)

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.network,
    pytest.mark.p1,
    pytest.mark.requires_internet,
    pytest.mark.requires_capability(NETWORK_DYNAMIC_UPDATE),
]


DNS_LEARN_DOMAIN = os.environ.get("SDK_E2E_DNS_ALLOW_DOMAIN", "example.com")
DNS_LEARN_PORT = int(os.environ.get("SDK_E2E_DNS_ALLOW_TCP_PORT", "443"))


def probe(sdk_sandbox, sdk_e2e_config, target: str, port: int | None = None):
    kwargs = {"timeout": sdk_e2e_config.network_probe_timeout}
    if port is not None:
        kwargs["port"] = port
    return sdk_sandbox.run_command(
        tcp_probe_command(target, **kwargs),
        timeout=sdk_e2e_config.command_timeout,
    )


def resolve_first_ipv4(sdk_sandbox, sdk_e2e_config, host: str) -> str:
    """Resolve *host* inside the guest and return its first IPv4 address.

    Skips rather than fails when resolution does not settle: an unreachable
    resolver is an environment problem, and the case that follows can say
    nothing about policy without a concrete address to aim at.
    """
    result = sdk_sandbox.run_command(
        "python3 - <<'PY'\n"
        "import socket, time\n"
        f"host = {host!r}\n"
        "for attempt in range(3):\n"
        "    try:\n"
        "        infos = socket.getaddrinfo(host, None, family=socket.AF_INET)\n"
        "        print('RESOLVED:' + infos[0][4][0])\n"
        "        break\n"
        "    except Exception as exc:\n"
        "        if attempt == 2:\n"
        "            print(f'RESOLVE:ERROR:{type(exc).__name__}:{exc}')\n"
        "        else:\n"
        "            time.sleep(min(2 ** attempt, 5))\n"
        "PY",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    line = next(
        (
            ln.strip()
            for ln in result.stdout.splitlines()
            if ln.strip().startswith("RESOLVED:")
        ),
        "",
    )
    if not line:
        pytest.skip(
            f"could not resolve {host} inside the sandbox, so there is no learned "
            f"address to revoke; stdout={result.stdout!r}"
        )
    return line.split(":", 1)[1]


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_update_grants_new_destination(sdk_sandbox, sdk_e2e_config):
    """A destination blocked at create time becomes reachable after an update."""
    assert_tcp_blocked(probe(sdk_sandbox, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)

    sdk_sandbox.update_network(
        network={"allow_out": [TCP_TARGET_IP], "allow_internet_access": False}
    )

    assert_tcp_reachable(probe(sdk_sandbox, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_update_revokes_destination_for_new_connections(sdk_sandbox, sdk_e2e_config):
    """Dropping a target from the allow list blocks subsequent connections."""
    assert_tcp_reachable(probe(sdk_sandbox, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)

    # Empty policy: the update replaces rather than patches, so this revokes all.
    sdk_sandbox.update_network(network={"allow_internet_access": False})

    assert_tcp_blocked(probe(sdk_sandbox, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_update_swaps_allowed_destination(sdk_sandbox, sdk_e2e_config):
    """Replacing the allow list moves access rather than accumulating it."""
    sdk_sandbox.update_network(
        network={"allow_out": [ALTERNATE_TCP_TARGET_IP], "allow_internet_access": False}
    )

    assert_tcp_reachable(
        probe(sdk_sandbox, sdk_e2e_config, ALTERNATE_TCP_TARGET_IP),
        ALTERNATE_TCP_TARGET_IP,
    )
    assert_tcp_blocked(probe(sdk_sandbox, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_update_tears_down_established_connection(sdk_sandbox, sdk_e2e_config):
    """The differentiating case: a revoked connection dies instead of surviving.

    Without datapath re-evaluation an established flow keeps its create-time
    verdict, so revocation would only apply to future connections and an already
    open channel would stay usable indefinitely.
    """
    if not start_established_flow(
        sdk_sandbox,
        TCP_TARGET_IP,
        command_timeout=sdk_e2e_config.command_timeout,
    ):
        pytest.skip(f"could not establish a connection to {TCP_TARGET_IP} to hold open")

    sdk_sandbox.update_network(network={"allow_internet_access": False})

    outcome = require_conclusive_flow_outcome(
        wait_established_flow_outcome(
            sdk_sandbox,
            command_timeout=sdk_e2e_config.command_timeout,
        ),
        TCP_TARGET_IP,
    )
    assert outcome == "RESET", (
        f"established connection to {TCP_TARGET_IP} survived after its policy was "
        f"revoked; holder reported {outcome!r}"
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_update_preserves_still_allowed_established_connection(
    sdk_sandbox,
    sdk_e2e_config,
):
    """The other half: an update must not disturb flows it still permits.

    Re-evaluating every flow on every update is only safe if unchanged verdicts
    are left alone. Without this case a datapath that simply killed all sessions
    on any update would pass the revocation test above.
    """
    if not start_established_flow(
        sdk_sandbox,
        TCP_TARGET_IP,
        command_timeout=sdk_e2e_config.command_timeout,
    ):
        pytest.skip(f"could not establish a connection to {TCP_TARGET_IP} to hold open")

    # Still allows TCP_TARGET_IP; only widens the policy with a second target.
    sdk_sandbox.update_network(
        network={"allow_out": [TCP_TARGET_IP, ALTERNATE_TCP_TARGET_IP], "allow_internet_access": False}
    )

    outcome = require_conclusive_flow_outcome(
        wait_established_flow_outcome(
            sdk_sandbox,
            command_timeout=sdk_e2e_config.command_timeout,
        ),
        TCP_TARGET_IP,
    )
    assert outcome == "ALIVE", (
        "an update that still permits this destination tore down its established "
        f"connection; holder reported {outcome!r}"
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": ["dns.google"]},
)
def test_update_keeps_dns_working_for_domain_policy(sdk_sandbox, sdk_e2e_config):
    """Updating a domain policy must not withdraw resolver access along with it.

    The resolver CIDRs are injected by the control plane, not authored by the
    caller, so an update carrying only user targets could silently drop them and
    black-hole every domain rule it just installed.
    """
    sdk_sandbox.update_network(
        network={"allow_out": ["dns.google", "one.one.one.one"], "allow_internet_access": False}
    )

    result = sdk_sandbox.run_command(
        "getent hosts dns.google >/dev/null && echo RESOLVED || echo UNRESOLVED",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "RESOLVED" in result.stdout, (
        "DNS resolution broke after updating a domain-based policy; the resolver "
        f"allowance was likely dropped. stdout={result.stdout!r}"
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_DNS_ALLOW)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "allow_out": [DNS_LEARN_DOMAIN],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_update_revokes_addresses_a_domain_rule_taught(sdk_sandbox, sdk_e2e_config):
    """Dropping a domain rule must retire the addresses DNS learned under it.

    The probe deliberately aims at the resolved IP rather than the domain. A
    domain rule installs allow entries for whatever its A records resolve to,
    and those entries carry the record's TTL — floored at 300s. So re-resolving
    would prove nothing: the domain is gone from the allow list either way. Only
    a direct connect to the address it taught can tell "revoked now" from
    "revoked in five minutes", which is what this behaviour is about.
    """
    learned_ip = resolve_first_ipv4(sdk_sandbox, sdk_e2e_config, DNS_LEARN_DOMAIN)

    assert_tcp_reachable(
        probe(sdk_sandbox, sdk_e2e_config, learned_ip, DNS_LEARN_PORT),
        learned_ip,
        DNS_LEARN_PORT,
    )

    # Replace the policy with one that no longer names the domain.
    sdk_sandbox.update_network(
        network={"deny_out": ["0.0.0.0/0"], "allow_internet_access": False}
    )

    assert_tcp_blocked(
        probe(sdk_sandbox, sdk_e2e_config, learned_ip, DNS_LEARN_PORT),
        learned_ip,
        DNS_LEARN_PORT,
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(NETWORK_DNS_ALLOW)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={
        "allow_out": [DNS_LEARN_DOMAIN],
        "deny_out": ["0.0.0.0/0"],
    },
)
def test_update_keeps_addresses_a_surviving_domain_rule_taught(
    sdk_sandbox,
    sdk_e2e_config,
):
    """The other half: an update that keeps the domain must keep its addresses.

    Without this, a revocation that simply dropped every learned address on any
    update would pass the case above — and every domain-based sandbox would lose
    connectivity for a full TTL after an unrelated policy change.
    """
    learned_ip = resolve_first_ipv4(sdk_sandbox, sdk_e2e_config, DNS_LEARN_DOMAIN)

    assert_tcp_reachable(
        probe(sdk_sandbox, sdk_e2e_config, learned_ip, DNS_LEARN_PORT),
        learned_ip,
        DNS_LEARN_PORT,
    )

    # Still names the domain; only widens the policy with an unrelated target.
    sdk_sandbox.update_network(
        network={
            "allow_out": [DNS_LEARN_DOMAIN, ALTERNATE_TCP_TARGET_IP],
            "deny_out": ["0.0.0.0/0"],
            "allow_internet_access": False,
        }
    )

    assert_tcp_reachable(
        probe(sdk_sandbox, sdk_e2e_config, learned_ip, DNS_LEARN_PORT),
        learned_ip,
        DNS_LEARN_PORT,
    )


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(ROLLBACK_CLONE)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_clone_after_update_inherits_the_new_policy(sdk_sandbox, sdk_e2e_config):
    """A clone must carry the policy the sandbox has now, not the one it was born with.

    Clone is a snapshot plus a create from that snapshot, and the create replays
    the spec Master has stored. So this also covers read-your-writes: the update
    returns only after the spec is written, and the snapshot here is taken
    immediately afterwards.
    """
    sdk_sandbox.update_network(
        network={"allow_out": [ALTERNATE_TCP_TARGET_IP], "allow_internet_access": False}
    )

    clones = sdk_sandbox.clone(1)
    try:
        clone = clones[0]
        assert_tcp_reachable(
            probe(clone, sdk_e2e_config, ALTERNATE_TCP_TARGET_IP),
            ALTERNATE_TCP_TARGET_IP,
        )
        assert_tcp_blocked(probe(clone, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)
    finally:
        for clone in clones:
            clone.kill()


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.requires_capability(ROLLBACK_CLONE)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP, ALTERNATE_TCP_TARGET_IP]},
)
def test_clone_after_tightening_update_is_not_more_permissive(sdk_sandbox, sdk_e2e_config):
    """The dangerous direction: a stale spec would hand the clone wider access.

    If Master's spec lagged behind the node, a clone of a sandbox whose policy was
    just narrowed would come up with the older, broader allow list — a silent
    privilege escalation for every descendant of that sandbox.
    """
    sdk_sandbox.update_network(
        network={"allow_out": [TCP_TARGET_IP], "allow_internet_access": False}
    )

    clones = sdk_sandbox.clone(1)
    try:
        clone = clones[0]
        assert_tcp_blocked(
            probe(clone, sdk_e2e_config, ALTERNATE_TCP_TARGET_IP),
            ALTERNATE_TCP_TARGET_IP,
        )
        assert_tcp_reachable(probe(clone, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)
    finally:
        for clone in clones:
            clone.kill()


@pytest.mark.requires_capability(PAUSE_RESUME)
@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(
    allow_internet_access=False,
    network={"allow_out": [TCP_TARGET_IP]},
)
def test_update_survives_pause_resume(sdk_sandbox, sdk_e2e_config):
    """Resume must restore the updated policy, not the create-time one.

    Pause packages the sandbox from Cubelet's own store rather than from the
    network runtime's state file, so an update has to reach that store too or the
    policy silently reverts on resume.
    """
    sdk_sandbox.update_network(
        network={"allow_out": [ALTERNATE_TCP_TARGET_IP], "allow_internet_access": False}
    )

    sdk_sandbox.pause(timeout=sdk_e2e_config.default_timeout)
    wait_until_paused(sdk_sandbox, timeout=sdk_e2e_config.default_timeout)
    resumed = sdk_sandbox.resume_or_connect(timeout=sdk_e2e_config.default_timeout)
    try:
        wait_until_running(resumed, timeout=sdk_e2e_config.default_timeout)
        wait_until_data_plane_ready(
            resumed,
            timeout=sdk_e2e_config.default_timeout,
            command_timeout=sdk_e2e_config.command_timeout,
        )
        assert_tcp_reachable(
            probe(resumed, sdk_e2e_config, ALTERNATE_TCP_TARGET_IP),
            ALTERNATE_TCP_TARGET_IP,
        )
        assert_tcp_blocked(probe(resumed, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)
    finally:
        resumed.close()


@pytest.mark.requires_capability(NETWORK_ALLOW_DENY)
@pytest.mark.sandbox_create_options(allow_internet_access=False)
def test_repeated_updates_converge(sdk_sandbox, sdk_e2e_config):
    """Replaying updates is safe: the control plane diffs against live state.

    Guards the incremental map programming — a diff that leaked or double-freed
    entries would drift after a few rounds instead of landing on the same policy.
    """
    for _ in range(3):
        sdk_sandbox.update_network(
            network={"allow_out": [TCP_TARGET_IP], "allow_internet_access": False}
        )
        sdk_sandbox.update_network(
            network={"allow_out": [ALTERNATE_TCP_TARGET_IP], "allow_internet_access": False}
        )

    assert_tcp_reachable(
        probe(sdk_sandbox, sdk_e2e_config, ALTERNATE_TCP_TARGET_IP),
        ALTERNATE_TCP_TARGET_IP,
    )
    assert_tcp_blocked(probe(sdk_sandbox, sdk_e2e_config, TCP_TARGET_IP), TCP_TARGET_IP)
