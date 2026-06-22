---
title: "Cube Sandbox v0.2.2: One Step Closer to Drop-in E2B Compatibility, with a Stability Sweep"
date: 2026-05-19
author: Cube Sandbox Team
description: Following v0.2.0, Cube Sandbox shipped v0.2.2 on May 18. This release extends E2B compatibility from the SDK layer down to the wire-protocol layer, fixes seven recurring stability issues from the v0.1.x era, and lands the first round of CVE remediations for the 0.2 series.
featured: false
---

# Cube Sandbox v0.2.2: One Step Closer to Drop-in E2B Compatibility, with a Stability Sweep

Following v0.2.0, Cube Sandbox shipped v0.2.2 on the evening of May 18.

Compared with v0.2.0, this release focuses on three things: extending E2B compatibility from the SDK layer down to the wire-protocol layer; closing out a handful of recurring stability issues reported since v0.1.x; and shipping the first round of CVE remediations for the 0.2 series.

## 1. Compatibility down to the protocol layer — one step closer to drop-in E2B migration

In v0.2.0, our E2B compatibility only covered the SDK layer — you could swap the client from E2B to Cube without touching a line of code, but reverse proxies, firewall rules, and any client that hard-coded ports still needed adjusting.

v0.2.2 changes the sandbox's default exposed port from `8080/32000` to `49983`, aligning it with the E2B sandbox protocol. That means you no longer need to touch your config files when migrating to Cube.

We also unified the source of truth for the "default port" into CubeMaster. Previously, Cubelet and network-agent each hard-coded their own default, which occasionally caused the two sides to drift. After this refactor, neither component holds a default any more — behavior is governed by CubeMaster.

## 2. Stability: seven recurring user-reported issues, all fixed in one go

A handful of issues that the community kept hitting between v0.1.x and v0.2.0 have been swept up in this release:

1. **`cubecli exec` nil-deref panic on stdin EOF** ([#188](https://github.com/TencentCloud/CubeSandbox/pull/188)): the `exec` command panicked the moment stdin closed; the process was killed but no error showed up in logs, leading users to misdiagnose it as a network or permission issue. The fix uses `errors.Is(err, io.EOF)` to handle wrapped errors, and the shim now reliably emits paired exec-request / exit-code log entries.

2. **Duplicate template-image jobs in CubeMaster** ([#227](https://github.com/TencentCloud/CubeSandbox/pull/227)): concurrent or retried API calls could enqueue the same build twice. We added a `request_id` column and a `(request_id, operation)` unique index on the `template_image` table to enforce idempotency at the data layer. Legacy rows missing the new ID are handled by a migration script — no manual intervention is required when upgrading.

3. **Runtime-file materialization for PVM template ext4 artifacts** ([#282](https://github.com/TencentCloud/CubeSandbox/pull/282)): `RefreshArtifactRuntimeFiles`, `validateArtifactRuntimeFilesPresent`, and `ensureArtifactRuntimeFiles` used to live on three separate code paths whose state could drift. They have been consolidated into a single path that only handles kernel files, with the unit tests rewritten to match. PVM deployments now have a much more predictable template lifecycle.

4. **Configurable command timeout for the storage plugin** ([#236](https://github.com/TencentCloud/CubeSandbox/pull/236)): the 3-second timeout on ext4 operations was hard-coded, which could falsely kill the slow path of large-file `live-create` under concurrency. The plugin's TOML config now accepts a `cmd_timeout` field, so operators can tune it without rebuilding. When the field is absent, behavior is unchanged.

5. **Better diagnostics on storage failures** ([#237](https://github.com/TencentCloud/CubeSandbox/pull/237)): error logs from `newExt4RawByReflinkCopy` are now structured rather than a single-line message, with new unit tests for `describeStorageFailure`, `describeFile`, and `describeFreeBytes`.

6. **Deploy scripts now honor `.env` port placeholders** ([#210](https://github.com/TencentCloud/CubeSandbox/pull/210)): the MySQL/Redis ports in `cubemaster.yaml` are now `__CUBE_SANDBOX_MYSQL_PORT__` and `__CUBE_SANDBOX_REDIS_PORT__` placeholders that `install.sh` substitutes from `.env`. Non-default port deployments no longer require hand-editing the YAML.

7. **Smaller, faster template images**: the quick-start image dropped from ~4 GB to ~100 MB. Download failures and first-launch wait times went down noticeably. Combined with the overseas image registry that came online two weeks ago, the pull experience for users outside mainland China has also improved.

## 3. Security: first batch of CVE fixes for the 0.2 series

- **`vmm-sys-util` 0.11.x → 0.12.1**: closes CVE-2023-50711. The previous version's `FamStructWrapper::deserialize` did not check that the header length matched the flexible-array length, which could lead to out-of-bounds memory access from safe Rust.
- **`bytes` and `env_logger` upgraded** in the same PR ([#267](https://github.com/TencentCloud/CubeSandbox/pull/267)).
- **`time` crate upgrade deferred (CVE-2026-25727)**: the upgrade requires an MSRV bump. After review, we confirmed Cube only uses `time::format_description::well_known::Rfc3339` for outbound timestamp formatting and never calls `Rfc2822` parsing on untrusted input — the affected attack vector is unreachable. We will land the upgrade once the MSRV is ready.

## 4. Phase-1 community contribution program now live

Alongside v0.2.2, we have launched the first phase of the Cube Sandbox community contribution program. The repo now has three new doc tracks aimed at community contributors:

- **Troubleshooting**: postmortems and tips on deployment, configuration, and error scenarios.
- **Use Cases**: end-to-end stories of real-world problems solved with Cube.
- **Integrations**: hands-on integration playbooks for combinations like Cube × LangChain / Dify / Claude Code / OpenHands.

Each track ships with a template and an index page; contribution instructions live in `CONTRIBUTING.md` at the repo root.

We would love your contributions. Once your PR is merged, you become eligible for: an official Cube Sandbox contributor certificate, a permanent name on the website's wall of contributors, early access to upcoming releases, and a piece of limited-edition open-source swag.

- Task board: <https://github.com/TencentCloud/CubeSandbox/contribute>
- Full release notes: <https://github.com/TencentCloud/CubeSandbox/releases/tag/v0.2.2>
