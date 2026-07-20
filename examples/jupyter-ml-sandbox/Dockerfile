# syntax=docker/dockerfile:1.7
#
# Jupyter ML workbench template for CubeSandbox.
# The base image keeps envd running on :49983; this image starts JupyterLab on
# :8888 so the template can be probed via /api/status and used from a browser.
#
# Build:
#   docker build --platform linux/amd64 \
#     -t cubesandbox-jupyter-ml:latest examples/jupyter-ml-sandbox
#
# Run locally:
#   docker run --rm -d -p 8888:8888 -p 49983:49983 \
#     --name cube-jupyter-ml cubesandbox-jupyter-ml:latest
#   curl -s http://127.0.0.1:8888/api/status | jq .
#   curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

ENV VIRTUAL_ENV=/opt/jupyter-ml
ENV PATH="${VIRTUAL_ENV}/bin:${PATH}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        python3 \
        python3-pip \
        python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m venv "${VIRTUAL_ENV}" \
    && pip install --no-cache-dir --upgrade pip \
    && pip install --no-cache-dir \
        jupyterlab \
        nbconvert \
        nbformat \
        ipykernel \
        pandas \
        matplotlib \
        scikit-learn \
        seaborn \
    && pip install --no-cache-dir \
        --extra-index-url https://download.pytorch.org/whl/cpu \
        torch==2.4.1

COPY --chmod=755 jupyter_start.sh /usr/local/bin/jupyter_start.sh

EXPOSE 8888 49983

CMD ["/usr/local/bin/jupyter_start.sh"]
