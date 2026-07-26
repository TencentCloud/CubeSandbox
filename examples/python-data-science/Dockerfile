# syntax=docker/dockerfile:1.7
#
# A Cube-ready sandbox image preconfigured for Python Data Science & AI Code Interpretation.
# It includes:
#   - Python3 & pip
#   - Core science stack: numpy, pandas, matplotlib, seaborn, scipy
#   - Chinese fonts (WenQuanYi Zen Hei) to prevent Matplotlib rendering square blocks for Chinese characters
#
# Build:
#   docker build -t cubesandbox-data-science:latest examples/python-data-science
#

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:latest@sha256:34ea312a63a5534e66ab17005c23d7fbaf33c38bccd5411ee402d901e63a3193
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG PIP_VERSION=24.3.1
ARG NUMPY_VERSION=1.26.4
ARG PANDAS_VERSION=2.2.3
ARG MATPLOTLIB_VERSION=3.9.2
ARG SEABORN_VERSION=0.13.2
ARG SCIPY_VERSION=1.14.1
ARG TABULATE_VERSION=0.9.0

# Install core packages, Python build tools, and Chinese fonts
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pip \
    python3-dev \
    build-essential \
    libfreetype6-dev \
    libpng-dev \
    fonts-wqy-zenhei \
    && rm -rf /var/lib/apt/lists/*

# Install a reproducible data science suite.
RUN pip3 install --no-cache-dir --upgrade "pip==${PIP_VERSION}" && \
    pip3 install --no-cache-dir \
    "numpy==${NUMPY_VERSION}" \
    "pandas==${PANDAS_VERSION}" \
    "matplotlib==${MATPLOTLIB_VERSION}" \
    "seaborn==${SEABORN_VERSION}" \
    "scipy==${SCIPY_VERSION}" \
    "tabulate==${TABULATE_VERSION}" && \
    apt-get purge -y --auto-remove \
    python3-dev \
    build-essential \
    libfreetype6-dev \
    libpng-dev && \
    rm -rf /var/lib/apt/lists/*

EXPOSE 49983
