# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import os
from textwrap import dedent

from dotenv import load_dotenv
from e2b_code_interpreter import Sandbox

WORKSPACE_ROOT = "/workspace"
NOTEBOOK_DIR = f"{WORKSPACE_ROOT}/notebooks"
EXECUTED_NOTEBOOK_DIR = f"{WORKSPACE_ROOT}/artifacts"
DATA_DIR = f"{WORKSPACE_ROOT}/data"
NOTEBOOK_PATH = f"{NOTEBOOK_DIR}/jupyter_ml_workbench.ipynb"
SUMMARY_PATH = f"{EXECUTED_NOTEBOOK_DIR}/summary.json"
CHECKPOINT_PATH = f"{EXECUTED_NOTEBOOK_DIR}/pause_resume_checkpoint.json"


def load_env() -> None:
    load_dotenv()

    for key in ("E2B_API_URL", "E2B_API_KEY", "CUBE_TEMPLATE_ID"):
        if not os.environ.get(key):
            raise SystemExit(f"Missing env var: {key}")

    cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE")
    if cube_ssl and os.path.isfile(cube_ssl):
        os.environ["SSL_CERT_FILE"] = cube_ssl


def create_sandbox(*, template: str, timeout: int = 600):
    return Sandbox.create(template=template, timeout=timeout)


def sandbox_url(sandbox, port: int) -> str:
    return f"https://{sandbox.get_host(port)}"


def ensure_success(result, action: str) -> None:
    exit_code = getattr(result, "exit_code", None)
    if exit_code not in (None, 0):
        stdout = getattr(result, "stdout", "")
        stderr = getattr(result, "stderr", "")
        raise SystemExit(
            f"Failed to {action} (exit {exit_code}).\nSTDOUT:\n{stdout}\nSTDERR:\n{stderr}"
        )


def prepare_workspace(sandbox) -> None:
    ensure_success(
        sandbox.commands.run(
            f"mkdir -p {NOTEBOOK_DIR} {EXECUTED_NOTEBOOK_DIR} {DATA_DIR}"
        ),
        "prepare workspace",
    )


def write_text(sandbox, path: str, content: str) -> None:
    sandbox.files.write(path, content)


def build_workbench_notebook() -> str:
    notebook = {
        "cells": [
            {
                "cell_type": "markdown",
                "id": "overview",
                "metadata": {},
                "source": dedent(
                    """
                    # Jupyter ML Workbench

                    This notebook proves the template can run a notebook server,
                    execute pandas / scikit-learn / PyTorch code, and persist
                    artifacts under `/workspace/artifacts`.
                    """
                ).strip(),
            },
            {
                "cell_type": "code",
                "id": "iris-summary",
                "execution_count": None,
                "metadata": {},
                "outputs": [],
                "source": dedent(
                    """
                    from pathlib import Path

                    import matplotlib.pyplot as plt
                    import numpy as np
                    import pandas as pd
                    import torch
                    from sklearn.datasets import load_iris
                    from sklearn.linear_model import LogisticRegression
                    from sklearn.metrics import accuracy_score
                    from sklearn.model_selection import train_test_split

                    workspace = Path("/workspace")
                    artifacts = workspace / "artifacts"
                    artifacts.mkdir(parents=True, exist_ok=True)

                    iris = load_iris(as_frame=True)
                    df = iris.frame.copy()
                    df["label"] = df["target"].map(dict(enumerate(iris.target_names)))
                    grouped = df.groupby("label").mean(numeric_only=True)
                    print(grouped.round(2).to_string())

                    ax = grouped[["sepal length (cm)", "petal length (cm)"]].plot(
                        kind="bar",
                        figsize=(8, 4),
                        title="Mean iris feature values",
                    )
                    ax.set_xlabel("Class")
                    plt.tight_layout()
                    plt.savefig(artifacts / "iris_summary.png", dpi=160)
                    plt.show()
                    """
                ).strip(),
            },
            {
                "cell_type": "code",
                "id": "sklearn-model",
                "execution_count": None,
                "metadata": {},
                "outputs": [],
                "source": dedent(
                    """
                    from pathlib import Path

                    import pandas as pd
                    from sklearn.datasets import load_iris
                    from sklearn.linear_model import LogisticRegression
                    from sklearn.metrics import accuracy_score
                    from sklearn.model_selection import train_test_split

                    artifacts = Path("/workspace/artifacts")

                    iris = load_iris(as_frame=True)
                    df = iris.frame
                    X = df[[
                        "sepal length (cm)",
                        "sepal width (cm)",
                        "petal length (cm)",
                        "petal width (cm)",
                    ]]
                    y = df["target"]
                    X_train, X_test, y_train, y_test = train_test_split(
                        X,
                        y,
                        test_size=0.25,
                        random_state=42,
                        stratify=y,
                    )

                    model = LogisticRegression(max_iter=200, n_jobs=1)
                    model.fit(X_train, y_train)
                    prediction = model.predict(X_test)
                    accuracy = accuracy_score(y_test, prediction)
                    print(f"sklearn_accuracy={accuracy:.4f}")

                    (artifacts / "sklearn_accuracy.txt").write_text(
                        f"{accuracy:.4f}\\n",
                        encoding="utf-8",
                    )
                    """
                ).strip(),
            },
            {
                "cell_type": "code",
                "id": "torch-model",
                "execution_count": None,
                "metadata": {},
                "outputs": [],
                "source": dedent(
                    """
                    import json
                    from pathlib import Path

                    import numpy as np
                    import torch

                    artifacts = Path("/workspace/artifacts")

                    rng = np.random.default_rng(42)
                    x = torch.tensor(
                        rng.uniform(-2.0, 2.0, size=(128, 1)),
                        dtype=torch.float32,
                    )
                    y = 3.0 * x - 1.5 + 0.1 * torch.randn_like(x)

                    model = torch.nn.Linear(1, 1)
                    optimizer = torch.optim.SGD(model.parameters(), lr=0.1)
                    loss_fn = torch.nn.MSELoss()

                    for _ in range(200):
                        optimizer.zero_grad()
                        loss = loss_fn(model(x), y)
                        loss.backward()
                        optimizer.step()

                    summary = {
                        "torch_loss": float(loss.item()),
                        "torch_weight": float(model.weight.detach().item()),
                        "torch_bias": float(model.bias.detach().item()),
                    }

                    torch.save(model.state_dict(), artifacts / "torch_linear.pt")
                    (artifacts / "summary.json").write_text(
                        json.dumps(summary, indent=2),
                        encoding="utf-8",
                    )
                    print(json.dumps(summary, indent=2))
                    """
                ).strip(),
            },
        ],
        "metadata": {
            "kernelspec": {
                "display_name": "Python 3",
                "language": "python",
                "name": "python3",
            },
            "language_info": {
                "name": "python",
                "version": "3.10",
            },
        },
        "nbformat": 4,
        "nbformat_minor": 5,
    }
    return json.dumps(notebook, ensure_ascii=False, indent=2)
