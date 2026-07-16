FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cubelet

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      iproute2 \
      iptables \
      iputils-ping \
      kmod \
      lsof \
      net-tools \
      procps \
      psmisc \
      e2fsprogs \
      tini \
      util-linux \
      xfsprogs \
      iputils-ping \
    && rm -rf /var/lib/apt/lists/*

COPY package/Cubelet ${IMAGE_ROOT}/Cubelet
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

RUN chmod +x \
      ${IMAGE_ROOT}/Cubelet/bin/cubelet \
      ${IMAGE_ROOT}/Cubelet/bin/cubecli \
      /usr/local/bin/component-entrypoint.sh \
    && mkdir -p \
      /data/cubelet \
      /data/log/Cubelet \
      /data/log/CubeShim \
      /data/log/CubeVmm \
      /data/cube-shim/disks \
      /data/snapshot_pack/disks \
      /tmp/cube \
      /run/cube-node

EXPOSE 9999 9998 9966
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
