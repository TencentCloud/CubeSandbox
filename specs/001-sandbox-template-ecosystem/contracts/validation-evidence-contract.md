# Contract: Validation Evidence

Validation evidence proves that a contribution was built or obtained and run successfully in a supported local CubeSandbox deployment.

## Required Evidence

- Validation date.
- Validator identity or role.
- CubeSandbox deployment type.
- Host architecture when relevant.
- Template build command or acquisition source.
- Template readiness result.
- Environment variables required, with secret values redacted.
- Dependency installation command.
- Smoke-test command.
- Observed output.
- Cleanup result.
- Known limitations discovered during validation.

## Evidence Location

Evidence may be placed in the example README or a small validation section/file in the example directory. It must be committed with the contribution or included in the PR in a way maintainers can preserve.

## Redaction Rules

- Do not include real API keys.
- Do not include private hostnames or IP addresses unless they are examples.
- Do not include sensitive logs.
- Use placeholders such as `<cube-host>`, `<template-id>`, and `<redacted>`.

## Pass Criteria

- A maintainer can repeat the validation steps from the evidence and README.
- The observed output matches the README's expected output.
- Resource and network observations do not contradict the README.
- Failures are documented with likely causes or linked troubleshooting guidance.
