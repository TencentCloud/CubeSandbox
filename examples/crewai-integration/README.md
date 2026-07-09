# CrewAI + Cube Sandbox

This example gives a CrewAI agent Python execution through Cube Sandbox's E2B-compatible API. Agent-generated code runs in an isolated MicroVM, not in the local CrewAI process.

## Files

- `smoke_test.py` validates Cube connectivity without calling an LLM.
- `main.py` runs a deterministic data-analysis task through a CrewAI agent.
- `.env.example` documents the required Cube and LLM configuration.

## Setup

1. Deploy Cube Sandbox and create a code-interpreter template. The resulting template ID must use the `tpl-...` format.
2. Create a Python environment and install dependencies:

   ```bash
   python -m venv .venv
   source .venv/bin/activate
   pip install -r requirements.txt
   ```

3. Configure the environment:

   ```bash
   cp .env.example .env
   ```

   Set `E2B_API_URL` to the Cube API Server, normally `http://<host>:3000`. Do not use the CubeProxy endpoint.

   `http://` is suitable only for local development on a trusted machine. For production, configure TLS on CubeAPI and use `https://`, or bind CubeAPI to loopback and use `http://127.0.0.1:3000` so `E2B_API_KEY` is not sent across the network in plaintext.

## Verify Cube

Run the smoke test before involving CrewAI's LLM:

```bash
python smoke_test.py
```

The smoke test should print this exact JSON payload:

```text
{"runtime": "cube", "sum": 45}
```

The smoke test exits non-zero if the parsed payload does not have `runtime == "cube"` and `sum == 45`.

## Run the Crew

```bash
python main.py
```

The agent must call `E2BPythonTool` to simulate dice rolls inside Cube and compare the result with `5/36`.

Set `CREWAI_VERBOSE=true` only when debugging locally. Verbose CrewAI logs can include provider configuration, so review logs before sharing them.

## Security Defaults

The example uses `persistent=False`, so each tool invocation gets a fresh MicroVM that is destroyed after use. Keep this default for untrusted inputs. If state must survive between calls, set `persistent=True`, use a short `sandbox_timeout`, and call `tool.close()` during shutdown.

For network isolation and host mounts, see the full [CrewAI integration guide](../../docs/guide/integrations/crewai.md).
