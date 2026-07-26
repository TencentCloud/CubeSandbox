# Python Incident RCA Code Interpreter Sandbox

This example shows how to build and verify a reusable CubeSandbox template for AI Agent code-interpreter workflows. The template provides a Python data-science runtime for multi-file analysis, stateful follow-up rounds, checkpoint/fork recovery, and downloadable artifacts. The included production incident RCA workflow is the validation scenario: the host uploads service metrics, deployment events, alerts, and a runbook; the sandbox runs Python analysis, checkpoints the intermediate workspace, forks a fresh sandbox from that checkpoint, and produces a Chinese RCA report, chart, structured summaries, and a packaged result archive.

## What It Demonstrates

1. **Industrial data-science workflow**: Pandas analyzes service metrics, SLO thresholds, deployment events, and alert timelines to identify an incident window and likely trigger.
2. **Stateful code-interpreter workspace**: round 1 writes intermediate state under `/tmp/cubesandbox-incident-rca/state`.
3. **Snapshot checkpoint and fork**: the client snapshots the round 1 workspace, starts a fresh sandbox from that checkpoint, verifies the inherited state, and runs round 2 in the forked sandbox.
4. **Chinese Matplotlib rendering**: the image installs `fonts-wqy-zenhei` and the analysis script configures Matplotlib to use `WenQuanYi Zen Hei`, so Chinese chart titles and labels render correctly.
5. **Dynamic package installation**: the client installs version-pinned `emoji` and `humanize` packages with `pip3 install` inside the running sandbox, then imports them in the follow-up analysis round.
6. **Multi-file input and packaged output**: the sandbox generates a Chinese incident chart, Markdown RCA report, SLO summary CSV, deployment-correlation JSON, manifest JSON, and a downloadable tarball.

## Reusable Template Pattern

The RCA scenario is intentionally concrete, but the template pattern is reusable for other code-interpreter tasks:

- **Inputs**: CSV, JSON, logs, runbooks, or generated analysis programs uploaded by the host or an agent.
- **Workspace state**: intermediate files are written under a predictable `state/` directory inside the sandbox.
- **Checkpoint/fork**: a snapshot captures the post-ingestion workspace so another sandbox can continue analysis, retry a branch, or preserve an audit point.
- **Artifacts**: reports, charts, structured summaries, manifests, and selected state files are packaged into a tarball for download.

This differs from the existing OpenAI Agents code-interpreter example by focusing on a reusable CubeSandbox template and a production-style RCA workflow rather than SDK integration alone.

## Restricted or Offline Deployments

The runtime `pip3 install emoji humanize` step demonstrates the interactive code-interpreter pattern where an agent can add lightweight follow-up dependencies during analysis. In production, restricted-egress, or fully offline deployments, preinstall those packages in the Dockerfile or serve them from an internal PyPI mirror. Once dependencies are baked into the template image, the core RCA workflow does not require unrestricted public internet access.

## Files

- `Dockerfile`: builds the reusable Python data-science sandbox image.
- `incident_metrics.csv`: time-series service metrics for `checkout-api`.
- `deployments.json`: deployment events used for correlation analysis.
- `alerts.csv`: alert timeline exported from the monitoring system.
- `runbook.json`: SLO thresholds and incident policy hints.
- `round1_detect.py`: first-round anomaly detection program uploaded into the sandbox.
- `round2_rca.py`: second-round RCA/report packaging program uploaded into the forked sandbox.
- `test_data_science.py`: end-to-end client that creates a sandbox, uploads files, runs two analysis rounds, downloads the result archive, and validates the manifest.
- `test_data_science_unit.py`: offline coverage for sandbox ID parsing and safe archive extraction.
- `env.example`: local CubeSandbox API/proxy settings and template ID placeholder.

## Step 1: Build the Image

Build the image where the Cube node runtime can access it:

```bash
docker build -t cubesandbox-data-science:latest .
```

The image includes Python, Pandas, Matplotlib, scientific dependencies, and Chinese fonts. It is larger than a minimal image because it is intended for code-interpreter style workloads.

## Step 2: Register a CubeSandbox Template

Inside the CubeSandbox dev VM or on the node where `cubemastercli` is installed, register the image:

```bash
cubemastercli tpl create-from-image \
    --image               cubesandbox-data-science:latest \
    --writable-layer-size 2G \
    --expose-port         49983 \
    --probe               49983 \
    --probe-path          /health
```

Copy the returned template ID, for example `tpl-xxxxxxxxxxxxxxxxxxxxxxxx`.

## Step 3: Configure the Client

Install client requirements:

```bash
pip3 install -r requirements.txt
```

Run the focused offline tests before using a cluster:

```bash
pytest -q test_data_science_unit.py
```

Create `.env` and set the template ID:

```bash
cp env.example .env
```

For the local dev VM, `.env` should look like this:

```bash
E2B_API_URL="http://127.0.0.1:13000"
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxxxxxxxxxx"
E2B_API_KEY=e2b_dummyapikeyforlocaltest
```

## Step 4: Run the End-to-End Demo

```bash
python3 test_data_science.py
```

The script will:

1. Start a sandbox from the registered template.
2. Upload `incident_metrics.csv`, `deployments.json`, `alerts.csv`, `runbook.json`, `round1_detect.py`, and `round2_rca.py`.
3. Install `emoji` and `humanize` dynamically inside the sandbox.
4. Run round 1 anomaly detection and save intermediate state in the sandbox workspace.
5. Create a CubeSandbox snapshot checkpoint from the round 1 workspace.
6. Start a fresh sandbox from the checkpoint and verify that the state files were inherited.
7. Run round 2 RCA analysis in the forked sandbox using the saved state, deployment events, alerts, and runbook policy.
8. Create `/tmp/cubesandbox-incident-rca-results.tar.gz` in the forked sandbox.
9. Download and extract the archive to `output/results/`.

Expected extracted files:

- `事故分析图.png`: Chinese incident chart proving font rendering works.
- `incident_report.md`: Markdown RCA report with likely trigger and suggested actions.
- `slo_summary.csv`: baseline vs incident SLO summary.
- `deployment_correlation.json`: structured deployment-to-incident correlation.
- `manifest.json`: machine-readable artifact manifest and key metrics.
- `state/anomaly_windows.csv`, `state/baseline.json`, and `state/checkpoint_snapshot_id.txt`: intermediate state produced by round 1 plus the checkpoint used to fork round 2, packaged for auditability.
