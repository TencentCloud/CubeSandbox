FROM ubuntu:20.04

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      e2fsprogs \
      iproute2 \
      kmod \
      mount \
      tini \
      util-linux \
      xfsprogs \
    && rm -rf /var/lib/apt/lists/*

COPY scripts/cube-node-init.sh /scripts/cube-node-init.sh

RUN chmod +x /scripts/cube-node-init.sh

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/scripts/cube-node-init.sh"]
