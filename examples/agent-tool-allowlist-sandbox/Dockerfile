# syntax=docker/dockerfile:1.7
#
# Restricted toolbox BYOI: cubesandbox-base + cube-tool + toolbox-hello workload.
# Happy path on the host: python run.py
#
# Inherited cube-entrypoint keeps envd on :49983 (/health). This Dockerfile
# only EXPOSEs that port — it does not add a separate health server.
#
#   docker build -t agent-tool-allowlist-sandbox:latest .
#
# cubesandbox-base already ships coreutils and curl.

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG INSTALL_CURL=0

RUN mkdir -p /etc/cube-sandbox /workspace/out /usr/local/bin \
    && if [ "$INSTALL_CURL" = "1" ]; then \
         apt-get update \
         && apt-get install -y --no-install-recommends curl \
         && rm -rf /var/lib/apt/lists/*; \
       fi

COPY tool-profile.txt /etc/cube-sandbox/tool-profile.txt
COPY guest/cube-tool /usr/local/bin/cube-tool
COPY guest/toolbox-hello /usr/local/bin/toolbox-hello
COPY workspace/ /workspace/

# Base image default user is ``user`` (uid 1000).
RUN sed -i 's/\r$//' \
      /usr/local/bin/cube-tool \
      /usr/local/bin/toolbox-hello \
      /etc/cube-sandbox/tool-profile.txt \
    && chmod 0755 /usr/local/bin/cube-tool /usr/local/bin/toolbox-hello \
    && chown -R 1000:1000 /workspace \
    && /usr/local/bin/cube-tool echo "cube-tool-ok" \
    && /usr/local/bin/cube-tool toolbox-hello | grep -q WORKLOAD_OK \
    && if /usr/local/bin/cube-tool bash -c id; then echo "cube-tool must deny bash" >&2; exit 1; fi \
    && test -f /workspace/README.txt

WORKDIR /workspace
ENV CUBE_TOOL_PROFILE=/etc/cube-sandbox/tool-profile.txt

EXPOSE 49983
