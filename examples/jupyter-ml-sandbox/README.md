# Jupyter ML Sandbox

[中文文档](README_zh.md)

Run a JupyterLab-based data science and ML workbench inside CubeSandbox.
This template ships with pandas, matplotlib, scikit-learn, seaborn, and a CPU
PyTorch stack, so you can open a notebook in the browser, execute it, and keep
the generated artifacts inside the sandbox workspace.

## What it shows

- JupyterLab exposed on port `8888`
- envd still running on port `49983`
- notebook execution with `nbconvert`
- a small pause/resume state check
- notebook artifacts persisted under `/workspace/artifacts`

## Prerequisites

- Python 3.10+
- A running CubeSandbox deployment

```bash
pip install -r requirements.txt
```

## Build the image

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/cubesandbox-jupyter-ml:latest .
docker push <your-registry>/cubesandbox-jupyter-ml:latest
```

## Register the template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-jupyter-ml:latest \
  --writable-layer-size 4G \
  --expose-port 8888 \
  --expose-port 49983 \
  --probe 8888 \
  --probe-path /api/status
```

The template is ready once JupyterLab answers `/api/status` with HTTP 200.

## Configure the host demo

```bash
cp .env.example .env
# edit .env and fill in E2B_API_URL, E2B_API_KEY, and CUBE_TEMPLATE_ID
```

## Run the demos

```bash
python notebook_demo.py
python pause_resume_demo.py
```

The notebook demo prints the JupyterLab URL and the generated artifact list.
The pause/resume demo verifies that a file created before `pause()` is still
present after `connect()`.

## Directory structure

```
jupyter-ml-sandbox/
├── Dockerfile
├── README.md
├── README_zh.md
├── common.py
├── jupyter_start.sh
├── notebook_demo.py
├── pause_resume_demo.py
├── requirements.txt
└── .env.example
```
