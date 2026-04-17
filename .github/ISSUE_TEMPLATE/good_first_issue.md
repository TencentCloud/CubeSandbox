---
name: Good First Issue
about: A well-scoped task suitable for first-time contributors
title: "[good first issue] "
labels: ["good first issue", "help wanted"]
assignees: ""
---

<!--
Good First Issue Template for CubeSandbox
This template is for tasks that are well-scoped, clearly defined, and suitable for
contributors who are new to the project.

Tips for filling out this template:
- Be specific about the acceptance criteria
- Link to relevant code locations with file:line references
- Provide enough context so someone unfamiliar with the codebase can get started
- Indicate the expected difficulty and required background knowledge
-->

> [!NOTE]
> **This template is for maintainers only.**
> Maintainers use this template to publish well-scoped tasks for new contributors.
> If you are looking to contribute, please browse existing issues labeled
> [`good first issue`](../../issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
> and leave a comment to claim one — do not open a new issue with this template.

## Summary

<!-- A one-paragraph description of the task and why it matters. -->


## Background

<!-- Provide context about the component or system involved.
     What does it do? Why does this issue exist?
     Link to relevant documentation or architecture notes if available. -->


## Task Description

<!-- Clearly describe what needs to be done.
     For feature additions: what behavior should be added?
     For bug fixes: what is the current behavior vs expected?
     For refactoring: what should change and why? -->


## Acceptance Criteria

<!-- List the concrete conditions that must be met for this issue to be considered done.
     Use checkboxes. -->

- [ ] ...
- [ ] ...
- [ ] Tests are added or updated to cover the change
- [ ] Documentation is updated if applicable

## Relevant Code Locations

<!-- Point the contributor to specific files and line numbers to start from.
     Example:
     - `Cubelet/pkg/sandbox/lifecycle.go:123` — sandbox start logic
     - `agent/src/metrics.rs:45` — metrics collection entry point
-->

- ...

## Suggested Approach

<!-- Optional: outline a high-level approach or algorithm.
     Don't be too prescriptive — leave room for the contributor's creativity.
     If there are multiple valid approaches, mention the tradeoffs. -->


## Required Background Knowledge

<!-- What does a contributor need to know to tackle this?
     Examples: Go, Rust, KVM concepts, eBPF, gRPC, etc.
     Be honest — this helps contributors self-select. -->

- **Language**: <!-- e.g., Go / Rust -->
- **Domain knowledge**: <!-- e.g., basic Linux networking / virtio / OCI runtime spec -->
- **Estimated difficulty**: <!-- Beginner / Intermediate -->

## How to Set Up a Dev Environment

<!-- Link to or summarize the steps needed to build and test the relevant component.
     This is critical for first-time contributors. -->

See the [Getting Started guide](../../docs/guide/quickstart.md) and [CONTRIBUTING.md](../../CONTRIBUTING.md).

For this specific task, you will primarily need to build/run:
- [ ] `make <component>` — <!-- e.g., `make cubelet` -->
- [ ] <!-- Any additional setup steps -->

## Resources

<!-- Links to relevant documentation, RFCs, Linux man pages, upstream issues, etc. -->

- [CubeSandbox Architecture Overview](../../docs/architecture/overview.md)
- ...

## Mentorship

<!-- Who is the point of contact for this issue? Tag a maintainer. -->

Feel free to ask questions in this issue or reach out to @<!-- maintainer --> for guidance.
