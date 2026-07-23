FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox

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
    && rm -rf /var/lib/apt/lists/*

COPY package/Cubelet ${TOOLBOX_ROOT}/Cubelet
COPY package/network-agent ${TOOLBOX_ROOT}/network-agent
COPY package/cube-shim ${TOOLBOX_ROOT}/cube-shim
COPY package/cube-kernel-scf ${TOOLBOX_ROOT}/cube-kernel-scf
COPY package/cube-image ${TOOLBOX_ROOT}/cube-image
COPY package/cube-vs ${TOOLBOX_ROOT}/cube-vs
COPY package/cube-snapshot ${TOOLBOX_ROOT}/cube-snapshot
COPY package/scripts/common ${TOOLBOX_ROOT}/scripts/common
COPY scripts/cube-node-entrypoint.sh /usr/local/bin/cube-node-entrypoint.sh
COPY scripts/stage-toolbox.sh /usr/local/bin/stage-toolbox.sh

RUN chmod +x \
      ${TOOLBOX_ROOT}/Cubelet/bin/cubelet \
      ${TOOLBOX_ROOT}/Cubelet/bin/cubecli \
      ${TOOLBOX_ROOT}/network-agent/bin/network-agent \
      ${TOOLBOX_ROOT}/network-agent/bin/cubevsmapdump \
      ${TOOLBOX_ROOT}/cube-shim/bin/cube-runtime \
      ${TOOLBOX_ROOT}/cube-shim/bin/containerd-shim-cube-rs \
      /usr/local/bin/cube-node-entrypoint.sh \
      /usr/local/bin/stage-toolbox.sh \
    && ln -sfn ${TOOLBOX_ROOT}/cube-shim/bin/containerd-shim-cube-rs /usr/local/bin/containerd-shim-cube-rs \
    && ln -sfn ${TOOLBOX_ROOT}/cube-shim/bin/cube-runtime /usr/local/bin/cube-runtime \
    && ln -sfn ${TOOLBOX_ROOT}/Cubelet/bin/cubecli /usr/local/bin/cubecli \
    && ln -sfn ${TOOLBOX_ROOT}/network-agent/bin/cubevsmapdump /usr/local/bin/cubevsmapdump \
    && mkdir -p \
      /data/cubelet \
      /data/log/Cubelet \
      /data/log/CubeShim \
      /data/log/CubeVmm \
      /data/cube-shim/disks \
      /data/snapshot_pack/disks \
      /tmp/cube

EXPOSE 9999 9998 9966 19090
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/cube-node-entrypoint.sh"]
