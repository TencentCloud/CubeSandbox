# PR #732 WSL2 validation evidence

This directory contains the reviewer-facing evidence for candidate commit
`84ae673a78c933ab8964fb6376f04884efc963b8`.

## Results

| Check | Result |
| --- | --- |
| Node test suite | 16/16 passed |
| Docker image build | Passed |
| Default image startup without a command override | Passed |
| envd and Fastify readiness | HTTP 204 and HTTP 200 |
| Counter, note validation, and dependency pruning | Passed |
| Default container shutdown | Exit code 0 |
| In-flight HTTP request during SIGTERM | HTTP 200, `DRAIN_COMPLETE`, exit code 0 |

The graceful-drain test started a slow HTTP request before `docker stop` sent
SIGTERM. The request completed successfully, shutdown waited about 1.8 seconds,
and the container exited with code 0.

## Files

- [`01-wsl2-final-pass.png`](01-wsl2-final-pass.png): terminal-only result screenshot.
- [`02-environment.log`](02-environment.log): candidate, WSL2, Debian, Docker, and image metadata.
- [`03-npm-test-16-of-16.log`](03-npm-test-16-of-16.log): clean Node test output.
- [`04-docker-build.log`](04-docker-build.log): completed Docker build output.
- [`05-docker-runtime-and-graceful-drain.log`](05-docker-runtime-and-graceful-drain.log):
  default runtime and in-flight graceful-drain evidence.
- [`SHA256SUMS.csv`](SHA256SUMS.csv): size and SHA256 values for the five evidence attachments.

## Scope

This is WSL2-based Docker validation on Windows, not a native Linux bare-metal
test. The fixed revision was not revalidated through CubeSandbox template
creation, `run_demo.py`, or snapshot/resume end to end; those checks still
require a stable KVM host.

Host usernames, host absolute paths, credentials, and email addresses were
removed from the reviewer-facing package. Ephemeral container details remain
where they are part of the runtime evidence.
