# examples/langgraph-integration/Dockerfile
# Stacks a Python data-science stack on top of cubesandbox-base so LangGraph
# agents can execute pandas / numpy / matplotlib code inside the MicroVM.
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive

# Pin data-science versions for reproducible builds. Bump as needed.
# Ubuntu 22.04's apt python3-pip is 22.0.2, which predates the
# --break-system-packages flag (added in pip 23.0), so upgrade pip first, then
# install with the flag.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        python3 python3-pip ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m pip install --no-cache-dir --upgrade pip \
    && python3 -m pip install --no-cache-dir --break-system-packages \
        pandas==2.2.3 numpy==1.26.4 matplotlib==3.9.3 scikit-learn==1.6.1

WORKDIR /workspace

# Pre-seed a demo dataset so the example agent can run without external inputs.
COPY sales.csv /workspace/sales.csv

EXPOSE 49983
