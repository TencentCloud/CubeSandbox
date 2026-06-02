# CubeSandbox Examples Runner

A small, declarative Python CLI to discover, run, and assert the example
scripts under `examples/`. Use it to verify — on every release — that the
shipped examples (and the code snippets the docs reference) still execute
against a real CubeSandbox deployment and produce the expected output.

## Why

The `examples/` directory ships ~12 demos. Previously each had to be run by
hand (`cd`, `pip install`, `python xxx.py`), with no automated pass/fail check.
This runner makes them a single repeatable command with explicit assertions and
machine + human reports.

## How it works

Each example directory contains a `cube-example.yaml` manifest declaring:

- `setup`: commands to prepare the example (e.g. `pip install -r requirements.txt`)
- `env`: extra environment variables
- `requires_template`: a logical template alias (`code`, `browser`, ...)
- `steps`: ordered commands with assertions (`expect_exit`,
  `expect_stdout_contains`, `expect_stdout_not_contains`)
- `tags`: for filtering (`smoke`, `sdk`, `network`, `snapshot`, `external`, ...)
- `skip` / `skip_reason`: for demos that need external credentials/services

The runner injects connection info via environment variables the example
scripts already read: `E2B_API_URL` / `CUBE_API_URL`, `E2B_API_KEY`,
`CUBE_TEMPLATE_ID`, and optionally `SSL_CERT_FILE`.

## Install

```bash
cd examples/runner
pip install -r requirements.txt
```

## Usage

```bash
# From examples/runner/
python -m cube_examples list                      # list all examples + tags
python -m cube_examples list --tags smoke          # filter by tag

# Run smoke examples against a deployment with a code template
python -m cube_examples run \
  --api-url http://127.0.0.1:3000 \
  --template code=tpl-aa14fc963b9c443aaff65b17 \
  --tags smoke \
  --report-md out/report.md --report-json out/report.json

# Run a single example, verbose
python -m cube_examples run --only code-sandbox-quickstart -v \
  --api-url http://127.0.0.1:3000 --template code=tpl-xxxx

# Map multiple template aliases
python -m cube_examples run \
  --template code=tpl-aaa --template browser=tpl-bbb --tags sdk
```

### Template aliases

Examples declare a logical template via `requires_template` (e.g. `code`).
Supply the concrete id on the command line with `--template <alias>=<id>`.
Find a template id with:

```bash
cubemastercli tpl list
```

If an example requires a template alias you did not provide, it is reported as
`skipped` (not failed), so partial runs are safe.

### Exit codes

- `0`: all selected examples passed (skipped is OK)
- `1`: at least one example failed or errored
- `2`: bad arguments / no examples matched

## Release verification flow

```bash
# 1. Install the one-click bundle on a clean node
# 2. Create a code template, note its id
# 3. Run the smoke + sdk examples and capture a report
python -m cube_examples run \
  --api-url http://<node-ip>:3000 \
  --template code=<template-id> \
  --tags smoke sdk \
  --label "$(cat ../../deploy/one-click/VERSION.txt 2>/dev/null || echo dev)" \
  --report-md release-examples-report.md
# 4. Attach release-examples-report.md to the release notes
```

## Adding a new example

1. Drop your script(s) into `examples/<your-example>/`.
2. Add a `cube-example.yaml` next to them (copy an existing one).
3. Verify locally: `python -m cube_examples run --only <your-example> ...`.
