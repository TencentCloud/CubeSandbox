# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Host-mount boundary and behavior tests not covered by the example.

The example (test_create_with_mount.py) covers the happy path and a single
disallowed hostPath. This module covers behaviors that only an end-to-end run
can prove:

- create-time rejection by CubeMaster's lexical validation of illegal hostPath
  shapes (traversal, prefix spoofing, relative/empty/root paths) and mountPath
  shapes (relative/empty), surfaced through the SDK/API;
- create-time rejection of malformed host-mount annotations (non-JSON string,
  JSON that is not an array of descriptors);
- runtime rejection by Cubelet for hostPaths that pass lexical validation but
  cannot be bind-mounted (source does not exist, source is a regular file);
- acceptance of the exact allowed-prefix directory and of an empty mount list;
- real read-write host sharing: two sandboxes bind-mounting the same host dir
  observe each other's writes (proving it is a genuine host mount, not a
  per-sandbox overlay);
- nested RO parent + RW child: child remains writable even when the child
  descriptor is listed before the parent (regression for Cubelet mount-order
  determinism; source PR #946 / issue #944). Provisions host dirs via a
  throwaway sandbox; a missing hostPath source on multi-node local-disk
  clusters is treated as pass-with-warning;
- the symlink TOCTOU escape: CubeMaster validates the path lexically while
  Cubelet resolves symlinks before mounting, so a symlink planted under the
  allowed prefix can redirect the mount to an arbitrary host path.
"""

from __future__ import annotations

import uuid

import pytest

from framework.assertions import assert_command_ok
from framework.capabilities import HOST_MOUNT
from framework.host_mount import (
    HOST_MOUNT_KEY,
    expect_create_rejected,
    host_mount_metadata,
    mount_option,
    provision_host_dirs,
    skip_if_host_mount_unavailable,
    under_prefix,
    warn_pass_missing_hostpath_source,
)
from framework.lifecycle import managed_control_sandbox

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.sdk_compat,
    pytest.mark.host_mount,
    pytest.mark.p1,
    pytest.mark.requires_capability(HOST_MOUNT),
]


@pytest.fixture(autouse=True)
def _require_host_mount_backend(sdk_backend, sdk_e2e_config):
    """Gate every test in this module on host-mount support.

    Most tests here create sandboxes directly (via ``expect_create_rejected`` or
    ``managed_control_sandbox``) instead of requesting the ``sdk_sandbox``
    fixture, so they never pass through the ``requires_capability`` check that
    fixture performs. Without this gate the
    create-rejection assertions would run against a backend (e.g. e2b) that
    silently ignores the host-mount metadata, creating the sandbox and failing
    the test spuriously. Skip the same cases ``sdk_sandbox`` would.
    """
    skip_if_host_mount_unavailable(sdk_backend, sdk_e2e_config)


# --- Create-time rejections --------------------------------------------------


# Each case pins the *reason* CubeMaster rejects it, so a regression that swaps
# which branch fires (absolute-path check vs. allowed-prefix check) is caught
# rather than masked by a permissive OR. "absolute": rejected by the
# leading-slash check before validateHostPath; "prefix": rejected by
# validateHostPath's allowed-prefix comparison (see hostdir_mount.go).
#
# The values are the *exact* production phrases CubeMaster emits (hostdir_mount.go:
# "hostPath must be an absolute path", "is not within an allowed mount prefix"),
# so a generic error that merely contains the word "prefix"/"absolute" cannot
# satisfy the assertion for the wrong reason.
_REASON_PHRASE = {
    "absolute": "must be an absolute path",
    "prefix": "is not within an allowed mount prefix",
}

_ILLEGAL_HOSTPATHS = [
    ("path_traversal", under_prefix("..", "etc", "shadow"), "prefix"),
    ("prefix_spoof", f"{under_prefix()}_evil/data", "prefix"),
    ("relative_host_path", "data/shared/foo", "absolute"),
    ("outside_prefix", "/var/run/secret", "prefix"),
    ("root_hostpath", "/", "prefix"),
    ("empty_hostpath", "", "absolute"),
]


@pytest.mark.parametrize(
    ("case", "host_path", "reason"),
    _ILLEGAL_HOSTPATHS,
    ids=[c[0] for c in _ILLEGAL_HOSTPATHS],
)
def test_illegal_hostpath_rejected(case, host_path, reason, sdk_backend, sdk_e2e_config):
    """Illegal hostPath shapes are rejected before the sandbox boots."""
    metadata = host_mount_metadata([mount_option(host_path, "/mnt/x")])
    message = expect_create_rejected(
        sdk_backend, sdk_e2e_config, metadata, role=f"reject_{case}"
    )
    phrase = _REASON_PHRASE[reason]
    assert phrase in message.lower(), (
        f"expected a {reason!r} rejection ({phrase!r}) for {case!r} "
        f"({host_path!r}), got: {message!r}"
    )


@pytest.mark.parametrize(
    ("case", "mount_path"),
    [
        ("relative_mountpath", "mnt/rel"),
        ("empty_mountpath", ""),
    ],
    ids=["relative_mountpath", "empty_mountpath"],
)
def test_illegal_mountpath_rejected(case, mount_path, sdk_backend, sdk_e2e_config):
    """mountPath must be an absolute path; relative or empty is rejected."""
    metadata = host_mount_metadata([mount_option(under_prefix("ok"), mount_path)])
    message = expect_create_rejected(
        sdk_backend, sdk_e2e_config, metadata, role=f"reject_{case}"
    )
    assert "mountpath must be an absolute path" in message.lower(), (
        f"expected an absolute-mountPath rejection for {case!r} "
        f"({mount_path!r}), got: {message!r}"
    )


def test_mixed_valid_and_invalid_entries_rejected(sdk_backend, sdk_e2e_config):
    """A list mixing a legal and an illegal entry is rejected atomically.

    Neither mount should be applied: one bad entry fails the whole create.
    """
    metadata = host_mount_metadata(
        [
            mount_option(under_prefix("ok"), "/mnt/ok"),
            mount_option("/etc/secret", "/mnt/secret"),
        ]
    )
    message = expect_create_rejected(
        sdk_backend, sdk_e2e_config, metadata, role="reject_mixed"
    )
    assert "is not within an allowed mount prefix" in message.lower(), (
        f"expected the illegal entry to fail the whole create, got: {message!r}"
    )


@pytest.mark.parametrize(
    ("case", "raw"),
    [
        ("not_json", "this-is-not-json"),
        ("json_object_not_array", '{"hostPath": "/data/shared", "mountPath": "/mnt/x"}'),
        ("json_scalar", '"just-a-string"'),
    ],
    ids=["not_json", "json_object_not_array", "json_scalar"],
)
def test_malformed_annotation_rejected(case, raw, sdk_backend, sdk_e2e_config):
    """A host-mount annotation that is not a JSON array of descriptors is rejected.

    CubeMaster unmarshals the value into ``[]HostDirMountOption``; a non-JSON
    string, a bare object, or a scalar all fail to decode and abort create.
    """
    message = expect_create_rejected(
        sdk_backend,
        sdk_e2e_config,
        {HOST_MOUNT_KEY: raw},
        role=f"reject_{case}",
    )
    # CubeMaster wraps a decode failure as `invalid "host-mount" annotation: ...`
    # (hostdir_mount.go). Assert on that decode-specific text so an unrelated
    # transient API/network error cannot satisfy the assertion for the wrong
    # reason.
    lowered = message.lower()
    assert "invalid" in lowered and HOST_MOUNT_KEY in lowered, (
        f"expected a decode-failure rejection for {case!r}, got: {message!r}"
    )


# --- Runtime bind-mount failures (pass lexical check, fail in Cubelet) --------


def test_nonexistent_hostpath_rejected_at_create(sdk_backend, sdk_e2e_config):
    """A hostPath under the allowed prefix that does not exist fails create.

    This passes CubeMaster's lexical validation but Cubelet cannot bind-mount a
    missing source, so ``prepareHostDirVolume`` fails the create. We assert on
    Cubelet's own wrapper text (``bind mount ... ->``, see hostdir.go) rather
    than the underlying mount(8) stderr, which is util-linux-version- and
    locale-dependent. Uses a random unprovisioned subdir so it is guaranteed
    absent.
    """
    missing = under_prefix("definitely-missing", uuid.uuid4().hex)
    metadata = host_mount_metadata([mount_option(missing, "/mnt/missing")])
    message = expect_create_rejected(
        sdk_backend, sdk_e2e_config, metadata, role="reject_nonexistent_hostpath"
    )
    assert "bind mount" in message.lower(), (
        f"expected a bind-mount failure for the missing source {missing!r}, "
        f"got: {message!r}"
    )


def test_file_hostpath_rejected_at_create(sdk_backend, sdk_e2e_config):
    """A hostPath that is a regular file (not a directory) fails create.

    Cubelet creates the bind *destination* as a directory and bind-mounts the
    source onto it; binding a file onto a directory fails at mount time. We
    first materialize a file on the host under the allowed prefix, then attempt
    to mount it.
    """
    token = uuid.uuid4().hex
    rel_dir = f"filecase/{token}"
    file_rel = f"{rel_dir}/regular.txt"
    file_host_path = under_prefix(file_rel)

    # Materialize the host file: mount the prefix root RW and write it. Scope the
    # RW mount to the per-test subdir, not the whole prefix root, to avoid a
    # broad writable mount of shared host state.
    provision_host_dirs(sdk_backend, sdk_e2e_config, [rel_dir])
    seed_mount = "/mnt/seed"
    seed_metadata = host_mount_metadata(
        [mount_option(under_prefix(rel_dir), seed_mount, read_only=False)]
    )
    with managed_control_sandbox(
        sdk_backend,
        sdk_e2e_config,
        metadata={"test_role": "host_mount_file_seed", **seed_metadata},
    ) as seeder:
        seed = seeder.run_command(
            f"echo file-not-dir > {seed_mount}/regular.txt",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(seed)

        try:
            metadata = host_mount_metadata([mount_option(file_host_path, "/mnt/asfile")])
            message = expect_create_rejected(
                sdk_backend, sdk_e2e_config, metadata, role="reject_file_hostpath"
            )
            # Cubelet wraps every bind failure with "bind mount ... ->" (hostdir.go);
            # assert on that stable text rather than the mount(8) "not a directory"
            # tail, which varies by util-linux version/locale.
            assert "bind mount" in message.lower(), (
                f"expected a bind-mount failure for the file source {file_host_path!r}, "
                f"got: {message!r}"
            )
        finally:
            # Reuse the still-open seeder (RW-mounted at seed_mount) to remove the
            # per-token subtree so it does not accumulate on shared host storage
            # across CI runs. seed_mount maps to <prefix>/filecase/<token>.
            seeder.run_command(
                f"rm -rf {seed_mount}/regular.txt",
                timeout=sdk_e2e_config.command_timeout,
            )


# --- Accepted edge cases -----------------------------------------------------


@pytest.mark.sandbox_create_options(
    metadata=host_mount_metadata([mount_option(under_prefix(), "/mnt/exact")])
)
def test_exact_allowed_prefix_dir_mounts(sdk_sandbox, sdk_e2e_config):
    """The allowed-prefix directory itself (no child) is a valid hostPath."""
    result = sdk_sandbox.run_command(
        "test -d /mnt/exact && echo mounted",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert "mounted" in result.stdout, (
        f"expected /mnt/exact to be mounted; "
        f"stdout={result.stdout!r} stderr={result.stderr!r}"
    )


@pytest.mark.sandbox_create_options(metadata=host_mount_metadata([]))
def test_empty_mount_list_is_noop(sdk_sandbox, sdk_e2e_config):
    """An empty host-mount list creates a normal sandbox with no extra mount."""
    result = sdk_sandbox.run_command(
        "echo alive",
        timeout=sdk_e2e_config.command_timeout,
    )
    assert_command_ok(result)
    assert result.stdout.strip() == "alive"


# --- Real host sharing -------------------------------------------------------


def test_rw_mount_shares_data_across_sandboxes(sdk_backend, sdk_e2e_config):
    """Two sandboxes mounting the same host dir see each other's writes.

    Proves the mount is a genuine host bind-mount (shared state on the node),
    not a per-sandbox copy. Uses a unique subdirectory so parallel runs do not
    collide.
    """
    token = uuid.uuid4().hex[:12]
    rel = f"share/{token}"
    host_dir = under_prefix(rel)
    mount = "/mnt/shared"
    payload = f"payload-{token}"
    metadata = host_mount_metadata([mount_option(host_dir, mount, read_only=False)])

    # The host dir must exist before it can be bind-mounted (Cubelet does not
    # create the source path).
    provision_host_dirs(sdk_backend, sdk_e2e_config, [rel])

    with managed_control_sandbox(
        sdk_backend,
        sdk_e2e_config,
        metadata={"test_role": "host_mount_writer", **metadata},
    ) as writer:
        write = writer.run_command(
            f"echo {payload} > {mount}/note.txt && sync",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(write)

        try:
            with managed_control_sandbox(
                sdk_backend,
                sdk_e2e_config,
                metadata={"test_role": "host_mount_reader", **metadata},
            ) as reader:
                read = reader.run_command(
                    f"cat {mount}/note.txt",
                    timeout=sdk_e2e_config.command_timeout,
                )
                assert_command_ok(read)
                assert payload in read.stdout, (
                    "second sandbox did not observe the first sandbox's write to the "
                    f"shared host dir {host_dir}; "
                    f"stdout={read.stdout!r} stderr={read.stderr!r}"
                )
        finally:
            # Reuse the still-open writer (RW-mounted at mount) to remove the
            # per-token payload so it does not accumulate on shared host storage
            # across CI runs. mount maps to <prefix>/share/<token>.
            writer.run_command(
                f"rm -rf {mount}/note.txt",
                timeout=sdk_e2e_config.command_timeout,
            )


# --- Nested host mounts (RO parent + RW child) --------------------------------
#
# Regression for https://github.com/TencentCloud/CubeSandbox/pull/946
# (issue #944): when a read-only parent mountPath contains a writable nested
# child, Cubelet must apply the parent before the child so the child is not
# hidden by a later RO parent remount. Descriptor input order must not matter.
#
# Topology: default **single-node**. The nested host dirs are provisioned via a
# throwaway sandbox (``provision_host_dirs``) before the seed/agent creates. On
# a single-node cluster this materializes the source on the one Cubelet; on a
# multi-node local-disk cluster the provision may land on a different node than a
# later create, so a missing source there is still treated as a warned pass.

# Fixed layout under the allowed prefix. Provisioned per test via
# ``provision_host_dirs`` (mkdir -p also creates the parent ``nested-e2e``):
#   <prefix>/nested-e2e/members/agent-a
_NESTED_TEAM_REL = "nested-e2e"
_NESTED_MEMBER_REL = "nested-e2e/members/agent-a"


def test_nested_ro_parent_rw_child_writable_when_child_listed_first(
    sdk_backend, sdk_e2e_config
):
    """RO parent + RW nested child stays writable with child-first descriptors.

    Layout mirrors the collaboration model in docs/guide/persistent-storage.md
    and issue #944:

        <prefix>/nested-e2e/                     -> /workspace                  (RO)
        <prefix>/nested-e2e/members/agent-a/     -> /workspace/members/agent-a  (RW)

    Descriptors intentionally list the child before the parent. Before #946,
    restore/apply order was nondeterministic and a late RO parent could hide the
    RW child (``Read-only file system`` on writes). Assert the child accepts
    writes and the parent path rejects them.

    Single-node default
    -------------------
    This case assumes a single Cubelet node. It provisions the nested layout via
    ``provision_host_dirs`` (a throwaway sandbox that ``mkdir -p``s the source),
    so no manual host prep is required. ``mkdir -p .../members/agent-a`` also
    creates the ``nested-e2e`` parent, giving both mount sources.

    Multi-node / missing source
    ---------------------------
    On a cluster with node-local disks, ``provision_host_dirs`` may create the
    source on one node while a later create is scheduled on another. When the
    scheduled node lacks the source, create fails with Cubelet's
    ``bind mount ...: No such file or directory``. That is treated as a **pass
    with a warning** — topology and shared-storage setup are outside this
    regression's scope. Use shared storage at the same path on every node (see
    persistent-storage.md) if the nested assertion must run in multi-node CI.
    """
    token = uuid.uuid4().hex[:12]
    team_host = under_prefix(_NESTED_TEAM_REL)
    member_host = under_prefix(_NESTED_MEMBER_REL)
    parent_mount = "/workspace"
    child_mount = "/workspace/members/agent-a"
    payload = f"nested-write-{token}"
    seed_marker = f"shared-input-{token}.txt"
    child_note = f"note-{token}.txt"

    # Provision the nested layout on the host. ``mkdir -p .../members/agent-a``
    # also creates the ``nested-e2e`` parent, so both mount sources exist.
    provision_host_dirs(sdk_backend, sdk_e2e_config, [_NESTED_MEMBER_REL])

    seed_mount = "/mnt/seed"
    seed_metadata = host_mount_metadata(
        [mount_option(team_host, seed_mount, read_only=False)]
    )
    try:
        with managed_control_sandbox(
            sdk_backend,
            sdk_e2e_config,
            metadata={"test_role": "host_mount_nested_seed", **seed_metadata},
        ) as seeder:
            seed = seeder.run_command(
                f"echo shared-from-host > {seed_mount}/{seed_marker} && sync",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert_command_ok(seed)

            # Child listed first — the order that previously exposed the bug.
            nested_metadata = host_mount_metadata(
                [
                    mount_option(member_host, child_mount, read_only=False),
                    mount_option(team_host, parent_mount, read_only=True),
                ]
            )
            try:
                try:
                    with managed_control_sandbox(
                        sdk_backend,
                        sdk_e2e_config,
                        metadata={
                            "test_role": "host_mount_nested_agent",
                            **nested_metadata,
                        },
                    ) as agent:
                        read_shared = agent.run_command(
                            f"cat {parent_mount}/{seed_marker}",
                            timeout=sdk_e2e_config.command_timeout,
                        )
                        assert_command_ok(read_shared)
                        assert "shared-from-host" in read_shared.stdout, (
                            "expected RO parent content visible at "
                            f"{parent_mount}/{seed_marker}; "
                            f"stdout={read_shared.stdout!r} "
                            f"stderr={read_shared.stderr!r}"
                        )

                        write_child = agent.run_command(
                            f"echo {payload} > {child_mount}/{child_note} && "
                            f"cat {child_mount}/{child_note}",
                            timeout=sdk_e2e_config.command_timeout,
                        )
                        assert_command_ok(write_child)
                        assert payload in write_child.stdout, (
                            "expected RW nested child to accept writes at "
                            f"{child_mount}; "
                            f"stdout={write_child.stdout!r} "
                            f"stderr={write_child.stderr!r}"
                        )

                        write_parent = agent.run_command(
                            f"echo should-fail > {parent_mount}/parent-write-{token}.txt",
                            timeout=sdk_e2e_config.command_timeout,
                        )
                        assert write_parent.exit_code != 0, (
                            "expected RO parent mount to reject writes at "
                            f"{parent_mount}/parent-write-{token}.txt, "
                            "but the write succeeded"
                        )
                        combined = (
                            f"{write_parent.stdout}\n{write_parent.stderr}".lower()
                        )
                        assert (
                            "read-only" in combined
                            or "readonly" in combined
                            or "read only" in combined
                        ), (
                            "expected a read-only filesystem error when writing "
                            f"the RO parent; stdout={write_parent.stdout!r} "
                            f"stderr={write_parent.stderr!r}"
                        )
                except Exception as exc:  # noqa: BLE001 - missing source => pass
                    if warn_pass_missing_hostpath_source(
                        exc,
                        host_paths=[team_host, member_host],
                        role="host_mount_nested_agent",
                    ):
                        return
                    raise
            finally:
                seeder.run_command(
                    f"rm -f {seed_mount}/{seed_marker} "
                    f"{seed_mount}/members/agent-a/{child_note} "
                    f"{seed_mount}/parent-write-{token}.txt",
                    timeout=sdk_e2e_config.command_timeout,
                )
    except Exception as exc:  # noqa: BLE001 - missing source => pass
        if warn_pass_missing_hostpath_source(
            exc,
            host_paths=[team_host, member_host],
            role="host_mount_nested_seed",
        ):
            return
        raise


# --- Symlink TOCTOU escape ---------------------------------------------------


@pytest.mark.xfail(
    strict=True,
    reason="known-open host-mount symlink TOCTOU (see docstring); strict=True "
    "turns the fix into a loud XPASS and catches later regressions.",
)
def test_symlink_escapes_allowed_prefix(sdk_backend, sdk_e2e_config):
    """Reproduce the symlink TOCTOU escape of the host-mount whitelist.

    1. Sandbox A mounts the allowed prefix and plants a symlink
       `escape-<id> -> /bin` inside it (materialized on the host as
       `<prefix>/escape-<id> -> /bin`).
    2. Sandbox B requests host-mount of `<prefix>/escape-<id>`. CubeMaster's
       lexical check sees a path under the allowed prefix and admits it;
       Cubelet resolves the symlink and bind-mounts the host `/bin`.
    3. If B can read host `/bin` through the mount, the whitelist is bypassed:
       `/bin` is the node root filesystem, which cannot appear under the prefix.

    The escape target is `/bin` rather than a secret directory such as `/etc`
    on purpose: it proves the same lexical-vs-resolved bypass (a directory
    outside the allowed prefix, on the node root filesystem) while reading no
    host secret, and a stray `-> /bin` link left on shared host state if the
    harness is killed mid-test is far less dangerous than `-> /etc`. `/bin` also
    exists on every Cubelet node, so the probe is deterministic.

    Two secure outcomes make this test pass: the escaper create is rejected
    (path re-validated after symlink resolution), or the create succeeds but the
    mount does not expose the symlink target. The current code allows the
    escape, so the test xfails. Once the resolved path is validated it will
    XPASS, and because the marker is ``strict=True`` that XPASS fails the suite,
    forcing removal of the marker; a later regression re-introducing the escape
    then fails loudly instead of being silently tolerated.
    """
    token = uuid.uuid4().hex[:12]
    link_name = f"escape-{token}"
    link_host_path = under_prefix(link_name)
    symlink_target = "/bin"

    plant_mount = "/mnt/hostdir-plant"
    plant_metadata = host_mount_metadata(
        [mount_option(under_prefix(), plant_mount, read_only=False)]
    )
    with managed_control_sandbox(
        sdk_backend,
        sdk_e2e_config,
        metadata={"test_role": "host_mount_planter", **plant_metadata},
    ) as planter:
        plant = planter.run_command(
            f"ln -sfn {symlink_target} {plant_mount}/{link_name} && "
            f"readlink {plant_mount}/{link_name}",
            timeout=sdk_e2e_config.command_timeout,
        )
        assert_command_ok(plant)
        assert plant.stdout.strip() == symlink_target, (
            f"failed to plant symlink; stdout={plant.stdout!r} stderr={plant.stderr!r}"
        )

        try:
            victim_mount = "/mnt/hostdir-escaped"
            # Read-only: the probe only reads the target, and a RO mount removes
            # any chance of a stray write reaching the symlink target (host /bin).
            escape_metadata = host_mount_metadata(
                [mount_option(link_host_path, victim_mount, read_only=True)]
            )
            try:
                with managed_control_sandbox(
                    sdk_backend,
                    sdk_e2e_config,
                    metadata={"test_role": "host_mount_escaper", **escape_metadata},
                ) as escaper:
                    # Detect a core binary that only exists on the node root
                    # filesystem's /bin, never under the allowed prefix.
                    probe = escaper.run_command(
                        f"ls {victim_mount} 2>/dev/null | "
                        f"grep -Ex '(sh|ls|cat)' | head",
                        timeout=sdk_e2e_config.command_timeout,
                    )
                    escaped = bool(probe.stdout.strip())
                    assert not escaped, (
                        "host-mount whitelist bypassed via symlink: sandbox B "
                        f"mounted host {symlink_target} through {link_host_path}. "
                        f"probe stdout={probe.stdout!r} stderr={probe.stderr!r}. "
                        "Cubelet must re-validate the symlink-resolved path "
                        "against the allowed prefix before mounting."
                    )
            except AssertionError:
                # The deliberate ``assert not escaped`` above (the bypass
                # detection) must surface with its own diagnostic, not be
                # mistaken for a create failure below.
                raise
            except Exception as exc:  # noqa: BLE001 - a rejected create is the secure outcome
                # A backend that re-validates the resolved path rejects the
                # escaper create outright; that is a pass, not an error. The
                # rejection may come from the control-plane prefix/absolute check
                # or, if the fix lives in Cubelet, from a bind-mount/symlink
                # failure at mount time -- accept any of these secure signals.
                secure_tokens = ("prefix", "absolute", "bind mount", "symlink")
                assert any(t in str(exc).lower() for t in secure_tokens), (
                    f"escaper create failed for an unexpected reason: {exc!r}"
                )
        finally:
            # Remove the planted symlink from the shared host dir so a "-> /bin"
            # link cannot leak to later tests or other tenants on the node. The
            # planter still has the prefix mounted RW. Assert the unlink so a
            # failed cleanup surfaces loudly instead of leaving a live escape
            # hatch on shared host state. rm -f on the link path unlinks the
            # symlink itself; it does not follow the link.
            cleanup = planter.run_command(
                f"rm -f {plant_mount}/{link_name} && "
                f"test ! -e {plant_mount}/{link_name} && echo removed",
                timeout=sdk_e2e_config.command_timeout,
            )
            assert cleanup.exit_code == 0 and "removed" in cleanup.stdout, (
                f"failed to remove planted symlink {link_host_path!r}; it may "
                f"leak to other sandboxes. stdout={cleanup.stdout!r} "
                f"stderr={cleanup.stderr!r}"
            )
