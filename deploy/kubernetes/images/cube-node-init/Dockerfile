FROM ubuntu:22.04

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

COPY scripts/node-prep-lib.sh /scripts/node-prep-lib.sh
COPY scripts/cube-node-init.sh /scripts/cube-node-init.sh

RUN chmod +x /scripts/cube-node-init.sh \
    && chmod 644 /scripts/node-prep-lib.sh

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/scripts/cube-node-init.sh"]
