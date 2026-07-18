FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cube-guest
ENV CUBE_ROLE=install

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY package/cube-image ${IMAGE_ROOT}/cube-image
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

ARG CUBE_VERSION=unknown
RUN set -eu; \
    img_ver="$(tr -d '[:space:]' < ${IMAGE_ROOT}/cube-image/version 2>/dev/null || true)"; \
    agent_ver="$(tr -d '[:space:]' < ${IMAGE_ROOT}/cube-image/agent-version 2>/dev/null || true)"; \
    : "${img_ver:=${CUBE_VERSION}}"; \
    { \
      printf '{\n  "schema_version": 1,\n  "components": {\n'; \
      printf '    "guest-image": {"version": "%s"}' "${img_ver}"; \
      if [ -n "${agent_ver}" ]; then \
        printf ',\n    "cube-agent": {"version": "%s"}' "${agent_ver}"; \
      fi; \
      printf '\n  }\n}\n'; \
    } > ${IMAGE_ROOT}/cube-image/version.json \
    && chmod +x /usr/local/bin/component-entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
