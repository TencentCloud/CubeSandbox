# Contract: Template and Example Contribution Entry

This contract defines the minimum acceptance standard for one ecosystem contribution.

## Required Artifacts

Each contribution must include:

- One directory under `examples/<slug>/`.
- A template definition or reproducible template source.
- A build or acquisition path for the template.
- A minimum runnable example flow.
- `README.md`.
- Dependency declarations needed by the local runner.
- Environment example file when runtime configuration is required.
- Documentation registration in the example index.
- Reviewer-verifiable validation evidence.

## Required Metadata

Each entry must document:

- Scenario category.
- Intended users.
- Prerequisites.
- Template source.
- Required exposed ports.
- Required environment variables.
- Resource recommendation.
- Authentication and secret handling expectations.
- External network access assumptions.
- Public access behavior when services are exposed.
- Known limitations.
- Expected output for the minimum run.

## Acceptance Rules

- The entry is independently reviewable.
- The minimum flow can be reproduced in a supported local CubeSandbox deployment.
- The entry does not require unrelated examples to change.
- The entry does not weaken existing platform controls.
- The entry is discoverable from the docs index.
- The entry's scenario is not an unexplained duplicate of an existing example.

## Rejection or Revision Triggers

- Missing build or acquisition path.
- Missing expected output.
- Missing resource or security notes.
- Hard-coded secrets or private credentials.
- Unclear dependency installation.
- Example succeeds only with undocumented local state.
- Duplicate scenario with no differentiation.
- Documentation registration missing or inconsistent.
