# Gemini CLI + CubeSandbox

This example runs [Gemini CLI](https://github.com/google-gemini/gemini-cli) inside a hardware-isolated CubeSandbox MicroVM.

- `run_gemini.py`: one-shot coding task with host-side environment injection.
- `resume_gemini.py`: pause/resume workflow that proves `/workspace` survives the snapshot.
- `network_policy.py`: default-deny egress plus CubeEgress `x-goog-api-key` injection; the real key never enters the VM.

## 1. Build and register the template

```bash
cd examples/gemini-cli-integration
chmod +x build-template.sh
IMAGE=registry.example.com/cube/gemini-cli:2026-07-10 ./build-template.sh
```

Set the template ID printed by `cubemastercli` in `.env`:

```bash
cp .env.example .env
# Edit E2B_API_URL, E2B_API_KEY, CUBE_TEMPLATE_ID, GEMINI_API_KEY
python3 -m pip install -r requirements.txt
```

## 2. Run a one-shot task

```bash
python3 run_gemini.py --approve-all
```

`--approve-all` explicitly enables Gemini CLI's `--yolo` mode. Leave it off for read-only or approval-gated workloads.

## 3. Verify pause/resume persistence

```bash
python3 resume_gemini.py --approve-all
```

The script has Gemini create `plan.md`, pauses the sandbox, reconnects to the same sandbox, verifies the file, then asks Gemini to create `progress.md`.

## 4. Use the credential-vault path

```bash
python3 network_policy.py --approve-all
```

This path creates the sandbox with `allow_internet_access=False` and permits only `generativelanguage.googleapis.com`. CubeEgress injects the real `x-goog-api-key` header on the allowed request. The in-VM process receives only a placeholder key.

## Validation

```bash
python3 -m unittest test_common.py
python3 -m py_compile common.py run_gemini.py resume_gemini.py network_policy.py
bash -n build-template.sh
docker build -t gemini-cli-cube:local .
```

A live end-to-end run additionally requires a CubeSandbox cluster, a registered template, and a Google AI Studio API key. Do not commit `.env` or place API keys in the Docker image.
