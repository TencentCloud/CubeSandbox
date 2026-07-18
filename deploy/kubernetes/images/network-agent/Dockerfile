FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=network-agent

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      iproute2 \
      iptables \
      iputils-ping \
      kmod \
      net-tools \
      procps \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY package/network-agent ${IMAGE_ROOT}/network-agent
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

ARG CUBE_VERSION=unknown
RUN printf '{"schema_version":1,"components":{"network-agent":{"version":"%s"}}}\n' \
      "${CUBE_VERSION}" > ${IMAGE_ROOT}/network-agent/version.json \
    && chmod +x \
      ${IMAGE_ROOT}/network-agent/bin/network-agent \
      ${IMAGE_ROOT}/network-agent/bin/cubevsmapdump \
      /usr/local/bin/component-entrypoint.sh \
    && ln -sfn ${IMAGE_ROOT}/network-agent/bin/cubevsmapdump /usr/local/bin/cubevsmapdump \
    && mkdir -p /data/cubelet /data/log /tmp/cube /run/cube-node

EXPOSE 19090
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
