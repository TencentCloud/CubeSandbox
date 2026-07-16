FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cube-shim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY package/cube-shim ${IMAGE_ROOT}/cube-shim
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

RUN chmod +x \
      ${IMAGE_ROOT}/cube-shim/bin/cube-runtime \
      ${IMAGE_ROOT}/cube-shim/bin/containerd-shim-cube-rs \
      /usr/local/bin/component-entrypoint.sh

ENV CUBE_ROLE=install
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
