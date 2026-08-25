# Google ADK + CubeSandbox Integration Example

This example exposes CubeSandbox as a Google ADK function tool. The ADK agent
keeps running in your local development environment, while generated Python
code executes inside a temporary CubeSandbox MicroVM through the E2B-compatible
SDK.

## Files

```text
google-adk-integration/
  agent.py            ADK root_agent definition
  cube_code_tool.py   CubeSandbox-backed Python execution tool
  smoke_test.py       Offline import and wiring checks
  .env.example        Required environment variables
  requirements.txt
```

## Setup

```bash
cd examples/google-adk-integration
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Edit `.env` with your CubeAPI endpoint, CubeSandbox template ID, and Google API
key. A local unauthenticated CubeAPI deployment can use `E2B_API_KEY=e2b_000000`.

## Run the checks

```bash
python smoke_test.py
```

Expected output:

```text
GOOGLE_ADK_CUBE_SMOKE_OK
```

## Run the ADK agent

From the parent directory that contains this example:

```bash
adk run google-adk-integration
```

Ask the agent:

```text
Run Python in the sandbox to calculate the first 10 Fibonacci numbers.
```

The agent should call `run_python_in_cube`, create a CubeSandbox from
`CUBE_TEMPLATE_ID`, execute the Python code, return stdout, and delete the
temporary sandbox when the tool call finishes.

## Notes

- Use a template that supports the E2B code interpreter `run_code` path.
- The E2B packages are pinned to a pair that plain `pip` can install and that is
  listed in the repository's SDK compatibility notes. Revalidate before changing
  either package version.
- If your CubeAPI uses a self-signed certificate, set `CUBE_SSL_CERT_FILE`.
