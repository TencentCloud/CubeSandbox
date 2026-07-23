# CodeBuddy CI + CubeSandbox example

[中文文档](README_zh.md)

This example runs a non-interactive [CodeBuddy Code CLI](https://www.npmjs.com/package/@tencent-ai/codebuddy-code) task inside a disposable CubeSandbox MicroVM. A CI runner uploads a source archive, the agent inspects or tests the checkout, and the driver prints the generated report. The MicroVM never receives GitHub or registry credentials.

## Layout

```text
codebuddy-ci-integration/
├── Dockerfile                 # Node.js + pinned CodeBuddy CLI template
├── build-template.sh          # Build and push the template image
├── run_codebuddy_ci.py        # Create VM, upload .tar, run one CI task
├── resume_codebuddy_ci.py     # Resume a paused task and its CLI session
├── config.py                  # Validated env and headless command builder
├── github-actions.yml         # Copy into the consumer project
└── test_config.py             # Offline unit tests
```

## Build and register the image

```bash
cd examples/codebuddy-ci-integration
./build-template.sh <your-registry>/codebuddy-ci-cube:2.125.5

cubemastercli tpl create-from-image \
  --image <your-registry>/codebuddy-ci-cube:2.125.5 \
  --writable-layer-size 4G \
  --expose-port 49983 --probe 49983 --probe-path /health
cubemastercli tpl watch --job-id <job-id>
```

Put the ready template ID in `CUBE_TEMPLATE_ID`. The image pins `@tencent-ai/codebuddy-code` to `2.125.5`; upgrade deliberately and rerun the CLI interface checks first.

## Configure credentials and egress

```bash
cp .env.example .env
# Set E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID and CODEBUDDY_AUTH_TOKEN.
python -m pip install -r requirements.txt
```

For a shared cluster, apply a **default-deny CubeEgress policy** to this template and allow only the CodeBuddy API host your tenant uses. The runner needs CubeAPI access; the MicroVM should not receive GitHub, registry, or CI provider credentials. The driver uses `--permission-mode bypassPermissions` only inside a disposable MicroVM; do not use that mode with broad egress or production secrets.

## Run locally

Create an archive rather than giving the VM a host mount:

```bash
tar --exclude=.git -cf /tmp/project.tar .
cd examples/codebuddy-ci-integration
python run_codebuddy_ci.py --source-tar /tmp/project.tar
```

The default prompt asks CodeBuddy to run the smallest relevant test and write `/workspace/codebuddy-ci-report.md`. Use `--prompt` for a narrow review task. The example instructs the agent not to commit or push.

## Pause and resume a long CI task

```bash
python run_codebuddy_ci.py --source-tar /tmp/project.tar --pause
# Copy the printed resume handle, then:
python resume_codebuddy_ci.py <sandbox-id>
```

`--pause` snapshots `/workspace` and `/root/.codebuddy`. The resume driver reconnects to that sandbox and calls CodeBuddy with the same `--session-id` plus `--resume`. Treat snapshots as sensitive and kill them after the job finishes.

## Use from GitHub Actions

Copy [`github-actions.yml`](github-actions.yml) to `.github/workflows/codebuddy-cubesandbox.yml` in the consumer project, then configure `CUBE_API_URL`, `CUBE_API_KEY`, and `CODEBUDDY_AUTH_TOKEN` as secrets plus `CUBE_TEMPLATE_ID` as a variable. The sample never enables `pull_request_target`; untrusted PR code does not get repository write permissions.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `codebuddy: command not found` | Rebuild/re-register the image and verify `codebuddy --version`. |
| Authentication failure | Check the CI secret; do not add the token to the image or archive. |
| Egress `403` or model timeout | Add only the required CodeBuddy API hostname to CubeEgress. |
| Resume cannot find the session | Keep `CODEBUDDY_SESSION_ID` unchanged and do not kill the paused sandbox. |
| Archive rejected | Use a regular `.tar` below 100 MiB; exclude `.git` and secrets. |

## Validate without a cluster

```bash
python -m py_compile config.py run_codebuddy_ci.py resume_codebuddy_ci.py
python -m unittest -v test_config.py
bash -n build-template.sh
docker build --check .
```

The tests cover validation, headless JSON output, resume arguments, and the fact that only `CODEBUDDY_AUTH_TOKEN` is forwarded to the sandbox command.
