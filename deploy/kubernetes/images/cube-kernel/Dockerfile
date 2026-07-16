FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cube-kernel
ENV CUBE_ROLE=install

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY package/cube-kernel-scf ${IMAGE_ROOT}/cube-kernel-scf
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

RUN chmod +x /usr/local/bin/component-entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
